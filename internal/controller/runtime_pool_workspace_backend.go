/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

// Workspace-provider-backed RuntimePools materialize their single runtime
// instance through an externally operated execution-workspace provider
// (Phase 1: kubernetes-sigs Agent Sandbox) instead of a controller-owned
// Deployment. The rendered supervisor Pod template and image allowlist remain
// controller-owned, but the provider-visible template carries no credentials:
// after exact Sandbox materialization is attested, the controller seeds the
// supervisor through the signed one-time bootstrap endpoint. The authenticated
// fence probe, admission gates, drain semantics, and every prompt-level
// operation remain identical to plain pools. Provider-native identifiers
// (claim, sandbox, template names) never enter public Task status; the sanitized
// RuntimePool status is the only projection surface.
//
// Lifecycle mapping onto the provider-neutral execution-workspace contract:
//   - Acquire      -> ensure SandboxTemplate + SandboxWarmPool + SandboxClaim
//     for the exact RuntimePool generation (template revision).
//   - WaitReady    -> claim -> sandbox -> Pod Ready, then the authenticated
//     exact-instance fence probe selects the ActiveInstance.
//   - CreateRuntimeSession / ExecutePrompt / Cancel -> the existing fenced
//     orka.harness.v2 protocol against the ActiveInstance, unchanged.
//   - Drain        -> the existing authenticated supervisor drain.
//   - Delete       -> claim deletion (cascades sandbox + Pod) plus finalizer
//     child cleanup; restart-safe and idempotent.
//   - Recover      -> the existing exact-fence recovery; a missing pool or a
//     replaced instance is already proof of session cleanup.
//   - Suspend/Resume -> not supported by Agent Sandbox; unsupported requests
//     fail closed before any workspace or RuntimePool demand exists.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	sandboxextcontrollers "sigs.k8s.io/agent-sandbox/extensions/controllers"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	runtimePoolSandboxTemplateSuffix             = "sandbox-template"
	runtimePoolSandboxWarmPoolSuffix             = "sandbox-pool"
	runtimePoolSandboxClaimSuffix                = "sandbox-claim"
	runtimePoolSandboxTemplateRevisionAnnotation = "orka.ai/sandbox-template-revision"
)

var errWorkspaceCredentialConflict = errors.New("workspace supervisor credential bootstrap conflict")

// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims;sandboxtemplates;sandboxwarmpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch

func runtimePoolSandboxTemplateName(base string) string {
	return runtimePoolChildName(base, runtimePoolSandboxTemplateSuffix)
}

func runtimePoolSandboxWarmPoolName(base string) string {
	return runtimePoolChildName(base, runtimePoolSandboxWarmPoolSuffix)
}

func runtimePoolSandboxClaimName(base string) string {
	return runtimePoolChildName(base, runtimePoolSandboxClaimSuffix)
}

func validateRuntimePoolExecutionWorkspace(pool *corev1alpha1.RuntimePool) error {
	workspace := pool.Spec.ExecutionWorkspace
	if workspace == nil {
		return nil
	}
	switch workspace.Provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		if workspace.Substrate != nil {
			return fmt.Errorf("spec.executionWorkspace.substrate is only valid for provider substrate")
		}
	case corev1alpha1.WorkspaceProviderSubstrate:
		if workspace.Substrate == nil ||
			strings.TrimSpace(workspace.Substrate.BaseTemplateNamespace) == "" ||
			strings.TrimSpace(workspace.Substrate.BaseTemplateName) == "" {
			return fmt.Errorf("spec.executionWorkspace.substrate must name the operator-owned infrastructure ActorTemplate")
		}
	default:
		return fmt.Errorf("spec.executionWorkspace.provider %q is not supported", workspace.Provider)
	}
	if !validSHA256Digest(workspace.BindingDigest) {
		return fmt.Errorf("spec.executionWorkspace.bindingDigest must be a sha256 digest")
	}
	if pool.Spec.Capacity == nil || pool.Spec.Capacity.MaxResidentSessions != 1 || pool.Spec.Capacity.MaxRunningPrompts != 1 {
		return fmt.Errorf("workspace-backed RuntimePools host exactly one resident RuntimeSession; spec.capacity must be 1/1")
	}
	return nil
}

func validateRuntimePoolExecutionWorkspaceNamespace(
	pool *corev1alpha1.RuntimePool,
	runtimeNamespace string,
) error {
	if pool == nil || pool.Spec.ExecutionWorkspace == nil ||
		pool.Spec.ExecutionWorkspace.Provider != corev1alpha1.WorkspaceProviderSubstrate ||
		pool.Spec.ExecutionWorkspace.Substrate == nil {
		return nil
	}
	return validateSubstrateTemplateRuntimeNamespace(
		pool.Spec.ExecutionWorkspace.Substrate.BaseTemplateNamespace,
		runtimeNamespace,
	)
}

func validateSubstrateTemplateRuntimeNamespace(templateNamespace, runtimeNamespace string) error {
	templateNamespace = strings.TrimSpace(templateNamespace)
	runtimeNamespace = strings.TrimSpace(runtimeNamespace)
	if templateNamespace != "" && templateNamespace == runtimeNamespace {
		return fmt.Errorf("substrate infrastructure template namespace must differ from the resolved runtime namespace so provider templates cannot resolve RuntimePool Secrets")
	}
	return nil
}

// reconcileWorkspaceBackedRuntimePool converges a workspace-provider-backed
// pool. It mirrors the Deployment path exactly, replacing only workload
// materialization: SandboxTemplate + SandboxWarmPool render the supervisor Pod
// and one SandboxClaim hosts the single exact instance.
//
//nolint:gocyclo // Workload materialization decisions stay auditable together, mirroring the Deployment path.
func (r *RuntimePoolReconciler) reconcileWorkspaceBackedRuntimePool(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	authSecret *corev1.Secret,
	providerSecret *corev1.Secret,
) (ctrl.Result, error) {
	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	desiredTemplate := r.runtimePoolPodTemplate(pool, cfg, selector, authSecret.Name, providerSecret.Name)
	bootstrapNonce := strings.TrimSpace(string(authSecret.Data[runtimePoolBootstrapNonceKey]))
	if bootstrapNonce == "" {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("RuntimePool auth Secret is missing the credential bootstrap nonce"))
	}
	bootstrapPublicKey, err := harnessv2.CredentialBootstrapPublicKey(authSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("derive RuntimePool credential bootstrap public key: %w", err))
	}
	desiredTemplate = runtimePoolWorkspaceBootstrapTemplate(desiredTemplate, bootstrapNonce, bootstrapPublicKey)

	sandboxTemplate, err := r.getRuntimePoolSandboxTemplate(ctx, cfg)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	claim, err := r.getRuntimePoolSandboxClaim(ctx, cfg)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	pods, err := r.listWorkspaceRuntimePoolPods(ctx, pool, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	status := r.baseRuntimePoolStatus(pool, countRuntimePoolPods(pods))
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionPodSecurityReady, metav1.ConditionTrue, "PodSecurityConfigured", "runtime Pod security controls are configured")
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionQuotaReady, metav1.ConditionTrue, "ResourcesAdmitted", "runtime resources were admitted")

	if claim != nil && !runtimePoolSandboxChildOwnedByPool(claim, pool, cfg) {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("same-name SandboxClaim does not carry the exact RuntimePool ownership identity"))
	}
	if claim != nil && !runtimePoolSandboxClaimMatchesPool(claim, cfg) {
		if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "provider SandboxClaim contents do not match the controller-owned RuntimePool binding; recycling it before use"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	claimFailed := r.applySandboxClaimFailureConditions(pool, claim, &status)
	if claimFailed && pool.Spec.DesiredReplicas != 0 {
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if sandboxTemplate != nil {
		if !runtimePoolSandboxChildOwnedByPool(sandboxTemplate, pool, cfg) {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("same-name SandboxTemplate does not carry the exact RuntimePool ownership identity"))
		}
		trustedRevision := strings.TrimSpace(pool.Annotations[runtimePoolSandboxTemplateRevisionAnnotation])
		observedRevision, revisionErr := runtimePoolSandboxTemplateObjectRevision(sandboxTemplate)
		desiredRevision := runtimePoolSandboxTemplateSpecRevision(runtimePoolSandboxTemplateSpec(desiredTemplate))
		if trustedRevision == "" {
			if claim != nil || len(pods) > 0 {
				if claim != nil {
					if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
						return ctrl.Result{}, err
					}
				}
				status.ActiveInstance = nil
				status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
				status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
				status.Message = "provider SandboxTemplate has no controller-owned integrity record; recycling its workspace before trust is established"
				r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
				return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
			}
			if revisionErr != nil || observedRevision != desiredRevision {
				if err := r.updateRuntimePoolSandboxTemplate(ctx, sandboxTemplate, desiredTemplate); err != nil {
					return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
				}
			}
			if err := r.setRuntimePoolSandboxTemplateRevision(ctx, pool, desiredRevision); err != nil {
				return ctrl.Result{}, err
			}
		} else if revisionErr != nil || observedRevision != trustedRevision {
			if claim != nil {
				if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
					return ctrl.Result{}, err
				}
			}
			if claim != nil || len(pods) > 0 {
				status.ActiveInstance = nil
				status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
				status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
				status.Message = "provider SandboxTemplate failed its controller-owned integrity check; recycling the workspace before template repair"
				r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
				return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
			}
			if observedRevision != desiredRevision {
				if err := r.updateRuntimePoolSandboxTemplate(ctx, sandboxTemplate, desiredTemplate); err != nil {
					return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
				}
			}
			if err := r.setRuntimePoolSandboxTemplateRevision(ctx, pool, desiredRevision); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	readyPods := readyRuntimePoolPods(pods)
	if len(readyPods) > 1 {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleAmbiguous
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAmbiguous
		status.ActiveInstance = nil
		status.Message = fmt.Sprintf("found %d Ready runtime Pods; exact-instance admission is closed", len(readyPods))
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRuntimeAmbiguous, status.Message)
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRuntimeAmbiguous, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	if sandboxTemplate != nil && runtimePoolSandboxTemplateNeedsRollout(sandboxTemplate, desiredTemplate) {
		return r.reconcileWorkspaceRuntimePoolRollout(ctx, pool, cfg, sandboxTemplate, claim, pods, desiredTemplate, status)
	}

	if pool.Spec.DesiredReplicas == 0 {
		return r.reconcileWorkspaceRuntimePoolScaleDown(ctx, pool, cfg, claim, pods, readyPods, authSecret, status)
	}

	if sandboxTemplate == nil {
		if err := r.createRuntimePoolSandboxTemplate(ctx, pool, cfg, desiredTemplate); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if sandboxTemplate, err = r.getRuntimePoolSandboxTemplate(ctx, cfg); err != nil || sandboxTemplate == nil {
			if err == nil {
				err = fmt.Errorf("created RuntimePool sandbox template is not readable yet")
			}
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		if !runtimePoolSandboxChildOwnedByPool(sandboxTemplate, pool, cfg) {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("created SandboxTemplate does not carry the exact RuntimePool ownership identity"))
		}
		desiredRevision := runtimePoolSandboxTemplateSpecRevision(runtimePoolSandboxTemplateSpec(desiredTemplate))
		observedRevision, revisionErr := runtimePoolSandboxTemplateObjectRevision(sandboxTemplate)
		if revisionErr != nil || observedRevision != desiredRevision {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("created SandboxTemplate contents do not match the controller-rendered RuntimePool template"))
		}
		if err := r.setRuntimePoolSandboxTemplateRevision(ctx, pool, desiredRevision); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.ensureRuntimePoolSandboxWarmPool(ctx, pool, cfg); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	if err := r.pruneStaleWorkspaceRuntimePoolSecrets(ctx, pool, cfg, sandboxTemplate, authSecret.Name, providerSecret.Name); err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}

	if claim != nil && !claim.DeletionTimestamp.IsZero() {
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "replaced workspace claim is terminating before a fresh provider workspace is acquired"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if claim == nil {
		if err := r.createRuntimePoolSandboxClaim(ctx, pool, cfg); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		r.applyProviderRuntimePoolColdStartStatus(pool, &status, "waiting to observe the new provider SandboxClaim before credential bootstrap")
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}
	if len(readyPods) == 1 {
		materialized, err := r.attestWorkspaceRuntimePoolMaterialization(ctx, claim, sandboxTemplate, &readyPods[0])
		if err != nil {
			if deleteErr := r.deleteRuntimePoolSandboxClaim(ctx, claim); deleteErr != nil {
				return ctrl.Result{}, errors.Join(err, deleteErr)
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = sanitizeRuntimePoolMessage("provider workspace materialization does not match the validated controller template: " + err.Error())
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if !materialized {
			r.applyProviderRuntimePoolColdStartStatus(pool, &status, "waiting for the provider Sandbox materialization record before credential bootstrap")
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		alreadyComplete, seedErr := r.seedWorkspaceSupervisorCredentials(
			ctx, runtimePoolInstanceEndpoint(pool, &readyPods[0]), bootstrapNonce, authSecret, providerSecret,
		)
		if errors.Is(seedErr, errWorkspaceCredentialConflict) {
			if deleteErr := r.deleteRuntimePoolSandboxClaim(ctx, claim); deleteErr != nil {
				return ctrl.Result{}, errors.Join(seedErr, deleteErr)
			}
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "provider workspace was credential-seeded by another party; recycling the exact instance"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if seedErr != nil {
			r.applyProviderRuntimePoolColdStartStatus(pool, &status, sanitizeRuntimePoolMessage("credential bootstrap is not complete: "+seedErr.Error()))
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if alreadyComplete {
			// A 404 means the one-time bootstrap listener has already handed off
			// to the authenticated supervisor. The exact fence probe below is
			// still required before this instance can be admitted.
			status.Message = "provider workspace credential bootstrap already completed; verifying the exact authenticated instance"
		}
	}
	return r.reconcileRuntimePoolServing(ctx, pool, cfg, pods, readyPods, authSecret, status)
}

func (r *RuntimePoolReconciler) listWorkspaceRuntimePoolPods(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(cfg.namespace), client.MatchingLabels{
		runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel],
		runtimePoolUIDLabel: string(pool.UID),
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// reconcileWorkspaceRuntimePoolRollout mirrors the Deployment Recreate rollout:
// authenticated drain of the exact old instance, a persisted quiescence
// barrier, claim deletion, and only then the new immutable template.
//
//nolint:gocyclo // The rollout barriers intentionally mirror the audited Deployment path.
func (r *RuntimePoolReconciler) reconcileWorkspaceRuntimePoolRollout(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	sandboxTemplate *sandboxextv1beta1.SandboxTemplate,
	claim *sandboxextv1beta1.SandboxClaim,
	pods []corev1.Pod,
	desiredTemplate corev1.PodTemplateSpec,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	status.Message = "runtime template changed; admission is closed before provider workspace replacement"
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)

	readyPods := readyRuntimePoolPods(pods)
	if claim == nil || !claim.DeletionTimestamp.IsZero() {
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		if claim != nil || len(pods) > 0 {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
			status.Message = "waiting for the drained provider workspace to terminate before applying the new template"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		if err := r.updateRuntimePoolSandboxTemplate(ctx, sandboxTemplate, desiredTemplate); err != nil {
			return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
		}
		status.ActiveInstance = nil
		if pool.Spec.DesiredReplicas == 0 {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
			status.Message = "new runtime template is staged with no provider workspace demand"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "RolloutConverged", status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		status.Message = "old provider workspace terminated; starting the new immutable runtime template"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStarting, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if len(readyPods) == 0 {
		if pool.Status.ActiveInstance != nil {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "cannot authenticate the previous active runtime instance before workspace replacement"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if !runtimePoolRolloutControllerWorkIsQuiescent(status.Capacity) {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDraining
			status.Message = "waiting for controller reservations or finalization work before replacing an unadmitted provider workspace"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonDraining, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "replacing an unadmitted provider workspace before applying the new template"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	pod := &readyPods[0]
	deployedTemplate := sandboxTemplatePodTemplateSpec(sandboxTemplate)
	validationPool, validationConfig, err := runtimePoolPodTemplateValidationTarget(pool, deployedTemplate)
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	deployedAuthSecret, err := r.runtimePoolPodTemplateAuthSecret(ctx, pool, cfg.namespace, deployedTemplate.Spec)
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(pool, pod), string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]), deployedAuthSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated rollout status probe failed: %w", err))
	}
	active, err := validateRuntimePoolProbeForRollout(validationPool, validationConfig, pod, probe, r.now())
	if err != nil {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, err)
	}
	if runtimePoolSupervisorRestartedInPlace(pool.Status.ActiveInstance, active) {
		return r.reconcileRuntimePoolInPlaceSupervisorRestart(ctx, pool, nil, pod, active, status)
	}
	if !runtimePoolRolloutActiveInstanceMatches(pool.Status.ActiveInstance, active) {
		return r.finishRuntimePoolRolloutFailure(ctx, pool, status, fmt.Errorf("authenticated runtime identity changed before rollout drain"))
	}
	status.ActiveInstance = active
	applyRuntimePoolProbeCapacity(&status, cfg, probe)
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionTrue, "PodScheduled", "selected old runtime Pod remains scheduled during rollout drain")

	if !probe.Status.Drain.Requested {
		reason := "runtime_pool_rollout_" + runtimePoolShortRevision(desiredTemplate.Annotations[runtimePoolTemplateRevisionAnnotation])
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(pool, pod),
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
			status.Message = "timed out waiting for authenticated rollout drain barriers; preserving the old provider workspace"
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

	if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent old provider workspace is stopping before the template changes"
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionUnknown, runtimePoolRolloutReasonStopping, status.Message)
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// reconcileWorkspaceRuntimePoolScaleDown mirrors the Deployment scale-to-zero
// barriers: authenticated drain, an observed quiescent status, a persisted
// Quiescent barrier, then claim deletion.
//
//nolint:gocyclo // The scale-down barriers intentionally mirror the audited Deployment path.
func (r *RuntimePoolReconciler) reconcileWorkspaceRuntimePoolScaleDown(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	claim *sandboxextv1beta1.SandboxClaim,
	pods []corev1.Pod,
	readyPods []corev1.Pod,
	authSecret *corev1.Secret,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "runtime pool is scaling down")

	if claim == nil || !claim.DeletionTimestamp.IsZero() {
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		if claim == nil && len(pods) == 0 {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
			status.ActiveInstance = nil
			status.Message = runtimePoolMessageStopped
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionUnknown, "ScaledToZero", status.Message)
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ScaledToZero", status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.Message = "waiting for the quiescent provider workspace to terminate"
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if len(readyPods) == 0 {
		status.ActiveInstance = nil
		if pool.Status.ActiveInstance == nil && runtimePoolControllerWorkIsQuiescent(pool.Status.Capacity) {
			if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
				return ctrl.Result{}, err
			}
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "stopping a provider workspace that never became active"
			return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
		}
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = runtimePoolMessageDrainUnauthenticated
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}

	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(pool, &readyPods[0]), string(authSecret.Data[runtimePoolControllerTokenKey]), authSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage("authenticated drain status probe failed: " + err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	active, err := validateRuntimePoolProbe(pool, cfg, &readyPods[0], probe, r.now())
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
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionSchedulingReady, metav1.ConditionTrue, "PodScheduled", "selected runtime Pod is scheduled")
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "ExactInstanceReady", "selected runtime Pod and supervisor profile are ready")

	if !probe.Status.Drain.Requested {
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(pool, &readyPods[0]),
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

	if err := r.deleteRuntimePoolSandboxClaim(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent provider workspace is stopping"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// recycleRuntimePoolInstance replaces the exact selected runtime instance:
// Deployment-backed pools delete the runtime Pod for controller-owned emptyDir
// replacement; workspace-backed pools delete the SandboxClaim so the provider
// cascades the sandbox and Pod.
func (r *RuntimePoolReconciler) recycleRuntimePoolInstance(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	pod *corev1.Pod,
) error {
	if pool.Spec.ExecutionWorkspace == nil {
		if err := r.Delete(ctx, pod, deleteCurrentObjectPreconditions(pod)...); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	cfg, err := r.runtimePoolConfigForDeletion(pool)
	if err != nil {
		return err
	}
	if runtimePoolIsSubstrateBacked(pool) {
		control, controlErr := r.substrateActorControlForCleanup()
		if controlErr != nil {
			return controlErr
		}
		defer control.Close() //nolint:errcheck // best-effort connection teardown
		return r.recycleSubstrateActor(ctx, pool, control, runtimePoolSubstrateActorID(cfg.baseName))
	}
	claim, err := r.getRuntimePoolSandboxClaim(ctx, cfg)
	if err != nil {
		return err
	}
	if claim == nil {
		return nil
	}
	return r.deleteRuntimePoolSandboxClaim(ctx, claim)
}

// sandboxReader returns the uncached reader for provider workload objects:
// the namespace-scoped manager cache does not watch sandbox extension kinds in
// the runtime namespace, and the provider CRDs may legitimately be absent.
func (r *RuntimePoolReconciler) sandboxReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *RuntimePoolReconciler) getRuntimePoolSandboxTemplate(ctx context.Context, cfg runtimePoolConfig) (*sandboxextv1beta1.SandboxTemplate, error) {
	template := &sandboxextv1beta1.SandboxTemplate{}
	err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: runtimePoolSandboxTemplateName(cfg.baseName)}, template)
	if err != nil {
		return nil, ignoreSandboxAPIAbsence("read RuntimePool sandbox template", err)
	}
	return template, nil
}

func (r *RuntimePoolReconciler) getRuntimePoolSandboxClaim(ctx context.Context, cfg runtimePoolConfig) (*sandboxextv1beta1.SandboxClaim, error) {
	claim := &sandboxextv1beta1.SandboxClaim{}
	err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: runtimePoolSandboxClaimName(cfg.baseName)}, claim)
	if err != nil {
		return nil, ignoreSandboxAPIAbsence("read RuntimePool sandbox claim", err)
	}
	return claim, nil
}

// ignoreSandboxAPIAbsence maps NotFound to nil and a missing agent-sandbox CRD
// installation to an explicit fail-closed configuration error.
func ignoreSandboxAPIAbsence(operation string, err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	if apimeta.IsNoMatchError(err) || k8sRuntimeIsMissingKindError(err) {
		return fmt.Errorf("%s: the agent-sandbox provider CRDs are not installed; workspace-backed RuntimePools require an externally operated agent-sandbox installation", operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func k8sRuntimeIsMissingKindError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no kind is registered")
}

func runtimePoolWorkspaceBootstrapTemplate(
	template corev1.PodTemplateSpec,
	nonce, publicKey string,
) corev1.PodTemplateSpec {
	result := *template.DeepCopy()
	if len(result.Spec.Containers) != 1 {
		return result
	}
	container := &result.Spec.Containers[0]
	env := make([]corev1.EnvVar, 0, len(container.Env)+2)
	for i := range container.Env {
		switch container.Env[i].Name {
		case "ORKA_ACP_CONTROLLER_TOKEN_FILE", "ORKA_ACP_CAPABILITY_SECRET_FILE", "ORKA_ACP_PROVIDER_TOKEN_FILE",
			"ORKA_ACP_CONTROLLER_TOKEN_BOOTSTRAP", "ORKA_ACP_CAPABILITY_SECRET_BOOTSTRAP", "ORKA_ACP_PROVIDER_TOKEN_BOOTSTRAP":
			continue
		default:
			env = append(env, container.Env[i])
		}
	}
	env = append(env,
		corev1.EnvVar{Name: "ORKA_ACP_CREDENTIAL_BOOTSTRAP_NONCE", Value: nonce},
		corev1.EnvVar{Name: harnessv2.CredentialBootstrapPublicKeyEnv, Value: publicKey},
	)
	container.Env = env
	container.EnvFrom = nil
	mounts := container.VolumeMounts[:0]
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name != runtimePoolAuthVolume && container.VolumeMounts[i].Name != runtimePoolProviderCapabilityVolume {
			mounts = append(mounts, container.VolumeMounts[i])
		}
	}
	container.VolumeMounts = mounts
	volumes := result.Spec.Volumes[:0]
	for i := range result.Spec.Volumes {
		if result.Spec.Volumes[i].Name != runtimePoolAuthVolume && result.Spec.Volumes[i].Name != runtimePoolProviderCapabilityVolume {
			volumes = append(volumes, result.Spec.Volumes[i])
		}
	}
	result.Spec.Volumes = volumes
	result.Annotations[runtimePoolTemplateRevisionAnnotation] = runtimePoolPodTemplateRevision(result)
	return result
}

func (r *RuntimePoolReconciler) attestWorkspaceRuntimePoolMaterialization(
	ctx context.Context,
	claim *sandboxextv1beta1.SandboxClaim,
	template *sandboxextv1beta1.SandboxTemplate,
	pod *corev1.Pod,
) (bool, error) {
	if claim == nil || template == nil || pod == nil {
		return false, nil
	}
	sandbox := &sandboxv1beta1.Sandbox{}
	err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, sandbox)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, ignoreSandboxAPIAbsence("read RuntimePool sandbox materialization", err)
	}
	if claim.UID != "" && !metav1.IsControlledBy(sandbox, claim) {
		return false, fmt.Errorf("provider Sandbox is not controlled by the exact SandboxClaim")
	}
	if claim.UID != "" && sandbox.Labels[sandboxextv1beta1.SandboxIDLabel] != string(claim.UID) {
		return false, fmt.Errorf("provider Sandbox does not carry the exact SandboxClaim identity")
	}
	if sandbox.Annotations[sandboxv1beta1.SandboxTemplateRefAnnotation] != template.Name {
		return false, fmt.Errorf("provider Sandbox does not record the validated SandboxTemplate")
	}
	expectedSpec := template.Spec.PodTemplate.Spec.DeepCopy()
	sandboxextcontrollers.ApplySandboxSecureDefaults(template, expectedSpec)
	if !apiequality.Semantic.DeepEqual(*expectedSpec, sandbox.Spec.PodTemplate.Spec) {
		return false, fmt.Errorf("provider Sandbox PodSpec differs from the validated SandboxTemplate revision")
	}
	if !apiequality.Semantic.DeepEqual(template.Spec.VolumeClaimTemplates, sandbox.Spec.VolumeClaimTemplates) {
		return false, fmt.Errorf("provider Sandbox volume claims differ from the validated SandboxTemplate revision")
	}
	if !reflect.DeepEqual(template.Spec.PodTemplate.ObjectMeta.Annotations, sandbox.Spec.PodTemplate.ObjectMeta.Annotations) {
		return false, fmt.Errorf("provider Sandbox Pod annotations differ from the validated SandboxTemplate revision")
	}
	if claim.UID != "" && sandbox.Spec.PodTemplate.ObjectMeta.Labels[sandboxextv1beta1.SandboxIDLabel] != string(claim.UID) {
		return false, fmt.Errorf("provider Sandbox Pod template does not carry the exact SandboxClaim identity")
	}
	materializedLabels := cloneStringMap(sandbox.Spec.PodTemplate.ObjectMeta.Labels)
	delete(materializedLabels, sandboxextv1beta1.SandboxIDLabel)
	delete(materializedLabels, sandboxv1beta1.SandboxTemplateRefHashLabel)
	if !reflect.DeepEqual(template.Spec.PodTemplate.ObjectMeta.Labels, materializedLabels) {
		return false, fmt.Errorf("provider Sandbox Pod labels differ from the validated SandboxTemplate revision")
	}
	if sandbox.UID != "" && !metav1.IsControlledBy(pod, sandbox) {
		return false, fmt.Errorf("runtime Pod is not controlled by the attested provider Sandbox")
	}
	if !runtimePoolWorkspacePodLabelsMatch(sandbox, pod) {
		return false, fmt.Errorf("runtime Pod labels differ from the attested provider Sandbox")
	}
	if !runtimePoolWorkspacePodSpecsMatch(sandbox.Spec.PodTemplate.Spec, pod.Spec) {
		return false, fmt.Errorf("runtime PodSpec differs from the attested provider Sandbox")
	}
	return true, nil
}

func runtimePoolWorkspacePodLabelsMatch(sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod) bool {
	if sandbox == nil || pod == nil {
		return false
	}
	expected := cloneStringMap(sandbox.Spec.PodTemplate.ObjectMeta.Labels)
	// agent-sandbox reserves agents.x-k8s.io/* labels and does not propagate
	// the claim UID from the Sandbox PodTemplate onto the Pod. The exact claim
	// identity is instead attested above through the claim -> Sandbox -> Pod
	// owner chain and the controller-owned claim labels on the Sandbox.
	delete(expected, sandboxextv1beta1.SandboxIDLabel)
	expected[sandboxcontrollers.SandboxNameHashLabel] = sandboxcontrollers.NameHash(sandbox.Name)
	return reflect.DeepEqual(expected, pod.Labels)
}

// runtimePoolWorkspacePodSpecsMatch compares the realized Pod against the
// attested Sandbox template before any credentials cross the network. The
// provider may adopt an existing Pod without reconciling its spec, so owner
// references and identity labels are not sufficient proof. Normalize only
// fields populated by the core API server, admission, or scheduler; every
// container, volume, security, network, and runtime field remains fail-closed.
func runtimePoolWorkspacePodSpecsMatch(expected, actual corev1.PodSpec) bool {
	expectedSpec := normalizeRuntimePoolWorkspacePodSpec(expected)
	actualSpec := normalizeRuntimePoolWorkspacePodSpec(actual)

	// These fields are derived from cluster scheduling/admission state rather
	// than from the provider-visible Sandbox template.
	expectedSpec.NodeName, actualSpec.NodeName = "", ""
	expectedSpec.Priority, actualSpec.Priority = nil, nil
	expectedSpec.PreemptionPolicy, actualSpec.PreemptionPolicy = nil, nil
	expectedSpec.Overhead, actualSpec.Overhead = nil, nil
	if len(expectedSpec.ImagePullSecrets) == 0 {
		actualSpec.ImagePullSecrets = nil
	}
	if expectedSpec.PriorityClassName == "" {
		actualSpec.PriorityClassName = ""
	}

	return apiequality.Semantic.DeepEqual(expectedSpec, actualSpec)
}

func normalizeRuntimePoolWorkspacePodSpec(spec corev1.PodSpec) corev1.PodSpec {
	result := *spec.DeepCopy()
	if result.DNSPolicy == "" {
		result.DNSPolicy = corev1.DNSClusterFirst
	}
	if result.RestartPolicy == "" {
		result.RestartPolicy = corev1.RestartPolicyAlways
	}
	if result.SecurityContext == nil {
		result.SecurityContext = &corev1.PodSecurityContext{}
	}
	if result.TerminationGracePeriodSeconds == nil {
		result.TerminationGracePeriodSeconds = new(int64)
		*result.TerminationGracePeriodSeconds = corev1.DefaultTerminationGracePeriodSeconds
	}
	if result.SchedulerName == "" {
		result.SchedulerName = corev1.DefaultSchedulerName
	}
	if result.EnableServiceLinks == nil {
		result.EnableServiceLinks = new(bool)
		*result.EnableServiceLinks = corev1.DefaultEnableServiceLinks
	}
	if result.ServiceAccountName == "" {
		result.ServiceAccountName = runtimePoolDefaultServiceAccountName
	}
	for i := range result.InitContainers {
		normalizeRuntimePoolWorkspaceContainer(&result.InitContainers[i])
	}
	for i := range result.Containers {
		normalizeRuntimePoolWorkspaceContainer(&result.Containers[i])
	}
	result.Tolerations = runtimePoolWorkspaceExplicitTolerations(result.Tolerations)
	return result
}

func normalizeRuntimePoolWorkspaceContainer(container *corev1.Container) {
	if container == nil {
		return
	}
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for i := range container.Ports {
		if container.Ports[i].Protocol == "" {
			container.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for i := range container.Env {
		fieldRef := container.Env[i].ValueFrom
		if fieldRef != nil && fieldRef.FieldRef != nil && fieldRef.FieldRef.APIVersion == "" {
			fieldRef.FieldRef.APIVersion = "v1"
		}
	}
}

func runtimePoolWorkspaceExplicitTolerations(tolerations []corev1.Toleration) []corev1.Toleration {
	result := make([]corev1.Toleration, 0, len(tolerations))
	for i := range tolerations {
		toleration := tolerations[i]
		if toleration.Operator == corev1.TolerationOpExists && toleration.Effect == corev1.TaintEffectNoExecute &&
			(toleration.Key == corev1.TaintNodeNotReady || toleration.Key == corev1.TaintNodeUnreachable) {
			continue
		}
		result = append(result, toleration)
	}
	return result
}

func (r *RuntimePoolReconciler) seedWorkspaceSupervisorCredentials(
	ctx context.Context,
	endpoint, nonce string,
	authSecret, providerSecret *corev1.Secret,
) (bool, error) {
	request := harnessv2.CredentialBootstrapRequest{
		ControllerToken:  strings.TrimSpace(string(authSecret.Data[runtimePoolControllerTokenKey])),
		CapabilitySecret: strings.TrimSpace(string(authSecret.Data[runtimePoolCapabilitySecretKey])),
		ProviderToken:    strings.TrimSpace(string(providerSecret.Data[runtimePoolProviderTokenKey])),
	}
	if err := request.Validate(); err != nil {
		return false, fmt.Errorf("pool credentials are incomplete: %w", err)
	}
	if r.WorkspaceCredentialSeeder != nil {
		return r.WorkspaceCredentialSeeder(ctx, endpoint, nonce, authSecret.Data[runtimePoolCapabilitySecretKey], request)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	seedCtx, cancel := context.WithTimeout(ctx, runtimePoolProbeTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		seedCtx, http.MethodPut, strings.TrimRight(endpoint, "/")+harnessv2.CredentialBootstrapPath, bytes.NewReader(payload),
	)
	if err != nil {
		return false, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	signature, err := harnessv2.SignCredentialBootstrap(authSecret.Data[runtimePoolCapabilitySecretKey], nonce, payload)
	if err != nil {
		return false, fmt.Errorf("sign credential bootstrap request: %w", err)
	}
	httpRequest.Header.Set(harnessv2.CredentialBootstrapSignatureHeader, signature)
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: runtimePoolProbeTimeout, Transport: harnessv2.NewProxylessTransport()}
	}
	isolationClient := *httpClient
	isolationClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := isolationClient.Do(httpRequest)
	if err != nil {
		return false, err
	}
	defer response.Body.Close() //nolint:errcheck // response body is unused
	switch response.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return false, nil
	case http.StatusConflict:
		return false, errWorkspaceCredentialConflict
	case http.StatusNotFound:
		return true, nil
	default:
		return false, fmt.Errorf("credential bootstrap returned status %d", response.StatusCode)
	}
}

func (r *RuntimePoolReconciler) createRuntimePoolSandboxTemplate(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	desiredTemplate corev1.PodTemplateSpec,
) error {
	template := &sandboxextv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runtimePoolSandboxTemplateName(cfg.baseName),
			Namespace: cfg.namespace,
			Labels:    cloneStringMap(cfg.labels),
		},
		Spec: runtimePoolSandboxTemplateSpec(desiredTemplate),
	}
	if err := r.setRuntimePoolControllerReference(pool, template); err != nil {
		return err
	}
	if err := r.Create(ctx, template); err != nil && !apierrors.IsAlreadyExists(err) {
		return ignoreSandboxAPIAbsence("create RuntimePool sandbox template", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) updateRuntimePoolSandboxTemplate(
	ctx context.Context,
	template *sandboxextv1beta1.SandboxTemplate,
	desiredTemplate corev1.PodTemplateSpec,
) error {
	if template == nil {
		return fmt.Errorf("RuntimePool sandbox template is required for a template update")
	}
	base := template.DeepCopy()
	template.Spec = runtimePoolSandboxTemplateSpec(desiredTemplate)
	if err := r.Patch(ctx, template, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("update RuntimePool sandbox template: %w", err)
	}
	return nil
}

// runtimePoolSandboxTemplateSpec renders the exact controller-owned supervisor
// Pod template into the provider's template shape. The provider's managed
// NetworkPolicy is disabled: the pool's own default-deny NetworkPolicies select
// the workspace Pod through the propagated pool labels, and claim-side env or
// volume injection stays disallowed so credentials never cross the provider API.
func runtimePoolSandboxTemplateSpec(desiredTemplate corev1.PodTemplateSpec) sandboxextv1beta1.SandboxTemplateSpec {
	return sandboxextv1beta1.SandboxTemplateSpec{
		SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
			PodTemplate: sandboxv1beta1.PodTemplate{
				ObjectMeta: sandboxv1beta1.PodMetadata{
					Labels:      cloneStringMap(desiredTemplate.Labels),
					Annotations: cloneStringMap(desiredTemplate.Annotations),
				},
				Spec: *desiredTemplate.Spec.DeepCopy(),
			},
		},
		NetworkPolicyManagement:    sandboxextv1beta1.NetworkPolicyManagementUnmanaged,
		EnvVarsInjectionPolicy:     sandboxextv1beta1.EnvVarsInjectionPolicyDisallowed,
		VolumeClaimTemplatesPolicy: sandboxextv1beta1.VolumeClaimTemplatesPolicyDisallowed,
	}
}

func sandboxTemplatePodTemplateSpec(template *sandboxextv1beta1.SandboxTemplate) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      cloneStringMap(template.Spec.PodTemplate.ObjectMeta.Labels),
			Annotations: cloneStringMap(template.Spec.PodTemplate.ObjectMeta.Annotations),
		},
		Spec: *template.Spec.PodTemplate.Spec.DeepCopy(),
	}
}

func runtimePoolSandboxTemplateSpecRevision(spec sandboxextv1beta1.SandboxTemplateSpec) string {
	revision, err := runtimePoolJSONRevision(spec)
	if err != nil {
		panic(fmt.Sprintf("marshal RuntimePool SandboxTemplate revision: %v", err))
	}
	return revision
}

func runtimePoolSandboxTemplateObjectRevision(template *sandboxextv1beta1.SandboxTemplate) (string, error) {
	if template == nil {
		return "", fmt.Errorf("RuntimePool SandboxTemplate is required")
	}
	revision, err := runtimePoolJSONRevision(template.Spec)
	if err != nil {
		return "", fmt.Errorf("compute RuntimePool SandboxTemplate revision: %w", err)
	}
	return revision, nil
}

func runtimePoolSandboxChildOwnedByPool(object client.Object, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig) bool {
	if object == nil || pool == nil || object.GetNamespace() != cfg.namespace {
		return false
	}
	labels := object.GetLabels()
	for key, value := range cfg.labels {
		if value == "" || labels[key] != value {
			return false
		}
	}
	if object.GetNamespace() == pool.Namespace && !metav1.IsControlledBy(object, pool) {
		return false
	}
	return true
}

func runtimePoolSandboxClaimMatchesPool(claim *sandboxextv1beta1.SandboxClaim, cfg runtimePoolConfig) bool {
	return claim != nil && claim.Spec.WarmPoolRef.Name == runtimePoolSandboxWarmPoolName(cfg.baseName) &&
		len(claim.Spec.Env) == 0 && len(claim.Spec.VolumeClaimTemplates) == 0
}

func (r *RuntimePoolReconciler) setRuntimePoolSandboxTemplateRevision(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	revision string,
) error {
	if strings.TrimSpace(revision) == "" {
		return fmt.Errorf("RuntimePool SandboxTemplate revision is required")
	}
	if pool.Annotations[runtimePoolSandboxTemplateRevisionAnnotation] == revision {
		return nil
	}
	base := pool.DeepCopy()
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[runtimePoolSandboxTemplateRevisionAnnotation] = revision
	if err := r.Patch(ctx, pool, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("record RuntimePool SandboxTemplate revision: %w", err)
	}
	return nil
}

func runtimePoolSandboxTemplateNeedsRollout(template *sandboxextv1beta1.SandboxTemplate, desiredTemplate corev1.PodTemplateSpec) bool {
	if template == nil {
		return false
	}
	desiredRevision := strings.TrimSpace(desiredTemplate.Annotations[runtimePoolTemplateRevisionAnnotation])
	deployedRevision := strings.TrimSpace(template.Spec.PodTemplate.ObjectMeta.Annotations[runtimePoolTemplateRevisionAnnotation])
	return desiredRevision == "" || deployedRevision != desiredRevision
}

func (r *RuntimePoolReconciler) ensureRuntimePoolSandboxWarmPool(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) error {
	warmPool := &sandboxextv1beta1.SandboxWarmPool{}
	name := runtimePoolSandboxWarmPoolName(cfg.baseName)
	err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: name}, warmPool)
	if apierrors.IsNotFound(err) {
		// Zero replicas: every claim cold-starts from the exact current
		// template, so a stale pre-warmed Pod can never be adopted.
		warmPool = &sandboxextv1beta1.SandboxWarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.namespace, Labels: cloneStringMap(cfg.labels)},
			Spec: sandboxextv1beta1.SandboxWarmPoolSpec{
				Replicas:    new(int32),
				TemplateRef: sandboxextv1beta1.SandboxTemplateRef{Name: runtimePoolSandboxTemplateName(cfg.baseName)},
			},
		}
		if err := r.setRuntimePoolControllerReference(pool, warmPool); err != nil {
			return err
		}
		if err := r.Create(ctx, warmPool); err != nil && !apierrors.IsAlreadyExists(err) {
			return ignoreSandboxAPIAbsence("create RuntimePool sandbox warm pool", err)
		}
		return nil
	}
	if err != nil {
		return ignoreSandboxAPIAbsence("read RuntimePool sandbox warm pool", err)
	}
	if !runtimePoolSandboxChildOwnedByPool(warmPool, pool, cfg) {
		return fmt.Errorf("same-name SandboxWarmPool does not carry the exact RuntimePool ownership identity")
	}
	if warmPool.Spec.TemplateRef.Name != runtimePoolSandboxTemplateName(cfg.baseName) ||
		warmPool.Spec.Replicas == nil || *warmPool.Spec.Replicas != 0 {
		base := warmPool.DeepCopy()
		warmPool.Spec.Replicas = new(int32)
		warmPool.Spec.TemplateRef = sandboxextv1beta1.SandboxTemplateRef{Name: runtimePoolSandboxTemplateName(cfg.baseName)}
		if err := r.Patch(ctx, warmPool, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("update RuntimePool sandbox warm pool: %w", err)
		}
	}
	return nil
}

func (r *RuntimePoolReconciler) createRuntimePoolSandboxClaim(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) error {
	claim := &sandboxextv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runtimePoolSandboxClaimName(cfg.baseName),
			Namespace: cfg.namespace,
			Labels:    cloneStringMap(cfg.labels),
		},
		Spec: sandboxextv1beta1.SandboxClaimSpec{
			WarmPoolRef: sandboxextv1beta1.SandboxWarmPoolRef{Name: runtimePoolSandboxWarmPoolName(cfg.baseName)},
		},
	}
	if err := r.setRuntimePoolControllerReference(pool, claim); err != nil {
		return err
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return ignoreSandboxAPIAbsence("create RuntimePool sandbox claim", err)
	}
	return nil
}

func (r *RuntimePoolReconciler) deleteRuntimePoolSandboxClaim(ctx context.Context, claim *sandboxextv1beta1.SandboxClaim) error {
	if claim == nil || !claim.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := r.Delete(ctx, claim, deleteCurrentObjectPreconditions(claim)...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete RuntimePool sandbox claim: %w", err)
	}
	return nil
}

// applySandboxClaimFailureConditions surfaces provider claim failures through
// the sanitized RolloutReady condition without exposing provider identifiers.
func (r *RuntimePoolReconciler) applySandboxClaimFailureConditions(
	pool *corev1alpha1.RuntimePool,
	claim *sandboxextv1beta1.SandboxClaim,
	status *corev1alpha1.RuntimePoolStatus,
) bool {
	if claim == nil || status == nil {
		return false
	}
	for i := range claim.Status.Conditions {
		condition := claim.Status.Conditions[i]
		if condition.Type != string(sandboxv1beta1.SandboxConditionReady) || condition.Status != metav1.ConditionFalse {
			continue
		}
		if strings.TrimSpace(condition.Reason) == sandboxv1beta1.SandboxReasonDependenciesNotReady {
			// A provisioning workspace is expected while starting; readiness
			// gating is owned by the Ready-Pod and fence probes.
			continue
		}
		message := sanitizeRuntimePoolMessage("provider workspace claim is not ready: " + condition.Message)
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = message
		r.setRuntimePoolCondition(pool, status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, message)
		r.setRuntimePoolCondition(pool, status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, message)
		return true
	}
	return false
}

// pruneStaleWorkspaceRuntimePoolSecrets removes epoch-scoped credential Secrets
// no longer referenced by the current names, the provider template, or any live
// workspace Pod. It mirrors the Deployment-path pruning ownership rules.
func (r *RuntimePoolReconciler) pruneStaleWorkspaceRuntimePoolSecrets(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	sandboxTemplate *sandboxextv1beta1.SandboxTemplate,
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
	if sandboxTemplate != nil {
		addRuntimePoolSecretReferences(keep, sandboxTemplate.Spec.PodTemplate.Spec)
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(cfg.namespace), client.MatchingLabels{
		runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel],
		runtimePoolUIDLabel: string(pool.UID),
	}); err != nil {
		return fmt.Errorf("list RuntimePool workspace Pods for stale credential cleanup: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodSucceeded || pods.Items[i].Status.Phase == corev1.PodFailed {
			continue
		}
		addRuntimePoolSecretReferences(keep, pods.Items[i].Spec)
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

// deleteRuntimePoolWorkspaceChildren removes provider workload objects during
// pool finalization. Missing CRDs are tolerated: nothing provider-owned can
// exist without the provider installation.
func (r *RuntimePoolReconciler) deleteRuntimePoolWorkspaceChildren(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
) (bool, error) {
	remaining := false
	objects := []client.Object{
		&sandboxextv1beta1.SandboxClaim{ObjectMeta: metav1.ObjectMeta{Name: runtimePoolSandboxClaimName(cfg.baseName), Namespace: cfg.namespace}},
		&sandboxextv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: runtimePoolSandboxWarmPoolName(cfg.baseName), Namespace: cfg.namespace}},
		&sandboxextv1beta1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: runtimePoolSandboxTemplateName(cfg.baseName), Namespace: cfg.namespace}},
	}
	for _, obj := range objects {
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
		if err := r.sandboxReader().Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) || k8sRuntimeIsMissingKindError(err) {
				continue
			}
			return false, err
		}
		if !runtimePoolSandboxChildOwnedByPool(obj, pool, cfg) {
			return false, fmt.Errorf("refusing to delete foreign same-name provider resource %T %s/%s", obj, obj.GetNamespace(), obj.GetName())
		}
		if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		remaining = true
	}
	return remaining, nil
}

// runtimePoolPodTemplateValidationTarget reconstructs the deployed pool
// identity (generation, epoch, profile) from a rendered Pod template so a
// draining old instance validates against the exact profile it was booted with.
func runtimePoolPodTemplateValidationTarget(
	pool *corev1alpha1.RuntimePool,
	template corev1.PodTemplateSpec,
) (*corev1alpha1.RuntimePool, runtimePoolConfig, error) {
	if pool == nil || len(template.Spec.Containers) != 1 {
		return nil, runtimePoolConfig{}, fmt.Errorf("deployed RuntimePool template is invalid")
	}
	return runtimePoolValidationTargetFromTemplate(pool, template)
}

func (r *RuntimePoolReconciler) runtimePoolPodTemplateAuthSecret(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	namespace string,
	podSpec corev1.PodSpec,
) (*corev1.Secret, error) {
	secretName := ""
	for i := range podSpec.Volumes {
		volume := podSpec.Volumes[i]
		if volume.Name == runtimePoolAuthVolume && volume.Secret != nil {
			secretName = strings.TrimSpace(volume.Secret.SecretName)
			break
		}
	}
	var secret *corev1.Secret
	if secretName != "" {
		secret = &corev1.Secret{}
		if err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
			return nil, fmt.Errorf("get deployed RuntimePool auth Secret: %w", err)
		}
	} else {
		epochText := ""
		if len(podSpec.Containers) == 1 {
			epochText = strings.TrimSpace(runtimePoolLiteralEnvironment(podSpec.Containers[0].Env)["ORKA_ACP_CONTROLLER_EPOCH"])
		}
		epoch, err := strconv.ParseInt(epochText, 10, 64)
		if err != nil || epoch <= 0 {
			return nil, fmt.Errorf("deployed RuntimePool auth Secret reference is missing")
		}
		var secrets corev1.SecretList
		if err := r.sandboxReader().List(ctx, &secrets, client.InNamespace(namespace), client.MatchingLabels{
			runtimePoolAuthLabel: "true",
			runtimePoolUIDLabel:  string(pool.UID),
		}); err != nil {
			return nil, fmt.Errorf("list deployed RuntimePool auth Secrets: %w", err)
		}
		secret, err = runtimePoolAuthSecretForEpoch(secrets.Items, epoch)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKey])) == "" ||
		len(secret.Data[runtimePoolCapabilitySecretKey]) == 0 {
		return nil, fmt.Errorf("deployed RuntimePool auth Secret is incomplete")
	}
	return secret, nil
}
