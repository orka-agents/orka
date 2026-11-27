/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	execevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func cancelTaskStatus(
	ctx context.Context,
	c client.Client,
	results store.ResultStore,
	controls store.DurableControlStore,
	epochs *ControllerEpochManager,
	task *corev1alpha1.Task,
	reason string,
) error {
	write := func(writeCtx context.Context) error {
		if err := revokeTaskJobAuthority(writeCtx, results, store.TaskJobIdentity{
			Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID,
		}); err != nil {
			return err
		}
		now := metav1.Now()
		task.Status.Phase = corev1alpha1.TaskPhaseCancelled
		task.Status.CompletionTime = &now
		task.Status.Message = reason
		return c.Status().Update(writeCtx, task)
	}
	if task.Status.Execution != nil && task.Status.Execution.ControllerEpoch > 0 {
		return withACPTaskStatusGuard(ctx, controls, epochs, write)
	}
	return write(ctx)
}

func requestedAgentTaskCancellation(task *corev1alpha1.Task) (string, bool) {
	message, ok := task.Annotations[labels.AnnotationCancelRequested]
	if !ok || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return "", false
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending && task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return "", false
	}
	if strings.TrimSpace(message) == "" {
		message = "cancellation requested"
	}
	return boundACPStatusMessage(message), true
}

func (r *TaskReconciler) cancelRequestedAgentTask(ctx context.Context, task *corev1alpha1.Task) error {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	var cancelled *corev1alpha1.Task
	message := ""
	err := retryTaskStatusOnConflict(retry.DefaultRetry, func() error {
		current := &corev1alpha1.Task{}
		if err := r.Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != task.UID {
			return nil
		}
		requested, ok := requestedAgentTaskCancellation(current)
		if !ok {
			return nil
		}
		if current.Status.Execution == nil {
			attempt, err := r.settleUnprojectedACPAttempt(ctx, current, acpCancelledReason, requested, "cancelled-before-status")
			if err != nil {
				return err
			}
			setCancelledBeforeStatusExecution(&current.Status, attempt, acpCancelledReason, requested, metav1.Now())
		}
		if err := cancelTaskStatus(ctx, r.Client, r.ResultStore, r.DurableControlStore, r.ControllerEpochManager, current, requested); err != nil {
			return err
		}
		cancelled, message = current, requested
		return nil
	})
	if err != nil {
		return err
	}
	if cancelled != nil {
		_ = r.recordTaskLifecycleEvent(ctx, cancelled, execevents.ExecutionEventTypeTaskCancelled,
			execevents.ExecutionEventSeverityWarning, message)
	}
	return nil
}
