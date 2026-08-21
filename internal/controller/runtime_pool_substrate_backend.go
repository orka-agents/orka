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
// fence environment, and no credential material — the supervisor boots into
// an awaiting phase and the controller seeds credentials post-boot through
// the nonce-gated bootstrap endpoint. Prompt-level operations use the
// unchanged orka.harness.v2 protocol against the ActiveInstance; only
// workload materialization and endpoint dialing differ.
//
// Checkpointing a live supervisor is prohibited: gVisor suspension captures
// process memory — including live pool and provider-proxy credentials — into
// provider snapshot storage. Because the provider deletes only suspended
// actors, teardown is staged: destroy the workload's memory by deleting its
// single-workload worker Pod, settle the memoryless actor (a suspension with
// nothing left to checkpoint), then delete it. The exact instance is recycled
// whenever the provider reports a suspension or a snapshot for a booted
// actor. Actor suspend/resume and snapshot restore remain fail-closed until
// the provider offers credential-safe sessions.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
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
	// substrateActorRecyclingAnnotation records an in-progress staged actor
	// teardown so every reconcile resumes it before any other decision.
	substrateActorRecyclingAnnotation = "orka.ai/substrate-actor-recycling"

	// substrateActorListenPort is the conventional actor service port the
	// provider router forwards to.
	substrateActorListenPort int32 = 80
	// substrateWorkerPoolLabel marks provider worker Pods; the staged actor
	// teardown refuses to delete any Pod that does not carry it.
	substrateWorkerPoolLabel = "ate.dev/worker-pool"
	// substrateRuntimeEntrypoint is the shared entrypoint of every immutable
	// ACP runtime image; the provider does not read image config, so the
	// rendered container must state it explicitly.
	substrateRuntimeEntrypoint = "/usr/local/bin/orka-acp-runtime"
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
	sum := sha256.Sum256([]byte("orka-substrate-runtime-instance\x00" + strings.TrimSpace(actorID)))
	return "workspace:" + hex.EncodeToString(sum[:])
}

func substrateActorRouteHost(actorID, dnsSuffix string) string {
	return actorID + "." + strings.Trim(strings.TrimSpace(dnsSuffix), ".")
}

func substrateActorMatchesRuntimeTemplate(
	actor *workspace.SubstrateRuntimeActor,
	actorID, templateNamespace, templateName string,
) bool {
	return actor != nil &&
		strings.TrimSpace(actor.ActorID) == actorID &&
		strings.TrimSpace(actor.TemplateNamespace) == templateNamespace &&
		strings.TrimSpace(actor.TemplateName) == templateName
}

func substrateRuntimeTemplateOwnedByPool(
	template, desired *unstructured.Unstructured,
) bool {
	if template == nil || desired == nil ||
		template.GetNamespace() != desired.GetNamespace() || template.GetName() != desired.GetName() {
		return false
	}
	observedLabels := template.GetLabels()
	desiredLabels := desired.GetLabels()
	for _, key := range []string{
		runtimePoolManagedByLabel,
		runtimePoolKeyLabel,
		runtimePoolNameLabel,
		runtimePoolNamespaceLabel,
		runtimePoolUIDLabel,
	} {
		if desiredLabels[key] == "" || observedLabels[key] != desiredLabels[key] {
			return false
		}
	}
	return true
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
	// The template and the actor never carry credentials: pool Secrets stay in
	// the controller-owned runtime namespace and are seeded into the booted
	// supervisor through the nonce-gated credential bootstrap.
	if err := r.ensureRuntimePoolNamespace(ctx, cfg); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	authSecret, providerSecret, err := r.ensureRuntimePoolSecrets(ctx, pool, cfg)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	bootstrapNonce := strings.TrimSpace(string(authSecret.Data[runtimePoolBootstrapNonceKey]))
	if bootstrapNonce == "" {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("RuntimePool auth Secret is missing the credential bootstrap nonce"))
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
	desired, err := r.renderSubstrateRuntimeTemplate(pool, cfg, baseTemplate, templateNamespace, actorID, bootstrapNonce)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	derivedTemplate, err := r.getSubstrateActorTemplate(ctx, templateNamespace, runtimePoolSubstrateTemplateName(cfg.baseName))
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	templateOwned := derivedTemplate == nil || substrateRuntimeTemplateOwnedByPool(derivedTemplate, desired.object)
	templateRevision := ""
	templateIntegrityErr := error(nil)
	if derivedTemplate != nil && templateOwned {
		templateRevision, templateIntegrityErr = substrateRuntimeTemplateIntegrity(derivedTemplate)
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

	if strings.TrimSpace(pool.Annotations[substrateActorRecyclingAnnotation]) != "" {
		// A staged teardown is in progress; resume it before any other
		// decision so a half-recycled actor can never be admitted or seeded.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "recycling the exact provider actor without checkpointing supervisor memory"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !templateOwned {
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "controller-derived substrate ActorTemplate does not carry the exact RuntimePool ownership identity"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	booted := pool.Annotations[substrateActorBootedAnnotation] == actorID
	if actor != nil && !substrateActorMatchesRuntimeTemplate(
		actor,
		actorID,
		templateNamespace,
		runtimePoolSubstrateTemplateName(cfg.baseName),
	) {
		// Actor IDs are deterministic, so an existing actor is not proof of
		// ownership. Never resume, record, probe, or credential-seed an actor
		// unless the provider reports the exact controller-derived template.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider actor does not use the controller-derived runtime template; recycling the exact instance before credential bootstrap"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if actor != nil && templateIntegrityErr != nil {
		// A same-name template with valid ownership labels is still not trusted
		// when its declared revision does not match its observed contents. Never
		// resume, probe, or credential-seed an actor booted from that workload.
		if err := r.recycleSubstrateActor(ctx, pool, control, actorID); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "controller-derived substrate ActorTemplate contents do not match their declared revision; recycling the exact actor before credential bootstrap"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
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

	if derivedTemplate != nil && (templateIntegrityErr != nil || templateRevision != desired.revision) {
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
	if err := r.pruneStaleSubstrateRuntimePoolSecrets(ctx, pool, cfg, authSecret.Name, providerSecret.Name); err != nil {
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
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "provider actor is booting the runtime workload")
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
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "provider actor boot is being recorded before exact-instance admission")
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if !actor.Running() {
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "waiting for the provider actor workload to run")
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	// Seed the booted supervisor's credentials before the authenticated
	// probe. The PUT is idempotent for identical payloads; a payload conflict
	// means another party seeded this workload first, so the exact instance is
	// recycled instead of trusted.
	if err := r.seedSubstrateSupervisorCredentials(ctx, routeHost, bootstrapNonce, authSecret, providerSecret); err != nil {
		if errors.Is(err, errSubstrateCredentialConflict) {
			if recycleErr := r.recycleSubstrateActor(ctx, pool, control, actorID); recycleErr != nil {
				return ctrl.Result{}, recycleErr
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "provider actor was credential-seeded by another party; recycling the exact instance"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		r.applyProviderRuntimePoolColdStartStatus(
			pool,
			&status,
			sanitizeRuntimePoolMessage("credential bootstrap is not complete: "+err.Error()),
		)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	syntheticPod := substrateSyntheticInstancePod(pool, cfg, templateNamespace, actor, actorID, routeHost)
	return r.reconcileRuntimePoolServing(ctx, pool, cfg, []corev1.Pod{*syntheticPod}, []corev1.Pod{*syntheticPod}, authSecret, status)
}

// errSubstrateCredentialConflict marks a seeding payload conflict: the
// supervisor was already seeded with different credentials.
var errSubstrateCredentialConflict = errors.New("supervisor credentials were seeded by another party")

// seedSubstrateSupervisorCredentials performs the one-time, idempotent
// credential bootstrap PUT against the exact actor route host.
func (r *RuntimePoolReconciler) seedSubstrateSupervisorCredentials(
	ctx context.Context,
	routeHost, nonce string,
	authSecret, providerSecret *corev1.Secret,
) error {
	request := harnessv2.CredentialBootstrapRequest{
		ControllerToken:  strings.TrimSpace(string(authSecret.Data[runtimePoolControllerTokenKey])),
		CapabilitySecret: strings.TrimSpace(string(authSecret.Data[runtimePoolCapabilitySecretKey])),
		ProviderToken:    strings.TrimSpace(string(providerSecret.Data[runtimePoolProviderTokenKey])),
	}
	if err := request.Validate(); err != nil {
		return fmt.Errorf("pool credentials are incomplete: %w", err)
	}
	if r.SubstrateCredentialSeeder != nil {
		return r.SubstrateCredentialSeeder(ctx, routeHost, nonce, request)
	}
	httpClient, err := r.substrateSupervisorHTTPClient()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	seedCtx, cancel := context.WithTimeout(ctx, runtimePoolProbeTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		seedCtx, http.MethodPut,
		urlSchemeHTTP+"://"+routeHost+harnessv2.CredentialBootstrapPath,
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // response body is unused
	switch response.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return errSubstrateCredentialConflict
	case http.StatusNotFound:
		// The supervisor already completed bootstrap and replaced the phase
		// server; the authenticated probe is now the authority.
		return nil
	default:
		return fmt.Errorf("credential bootstrap returned status %d", response.StatusCode)
	}
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
	deployedAuthSecret, err := r.substrateTemplateAuthSecret(ctx, cfg, deployedTemplate)
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

// recycleSubstrateActor advances the credential-safe staged teardown of the
// exact actor and, once it is fully gone, clears the boot and recycling
// records so the replacement boots from scratch. The teardown may span
// several reconciles; the recycling annotation guarantees every reconcile
// resumes it first.
func (r *RuntimePoolReconciler) recycleSubstrateActor(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
) error {
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorRecyclingAnnotation, actorID); err != nil {
		return err
	}
	gone, err := r.teardownSubstrateActor(ctx, pool, control, actorID)
	if err != nil {
		return err
	}
	if !gone {
		return nil
	}
	if err := r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorBootedAnnotation, ""); err != nil {
		return err
	}
	return r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorRecyclingAnnotation, "")
}

// teardownSubstrateActor advances one stage of the credential-safe actor
// teardown and reports whether the actor is fully gone. A live workload is
// never checkpointed: its memory is destroyed first by deleting the
// single-workload provider worker Pod; once the workload is provably absent
// the actor is settled into the provider's deletable suspended state — with
// nothing left to checkpoint — and then deleted. Callers requeue while the
// teardown is in progress.
func (r *RuntimePoolReconciler) teardownSubstrateActor(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	control workspace.SubstrateRuntimeActorControl,
	actorID string,
) (bool, error) {
	actor, err := control.GetActor(ctx, actorID)
	if err != nil {
		return false, err
	}
	if actor == nil {
		return true, nil
	}
	if actor.Suspended() {
		if err := control.DeleteActor(ctx, actorID); err != nil {
			return false, fmt.Errorf("delete RuntimePool substrate actor: %w", err)
		}
		return true, nil
	}
	if actor.Suspending() {
		return false, nil
	}
	workloadGone, err := r.destroySubstrateActorWorkload(ctx, pool, actor)
	if err != nil {
		return false, err
	}
	if !workloadGone {
		return false, nil
	}
	if _, err := control.SettleActor(ctx, actorID); err != nil {
		return false, fmt.Errorf("settle RuntimePool substrate actor: %w", err)
	}
	return false, nil
}

// destroySubstrateActorWorkload destroys the memory of a live actor workload
// by deleting its assigned provider worker Pod (provider workers host exactly
// one workload; the pool Deployment replaces the Pod fresh) and reports
// whether the workload is provably gone.
func (r *RuntimePoolReconciler) destroySubstrateActorWorkload(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actor *workspace.SubstrateRuntimeActor,
) (bool, error) {
	workerNamespace, workerPool, err := r.substrateRuntimePoolWorkerPlacement(ctx, pool)
	if err != nil {
		return false, err
	}
	namespace := strings.TrimSpace(actor.PodNamespace)
	name := strings.TrimSpace(actor.PodName)
	if namespace == "" || name == "" {
		return false, fmt.Errorf("refusing to settle RuntimePool substrate actor: provider worker placement is unknown")
	}
	if namespace != workerNamespace {
		return false, fmt.Errorf(
			"refusing to delete Pod %s/%s: provider worker namespace does not match infrastructure WorkerPool namespace %s",
			namespace, name, workerNamespace,
		)
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	pod := &corev1.Pod{}
	err = reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(pod.Labels[substrateWorkerPoolLabel]) != workerPool {
		return false, fmt.Errorf(
			"refusing to delete Pod %s/%s: %s label does not match infrastructure WorkerPool %s",
			namespace, name, substrateWorkerPoolLabel, workerPool,
		)
	}
	if pod.DeletionTimestamp != nil {
		return false, nil
	}
	if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete provider worker Pod hosting the recycled actor: %w", err)
	}
	return false, nil
}

func (r *RuntimePoolReconciler) substrateRuntimePoolWorkerPlacement(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
) (string, string, error) {
	if pool == nil || pool.Spec.ExecutionWorkspace == nil || pool.Spec.ExecutionWorkspace.Substrate == nil {
		return "", "", fmt.Errorf("RuntimePool substrate infrastructure template binding is missing")
	}
	substrateSpec := pool.Spec.ExecutionWorkspace.Substrate
	template, err := r.getSubstrateActorTemplate(
		ctx,
		strings.TrimSpace(substrateSpec.BaseTemplateNamespace),
		strings.TrimSpace(substrateSpec.BaseTemplateName),
	)
	if err != nil {
		return "", "", err
	}
	if template == nil {
		return "", "", fmt.Errorf("RuntimePool substrate infrastructure ActorTemplate is unavailable during actor teardown")
	}
	workerPool, found, err := unstructured.NestedString(template.Object, "spec", "workerPoolRef", "name")
	if err != nil || !found || strings.TrimSpace(workerPool) == "" {
		return "", "", fmt.Errorf("RuntimePool substrate infrastructure ActorTemplate has no workerPoolRef.name")
	}
	workerNamespace, _, err := unstructured.NestedString(template.Object, "spec", "workerPoolRef", "namespace")
	if err != nil {
		return "", "", fmt.Errorf("RuntimePool substrate infrastructure ActorTemplate has an invalid workerPoolRef.namespace")
	}
	workerNamespace = strings.TrimSpace(workerNamespace)
	if workerNamespace == "" {
		workerNamespace = template.GetNamespace()
	}
	if workerNamespace == "" {
		return "", "", fmt.Errorf("RuntimePool substrate infrastructure ActorTemplate has no WorkerPool namespace")
	}
	return workerNamespace, strings.TrimSpace(workerPool), nil
}

func (r *RuntimePoolReconciler) setSubstrateActorBootedAnnotation(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	actorID string,
) error {
	return r.setSubstrateRuntimePoolAnnotation(ctx, pool, substrateActorBootedAnnotation, actorID)
}

func (r *RuntimePoolReconciler) setSubstrateRuntimePoolAnnotation(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	key, value string,
) error {
	current := pool.Annotations[key]
	if current == value || (value == "" && current == "") {
		return nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	if value == "" {
		delete(pool.Annotations, key)
	} else {
		pool.Annotations[key] = value
	}
	if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record RuntimePool substrate actor lifecycle annotation: %w", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) getSubstrateActorTemplate(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, template); err != nil {
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
	templateNamespace, actorID, bootstrapNonce string,
) (substrateRuntimeTemplateRender, error) {
	baseSpec, found, err := unstructured.NestedMap(baseTemplate.Object, "spec")
	if err != nil || !found {
		return substrateRuntimeTemplateRender{}, fmt.Errorf("substrate infrastructure ActorTemplate %s/%s has no readable spec", templateNamespace, baseTemplate.GetName())
	}
	infrastructure := k8sruntime.DeepCopyJSON(baseSpec)
	delete(infrastructure, "containers")
	// snapshotsConfig is copied verbatim: the provider requires it and builds a
	// per-template "golden snapshot" by booting one instance and checkpointing
	// it. That checkpoint is safe only because the rendered container carries no
	// credentials at all — the supervisor boots into the awaiting-bootstrap
	// phase and receives credentials from the controller after the real actor
	// is booted, so a golden snapshot captures a waiting, credential-free
	// process plus the public per-pool nonce.

	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	// Secret names are irrelevant to the rendered container (credentials are
	// bootstrap-seeded, never template-referenced); the canonical template only
	// contributes the immutable image, fence identity, and non-secret env.
	canonical := r.runtimePoolPodTemplate(pool, cfg, selector, "unused-auth", "unused-provider")
	container := substrateRuntimeContainer(canonical.Spec.Containers[0], templateNamespace, actorID, bootstrapNonce)

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

	object := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	object.SetGroupVersionKind(substrateActorTemplateGVK)
	object.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
	object.SetNamespace(templateNamespace)
	object.SetLabels(labels)
	object.SetAnnotations(annotations)
	revision, err := substrateRuntimeTemplateObjectRevision(object)
	if err != nil {
		return substrateRuntimeTemplateRender{}, err
	}
	annotations[runtimePoolTemplateRevisionAnnotation] = revision
	object.SetAnnotations(annotations)
	return substrateRuntimeTemplateRender{object: object, revision: revision}, nil
}

// substrateRuntimeContainer adapts the canonical supervisor container to
// provider hosting: Kubernetes downward-API field refs become exact literals,
// credential file mounts disappear entirely (the supervisor boots in the
// awaiting-bootstrap phase gated by the public per-pool nonce), and Pod-only
// surfaces (mounts, probes, security context) are dropped because the
// provider's gVisor sandbox owns them.
func substrateRuntimeContainer(
	canonical corev1.Container,
	templateNamespace, actorID, bootstrapNonce string,
) corev1.Container {
	container := *canonical.DeepCopy()
	container.VolumeMounts = nil
	container.SecurityContext = nil
	container.StartupProbe = nil
	container.ReadinessProbe = nil
	container.LivenessProbe = nil
	container.Lifecycle = nil
	container.Resources = corev1.ResourceRequirements{}
	// Unlike kubelet, the provider builds the OCI runtime spec strictly from
	// the template and never reads the image config, so the immutable runtime
	// entrypoint must be stated explicitly — otherwise `runsc create` receives
	// an empty argv and rejects the workload.
	container.Command = []string{substrateRuntimeEntrypoint}
	// The provider router forwards actor traffic on the conventional actor
	// port 80 (every proven actor workload listens there); the controller-side
	// route transport dials the router, so the logical URL port never matters.
	container.Ports = []corev1.ContainerPort{{ContainerPort: substrateActorListenPort, Protocol: corev1.ProtocolTCP}}

	env := make([]corev1.EnvVar, 0, len(container.Env)+3)
	for _, item := range container.Env {
		switch item.Name {
		case "ORKA_ACP_LISTEN_ADDRESS":
			env = append(env, corev1.EnvVar{Name: item.Name, Value: fmt.Sprintf(":%d", substrateActorListenPort)})
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
	env = append(env, corev1.EnvVar{Name: "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE", Value: bootstrapNonce})
	container.Env = env
	return container
}

func substrateRuntimeTemplateObjectRevision(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("RuntimePool substrate actor template is required")
	}
	spec, found, err := unstructured.NestedMap(template.Object, "spec")
	if err != nil || !found {
		return "", fmt.Errorf("RuntimePool substrate actor template has no readable spec")
	}
	annotations := cloneStringMap(template.GetAnnotations())
	delete(annotations, runtimePoolTemplateRevisionAnnotation)
	return runtimePoolJSONRevision(map[string]any{
		"labels":      cloneStringMap(template.GetLabels()),
		"annotations": annotations,
		"spec":        spec,
	})
}

func substrateRuntimeTemplateIntegrity(template *unstructured.Unstructured) (string, error) {
	if template == nil {
		return "", fmt.Errorf("deployed RuntimePool substrate actor template is missing")
	}
	declared := strings.TrimSpace(template.GetAnnotations()[runtimePoolTemplateRevisionAnnotation])
	if declared == "" {
		return "", fmt.Errorf("deployed RuntimePool substrate actor template has no declared revision")
	}
	observed, err := substrateRuntimeTemplateObjectRevision(template)
	if err != nil {
		return "", err
	}
	if observed != declared {
		return "", fmt.Errorf(
			"deployed RuntimePool substrate actor template revision does not match observed contents (declared %s, observed %s)",
			runtimePoolShortRevision(declared), runtimePoolShortRevision(observed),
		)
	}
	return declared, nil
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

// substrateTemplateAuthSecret resolves the epoch-scoped controller auth Secret
// the deployed instance was seeded with, derived from the deployed template's
// literal controller-epoch environment. Substrate templates never reference
// Secrets directly.
func (r *RuntimePoolReconciler) substrateTemplateAuthSecret(
	ctx context.Context,
	cfg runtimePoolConfig,
	deployed corev1.PodTemplateSpec,
) (*corev1.Secret, error) {
	epoch := ""
	if len(deployed.Spec.Containers) == 1 {
		epoch = strings.TrimSpace(runtimePoolLiteralEnvironment(deployed.Spec.Containers[0].Env)["ORKA_ACP_CONTROLLER_EPOCH"])
	}
	if epoch == "" {
		return nil, fmt.Errorf("deployed RuntimePool auth Secret reference is missing")
	}
	secretName := runtimePoolChildName(cfg.baseName, "auth-e"+epoch)
	namespace := cfg.namespace
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
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
	cfg runtimePoolConfig,
	currentNames ...string,
) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	keep := make(map[string]struct{}, len(currentNames))
	for _, name := range currentNames {
		addRuntimeSecretName(keep, name)
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(cfg.namespace), client.MatchingLabels{
		runtimePoolManagedByLabel: "orka",
		runtimePoolKeyLabel:       cfg.labels[runtimePoolKeyLabel],
		runtimePoolUIDLabel:       string(pool.UID),
	}); err != nil {
		return fmt.Errorf("list managed RuntimePool Secrets for stale credential cleanup: %w", err)
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if _, current := keep[secret.Name]; current || !runtimePoolManagedCredentialSecret(secret, cfg) {
			continue
		}
		if err := r.deleteRuntimePoolManagedSecret(ctx, secret); err != nil {
			return err
		}
	}
	return nil
}

// deleteSubstrateRuntimePoolChildren removes the provider actor and the
// derived template during pool finalization; pool Secrets live in the runtime
// namespace and are swept by the generic child cleanup. Actor deletion is
// mandatory: an unreachable Substrate control plane blocks finalization
// rather than leaking a credentialed runtime workload.
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
	gone, err := r.teardownSubstrateActor(ctx, pool, control, actorID)
	if err != nil {
		return false, err
	}
	if !gone {
		// The staged teardown still needs the derived template: the provider's
		// settle workflow resolves it while transitioning the memoryless actor
		// into the deletable suspended state. Delete it only after the actor
		// is gone.
		return true, nil
	}
	remaining := !gone

	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(substrateActorTemplateGVK)
	template.SetNamespace(templateNamespace)
	template.SetName(runtimePoolSubstrateTemplateName(cfg.baseName))
	if err := r.Delete(ctx, template); err != nil &&
		!apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) && !k8sRuntimeIsMissingKindError(err) {
		return false, fmt.Errorf("delete RuntimePool substrate actor template: %w", err)
	}

	// Pool Secrets live in the runtime namespace and are swept by the generic
	// pool child cleanup; nothing secret ever exists in the template namespace.
	return remaining, nil
}
