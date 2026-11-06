/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
)

const waitingForCapacityMetricResourceTask = "task"

// desiredWaitingForCapacityCondition derives the WaitingForCapacity condition
// from durable admission state. It returns nil when nothing needs recording:
// the Task never waited for capacity.
func desiredWaitingForCapacityCondition(task *corev1alpha1.Task) *metav1.Condition {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return nil
	}
	condition := &metav1.Condition{Type: corev1alpha1.TaskConditionWaitingForCapacity, ObservedGeneration: task.Generation}
	if task.Status.Phase == corev1alpha1.TaskPhasePending {
		if execution := task.Status.Execution; execution != nil &&
			execution.Reason == corev1alpha1.TaskExecutionReasonWaitingForCapacity &&
			(execution.State == corev1alpha1.TaskExecutionStateQueued || execution.State == corev1alpha1.TaskExecutionStateReserved) {
			condition.Status = metav1.ConditionTrue
			condition.Reason = corev1alpha1.TaskWaitingForCapacityReasonRuntimeCapacityUnavailable
			condition.Message = execution.Message
			return condition
		}
		if workspace := task.Status.ExecutionWorkspace; workspace != nil {
			for i := range workspace.Conditions {
				admission := workspace.Conditions[i]
				if admission.Status == metav1.ConditionFalse && admission.Reason == string(workspacev1alpha1.ReasonCapacityUnavailable) {
					condition.Status = metav1.ConditionTrue
					condition.Reason = corev1alpha1.TaskWaitingForCapacityReasonWorkspaceCapacityUnavailable
					condition.Message = admission.Message
					return condition
				}
			}
		}
	}
	existing := meta.FindStatusCondition(task.Status.Conditions, corev1alpha1.TaskConditionWaitingForCapacity)
	if existing == nil {
		return nil
	}
	condition.Status = metav1.ConditionFalse
	condition.Reason = corev1alpha1.TaskWaitingForCapacityReasonCapacityAvailable
	condition.Message = "capacity is no longer the blocking admission reason"
	return condition
}

func waitingForCapacityConditionChanged(task *corev1alpha1.Task, desired *metav1.Condition) bool {
	if desired == nil {
		return false
	}
	existing := meta.FindStatusCondition(task.Status.Conditions, desired.Type)
	return existing == nil || existing.Status != desired.Status || existing.Reason != desired.Reason || existing.Message != desired.Message
}

// syncACPWaitingForCapacity keeps the user-visible WaitingForCapacity
// condition in step with admission. The wait itself stays retryable and
// bounded by the Task deadline; this only reports it.
func (r *TaskReconciler) syncACPWaitingForCapacity(ctx context.Context, task *corev1alpha1.Task) error {
	if !waitingForCapacityConditionChanged(task, desiredWaitingForCapacityCondition(task)) {
		return nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != task.UID {
			return nil
		}
		desired := desiredWaitingForCapacityCondition(current)
		if !waitingForCapacityConditionChanged(current, desired) {
			return nil
		}
		previous := meta.FindStatusCondition(current.Status.Conditions, desired.Type)
		entering := desired.Status == metav1.ConditionTrue && (previous == nil || previous.Status != metav1.ConditionTrue)
		base := current.DeepCopy()
		meta.SetStatusCondition(&current.Status.Conditions, *desired)
		if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		if entering {
			metrics.RecordWaitingForCapacity(waitingForCapacityMetricResourceTask, desired.Reason)
		}
		task.ResourceVersion, task.Status = current.ResourceVersion, current.Status
		return nil
	})
}
