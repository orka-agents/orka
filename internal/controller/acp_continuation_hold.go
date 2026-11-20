/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/taskwait"
)

// continuationLinkPending holds a continuation's execution until the parked
// Task it continues has recorded this exact Task in its resumed wait. Until
// then the continuation provably never ran, so a reconcile that cannot prove
// it is Orka's may abandon the wait without discarding any work. It reports
// pending while the predecessor still reserves this Task, and orphaned once
// nothing can link it any more. Reads are authoritative; a failed read holds.
func (r *TaskReconciler) continuationLinkPending(ctx context.Context, task *corev1alpha1.Task) (bool, bool, error) {
	predecessorName := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfName])
	predecessorUID := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfUID])
	if predecessorName == "" && predecessorUID == "" {
		return false, false, nil
	}
	if predecessorName == "" || predecessorUID == "" {
		return false, true, nil
	}
	// Decide from one authoritative snapshot of the predecessor, so a link
	// completed between two reads is never mistaken for an orphan.
	predecessor := &corev1alpha1.Task{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: predecessorName}, predecessor); err != nil {
		if apierrors.IsNotFound(err) {
			return false, true, nil
		}
		return true, false, err
	}
	wait := predecessor.Status.Wait
	if string(predecessor.UID) != predecessorUID || wait == nil || wait.ContinuationTaskName != task.Name {
		return false, true, nil
	}
	switch wait.State {
	case corev1alpha1.TaskWaitStateResumed:
		if wait.ContinuationTaskUID == string(task.UID) {
			return false, false, nil
		}
		return false, true, nil
	case corev1alpha1.TaskWaitStateResuming:
		return true, false, nil
	default:
		return false, true, nil
	}
}

const acpContinuationUnlinkedReason corev1alpha1.TaskExecutionReason = "ContinuationUnlinked"

// cancelUnlinkedContinuation ends a continuation that its predecessor can no
// longer link. It never ran, so like a queued Task that reached its deadline
// it is cancelled directly, without a prompt attempt or Session transcript.
func (r *TaskReconciler) cancelUnlinkedContinuation(ctx context.Context, task *corev1alpha1.Task) error {
	const message = "the Task this continuation resumes never linked it, so it did not run"
	reader := uncachedReader(r.APIReader, r.Client)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != task.UID || (current.Status.Execution != nil && !taskExecutionStateTerminal(current.Status.Execution.State)) ||
			taskwait.TerminalPhase(current.Status.Phase) {
			return nil
		}
		now := metav1.Now()
		base := current.DeepCopy()
		current.Status.Phase = corev1alpha1.TaskPhaseCancelled
		current.Status.CompletionTime = &now
		current.Status.Message = message
		current.Status.Execution = &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateCancelled, Outcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			Reason: acpContinuationUnlinkedReason, Message: message, LastTransitionTime: &now,
		}
		current.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			LastTransitionTime: &now,
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeComplete, Status: metav1.ConditionTrue, LastTransitionTime: now,
			Reason: eventReasonTaskCancelled, Message: message,
		})
		return r.Status().Patch(ctx, current, client.MergeFrom(base))
	})
}
