package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskterminal"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// standaloneRuntimeCleanupFence grants exact session deletion, preceded when
// necessary by evidence-bound publication finalization, under an older runtime
// epoch. Mutable Task status never supplies that authority: the
// terminal attempt, immutable projection and frozen execution binding must
// agree, and every mutation revalidates those records under the current owner.
func (d *ACPDispatcher) standaloneRuntimeCleanupFence(
	ctx context.Context,
	task *corev1alpha1.Task,
	taskUID types.UID,
	owner store.ControllerEpochFence,
) (*sessionRuntimeCleanupFence, error) {
	if task.Spec.SessionRef != nil {
		return nil, nil
	}
	projectionID := standaloneTaskTerminalProjectionIDForUID(task.Namespace, taskUID, task.Status.Execution.Attempt)
	projection, err := d.Store.GetOutboxProjection(ctx, projectionID)
	if errors.Is(err, store.ErrNotFound) {
		// Ordinary same-epoch cleanup does not need a terminal projection.
		// Without one it still cannot address a previous runtime epoch.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	validated, err := d.validateStandaloneRuntimeCleanupProjection(ctx, task, taskUID, projection, owner)
	if err != nil {
		return nil, err
	}
	if validated.Execution.ControllerEpoch == owner.Epoch {
		return nil, nil
	}
	key, liveUID := client.ObjectKeyFromObject(task), task.UID
	bindingDigest, projectionDigest := task.Status.AgentExecutionBinding.BindingDigest, projection.PayloadDigest
	scope := &sessionRuntimeCleanupFence{
		controller: owner, runtimeEpoch: uint64(validated.Execution.ControllerEpoch), allowPublicationFinalization: true,
	}
	scope.validateTask = func(checkCtx context.Context) error {
		current := &corev1alpha1.Task{}
		if err := d.APIReader.Get(checkCtx, key, current); err != nil {
			return err
		}
		if current.UID != liveUID || current.Status.AgentExecutionBinding == nil ||
			current.Status.AgentExecutionBinding.BindingDigest != bindingDigest {
			return fmt.Errorf("%w: standalone runtime cleanup Task authority changed", store.ErrConflict)
		}
		currentProjection, err := d.Store.GetOutboxProjection(checkCtx, projectionID)
		if err != nil {
			return err
		}
		if currentProjection.PayloadDigest != projectionDigest {
			return fmt.Errorf("%w: standalone runtime cleanup projection changed", store.ErrConflict)
		}
		currentProof, err := d.validateStandaloneRuntimeCleanupProjection(checkCtx, current, taskUID, currentProjection, owner)
		if err != nil {
			return err
		}
		if uint64(currentProof.Execution.ControllerEpoch) != scope.runtimeEpoch {
			return fmt.Errorf("%w: standalone runtime cleanup epoch changed", store.ErrConflict)
		}
		return nil
	}
	return scope, nil
}

func (d *ACPDispatcher) validateStandaloneRuntimeCleanupProjection(
	ctx context.Context,
	task *corev1alpha1.Task,
	taskUID types.UID,
	projection *store.OutboxProjection,
	owner store.ControllerEpochFence,
) (*taskterminal.Projection, error) {
	binding, err := standaloneRuntimeCleanupBinding(task, taskUID)
	if err != nil {
		return nil, err
	}
	// Reuse snapshot integrity validation without trusting current endpoint or
	// admission readiness. The cleanup client authenticates the frozen boot.
	runtime := &corev1alpha1.AgentRuntime{}
	runtime.Namespace, runtime.Name, runtime.UID = task.Namespace, binding.RuntimeRef.Name, binding.RuntimeRef.UID
	frozen, _, _, err := d.verifiedExternalRuntimeRecoveryTarget(ctx, task, taskUID, runtime)
	if err != nil {
		return nil, err
	}
	attemptID, err := promptAttemptIDFromTaskUID(task, taskUID)
	if err != nil {
		return nil, err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	if err := validateRestoredTaskSourceAttempt(task, attempt, attemptID); err != nil {
		return nil, err
	}
	if projection == nil || projection.ID != standaloneTaskTerminalProjectionIDForUID(task.Namespace, taskUID, task.Status.Execution.Attempt) ||
		projection.AggregateKind != taskResourceKind || projection.AggregateID != string(taskUID) ||
		projection.ProjectionKind != taskTerminalProjectionKind || projection.PayloadDigest != store.CanonicalBytesDigest(projection.Payload) {
		return nil, fmt.Errorf("%w: standalone runtime cleanup projection integrity failed", store.ErrConflict)
	}
	validated, err := taskterminal.ValidateRestoredProjection(projection.Payload, task, string(taskUID), attempt)
	if err != nil {
		return nil, err
	}
	e := validated.Execution
	if e.ControllerEpoch < 1 || e.ControllerEpoch > owner.Epoch ||
		strings.TrimSpace(e.RuntimeSessionUID) == "" || e.RuntimeSessionGeneration < 1 ||
		strings.TrimSpace(e.RuntimeSessionSupervisorBootID) == "" || e.RuntimeInstanceID != frozen.RuntimeInstanceID ||
		(e.RuntimeSessionProfileDigest != "" && e.RuntimeSessionProfileDigest != binding.RuntimeProfileDigest) ||
		validated.Phase != task.Status.Phase || e.State != task.Status.Execution.State || e.Outcome != task.Status.Execution.Outcome {
		return nil, fmt.Errorf("%w: standalone runtime cleanup lacks an exact terminal runtime fence", store.ErrConflict)
	}
	return validated, nil
}

func standaloneRuntimeCleanupBinding(task *corev1alpha1.Task, taskUID types.UID) (*corev1alpha1.AgentExecutionBinding, error) {
	// taskUID is the frozen source identity; a restored incarnation's live UID
	// may differ. The live object is bound separately by validateTask.
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent ||
		task.Spec.SessionRef != nil || task.Status.Execution == nil ||
		!store.IsTerminalPromptExecutionState(store.PromptExecutionState(task.Status.Execution.State)) ||
		task.Status.Delivery == nil || !store.IsTerminalPromptDeliveryState(store.PromptDeliveryState(task.Status.Delivery.State)) {
		return nil, fmt.Errorf("%w: standalone runtime cleanup requires a terminal Task", store.ErrConflict)
	}
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	if binding == nil || binding.RuntimeRef == nil || binding.Task.UID != taskUID ||
		binding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint {
		return nil, fmt.Errorf("%w: standalone runtime cleanup frozen binding is incomplete", store.ErrConflict)
	}
	return binding, nil
}

func (d *ACPDispatcher) markRecoveredRuntimeCleanupComplete(
	ctx context.Context,
	task *corev1alpha1.Task,
	taskUID types.UID,
	cleanup *sessionRuntimeCleanupFence,
) error {
	mark := func(writeCtx context.Context) error {
		if cleanup != nil {
			if _, err := d.externalRuntimeCleanupEpoch(writeCtx, cleanup); err != nil {
				return err
			}
		}
		e := task.Status.Execution
		return d.markTaskScopedRuntimeSessionCleanupComplete(writeCtx, task, taskUID, e.RuntimeInstanceID, e.RuntimeSessionUID, e.RuntimeSessionGeneration)
	}
	if cleanup != nil {
		return agentRuntimeRecoveryGuard(ctx, d.Store, cleanup.controller, mark)
	}
	return mark(ctx)
}
