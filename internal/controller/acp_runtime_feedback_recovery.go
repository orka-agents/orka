package controller

import (
	"context"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const runtimeFeedbackPreSubmissionCleanup = "runtime-feedback-pre-submission-cleanup"

func runtimeFeedbackPreSubmissionCleanupCandidate(task *corev1alpha1.Task) bool {
	return task != nil && task.Status.Execution != nil && task.Spec.SessionRef == nil &&
		task.Status.Execution.State == corev1alpha1.TaskExecutionStatePlanned && task.Status.Execution.RuntimePoolName != ""
}

// Planned is the durable cleanup intent: no prompt crossed the submission
// boundary, and its exact session binding remains until retirement is proven.
// The scheduler holds the active-Task exclusion used by normal dispatch.
func (d *ACPDispatcher) recoverRuntimeFeedbackPreSubmission(ctx context.Context, candidate *corev1alpha1.Task, fence store.ControllerEpochFence) error {
	ctx, cancel := context.WithTimeout(ctx, acpPreSubmissionCleanupTimeout)
	defer cancel()
	task, recoverable, err := d.readRecoverableTask(ctx, candidate)
	if err != nil || !recoverable {
		return err
	}
	if !runtimeFeedbackPreSubmissionCleanupCandidate(task) || task.Status.Execution.ControllerEpoch != fence.Epoch {
		return nil
	}
	verifier := TaskReconciler{Client: d.Client, APIReader: d.APIReader, ControllerEpochManager: d.Epochs, AgentExecutionSnapshots: d.Snapshots}
	bound, err := verifier.loadVerifiedBoundExecution(ctx, task, task.Status.AgentExecutionBinding)
	if err != nil {
		return err
	}
	if !bound.mcpConfiguration.ToolPolicy.Allows(RuntimeFeedbackToolName) || bound.plan.Workspace != nil {
		return nil
	}
	task = bound.frozenTask
	if !runtimeFeedbackPreSubmissionCleanupCandidate(task) || task.Status.Execution.ControllerEpoch != fence.Epoch {
		return nil
	}
	attemptID, err := promptAttemptIDFromTask(task)
	if err != nil {
		return err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	if attempt.ExecutionState != store.PromptExecutionPlanned && attempt.ExecutionState != store.PromptExecutionReserved {
		return nil
	}
	if err := validateRuntimeFeedbackCleanupAttempt(attempt, task, bound, fence); err != nil {
		return err
	}
	e := task.Status.Execution
	if !taskScopedRuntimeSessionCleanupComplete(task) {
		pool := &corev1alpha1.RuntimePool{}
		if err := d.APIReader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: e.RuntimePoolName}, pool); err != nil {
			return err
		}
		// A queued attempt can have admitted a newer image while preserving
		// its frozen profile. Cleanup must address that exact selected pool.
		deliveryPlan, err := acpRuntimeDeliveryPlanForBoundPool(bound.plan, e, pool)
		if err != nil {
			return err
		}
		if err := validateFrozenACPDispatchTarget(task, acpDispatchTarget{pool: pool}, bound, deliveryPlan); err != nil {
			return err
		}
		if !runtimeFeedbackPoolMatches(pool, string(task.UID)) {
			return fmt.Errorf("%w: runtime feedback cleanup pool is not dedicated", store.ErrConflict)
		}
	}
	ready, err := d.reconcileRecoveredTaskScopedRuntimeSession(ctx, task, task.UID, true)
	if err != nil || !ready {
		return err
	}
	return d.requeuePreSubmissionTask(ctx, task, attemptID, fence, errRuntimeFeedbackUnavailable)
}

func validateRuntimeFeedbackCleanupAttempt(attempt *store.PromptAttempt, task *corev1alpha1.Task, bound *verifiedAgentExecution, fence store.ControllerEpochFence) error {
	if !queuedPromptAttemptMatchesTask(attempt, task) || attempt.BindingDigest != bound.binding.BindingDigest ||
		attempt.SnapshotDigest != bound.snapshot.Digest || attempt.ControllerEpochName != fence.Name || attempt.ControllerEpoch != fence.Epoch {
		return fmt.Errorf("%w: runtime feedback cleanup attempt authority changed", store.ErrConflict)
	}
	e := task.Status.Execution
	switch attempt.ExecutionState {
	case store.PromptExecutionPlanned:
		if attempt.RuntimeInstanceID != e.RuntimeInstanceID || attempt.SessionUID != e.RuntimeSessionUID ||
			attempt.SessionLeaseGeneration != e.RuntimeSessionGeneration {
			return fmt.Errorf("%w: runtime feedback cleanup session binding changed", store.ErrConflict)
		}
	case store.PromptExecutionReserved:
		// Recovery may have reset the durable binding before its status write
		// failed. That window still requires the exact prior cleanup receipt.
		if !taskScopedRuntimeSessionCleanupComplete(task) || attempt.RuntimeInstanceID != "" || attempt.SessionUID != "" || attempt.SessionLeaseGeneration != 0 {
			return fmt.Errorf("%w: runtime feedback cleanup reset lacks retirement proof", store.ErrConflict)
		}
	}
	return nil
}
