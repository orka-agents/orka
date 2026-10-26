package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/enforcement"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var errEnforcedACPTaskAuthority = errors.New("enforced ACP consumer lifetime ended")

func (d *ACPDispatcher) admitEnforcedRuntimeSession(
	ctx context.Context,
	runtimeClient *harnessv2.Client,
	task *corev1alpha1.Task,
	target acpDispatchTarget,
	create harnessv2.CreateRuntimeSessionRequest,
	launch bool,
	controllerFence store.ControllerEpochFence,
) (*corev1alpha1.ContainerEnforcementBinding, error) {
	binding, err := d.ensureRuntimePoolEnforcement(ctx, task, target, create.Metadata.Fence)
	if err != nil || binding == nil {
		return nil, err
	}
	if err := d.verifyEnforcedACPTaskAuthority(ctx, task, create.Metadata.Fence, controllerFence); err != nil {
		return nil, err
	}
	if launch {
		if !create.PrepareOnly {
			return nil, errors.New("enforced ACP launch requires a prepared session")
		}
		request := harnessv2.LaunchRuntimeSessionRequest{
			Protocol: harnessv2.ProtocolVersion, Metadata: create.Metadata,
			PreparationOperationID: create.Metadata.OperationID,
		}
		request.Metadata.OperationID = "launch-" + create.Metadata.OperationID
		if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
			return nil, err
		}
		if _, err := runtimeClient.LaunchRuntimeSession(ctx, create.RuntimeSessionID, request); err != nil {
			return nil, err
		}
	}
	if err := d.verifyEnforcedACPTaskAuthority(ctx, task, create.Metadata.Fence, controllerFence); err != nil {
		return nil, err
	}
	return binding, nil
}

func (d *ACPDispatcher) verifyEnforcedACPTaskAuthority(
	ctx context.Context,
	expected *corev1alpha1.Task,
	runtimeFence harnessv2.Fence,
	fence store.ControllerEpochFence,
) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(errEnforcedACPTaskAuthority, err)
	}
	if d.APIReader == nil || d.Store == nil || expected.Status.Execution == nil {
		return errors.New("enforced ACP admission requires fresh Task and durable attempt authority")
	}
	current := &corev1alpha1.Task{}
	if err := d.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
		return err
	}
	if current.UID != expected.UID || !current.DeletionTimestamp.IsZero() ||
		current.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return errors.Join(errEnforcedACPTaskAuthority, context.Canceled)
	}
	if current.Status.Execution == nil || current.Status.Execution.State != corev1alpha1.TaskExecutionStatePlanned ||
		current.Status.Execution.ControllerEpoch != fence.Epoch ||
		current.Status.Execution.RuntimeInstanceID != string(runtimeFence.RuntimeInstanceID) ||
		current.Status.Execution.RuntimeSessionUID != string(runtimeFence.RuntimeSessionUID) ||
		current.Status.Execution.RuntimeSessionGeneration != int64(runtimeFence.RuntimeSessionGeneration) ||
		!reflect.DeepEqual(current.Status.AgentExecutionBinding, expected.Status.AgentExecutionBinding) ||
		!reflect.DeepEqual(current.Spec.Enforcement, expected.Spec.Enforcement) {
		return fmt.Errorf("%w: enforced ACP Task authority changed before admission", store.ErrConflict)
	}
	if deadline, ok := acpTaskDeadline(current, time.Now().UTC()); ok && !time.Now().Before(deadline) {
		return errors.Join(errEnforcedACPTaskAuthority, context.DeadlineExceeded)
	}
	attemptID, err := promptAttemptIDFromTask(expected)
	if err != nil {
		return err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	if !queuedPromptAttemptMatchesTask(attempt, current) || attempt.ExecutionState != store.PromptExecutionPlanned ||
		attempt.ControllerEpochName != fence.Name || attempt.ControllerEpoch != fence.Epoch ||
		attempt.RuntimeInstanceID != string(runtimeFence.RuntimeInstanceID) ||
		attempt.SessionUID != string(runtimeFence.RuntimeSessionUID) {
		return fmt.Errorf("%w: enforced ACP attempt no longer owns the selected session", store.ErrConflict)
	}
	epoch, err := d.Store.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		return err
	}
	if epoch == nil || epoch.Epoch != fence.Epoch || epoch.HolderID != fence.HolderID {
		return fmt.Errorf("%w: enforced ACP controller ownership changed", store.ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(errEnforcedACPTaskAuthority, err)
	}
	return nil
}

func (d *ACPDispatcher) ensureRuntimePoolEnforcement(
	ctx context.Context,
	task *corev1alpha1.Task,
	target acpDispatchTarget,
	fence harnessv2.Fence,
) (*corev1alpha1.ContainerEnforcementBinding, error) {
	requirement, err := d.runtimePoolEnforcementRequirement(task, target)
	if err != nil {
		return nil, err
	}
	if requirement == nil {
		return nil, nil
	}
	digest, err := enforcement.RequirementDigest(requirement)
	if err != nil {
		return nil, err
	}
	binding, err := d.runtimePoolEnforcementTarget(ctx, target.pool, fence, digest)
	if err != nil {
		return nil, err
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := d.selectedEnforcementPool(ctx, target.pool, fence, digest)
		if err != nil {
			return err
		}
		if current.Status.EnforcementBinding != nil {
			if !reflect.DeepEqual(current.Status.EnforcementBinding, binding) {
				return fmt.Errorf("%w: previous physical enforcement binding must retire before replacement", store.ErrNotReady)
			}
			return nil
		}
		base := current.DeepCopy()
		current.Status.EnforcementBinding = binding.DeepCopy()
		return d.Client.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The deadline bounds only this RPC, not the policy or container lifetime.
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	proofs, outcome, err := d.Enforcement.Ensure(callCtx, binding, requirement)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("required ACP enforcement verification uncertain: %w", err)
	}
	if outcome != nil {
		if refused := outcome.GetRefused(); refused != nil {
			return nil, fmt.Errorf("%w: %w", store.ErrValidation, &enforcement.RefusalError{Needs: refused.Needs})
		}
		if outcome.GetPending() != nil {
			return nil, fmt.Errorf("%w: %s", store.ErrNotReady,
				enforcement.OutcomeMessage(enforcement.Outcome("EnforcementPending", "provider-recovery")))
		}
	}
	if outcome != nil || len(proofs) == 0 {
		return nil, errors.New("mandatory providers returned no complete ACP enforcement authority")
	}
	observed, err := d.runtimePoolEnforcementTarget(ctx, target.pool, fence, digest)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(observed, binding) {
		return nil, fmt.Errorf("%w: physical enforcement target changed during verification", store.ErrConflict)
	}
	current, err := d.selectedEnforcementPool(ctx, target.pool, fence, digest)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(current.Status.EnforcementBinding, binding) {
		return nil, fmt.Errorf("%w: physical enforcement intent changed during verification", store.ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return binding, nil
}

func (d *ACPDispatcher) runtimePoolEnforcementRequirement(task *corev1alpha1.Task, target acpDispatchTarget) (*enforcementv1.Requirement, error) {
	requirement, err := enforcement.TaskRequirement(task)
	if err != nil {
		return nil, err
	}
	if requirement == nil {
		if target.pool != nil && (target.pool.Spec.Runtime.Profile.EnforcementRequirementDigest != "" || target.pool.Status.EnforcementBinding != nil) {
			return nil, store.ValidationErrorf("ordinary Task cannot reuse an enforcement-bound RuntimePool")
		}
		return nil, nil
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent || target.external != nil || target.pool == nil ||
		target.pool.Spec.ExecutionWorkspace != nil || target.pool.Namespace != task.Namespace {
		return nil, store.ValidationErrorf("required ACP enforcement needs the Task's managed Kubernetes RuntimePool")
	}
	if d.Client == nil || d.APIReader == nil || d.Enforcement == nil {
		return nil, store.ValidationErrorf("required ACP enforcement needs fresh Kubernetes reads and operator-trusted providers")
	}
	digest, err := enforcement.RequirementDigest(requirement)
	if err != nil {
		return nil, err
	}
	if target.pool.Spec.Runtime.Profile.EnforcementRequirementDigest != digest {
		return nil, store.ValidationErrorf("required ACP enforcement does not match the selected pool profile")
	}
	return requirement, nil
}

func (d *ACPDispatcher) selectedEnforcementPool(
	ctx context.Context,
	expected *corev1alpha1.RuntimePool,
	fence harnessv2.Fence,
	requirementDigest string,
) (*corev1alpha1.RuntimePool, error) {
	current := &corev1alpha1.RuntimePool{}
	if err := d.APIReader.Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
		return nil, err
	}
	if current.UID != expected.UID || current.Generation <= 0 || !current.DeletionTimestamp.IsZero() ||
		current.Spec.ExecutionWorkspace != nil || !controllerutil.ContainsFinalizer(current, runtimePoolFinalizer) ||
		current.Spec.Runtime.Profile.EnforcementRequirementDigest != requirementDigest || current.Status.ActiveInstance == nil ||
		expected.Status.ActiveInstance == nil || !runtimePoolRolloutActiveInstanceMatches(expected.Status.ActiveInstance, current.Status.ActiveInstance) {
		return nil, fmt.Errorf("%w: required enforcement lost its selected RuntimePool", store.ErrConflict)
	}
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing &&
		current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDraining {
		return nil, fmt.Errorf("%w: required enforcement RuntimePool is not serving its reserved session", store.ErrNotReady)
	}
	active := current.Status.ActiveInstance
	observed := harnessv2.Fence{
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(active.RuntimeInstanceID), SupervisorBootID: harnessv2.SupervisorBootID(active.BootID),
		ControllerEpoch: uint64(active.ControllerEpoch), RuntimePoolUID: harnessv2.RuntimePoolUID(current.UID),
		RuntimePoolGeneration: uint64(current.Generation), RuntimeProfileDigest: harnessv2.ProfileDigest(current.Spec.Runtime.Profile.Digest),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	if mismatch := harnessv2.CompareFence(fence, observed, false); mismatch != harnessv2.FenceMatch ||
		active.ProfileDigest != current.Spec.Runtime.Profile.Digest || current.Status.ControllerEpoch != active.ControllerEpoch {
		return nil, fmt.Errorf("%w: required enforcement RuntimePool fence changed", store.ErrConflict)
	}
	profile, err := runtimePoolHarnessProfile(current.Spec.Runtime.Profile)
	if err != nil {
		return nil, err
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		return nil, err
	}
	if digest != fence.RuntimeProfileDigest {
		return nil, fmt.Errorf("%w: required enforcement profile does not match its immutable digest", store.ErrConflict)
	}
	// This is an already-reserved session. Capacity-only admission closure
	// must not veto its launch; normal drain and mutation fences still apply.
	return current, nil
}

func (d *ACPDispatcher) runtimePoolEnforcementTarget(
	ctx context.Context,
	expected *corev1alpha1.RuntimePool,
	fence harnessv2.Fence,
	requirementDigest string,
) (*corev1alpha1.ContainerEnforcementBinding, error) {
	current, err := d.selectedEnforcementPool(ctx, expected, fence, requirementDigest)
	if err != nil {
		return nil, err
	}
	active := current.Status.ActiveInstance
	pod := &corev1.Pod{}
	if err := d.APIReader.Get(ctx, client.ObjectKey{Namespace: active.PodNamespace, Name: active.PodName}, pod); err != nil {
		return nil, err
	}
	if !runtimePoolActiveInstanceMatchesPod(active, pod) || pod.Labels[runtimePoolUIDLabel] != string(current.UID) ||
		len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != runtimeField ||
		pod.Spec.Containers[0].Image != current.Spec.Runtime.Image {
		return nil, fmt.Errorf("%w: required enforcement lost its exact managed runtime Pod", store.ErrConflict)
	}
	digestInjected := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "ORKA_ACP_ENFORCEMENT_REQUIREMENT_DIGEST" {
			digestInjected = env.ValueFrom == nil && env.Value == requirementDigest
		}
	}
	if !digestInjected {
		return nil, errors.New("runtime Pod does not provide the frozen enforcement preparation profile")
	}
	node := &corev1.Node{}
	if err := d.APIReader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return nil, err
	}
	providerDigest, err := d.Enforcement.ProviderSetDigest(node.Name, string(node.UID))
	if err != nil {
		return nil, err
	}
	return enforcement.BindContainer(pod, node, runtimeField, requirementDigest, providerDigest)
}
