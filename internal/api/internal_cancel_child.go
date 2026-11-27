/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const cancellationRequestedStatus = "cancellation requested"

func (h *InternalHandlers) CancelChildTask(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	taskName := strings.TrimSpace(c.Params("taskName"))
	childName := strings.TrimSpace(c.Params("child"))
	if namespace == "" || taskName == "" || childName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, taskName, and child are required")
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&request); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		}
	}
	reason := strings.TrimSpace(request.Reason)
	if utf8.RuneCountInString(reason) > tools.MaxCancelReasonLength {
		return fiber.NewError(fiber.StatusBadRequest,
			fmt.Sprintf("reason must be at most %d characters", tools.MaxCancelReasonLength))
	}
	message := "cancelled by parent task"
	if reason != "" {
		message += ": " + reason
	}

	authorizer := h.internalCallerAuthorizer()
	parent, err := authorizer.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}
	var child *corev1alpha1.Task
	result := tools.CancelTaskResult{TaskName: childName}
	if err := withInternalTaskData(c, h.resultStore, taskName, func(ctx context.Context) error {
		if err := authorizer.revalidateTaskCaller(c, parent); err != nil {
			return err
		}
		verified, err := authorizer.verifiedCoordinationChild(ctx, parent, childName)
		child = verified
		return err
	}, func(ctx context.Context) error {
		status, err := h.requestChildCancellation(ctx, parent, child.Namespace, child.Name, child.UID, message)
		result.Status = status
		return err
	}); err != nil {
		return err
	}
	return c.JSON(result)
}

func (h *InternalHandlers) requestChildCancellation(
	ctx context.Context,
	parent *corev1alpha1.Task,
	namespace, name string,
	uid types.UID,
	message string,
) (string, error) {
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	var status string
	var committedMessage *string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		child := &corev1alpha1.Task{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, child); err != nil {
			if apierrors.IsNotFound(err) {
				return fiber.NewError(fiber.StatusForbidden, "task is outside caller coordination scope")
			}
			return err
		}
		if child.UID != uid {
			return fiber.NewError(fiber.StatusForbidden, "task is outside caller coordination scope")
		}
		if child.Status.Phase != corev1alpha1.TaskPhasePending && child.Status.Phase != corev1alpha1.TaskPhaseRunning {
			status = fmt.Sprintf("task is already in %s phase, cannot cancel", child.Status.Phase)
			return nil
		}
		if child.Spec.Type == corev1alpha1.TaskTypeAgent ||
			(child.Status.Execution != nil && child.Status.Execution.ControllerEpoch > 0) {
			return fiber.NewError(fiber.StatusConflict,
				"the child task runs on an agent runtime; cancel_task cannot cancel it")
		}
		status = cancellationRequestedStatus
		if _, requested := child.Annotations[labels.AnnotationCancelRequested]; requested {
			return nil
		}
		if committedMessage == nil {
			intentMessage, err := h.recordChildCancellationIntent(ctx, parent, child, message)
			if err != nil {
				return err
			}
			committedMessage = &intentMessage
		}
		patched := child.DeepCopy()
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		patched.Annotations[labels.AnnotationCancelRequested] = *committedMessage
		return h.k8sClient.Patch(ctx, patched, client.MergeFromWithOptions(child, client.MergeFromWithOptimisticLock{}))
	})
	if apierrors.IsConflict(err) {
		return "", fiber.NewError(fiber.StatusConflict, "the child task kept changing while it was being cancelled; retry")
	}
	return status, err
}

type childCancellationIntent struct {
	ParentUID string `json:"parentUID"`
	ChildUID  string `json:"childUID"`
	ChildName string `json:"childName"`
	Message   string `json:"message"`
}

func (h *InternalHandlers) recordChildCancellationIntent(ctx context.Context, parent, child *corev1alpha1.Task, message string) (string, error) {
	intents, ok := h.resultStore.(store.DeduplicatingExecutionEventStore)
	if !ok {
		return "", fiber.NewError(fiber.StatusServiceUnavailable, "cancellation intent storage unavailable")
	}
	intent := childCancellationIntent{
		ParentUID: string(parent.UID), ChildUID: string(child.UID), ChildName: child.Name, Message: message,
	}
	content, err := json.Marshal(intent)
	if err != nil {
		return "", err
	}
	persisted, _, err := intents.AppendExecutionEventIfAbsent(ctx, &store.ExecutionEvent{
		Namespace: parent.Namespace, StreamType: store.ExecutionEventStreamTypeTask,
		StreamID: parent.Name, TaskName: parent.Name, ToolName: promptToolCancel,
		Type: events.ExecutionEventTypeTaskCancellationRequested, Severity: events.ExecutionEventSeverityInfo,
		Summary: "Child task cancellation requested", Content: content,
	}, fmt.Sprintf("cancel-child:%s:%s", parent.UID, child.UID))
	if err != nil {
		return "", err
	}
	var committed childCancellationIntent
	if persisted == nil || json.Unmarshal(persisted.Content, &committed) != nil ||
		committed.ParentUID != intent.ParentUID || committed.ChildUID != intent.ChildUID {
		return "", fmt.Errorf("invalid child cancellation intent")
	}
	return committed.Message, nil
}
