package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// agentRuntimeSessionExposure records possible runtime admission, not prompt
// success. A controller restart may clear a Planned attempt's mutable binding;
// this immutable record still identifies every boot which could own its work.
type agentRuntimeSessionExposure struct {
	SchemaVersion  int             `json:"schemaVersion"`
	Namespace      string          `json:"namespace"`
	TaskUID        types.UID       `json:"taskUID"`
	Attempt        int32           `json:"attempt"`
	PromptID       string          `json:"promptID"`
	RequestDigest  string          `json:"requestDigest"`
	BindingDigest  string          `json:"bindingDigest"`
	SnapshotDigest string          `json:"snapshotDigest"`
	RuntimeUID     types.UID       `json:"runtimeUID"`
	Fence          harnessv2.Fence `json:"fence"`
	WitnessDigest  string          `json:"witnessDigest"`
}

func runtimeExposureIdentity(task *corev1alpha1.Task, taskUID types.UID) (store.ExternalEffectIdentity, error) {
	if task == nil || task.Status.Execution == nil || taskUID == "" {
		return store.ExternalEffectIdentity{}, errors.New("runtime exposure Task identity is incomplete")
	}
	e := task.Status.Execution
	operation, err := acpDomainDigest("agent-runtime-session-exposure", []string{
		string(taskUID), strconv.FormatInt(int64(e.Attempt), 10), e.PromptID, e.AgentRuntimeUID,
		e.RuntimeSessionSupervisorBootID, e.RuntimeSessionUID, strconv.FormatInt(e.RuntimeSessionGeneration, 10),
	})
	return store.ExternalEffectIdentity{Kind: agentRuntimeExposureKind, Namespace: task.Namespace, AggregateID: string(taskUID), OperationID: operation}, err
}

func (d *ACPDispatcher) recordKubernetesRuntimeExposure(ctx context.Context, task *corev1alpha1.Task, runtime *corev1alpha1.AgentRuntime, runtimeFence harnessv2.Fence, fence store.ControllerEpochFence) error {
	if runtime == nil || runtime.Spec.Deployment.KubernetesRecovery == nil {
		return nil
	}
	witness, err := loadAgentRuntimeBootWitness(ctx, d.Store, runtime.Namespace, runtime.UID, runtimeFence.SupervisorBootID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(witness.Spec, runtime.Spec) || witness.RuntimeGeneration != runtime.Generation ||
		harnessv2.CompareFence(witness.Fence, runtimeFence, false) != harnessv2.FenceMatch {
		return errors.New("runtime exposure does not match enrolled boot authority")
	}
	if runtimeFence.ControllerEpoch != uint64(fence.Epoch) {
		return errors.New("runtime exposure epoch is not current")
	}
	if err := d.revalidateKubernetesRuntimeAdmission(ctx, runtime); err != nil {
		return err
	}
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	if binding == nil || binding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint || binding.RuntimeRef == nil ||
		binding.Task.UID != task.UID || binding.RuntimeRef.UID != runtime.UID || binding.RuntimeRef.Generation != runtime.Generation {
		return errors.New("runtime exposure lacks the immutable Task execution binding")
	}
	// Persist the complete process binding before creating the runtime Session.
	// In particular, task-scoped executions previously stamped the boot only
	// after create returned, leaving a crash window without durable identity.
	if err := agentRuntimeRecoveryGuard(ctx, d.Store, fence, func(writeCtx context.Context) error {
		latest := &corev1alpha1.Task{}
		if err := d.APIReader.Get(writeCtx, client.ObjectKeyFromObject(task), latest); err != nil {
			return err
		}
		if latest.UID != task.UID || latest.Status.Execution == nil || latest.Status.AgentExecutionBinding == nil ||
			latest.Status.AgentExecutionBinding.BindingDigest != binding.BindingDigest || latest.Status.Execution.Attempt != task.Status.Execution.Attempt ||
			latest.Status.Execution.PromptID != task.Status.Execution.PromptID || latest.Status.Execution.RequestDigest != task.Status.Execution.RequestDigest {
			return errors.New("bound Task identity changed before runtime exposure")
		}
		base := latest.DeepCopy()
		execution := latest.Status.Execution
		execution.RuntimeInstanceID = string(runtimeFence.RuntimeInstanceID)
		execution.RuntimeSessionSupervisorBootID = string(runtimeFence.SupervisorBootID)
		execution.RuntimeSessionUID = string(runtimeFence.RuntimeSessionUID)
		execution.RuntimeSessionGeneration = int64(runtimeFence.RuntimeSessionGeneration)
		execution.RuntimeSessionProfileDigest = string(runtimeFence.RuntimeProfileDigest)
		if err := d.Client.Status().Patch(writeCtx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		task.Status = latest.Status
		return nil
	}); err != nil {
		return err
	}
	witnessDigest, err := runtimeWitnessDigest(witness)
	if err != nil {
		return err
	}
	exposure := agentRuntimeSessionExposure{
		SchemaVersion: 1, Namespace: task.Namespace, TaskUID: task.UID, Attempt: task.Status.Execution.Attempt,
		PromptID: task.Status.Execution.PromptID, RequestDigest: task.Status.Execution.RequestDigest,
		BindingDigest: binding.BindingDigest, SnapshotDigest: binding.Snapshot.Digest, RuntimeUID: runtime.UID,
		Fence: runtimeFence, WitnessDigest: witnessDigest,
	}
	identity, err := runtimeExposureIdentity(task, task.UID)
	if err != nil {
		return err
	}
	digest, err := acpDomainDigest("runtime-exposure", exposure)
	if err != nil {
		return err
	}
	return persistAgentRuntimeRecoveryEffect(ctx, d.Store, fence, identity, digest, exposure)
}

func (d *ACPDispatcher) revalidateKubernetesRuntimeAdmission(ctx context.Context, runtime *corev1alpha1.AgentRuntime) error {
	if runtime == nil || runtime.Spec.Deployment.KubernetesRecovery == nil {
		return nil
	}
	if runtime.Status.ObservedCapabilities == nil {
		return errors.New("runtime admission has no conformed Kubernetes boot")
	}
	witness, err := loadAgentRuntimeBootWitness(ctx, d.Store, runtime.Namespace, runtime.UID, harnessv2.SupervisorBootID(runtime.Status.ObservedCapabilities.SupervisorBootID))
	if err != nil {
		return err
	}
	r := &AgentRuntimeReconciler{Client: d.Client, APIReader: d.APIReader, ControlStore: d.Store, ControllerEpochManager: d.Epochs}
	return r.validateRecoveryAdmissionWitness(ctx, runtime, witness)
}

func (r *AgentRuntimeReconciler) validateRecoveryAdmissionWitness(ctx context.Context, runtime *corev1alpha1.AgentRuntime, witness agentRuntimeBootWitness) error {
	backend, err := r.recoveryBackend(ctx, runtime)
	if err != nil {
		return err
	}
	observed := backend.witness
	observed.Fence = witness.Fence
	observed.ControllerAuthUID, observed.ControllerAuthVersion = witness.ControllerAuthUID, witness.ControllerAuthVersion
	observed.CapabilityAuthUID, observed.CapabilityAuthVersion = witness.CapabilityAuthUID, witness.CapabilityAuthVersion
	if !reflect.DeepEqual(observed, witness) || !controllerutil.ContainsFinalizer(backend.pod, agentRuntimeRecoveryPodFinalizer) {
		return errors.New("runtime admission Kubernetes container witness changed")
	}
	retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, witness)
	if err != nil {
		return err
	}
	if retired {
		return errors.New("retired supervisor boot cannot admit work")
	}
	return nil
}

func (r *AgentRuntimeReconciler) recoveryConformanceGuard(ctx context.Context, runtime *corev1alpha1.AgentRuntime, fence store.ControllerEpochFence) (*harnessv2.Fence, func(context.Context) error, error) {
	if runtime.Spec.Deployment.KubernetesRecovery == nil {
		return nil, nil, nil
	}
	witnesses, err := r.recoveryWitnesses(ctx, runtime)
	if err != nil {
		return nil, nil, err
	}
	var enrolled *agentRuntimeBootWitness
	for i := range witnesses {
		retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, witnesses[i])
		if err != nil {
			return nil, nil, err
		}
		if !retired {
			if enrolled != nil {
				return nil, nil, errors.New("multiple unretired supervisor boots prevent conformance")
			}
			enrolled = &witnesses[i]
		}
	}
	if enrolled == nil || enrolled.Fence.ControllerEpoch != uint64(fence.Epoch) {
		return nil, nil, errors.New("current supervisor boot is not enrolled")
	}
	guard := func(checkCtx context.Context) error {
		if err := requireAgentRuntimeRecoveryFence(checkCtx, r.ControlStore, fence); err != nil {
			return err
		}
		current := &corev1alpha1.AgentRuntime{}
		if err := r.endpointReader().Get(checkCtx, client.ObjectKeyFromObject(runtime), current); err != nil {
			return err
		}
		if current.UID != runtime.UID || current.DeletionTimestamp != nil || current.Generation != runtime.Generation || !reflect.DeepEqual(current.Spec, runtime.Spec) ||
			!controllerutil.ContainsFinalizer(current, agentRuntimeFinalizer) || !controllerutil.ContainsFinalizer(current, agentRuntimeSecretGCFinalizer) {
			return errors.New("conformance registration ownership changed")
		}
		auth, err := r.agentRuntimeAuthMaterial(checkCtx, current)
		if err != nil {
			return err
		}
		if auth.controllerSecretUID != enrolled.ControllerAuthUID || auth.capabilitySecretUID != enrolled.CapabilityAuthUID ||
			auth.controllerResourceVersion != enrolled.ControllerAuthVersion || auth.capabilityResourceVersion != enrolled.CapabilityAuthVersion {
			return errors.New("conformance authentication no longer matches enrolled boot")
		}
		return r.validateRecoveryAdmissionWitness(checkCtx, current, *enrolled)
	}
	if err := guard(ctx); err != nil {
		return nil, nil, err
	}
	return &enrolled.Fence, guard, nil
}

//nolint:gocyclo // Verify every immutable Task, attempt, Session and boot field together before accepting retirement evidence.
func verifiedKubernetesRuntimeRetirement(ctx context.Context, effects store.ExternalEffectStore, task *corev1alpha1.Task, taskUID types.UID) (bool, error) {
	if task == nil || task.Status.Execution == nil || task.Status.Execution.AgentRuntimeUID == "" {
		return false, nil
	}
	identity, err := runtimeExposureIdentity(task, taskUID)
	if err != nil {
		return false, err
	}
	var exposure agentRuntimeSessionExposure
	effect, err := readAgentRuntimeRecoveryEffect(ctx, effects, identity, &exposure)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	e := task.Status.Execution
	if binding == nil || binding.RuntimeRef == nil || binding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint ||
		binding.Task.UID != taskUID || binding.RuntimeRef.UID != types.UID(e.AgentRuntimeUID) || binding.RuntimeRef.Name != e.AgentRuntimeName ||
		exposure.SchemaVersion != 1 || exposure.Namespace != task.Namespace || exposure.TaskUID != taskUID || exposure.Attempt != e.Attempt ||
		exposure.PromptID != e.PromptID || exposure.RequestDigest != e.RequestDigest || exposure.BindingDigest != binding.BindingDigest || exposure.SnapshotDigest != binding.Snapshot.Digest ||
		exposure.RuntimeUID != types.UID(e.AgentRuntimeUID) || exposure.Fence.Validate(true) != nil ||
		string(exposure.Fence.RuntimeInstanceID) != e.RuntimeInstanceID || string(exposure.Fence.SupervisorBootID) != e.RuntimeSessionSupervisorBootID ||
		string(exposure.Fence.RuntimeSessionUID) != e.RuntimeSessionUID || int64(exposure.Fence.RuntimeSessionGeneration) != e.RuntimeSessionGeneration ||
		string(exposure.Fence.RuntimeProfileDigest) != binding.RuntimeProfileDigest {
		return false, fmt.Errorf("%w: runtime exposure does not match the exact Task attempt", store.ErrConflict)
	}
	digest, err := canonicalAgentExecutionBindingDigest(*binding)
	if err != nil || digest != binding.BindingDigest {
		return false, fmt.Errorf("%w: runtime exposure Task binding integrity failed", store.ErrConflict)
	}
	digest, err = acpDomainDigest("runtime-exposure", exposure)
	if err != nil || digest != effect.RequestDigest {
		return false, fmt.Errorf("%w: runtime exposure digest failed", store.ErrConflict)
	}
	witness, err := loadAgentRuntimeBootWitness(ctx, effects, task.Namespace, exposure.RuntimeUID, exposure.Fence.SupervisorBootID)
	if err != nil {
		return false, err
	}
	digest, err = runtimeWitnessDigest(witness)
	if err != nil || digest != exposure.WitnessDigest || witness.RuntimeGeneration != binding.RuntimeRef.Generation ||
		harnessv2.CompareFence(witness.Fence, exposure.Fence, false) != harnessv2.FenceMatch {
		return false, fmt.Errorf("%w: runtime exposure boot witness integrity failed", store.ErrConflict)
	}
	return loadAgentRuntimeBootRetirement(ctx, effects, witness)
}

func (r *AgentRuntimeReconciler) finalizeKubernetesAgentRuntime(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (ctrl.Result, error) {
	if r.ControlStore == nil || r.ControllerEpochManager == nil {
		return ctrl.Result{}, errors.New("runtime cleanup requires the durable control store for Kubernetes recovery")
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if controllerutil.ContainsFinalizer(runtime, agentRuntimeFinalizer) {
		if err := r.resumePreparedRecoveryPods(ctx, runtime, fence); err != nil {
			return ctrl.Result{}, err
		}
	}
	witnesses, err := r.recoveryWitnesses(ctx, runtime)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, witness := range witnesses {
		retired, err := r.drainRecoveryBoot(ctx, witness, fence)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !retired {
			return ctrl.Result{RequeueAfter: agentRuntimeDeleteRequeue}, nil
		}
	}
	ready, err := r.recordRetiredKubernetesRuntimeTasks(ctx, runtime, fence)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: agentRuntimeDeleteRequeue}, nil
	}
	for _, witness := range witnesses {
		if err := r.releaseRetiredRecoveryPod(ctx, witness, fence, true); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.releaseRecoveryBootSecret(ctx, runtime, witness, fence); err != nil {
			return ctrl.Result{}, err
		}
	}
	err = agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		current := &corev1alpha1.AgentRuntime{}
		if err := r.endpointReader().Get(writeCtx, client.ObjectKeyFromObject(runtime), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != runtime.UID || current.DeletionTimestamp.IsZero() {
			return errors.New("AgentRuntime deletion authority changed")
		}
		if err := r.requireSettledRecoveryPreparations(writeCtx, current); err != nil {
			return err
		}
		base := current.DeepCopy()
		controllerutil.RemoveFinalizer(current, agentRuntimeFinalizer)
		if err := r.Patch(writeCtx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		runtime = current
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if controllerutil.ContainsFinalizer(runtime, agentRuntimeSecretGCFinalizer) {
		var result ctrl.Result
		err = agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
			var cleanupErr error
			result, cleanupErr = r.finalizeAgentRuntimeCleanupSecret(writeCtx, runtime)
			return cleanupErr
		})
		return result, err
	}
	return ctrl.Result{}, nil
}

func (r *AgentRuntimeReconciler) recordRetiredKubernetesRuntimeTasks(ctx context.Context, runtime *corev1alpha1.AgentRuntime, fence store.ControllerEpochFence) (bool, error) {
	var tasks corev1alpha1.TaskList
	if err := r.endpointReader().List(ctx, &tasks, client.InNamespace(runtime.Namespace)); err != nil {
		return false, err
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
		if binding == nil || binding.RuntimeRef == nil || binding.RuntimeRef.UID != runtime.UID {
			continue
		}
		if runtimeSessionCleanupCompleteForUID(task, acpTaskControlUID(task)) {
			continue
		}
		retired, err := verifiedKubernetesRuntimeRetirement(ctx, r.ControlStore, task, acpTaskControlUID(task))
		if err != nil || !retired {
			return false, err
		}
		attemptID, err := promptAttemptIDFromTaskUID(task, acpTaskControlUID(task))
		if err != nil {
			return false, err
		}
		attempt, err := r.ControlStore.GetPromptAttempt(ctx, attemptID)
		if err != nil {
			return false, err
		}
		if !store.IsTerminalPromptExecutionState(attempt.ExecutionState) || !store.IsTerminalPromptDeliveryState(attempt.DeliveryState) {
			return false, nil
		}
		if err := agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
			e := task.Status.Execution
			return persistTaskScopedRuntimeSessionCleanupReceipt(writeCtx, r.Client, task, acpTaskControlUID(task), e.RuntimeInstanceID, e.RuntimeSessionUID, e.RuntimeSessionGeneration)
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *AgentRuntimeReconciler) releaseRecoveryBootSecret(ctx context.Context, runtime *corev1alpha1.AgentRuntime, witness agentRuntimeBootWitness, fence store.ControllerEpochFence) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		secret := &corev1.Secret{}
		err := r.endpointReader().Get(writeCtx, client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}, secret)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !metav1.IsControlledBy(secret, runtime) || secret.Immutable == nil || !*secret.Immutable || secret.Type != agentRuntimeCleanupSecretType {
			return errors.New("boot cleanup Secret ownership changed before release")
		}
		base := secret.DeepCopy()
		controllerutil.RemoveFinalizer(secret, agentRuntimeSecretFinalizer)
		if err := r.Patch(writeCtx, secret, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		return r.Delete(writeCtx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion}})
	})
}
