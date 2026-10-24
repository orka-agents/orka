/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	cancelTaskAction = "cancelTask"
	// requestedCancellationMessage completes a non-agent Task cancelled here.
	requestedCancellationMessage = "cancelled by request"
)

// CancelTaskResponse reports a recorded cancel request.
type CancelTaskResponse struct {
	Status string `json:"status"`
}

// CancelTask asks a queued or running Task to stop. The response is 202 once
// the request is recorded; the Task reaches Cancelled asynchronously, or the
// phase it finished in if it ended first. An agent turn is cancelled by the
// controller: it answers pending permission requests as cancelled, cancels the
// prompt, and stops the agent if the cancel does not settle in time.
func (h *Handlers) CancelTask(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, cancelTaskAction, h.contextTokenAuthorization.TaskUpdateScopes); err != nil {
		return err
	}
	task, err := h.taskAccess().loadAuthorized(c, cancelTaskAction, namespace, c.Params("id"))
	if err != nil {
		return err
	}
	if err := h.taskAccess().authorizeGatewayTaskOperate(c, cancelTaskAction, task); err != nil {
		return err
	}
	status, err := h.requestTaskCancellation(c.Context(), GetUserInfo(c), task)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(CancelTaskResponse{Status: status})
}

// requestTaskCancellation records a cancel request on a Task the caller may
// operate. A person may cancel only the Tasks they requested; ServiceAccount
// callers are bounded by the route's Kubernetes permission check.
func (h *Handlers) requestTaskCancellation(ctx context.Context, ui *UserInfo, task *corev1alpha1.Task) (string, error) {
	if ui == nil {
		return "", fiber.NewError(fiber.StatusUnauthorized, "missing authenticated identity")
	}
	if person := requesterFromUserInfo(ui); person != nil && !taskRequestedBy(task, person) {
		return "", fiber.NewError(fiber.StatusForbidden, "only the Task's requester may cancel it")
	} else if person == nil && ui.AuthType != AuthTypeTokenReview {
		return "", fiber.NewError(fiber.StatusForbidden, "cancelling a Task requires a verified identity")
	}
	reader := h.apiReader
	if reader == nil {
		reader = h.client
	}
	status := cancellationRequestedStatus
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
			if apierrors.IsNotFound(err) {
				return fiber.NewError(fiber.StatusNotFound, "task not found")
			}
			return err
		}
		// The name may have been reused since the Task was authorized.
		if current.UID != task.UID {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		if !current.DeletionTimestamp.IsZero() {
			return fiber.NewError(fiber.StatusGone, "task is deleting")
		}
		// A schedule is not a run: cancelling records nothing it acts on.
		if current.Spec.Schedule != "" || current.Status.Phase == corev1alpha1.TaskPhaseScheduled {
			return fiber.NewError(fiber.StatusConflict, "a scheduled Task is not cancelled; set spec.suspend to stop its runs, or cancel a run")
		}
		switch current.Status.Phase {
		case corev1alpha1.TaskPhaseCancelled:
			status = string(corev1alpha1.TaskPhaseCancelled)
			return nil
		case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed:
			return fiber.NewError(fiber.StatusConflict, fmt.Sprintf("task already %s", strings.ToLower(string(current.Status.Phase))))
		}
		if _, requested := current.Annotations[labels.AnnotationCancelRequested]; requested {
			return nil
		}
		patched := current.DeepCopy()
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		patched.Annotations[labels.AnnotationCancelRequested] = requestedCancellationMessage
		return h.client.Patch(ctx, patched, client.MergeFromWithOptions(current, client.MergeFromWithOptimisticLock{}))
	})
	if apierrors.IsConflict(err) {
		return "", fiber.NewError(fiber.StatusConflict, "the task kept changing while it was being cancelled; retry")
	}
	return status, err
}

// taskRequestedBy reports whether a verified person is the Task's requester.
// Only the API stamps requestedBy, and only controller identities may mark it.
func taskRequestedBy(task *corev1alpha1.Task, person *corev1alpha1.RequestedBy) bool {
	requester := task.Spec.RequestedBy
	return requester != nil && person != nil &&
		task.Annotations[labels.AnnotationRequestedBySource] == labels.RequestedBySourceAPI &&
		requester.Issuer != "" && requester.Subject != "" &&
		requester.Issuer == person.Issuer && requester.Subject == person.Subject
}
