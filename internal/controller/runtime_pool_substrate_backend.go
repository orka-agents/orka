/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

// Substrate-backed RuntimePools (execution-workspace Phase 2) host the pool's
// single runtime instance inside a gVisor-isolated Substrate Actor reached
// through the provider router, instead of a controller-owned Deployment or an
// Agent Sandbox claim. The runtime container is always controller-rendered:
// the operator-owned base ActorTemplate contributes only infrastructure
// (worker pool placement, runsc build, snapshot location), and a derived,
// controller-owned ActorTemplate carries the immutable runtime image, the
// fence environment, and read-once bootstrap secret references. Prompt-level
// operations use the unchanged orka.harness.v2 protocol against the
// ActiveInstance; only workload materialization and endpoint dialing differ.
//
// Suspension is prohibited: gVisor suspension checkpoints supervisor process
// memory — including live pool and provider-proxy credentials — into provider
// snapshot storage. This backend therefore never calls SuspendActor, tears
// down with DeleteActor directly, and recycles the exact instance whenever the
// provider reports a suspension or a snapshot for a booted actor. Actor
// suspend/resume and snapshot restore remain fail-closed until the provider
// offers credential-safe sessions.

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

const (
	runtimePoolSubstrateTemplateSuffix = "actor-template"
	runtimePoolSubstrateActorSuffix    = "actor"

	// substrateActorBootedAnnotation records the exact actor ID whose workload
	// this pool booted from scratch. It makes boot idempotent across controller
	// restarts and lets suspension detection ignore the provider's initial
	// created-but-never-resumed state.
	substrateActorBootedAnnotation = "orka.ai/substrate-actor-booted"
)

// +kubebuilder:rbac:groups=ate.dev,resources=actortemplates,verbs=get;list;watch;create;update;patch;delete

var substrateActorTemplateGVK = schema.GroupVersionKind{Group: "ate.dev", Version: "v1alpha1", Kind: "ActorTemplate"}

func runtimePoolSubstrateTemplateName(base string) string {
	return runtimePoolChildName(base, runtimePoolSubstrateTemplateSuffix)
}

func runtimePoolSubstrateActorID(base string) string {
	return runtimePoolChildName(base, runtimePoolSubstrateActorSuffix)
}

func substrateActorInstanceUID(actorID string) string {
	return "actor:" + actorID
}

func substrateActorRouteHost(actorID, dnsSuffix string) string {
	return actorID + "." + strings.Trim(strings.TrimSpace(dnsSuffix), ".")
}

func runtimePoolIsSubstrateBacked(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && pool.Spec.ExecutionWorkspace != nil &&
		pool.Spec.ExecutionWorkspace.Provider == corev1alpha1.WorkspaceProviderSubstrate
}

func (r *RuntimePoolReconciler) substrateActorControl() (workspace.SubstrateRuntimeActorControl, error) {
	if !r.SubstrateEnabled {
		return nil, fmt.Errorf("substrate provider is disabled; Substrate-backed workspace RuntimePools fail closed")
	}
	factory := r.SubstrateActorControlFactory
	if factory == nil {
		factory = defaultSubstrateRuntimeActorControlFactory
	}
	return factory(r.SubstrateConfig)
}

func defaultSubstrateRuntimeActorControlFactory(cfg SubstrateConfig) (workspace.SubstrateRuntimeActorControl, error) {
	return workspace.NewSubstrateRuntimeActorControl(workspace.SubstrateConfig{
		APIEndpoint:           cfg.APIEndpoint,
		APICAFile:             cfg.APICAFile,
		APIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
	})
}

// reconcileSubstrateBackedRuntimePool converges a Substrate-backed pool. It
// mirrors the Deployment and Agent Sandbox paths exactly, replacing only
// workload materialization and endpoint dialing.
//
//nolint:gocyclo // Workload materialization decisions stay auditable together, mirroring the other backends.
func (r *RuntimePoolReconciler) reconcileSubstrateBackedRuntimePool(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) (ctrl.Result, error) {
	substrateSpec := pool.Spec.ExecutionWorkspace.Substrate
	templateNamespace := substrateSpec.BaseTemplateNamespace
	// Provider-resolved secretKeyRef environment is looked up in the template's
	// namespace, so the epoch-scoped pool Secrets are created there.
	secretsCfg := cfg
	secretsCfg.namespace = templateNamespace
	authSecret, providerSecret, err := r.ensureRuntimePoolSecrets(ctx, pool, secretsCfg)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	baseTemplate, err := r.getSubstrateActorTemplate(ctx, templateNamespace, substrateSpec.BaseTemplateName)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if baseTemplate == nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf(
			"substrate infrastructure ActorTemplate %s/%s is not found",
			templateNamespace, substrateSpec.BaseTemplateName,
		))
	}
	actorID := runtimePoolSubstrateActorID(cfg.baseName)
	routeHost := substrateActorRouteHost(actorID, r.SubstrateConfig.ActorDNSSuffix)
	desired, err := r.renderSubstrateRuntimeTemplate(pool, cfg, baseTemplate, templateNamespace, actorID, authSecret.Name, providerSecret.Name)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	derivedTemplate, err := r.getSubstrateActorTemplate(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	control, err := r.substrateActorControl()
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	defer control.Close() //nolint:errcheck // best-effort connection teardown
	actor, err := control.GetActor(ctx, actorID)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}

	replicas := int32(0)
	if actor != nil {
		replicas = 1
	}
	status := r.baseRuntimePoolStatus(pool, replicas)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionPodSecurityReady, metav1.ConditionTrue, "ProviderIsolated", "provider-owned gVisor isolation hosts the runtime workload")
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionQuotaReady, metav1.ConditionTrue, "ResourcesAdmitted", "provider worker capacity admitted the runtime workload")

	booted := pool.Annotations[substrateActorBootedAnnotation] == actorID
	if actor != nil && booted && (actor.SuspendedOrSuspending() || actor.SnapshotObserved) {
		// A booted supervisor holds live pool and provider-proxy credentials in
		// process memory; a provider-side suspension or snapshot has therefore
		// captured state this backend prohibits. Recycle the exact instance.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider suspended or snapshotted the runtime actor; suspension is prohibited for ACP RuntimeSessions and the exact instance is being recycled"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if derivedTemplate != nil && substrateRuntimeTemplateNeedsRollout(derivedTemplate, desired.revision) {
		return r.reconcileSubstrateRuntimePoolRollout(ctx, pool, cfg, control, derivedTemplate, actor, actorID, routeHost, templateNamespace, desired, status)
	}

	if pool.Spec.DesiredReplicas == 0 {
		return r.reconcileSubstrateRuntimePoolScaleDown(ctx, pool, cfg, control, actor, actorID, routeHost, templateNamespace, authSecret, status)
	}

	if derivedTemplate == nil {
		if err := r.createSubstrateActorTemplate(ctx, pool, desired.object); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if derivedTemplate, err = r.getSubstrateActorTemplate(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName)); err != nil || derivedTemplate == nil {
			if err == nil {
				err = fmt.Errorf("created RuntimePool substrate actor template is not readable yet")
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
	}
	if err := r.pruneStaleSubstrateRuntimePoolSecrets(ctx, pool, secretsCfg, derivedTemplate, authSecret.Name, providerSecret.Name); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}

	if actor == nil {
		if _, err := control.CreateActor(ctx, actorID, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName)); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		// Boot from scratch exactly once per actor lifetime: a supervisor
		// lifetime is exactly one boot, so the fence boot ID is never resumed
		// from a snapshot.
		if _, err := control.ResumeActor(ctx, actorID, true); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if err := r.setSubstrateActorBootedAnnotation(ctx, pool, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider actor is booting the runtime workload"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !booted {
		if !actor.Running() {
			if _, err := control.ResumeActor(ctx, actorID, true); err != nil {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
		if err := r.setSubstrateActorBootedAnnotation(ctx, pool, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider actor boot is being recorded before exact-instance admission"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !actor.Running() {
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "waiting for the provider actor workload to run"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	syntheticPod := substrateSyntheticInstancePod(pool, cfg, templateNamespace, actor, actorID, routeHost)
	return r.reconcileRuntimePoolServing(ctx, pool, cfg, []corev1.Pod{*syntheticPod}, []corev1.Pod{*syntheticPod}, authSecret, status)
}

// substrateSyntheticInstancePod adapts the provider actor into the shared
// exact-instance selection shape. Its profile and provider-token-generation
// annotations mirror the pool intent rather than independently observed Pod
// state; the authoritative verification for Substrate is the derived-template
// revision rollout plus the authenticated probe fence.
func substrateSyntheticInstancePod(
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	templateNamespace string,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost string,
) *corev1.Pod {
	namespace := actor.PodNamespace
	if namespace == "" {
		namespace = templateNamespace
	}
	name := actor.PodName
	if name == "" {
		name = actorID
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       types.UID(substrateActorInstanceUID(actorID)),
			Annotations: map[string]string{
				runtimePoolProfileAnnotation:                 pool.Spec.Runtime.Profile.Digest,
				runtimePoolProviderTokenGenerationAnnotation: cfg.providerProxy.tokenGeneration,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: routeHost},
	}
}

// reconcileSubstrateRuntimePoolRollout mirrors the Recreate rollout barriers:
// authenticated drain of the exact old instance validated against the deployed
// derived-template identity, persisted quiescence, actor deletion, and only
// then the new immutable template.
//
//nolint:gocyclo // The rollout barriers intentionally mirror the audited Deployment path.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolRollout(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	control workspace.SubstrateRuntimeActorControl,
	derivedTemplate *unstructured.Unstructured,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost, templateNamespace string,
	desired substrateRuntimeTemplateRender,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	status.Message = "runtime template changed; admission is closed before provider actor replacement"
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)

	if actor == nil {
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		if err := r.updateSubstrateActorTemplate(ctx, derivedTemplate, desired.object); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		status.ActiveInstance = nil
		if pool.Spec.DesiredReplicas == 0 {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
			status.Message = "new runtime template is staged with no provider actor demand"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "RolloutConverged", status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.Message = "old provider actor terminated; starting the new immutable runtime template"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStarting, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if !actor.Running() {
		if pool.Status.ActiveInstance != nil {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "cannot authenticate the previous active runtime instance before actor replacement"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if !runtimePoolRolloutControllerWorkIsQuiescent(status.Capacity) {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
			status.Message = "waiting for controller reservations or finalization work before replacing an unadmitted provider actor"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "replacing an unadmitted provider actor before applying the new template"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	deployedTemplate, err := substrateTemplatePodTemplateSpec(derivedTemplate)
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	validationPool, validationConfig, err := runtimePoolPodTemplateValidationTarget(pool, deployedTemplate)
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	deployedAuthSecret, err := r.substrateTemplateAuthSecret(ctx, templateNamespace, deployedTemplate)
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	deployedSyntheticPod := substrateSyntheticInstancePod(validationPool, validationConfig, templateNamespace, actor, actorID, routeHost)
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(pool, deployedSyntheticPod), string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]), deployedAuthSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated rollout status probe failed: %w", err))
	}
	active, err := validateRuntimePoolProbeForRollout(validationPool, validationConfig, deployedSyntheticPod, probe, r.now())
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	if runtimePoolSupervisorRestartedInPlace(pool.Status.ActiveInstance, active) {
		return r.reconcileRuntimePoolInPlaceSupervisorRestart(ctx, pool, nil, deployedSyntheticPod, active, status)
	}
	if !runtimePoolRolloutActiveInstanceMatches(pool.Status.ActiveInstance, active) {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated runtime identity changed before rollout drain"))
	}
	status.ActiveInstance = active
	applyRuntimePoolProbeCapacity(&status, cfg, probe)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionTrue, "PodScheduled", "selected provider actor remains placed during rollout drain")

	if !probe.Status.Drain.Requested {
		reason := "runtime_pool_rollout_" + runtimePoolShortRevision(desired.revision)
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(pool, deployedSyntheticPod),
			string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]),
			deployedAuthSecret.Data[runtimePoolCapabilitySecretKey],
			probe.Status,
			reason,
		); err != nil {
			return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated rollout drain request failed: %w", err))
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.Message = runtimePoolMessageRolloutDrainRequested
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if !runtimePoolRolloutProbeIsQuiescent(status.Capacity, probe.Status) {
		if r.runtimePoolRolloutTimedOut(pool) {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "timed out waiting for authenticated rollout drain barriers; preserving the old provider actor"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, runtimePoolRolloutReasonTimedOut, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.Message = runtimePoolMessageRolloutSettling
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if !runtimePoolRolloutQuiescencePersisted(pool) {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleQuiescent
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageRolloutQuiescent
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonQuiescent, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent old provider actor is stopping before the template changes"
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// reconcileSubstrateRuntimePoolScaleDown mirrors the scale-to-zero barriers:
// authenticated drain, observed quiescence, a persisted Quiescent barrier,
// then direct actor deletion. Suspension is never used as a stop mechanism.
//
//nolint:gocyclo // The scale-down barriers intentionally mirror the audited Deployment path.
func (r *RuntimePoolReconciler) reconcileSubstrateRuntimePoolScaleDown(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	control workspace.SubstrateRuntimeActorControl,
	actor *workspace.SubstrateRuntimeActor,
	actorID, routeHost, templateNamespace string,
	authSecret *corev1.Secret,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "runtime pool is scaling down")

	if actor == nil {
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
		status.ActiveInstance = nil
		status.Message = runtimePoolMessageStopped
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionUnknown, "ScaledToZero", status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ScaledToZero", status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if !actor.Running() {
		if pool.Status.ActiveInstance == nil && runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) {
			if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
				return ctrl.Result{}, err
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "stopping a provider actor that is not running"
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = runtimePoolMessageDrainUnauthenticated
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	syntheticPod := substrateSyntheticInstancePod(pool, cfg, templateNamespace, actor, actorID, routeHost)
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(pool, syntheticPod), string(authSecret.Data[runtimePoolControllerTokenKey]), authSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage("authenticated drain status probe failed: " + err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	active, err := validateRuntimePoolProbe(pool, cfg, syntheticPod, probe, r.now())
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage(err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	status.ActiveInstance = active
	applyRuntimePoolProbeCapacity(&status, cfg, probe)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionTrue, "PodScheduled", "selected provider actor is placed")
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ExactInstanceReady", "selected provider actor and supervisor profile are ready")

	if !probe.Status.Drain.Requested {
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(pool, syntheticPod),
			string(authSecret.Data[runtimePoolControllerTokenKey]),
			authSecret.Data[runtimePoolCapabilitySecretKey],
			probe.Status,
			"runtime_pool_scale_to_zero",
		); err != nil {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = sanitizeRuntimePoolMessage("authenticated drain request failed: " + err.Error())
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainRequested
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if !runtimePoolProbeIsQuiescent(pool.Status.Capacity, probe.Status) {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainSettling
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleQuiescent {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleQuiescent
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainQuiescent
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent provider actor is stopping"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// recycleSubstrateActor deletes the exact actor directly (never via suspend)
// and clears the boot record so the replacement boots from scratch.
func (r *RuntimePoolReconciler) recycleSubstrateActor(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
) error {
	if err := control.DeleteActor(ctx, actorID); err != nil {
		return fmt.Errorf("delete RuntimePool substrate actor: %w", err)
	}
	return r.setSubstrateActorBootedAnnotation(ctx, pool, "")
}

func (r *RuntimePoolReconciler) setSubstrateActorBootedAnnotation(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actorID string,
) error {
	current := pool.Annotations[substrateActorBootedAnnotation]
	if current == actorID || (actorID == "" && current == "") {
		return nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if actorID == "" {
		delete(pool.Annotations, substrateActorBootedAnnotation)
	} else {
		pool.Annotations[substrateActorBootedAnnotation] = actorID
	}
	if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record RuntimePool substrate actor boot: %w", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) getSubstrateActorTemplate(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, template); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if apimeta.IsNoMatchError(err) || k8sRuntimeIsMissingKindError(err) {
			return nil, fmt.Errorf("read substrate ActorTemplate: the Substrate provider CRDs are not installed; Substrate-backed RuntimePools require an externally operated Substrate installation")
		}
		return nil, fmt.Errorf("read substrate ActorTemplate: %w", err)
	}
	return template, nil
}

func (r *RuntimePoolReconciler) createSubstrateActorTemplate(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	desired *unstructured.Unstructured,
) error {
	template := desired.DeepCopy()
	if err := r.setRuntimePoolControllerReference(pool, template); err != nil {
		return err
	}
	if err := r.Create(ctx, template); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RuntimePool substrate actor template: %w", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) updateSubstrateActorTemplate(
	ctx context.Context,
	template *unstructured.Unstructured,
	desired *unstructured.Unstructured,
) error {
	if template == nil {
		return fmt.Errorf("RuntimePool substrate actor template is required for a template update")
	}
	base := template.DeepCopy()
	template.Object["spec"] = desired.Object["spec"]
	template.SetLabels(desired.GetLabels())
	template.SetAnnotations(desired.GetAnnotations())
	if err := r.Patch(ctx, template, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("update RuntimePool substrate actor template: %w", err)
	}
	return nil
}

type substrateRuntimeTemplateRender struct {
	object   *unstructured.Unstructured
	revision string
}

// renderSubstrateRuntimeTemplate derives the controller-owned runtime
// ActorTemplate: operator infrastructure fields are copied verbatim from the
// base template, and the single runtime container is the exact canonical
// supervisor container with fence identity literals and read-once bootstrap
// secret references replacing Kubernetes-only mounts and field refs.
func (r *RuntimePoolReconciler) renderSubstrateRuntimeTemplate(
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	baseTemplate *unstructured.Unstructured,
	templateNamespace, actorID, authSecretName, providerSecretName string,
) (substrateRuntimeTemplateRender, error) {
	baseSpec, found, err := unstructured.NestedMap(baseTemplate.Object, "spec")
	if err != nil || !found {
		return substrateRuntimeTemplateRender{}, fmt.Errorf("substrate infrastructure ActorTemplate %s/%s has no readable spec", templateNamespace, baseTemplate.GetName())
	}
	infrastructure := k8sruntime.DeepCopyJSON(baseSpec)
	delete(infrastructure, "containers")

	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	canonical := r.runtimePoolPodTemplate(pool, cfg, selector, authSecretName, providerSecretName)
	container := substrateRuntimeContainer(canonical.Spec.Containers[0], templateNamespace, actorID, authSecretName, providerSecretName)

	containerMap, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&container)
	if err != nil {
		return substrateRuntimeTemplateRender{}, fmt.Errorf("render substrate runtime container: %w", err)
	}
	if resources, ok := containerMap["resources"].(map[string]any); ok && len(resources) == 0 {
		delete(containerMap, "resources")
	}
	spec := infrastructure
	spec["containers"] = []any{containerMap}

	labels := mergeStringMap(cloneStringMap(cfg.labels), map[string]string{
		"orka.ai/execution-workspace": scheduledRunLabelValue,
		"orka.ai/workspace-provider":  string(corev1alpha1.WorkspaceProviderSubstrate),
	})
	annotations := map[string]string{
		runtimePoolProfileAnnotation:                 pool.Spec.Runtime.Profile.Digest,
		runtimePoolProviderTokenGenerationAnnotation: cfg.providerProxy.tokenGeneration,
		runtimePoolPIDsAnnotation:                    "4096",
	}
	revision, err := substrateRuntimeTemplateRevision(container, labels, annotations, infrastructure)
	if err != nil {
		return substrateRuntimeTemplateRender{}, err
	}
	annotations[runtimePoolTemplateRevisionAnnotation] = revision

	object := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	object.SetGroupVersionKind(substrateActorTemplateGVK)
	object.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
	object.SetNamespace(templateNamespace)
	object.SetLabels(labels)
	object.SetAnnotations(annotations)
	return substrateRuntimeTemplateRender{object: object, revision: revision}, nil
}

// substrateRuntimeContainer adapts the canonical supervisor container to
// provider hosting: Kubernetes downward-API field refs become exact literals,
// Secret file mounts become read-once bootstrap secret references, and
// Pod-only surfaces (mounts, probes, security context) are dropped because the
// provider's gVisor sandbox owns them.
func substrateRuntimeContainer(
	canonical corev1.Container,
	templateNamespace, actorID, authSecretName, providerSecretName string,
) corev1.Container {
	container := *canonical.DeepCopy()
	container.VolumeMounts = nil
	container.SecurityContext = nil
	container.StartupProbe = nil
	container.ReadinessProbe = nil
	container.LivenessProbe = nil
	container.Lifecycle = nil
	container.Resources = corev1.ResourceRequirements{}
	container.Ports = []corev1.ContainerPort{{ContainerPort: runtimePoolPort, Protocol: corev1.ProtocolTCP}}

	env := make([]corev1.EnvVar, 0, len(container.Env)+3)
	for _, item := range container.Env {
		switch item.Name {
		case "ORKA_ACP_POD_UID":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: substrateActorInstanceUID(actorID)})
		case "ORKA_ACP_POD_NAME":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: actorID})
		case "ORKA_ACP_POD_NAMESPACE":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: templateNamespace})
		case "ORKA_ACP_CONTROLLER_TOKEN_FILE", "ORKA_ACP_CAPABILITY_SECRET_FILE", "ORKA_ACP_PROVIDER_TOKEN_FILE":
			// Provider workspaces have no Secret mounts; the read-once
			// bootstrap variables below replace the file paths.
		default:
			env = append(env, item)
		}
	}
	env = append(env,
		corev1.EnvVar{Name: "ORKA_ACP_CONTROLLER_TOKEN_BOOTSTRAP", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: authSecretName}, Key: runtimePoolControllerTokenKey,
			},
		}},
		corev1.EnvVar{Name: "ORKA_ACP_CAPABILITY_SECRET_BOOTSTRAP", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: authSecretName}, Key: runtimePoolCapabilitySecretKey,
			},
		}},
		corev1.EnvVar{Name: "ORKA_ACP_PROVIDER_TOKEN_BOOTSTRAP", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: providerSecretName}, Key: runtimePoolProviderTokenKey,
			},
		}},
	)
	container.Env = env
	return container
}

func substrateRuntimeTemplateRevision(
	container corev1.Container,
	labels, annotations map[string]string,
	infrastructure map[string]any,
) (string, error) {
	podTemplate := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: cloneStringMap(labels), Annotations: cloneStringMap(annotations)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
	}
	delete(podTemplate.Annotations, runtimePoolTemplateRevisionAnnotation)
	payload := map[string]any{
		"template":       podTemplate,
		"infrastructure": infrastructure,
	}
	return runtimePoolJSONRevision(payload)
}

func substrateRuntimeTemplateNeedsRollout(template *unstructured.Unstructured, desiredRevision string) bool {
	if template == nil {
		return false
	}
	deployed := strings.TrimSpace(template.GetAnnotations()[runtimePoolTemplateRevisionAnnotation])
	return desiredRevision == "" || deployed != desiredRevision
}

// substrateTemplatePodTemplateSpec reconstructs the logical Pod template shape
// from a deployed derived ActorTemplate so rollout drains validate the old
// instance against the exact identity it was booted with.
func substrateTemplatePodTemplateSpec(template *unstructured.Unstructured) (corev1.PodTemplateSpec, error) {
	containersRaw, found, err := unstructured.NestedSlice(template.Object, "spec", "containers")
	if err != nil || !found || len(containersRaw) != 1 {
		return corev1.PodTemplateSpec{}, fmt.Errorf("deployed RuntimePool substrate actor template is invalid")
	}
	containerMap, ok := containersRaw[0].(map[string]any)
	if !ok {
		return corev1.PodTemplateSpec{}, fmt.Errorf("deployed RuntimePool substrate actor template is invalid")
	}
	container := corev1.Container{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(containerMap, &container); err != nil {
		return corev1.PodTemplateSpec{}, fmt.Errorf("deployed RuntimePool substrate actor template is invalid: %w", err)
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      cloneStringMap(template.GetLabels()),
			Annotations: cloneStringMap(template.GetAnnotations()),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
	}, nil
}

// substrateTemplateAuthSecret resolves the deployed template's controller auth
// Secret through its read-once bootstrap secret reference.
func (r *RuntimePoolReconciler) substrateTemplateAuthSecret(
	ctx context.Context,
	namespace string,
	deployed corev1.PodTemplateSpec,
) (*corev1.Secret, error) {
	secretName := ""
	if len(deployed.Spec.Containers) == 1 {
		for _, item := range deployed.Spec.Containers[0].Env {
			if item.Name == "ORKA_ACP_CONTROLLER_TOKEN_BOOTSTRAP" && item.ValueFrom != nil && item.ValueFrom.SecretKeyRef != nil {
				secretName = strings.TrimSpace(item.ValueFrom.SecretKeyRef.Name)
				break
			}
		}
	}
	if secretName == "" {
		return nil, fmt.Errorf("deployed RuntimePool auth Secret reference is missing")
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
		return nil, fmt.Errorf("get deployed RuntimePool auth Secret: %w", err)
	}
	if strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKey])) == "" ||
		len(secret.Data[runtimePoolCapabilitySecretKey]) == 0 {
		return nil, fmt.Errorf("deployed RuntimePool auth Secret is incomplete")
	}
	return secret, nil
}

// pruneStaleSubstrateRuntimePoolSecrets removes epoch-scoped credential
// Secrets in the template namespace that are no longer referenced by the
// current names or the deployed derived template.
func (r *RuntimePoolReconciler) pruneStaleSubstrateRuntimePoolSecrets(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	secretsCfg runtimePoolConfig,
	derivedTemplate *unstructured.Unstructured,
	currentNames ...string,
) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	keep := make(map[string]struct{}, len(currentNames)+2)
	for _, name := range currentNames {
		addRuntimeSecretName(keep, name)
	}
	if derivedTemplate != nil {
		if deployed, err := substrateTemplatePodTemplateSpec(derivedTemplate); err == nil {
			addRuntimePoolSecretReferences(keep, deployed.Spec)
		}
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(secretsCfg.namespace), client.MatchingLabels{
		runtimePoolManagedByLabel: "orka",
		runtimePoolKeyLabel:       secretsCfg.labels[runtimePoolKeyLabel],
		runtimePoolUIDLabel:       string(pool.UID),
	}); err != nil {
		return fmt.Errorf("list managed RuntimePool Secrets for stale credential cleanup: %w", err)
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if _, current := keep[secret.Name]; current || !runtimePoolManagedCredentialSecret(secret, secretsCfg) {
			continue
		}
		if err := r.deleteRuntimePoolManagedSecret(ctx, secret); err != nil {
			return err
		}
	}
	return nil
}

// deleteSubstrateRuntimePoolChildren removes the provider actor, the derived
// template, and template-namespace Secrets during pool finalization. Actor
// deletion is mandatory: an unreachable Substrate control plane blocks
// finalization rather than leaking a credentialed runtime workload.
func (r *RuntimePoolReconciler) deleteSubstrateRuntimePoolChildren(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) (bool, error) {
	substrateSpec := pool.Spec.ExecutionWorkspace.Substrate
	if substrateSpec == nil {
		return false, nil
	}
	templateNamespace := substrateSpec.BaseTemplateNamespace
	actorID := runtimePoolSubstrateActorID(cfg.baseName)
	control, err := r.substrateActorControl()
	if err != nil {
		return false, err
	}
	defer control.Close() //nolint:errcheck // best-effort connection teardown
	if err := control.DeleteActor(ctx, actorID); err != nil {
		return false, fmt.Errorf("delete RuntimePool substrate actor: %w", err)
	}
	remaining := false
	if actor, err := control.GetActor(ctx, actorID); err != nil {
		return false, err
	} else if actor != nil {
		remaining = true
	}

	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	template.SetNamespace(templateNamespace)
	template.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
	if err := r.Delete(ctx, template); err != nil &&
		!apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) && !k8sRuntimeIsMissingKindError(err) {
		return false, fmt.Errorf("delete RuntimePool substrate actor template: %w", err)
	}

	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(templateNamespace), client.MatchingLabels{
		runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel],
	}); err != nil {
		return false, err
	}
	for i := range secrets.Items {
		if err := r.Delete(ctx, &secrets.Items[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	if len(secrets.Items) > 0 {
		remaining = true
	}
	return remaining, nil
}
