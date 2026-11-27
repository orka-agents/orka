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

func (r *TaskReconciler) continuationLinkPending(ctx context.Context, task *corev1alpha1.Task) (bool, bool, error) {
	predecessorName := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfName])
	predecessorUID := strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfUID])
	if predecessorName == "" && predecessorUID == "" {
		return false, false, nil
	}
	if predecessorName == "" || predecessorUID == "" {
		return false, true, nil
	}
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
