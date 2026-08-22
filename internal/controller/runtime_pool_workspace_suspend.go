/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	// runtimePoolWorkspaceSuspendAnnotation records the ACP workspace
	// adapter's data-only suspension intent for a workspace-backed pool. It is
	// honored only on suspend-capable pools; the backend-specific drain then
	// ends in a consensual data checkpoint instead of teardown.
	runtimePoolWorkspaceSuspendAnnotation = "orka.ai/workspace-suspend"
	// sandboxSuspendedAnnotation records the exact Sandbox (name and UID)
	// whose PVC-backed suspension this controller requested. It is written
	// before the provider mutation so a restart resumes the same consensual
	// suspension, and resume patches only that exact Sandbox.
	sandboxSuspendedAnnotation = "orka.ai/sandbox-suspended"
)

// sandboxSuspendRecord pins the exact suspended Sandbox identity.
type sandboxSuspendRecord struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// runtimePoolWorkspaceSuspendCapable reports whether the pool's immutable
// binding permits data-only cold suspension on either backend.
func runtimePoolWorkspaceSuspendCapable(pool *corev1alpha1.RuntimePool) bool {
	return substrateRuntimePoolSuspendCapable(pool) || sandboxRuntimePoolSuspendCapable(pool)
}

// sandboxRuntimePoolSuspendCapable reports an agent-sandbox pool whose binding
// permits PVC-backed cold suspension.
func sandboxRuntimePoolSuspendCapable(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && pool.Spec.ExecutionWorkspace != nil &&
		pool.Spec.ExecutionWorkspace.AgentSandbox != nil &&
		pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendMode == string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) &&
		pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume != nil
}

// sandboxWorkspaceSuspendRequested reports the workspace adapter's suspension
// intent for an agent-sandbox pool.
func sandboxWorkspaceSuspendRequested(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && strings.TrimSpace(pool.Annotations[runtimePoolWorkspaceSuspendAnnotation]) != "" &&
		sandboxRuntimePoolSuspendCapable(pool)
}

// runtimePoolWorkspaceSuspendConsentRecorded reports whether the backend
// persisted a consensual checkpoint record for either provider.
func runtimePoolWorkspaceSuspendConsentRecorded(pool *corev1alpha1.RuntimePool) bool {
	if pool == nil {
		return false
	}
	return strings.TrimSpace(pool.Annotations[substrateActorSuspendedAnnotation]) != "" ||
		strings.TrimSpace(pool.Annotations[sandboxSuspendedAnnotation]) != ""
}

// sandboxConsensualSuspendRecord parses the recorded suspended-Sandbox
// identity, or nil when no consensual suspension is recorded.
func sandboxConsensualSuspendRecord(pool *corev1alpha1.RuntimePool) *sandboxSuspendRecord {
	if pool == nil || !sandboxRuntimePoolSuspendCapable(pool) {
		return nil
	}
	raw := strings.TrimSpace(pool.Annotations[sandboxSuspendedAnnotation])
	if raw == "" {
		return nil
	}
	record := &sandboxSuspendRecord{}
	if err := json.Unmarshal([]byte(raw), record); err != nil || record.Name == "" || record.UID == "" {
		return nil
	}
	return record
}

// sandboxAwaitingWorkspaceResume reports a consensually suspended sandbox pool
// whose cold resume has not produced a running Pod yet, so consumed one-time
// bootstrap material rotates before the fresh boot exactly as it does for a
// replacement claim.
func sandboxAwaitingWorkspaceResume(pool *corev1alpha1.RuntimePool) bool {
	return sandboxConsensualSuspendRecord(pool) != nil
}

// runtimePoolDurableWorkspaceTemplate mounts the reserved durable workspace
// volume into the supervisor container and points the supervisor at it. The
// provider injects the matching PVC-backed Pod volume from the claim's
// volumeClaimTemplates. The revision annotation is restamped by the bootstrap
// template helper that calls this.
func runtimePoolDurableWorkspaceTemplate(template corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	result := *template.DeepCopy()
	if len(result.Spec.Containers) != 1 {
		return result
	}
	container := &result.Spec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: substrateDurableWorkspaceVolume, MountPath: substrateDurableWorkspaceMountPath,
	})
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "ORKA_ACP_DURABLE_WORKSPACE_DIR", Value: substrateDurableWorkspaceMountPath,
	})
	return result
}

// stripInjectedDurableWorkspaceVolume removes the provider-injected
// reserved-name PVC volume from the observed Pod volumes when the expected
// template does not declare it. Only that exact shape is stripped; every other
// unexpected volume still fails the attestation comparison.
func stripInjectedDurableWorkspaceVolume(expected, actual []corev1.Volume) []corev1.Volume {
	for _, volume := range expected {
		if volume.Name == substrateDurableWorkspaceVolume {
			return actual
		}
	}
	result := make([]corev1.Volume, 0, len(actual))
	for _, volume := range actual {
		if volume.Name == substrateDurableWorkspaceVolume && volume.PersistentVolumeClaim != nil {
			continue
		}
		result = append(result, volume)
	}
	return result
}

// runtimePoolDurableVolumeClaimTemplate renders the frozen durable workspace
// PVC template requested through the pool's SandboxClaim.
func runtimePoolDurableVolumeClaimTemplate(pool *corev1alpha1.RuntimePool) (sandboxv1beta1.PersistentVolumeClaimTemplate, error) {
	spec := pool.Spec.ExecutionWorkspace.AgentSandbox.SuspendVolume
	capacity, err := resource.ParseQuantity(spec.Capacity)
	if err != nil {
		return sandboxv1beta1.PersistentVolumeClaimTemplate{}, fmt.Errorf("frozen durable workspace capacity is invalid: %w", err)
	}
	modes := make([]corev1.PersistentVolumeAccessMode, 0, len(spec.AccessModes))
	for _, mode := range spec.AccessModes {
		modes = append(modes, corev1.PersistentVolumeAccessMode(mode))
	}
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	claimSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: modes,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: capacity},
		},
	}
	if spec.StorageClassName != "" {
		className := spec.StorageClassName
		claimSpec.StorageClassName = &className
	}
	template := sandboxv1beta1.PersistentVolumeClaimTemplate{Spec: claimSpec}
	template.Name = substrateDurableWorkspaceVolume
	return template, nil
}

// runtimePoolDurableVolumeClaimTemplatesMatch verifies a claim carries exactly
// the frozen durable workspace PVC template and nothing else.
func runtimePoolDurableVolumeClaimTemplatesMatch(
	claim *sandboxextv1beta1.SandboxClaim,
	pool *corev1alpha1.RuntimePool,
) bool {
	if !sandboxRuntimePoolSuspendCapable(pool) {
		return len(claim.Spec.VolumeClaimTemplates) == 0
	}
	expected, err := runtimePoolDurableVolumeClaimTemplate(pool)
	if err != nil || len(claim.Spec.VolumeClaimTemplates) != 1 {
		return false
	}
	actual := claim.Spec.VolumeClaimTemplates[0]
	if actual.Name != expected.Name || len(actual.Spec.AccessModes) != len(expected.Spec.AccessModes) {
		return false
	}
	for i := range expected.Spec.AccessModes {
		if actual.Spec.AccessModes[i] != expected.Spec.AccessModes[i] {
			return false
		}
	}
	expectedClass, actualClass := "", ""
	if expected.Spec.StorageClassName != nil {
		expectedClass = *expected.Spec.StorageClassName
	}
	if actual.Spec.StorageClassName != nil {
		actualClass = *actual.Spec.StorageClassName
	}
	expectedCapacity := expected.Spec.Resources.Requests[corev1.ResourceStorage]
	actualCapacity := actual.Spec.Resources.Requests[corev1.ResourceStorage]
	return expectedClass == actualClass && expectedCapacity.Cmp(actualCapacity) == 0
}

// resolveSuspendableWorkspaceSandbox resolves the exact Sandbox adopted by the
// claim and proves single controller ownership before any suspension mutation.
func (r *RuntimePoolReconciler) resolveSuspendableWorkspaceSandbox(
	ctx context.Context,
	claim *sandboxextv1beta1.SandboxClaim,
) (*sandboxv1beta1.Sandbox, error) {
	name := strings.TrimSpace(claim.Status.SandboxStatus.Name)
	if name == "" {
		name = strings.TrimSpace(claim.Annotations[sandboxextv1beta1.AssignedSandboxNameAnnotation])
	}
	if name == "" {
		return nil, fmt.Errorf("provider SandboxClaim has not adopted a Sandbox")
	}
	sandbox := &sandboxv1beta1.Sandbox{}
	if err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: name}, sandbox); err != nil {
		return nil, fmt.Errorf("read adopted Sandbox: %w", err)
	}
	owner := metav1.GetControllerOf(sandbox)
	if owner == nil || owner.UID != claim.UID {
		return nil, fmt.Errorf("adopted Sandbox is not controller-owned by the exact SandboxClaim")
	}
	return sandbox, nil
}

// reconcileWorkspaceRuntimePoolSuspend drives a requested PVC-backed cold
// suspension for an agent-sandbox pool: the same authenticated drain barriers
// as scale-down, then operatingMode: Suspended on the exact adopted Sandbox.
// The provider terminates the Pod while the Sandbox and its durable PVC
// persist; process memory is never preserved anywhere.
//
//nolint:gocyclo // The suspension state machine keeps every barrier and fail-closed branch auditable together.
func (r *RuntimePoolReconciler) reconcileWorkspaceRuntimePoolSuspend(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	claim *sandboxextv1beta1.SandboxClaim,
	pods []corev1.Pod,
	readyPods []corev1.Pod,
	status corev1alpha1.RuntimePoolStatus,
) (ctrl.Result, error) {
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
	r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, "runtime pool is suspending its data-only workspace")

	if record := sandboxConsensualSuspendRecord(pool); record != nil {
		sandbox := &sandboxv1beta1.Sandbox{}
		err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: record.Name}, sandbox)
		if err != nil || sandbox.UID != record.UID {
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "the consensually suspended provider Sandbox is missing or replaced; the checkpoint cannot settle"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		suspended := apimeta.IsStatusConditionTrue(sandbox.Status.Conditions, string(sandboxv1beta1.SandboxConditionSuspended))
		if suspended && len(pods) == 0 {
			status.ActiveInstance = nil
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = "provider Sandbox is suspended with its durable workspace volume retained; cold resume restores the logical session with a fresh boot"
			r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionTrue, "WorkspaceSuspended", status.Message)
			return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
		}
		if sandbox.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
			// A restart raced the provider patch; repeat it idempotently.
			base := sandbox.DeepCopy()
			sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
			if err := r.Patch(ctx, sandbox, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsConflict(err) {
				return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
			}
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "waiting for the provider Sandbox Pod to terminate with the durable workspace volume retained"
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	if claim == nil || !claim.DeletionTimestamp.IsZero() || len(readyPods) == 0 || pool.Status.ActiveInstance == nil {
		// Suspension preserves an admitted, quiescent instance. Anything else
		// has nothing coherent to checkpoint; the plain scale-down machine
		// settles it fail-closed (the workspace adapter then reports the
		// failed suspension instead of a suspended workspace).
		return r.reconcileWorkspaceRuntimePoolScaleDown(ctx, pool, cfg, claim, pods, readyPods, status)
	}

	pod := &readyPods[0]
	deployedTemplate := runtimePoolPodTemplateSpec(pod)
	validationPool, validationConfig, err := runtimePoolPodTemplateValidationTarget(pool, deployedTemplate)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	deployedAuthSecret, err := r.runtimePoolPodTemplateAuthSecret(ctx, pool, cfg.namespace, deployedTemplate.Spec)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	probe, err := r.supervisorClientForPool(pool).Probe(ctx, runtimePoolInstanceEndpoint(pool, pod), string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]), deployedAuthSecret.Data[runtimePoolCapabilitySecretKey])
	if err != nil {
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.ActiveInstance = nil
		status.Message = sanitizeRuntimePoolMessage("authenticated pre-suspension probe failed: " + err.Error())
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	active, err := validateRuntimePoolProbe(validationPool, validationConfig, pod, probe, r.now())
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

	if !probe.Status.Drain.Requested {
		if err := r.supervisorClientForPool(pool).RequestDrain(
			ctx,
			runtimePoolInstanceEndpoint(pool, pod),
			string(deployedAuthSecret.Data[runtimePoolControllerTokenKey]),
			deployedAuthSecret.Data[runtimePoolCapabilitySecretKey],
			probe.Status,
			"runtime_pool_workspace_suspend",
		); err != nil {
			status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			status.Message = sanitizeRuntimePoolMessage("authenticated pre-suspension drain request failed: " + err.Error())
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
		// The persisted Quiescent barrier proves prompt and workspace-writer
		// settlement across a reconcile boundary before any provider mutation.
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleQuiescent
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionDraining
		status.Message = runtimePoolMessageDrainQuiescent
		return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
	}

	sandbox, err := r.resolveSuspendableWorkspaceSandbox(ctx, claim)
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	record, err := json.Marshal(sandboxSuspendRecord{Name: sandbox.Name, UID: sandbox.UID})
	if err != nil {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, fmt.Errorf("encode Sandbox suspension record: %w", err))
	}
	// Persist consent for the exact Sandbox before the provider mutation so a
	// restart resumes the same consensual suspension idempotently.
	if err := r.patchRuntimePoolAnnotation(ctx, pool, sandboxSuspendedAnnotation, string(record)); err != nil {
		return ctrl.Result{}, err
	}
	base := sandbox.DeepCopy()
	sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeSuspended
	if err := r.Patch(ctx, sandbox, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsConflict(err) {
		return r.finishRuntimePoolResourceFailure(ctx, pool, cfg, err)
	}
	status.ActiveInstance = nil
	status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopping
	status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	status.Message = "quiescent provider Sandbox is suspending with its durable workspace volume retained"
	return r.finishRuntimePoolStatus(ctx, pool, status, time.Second)
}

// resumeSuspendedWorkspaceSandbox drives the exact consensually suspended
// Sandbox back to Running under the refreshed template so the replacement Pod
// boots with rotated bootstrap material against the preserved durable volume.
// done=false means the caller must return the accompanying result.
func (r *RuntimePoolReconciler) resumeSuspendedWorkspaceSandbox(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	cfg runtimePoolConfig,
	claim *sandboxextv1beta1.SandboxClaim,
	desiredTemplate corev1.PodTemplateSpec,
	readyPods []corev1.Pod,
	status *corev1alpha1.RuntimePoolStatus,
	record *sandboxSuspendRecord,
) (bool, ctrl.Result, error) {
	logf.FromContext(ctx).Info("resuming the consensually suspended workspace Sandbox",
		"pool", pool.Name, "sandbox", record.Name)
	sandbox := &sandboxv1beta1.Sandbox{}
	err := r.sandboxReader().Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: record.Name}, sandbox)
	if apierrors.IsNotFound(err) || (err == nil && sandbox.UID != record.UID) {
		// The suspended Sandbox is gone or was replaced: the durable data is
		// unrecoverable. Fail closed by recycling the claim so a fresh
		// workspace materializes explicitly instead of adopting an impostor.
		if claim != nil {
			if deleteErr := r.deleteRuntimePoolSandboxClaim(ctx, claim); deleteErr != nil {
				return false, ctrl.Result{}, deleteErr
			}
		}
		if annotationErr := r.patchRuntimePoolAnnotation(ctx, pool, sandboxSuspendedAnnotation, ""); annotationErr != nil {
			return false, ctrl.Result{}, annotationErr
		}
		status.ActiveInstance = nil
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "the consensually suspended provider Sandbox is missing or replaced; recycling the workspace claim"
		r.setRuntimePoolCondition(pool, status, corev1alpha1.RuntimePoolConditionRolloutReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonRolloutFailed, status.Message)
		result, finishErr := r.finishRuntimePoolStatus(ctx, pool, *status, time.Second)
		return false, result, finishErr
	}
	if err != nil {
		return false, ctrl.Result{}, err
	}
	owner := metav1.GetControllerOf(sandbox)
	if claim == nil {
		return false, ctrl.Result{}, fmt.Errorf("consensually suspended Sandbox has no owning SandboxClaim to resume under")
	}
	if owner == nil || owner.UID != claim.UID {
		return false, ctrl.Result{}, fmt.Errorf("consensually suspended Sandbox is not controller-owned by the exact SandboxClaim (owner=%v claim=%s)", owner, claim.UID)
	}
	if sandbox.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
		// Refresh the Sandbox blueprint with the rotated bootstrap material
		// before the replacement Pod boots: the provider builds the Pod from
		// the Sandbox spec, not from the updated SandboxTemplate. The Sandbox
		// has no Pod while suspended, so nothing can observe the transition.
		base := sandbox.DeepCopy()
		sandbox.Spec.PodTemplate = sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels:      cloneStringMap(desiredTemplate.Labels),
				Annotations: cloneStringMap(desiredTemplate.Annotations),
			},
			Spec: *desiredTemplate.Spec.DeepCopy(),
		}
		sandbox.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
		if err := r.Patch(ctx, sandbox, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsConflict(err) {
			return false, ctrl.Result{}, err
		}
		r.applyProviderRuntimePoolColdStartStatus(pool, status, "resuming the suspended provider Sandbox with rotated bootstrap material")
		result, finishErr := r.finishRuntimePoolStatus(ctx, pool, *status, time.Second)
		return false, result, finishErr
	}
	if len(readyPods) == 0 {
		r.applyProviderRuntimePoolColdStartStatus(pool, status, "waiting for the resumed provider Sandbox Pod against the preserved durable workspace")
		result, finishErr := r.finishRuntimePoolStatus(ctx, pool, *status, time.Second)
		return false, result, finishErr
	}
	// The resumed Pod exists; the checkpoint record retires so a later
	// replacement can never adopt it, and the normal attestation, bootstrap
	// binding, and credential seeding path takes over.
	if err := r.patchRuntimePoolAnnotation(ctx, pool, sandboxSuspendedAnnotation, ""); err != nil {
		return false, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}
