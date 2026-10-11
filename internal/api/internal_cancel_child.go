/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tools"
)

const cancelledChildStatus = "cancelled"

var errCancelledChildReplaced = errors.New("child task was replaced")

// CancelChildTask cancels a Pending or Running child of the calling Task for
// the native worker's cancel_task tool. Workers cannot write Task status, so
// the controller writes the Cancelled phase after verifying the caller's
// worker identity and the provenance-protected parent link.
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

	authorizer := h.internalCallerAuthorizer()
	parent, err := authorizer.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}
	verified, err := authorizer.verifiedCoordinationChild(c.Context(), parent, childName)
	if err != nil {
		return err
	}

	message := "cancelled by parent task"
	if reason := strings.TrimSpace(request.Reason); reason != "" {
		message += ": " + reason
	}
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	ctx := c.Context()
	result := tools.CancelTaskResult{TaskName: childName}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		child := &corev1alpha1.Task{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: childName}, child); err != nil {
			return err
		}
		// The name may have been reused since the parent link was verified.
		if child.UID != verified.UID {
			return errCancelledChildReplaced
		}
		if child.Status.Phase != corev1alpha1.TaskPhasePending && child.Status.Phase != corev1alpha1.TaskPhaseRunning {
			result.Status = fmt.Sprintf("task is already in %s phase, cannot cancel", child.Status.Phase)
			return nil
		}
		now := metav1.Now()
		child.Status.Phase = corev1alpha1.TaskPhaseCancelled
		child.Status.CompletionTime = &now
		child.Status.Message = message
		if err := h.k8sClient.Status().Update(ctx, child); err != nil {
			return err
		}
		result.Status = cancelledChildStatus
		return nil
	})
	switch {
	case err == nil:
		return c.JSON(result)
	case errors.Is(err, errCancelledChildReplaced), apierrors.IsNotFound(err):
		return fiber.NewError(fiber.StatusForbidden, "task is outside caller coordination scope")
	case apierrors.IsConflict(err):
		return fiber.NewError(fiber.StatusConflict, "the child task kept changing while it was being cancelled; retry")
	default:
		return fiber.NewError(fiber.StatusInternalServerError, "failed to cancel the child task")
	}
}
