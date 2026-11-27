/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
)

func (h *InternalHandlers) SealChildRequesterStamp(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	taskName := strings.TrimSpace(c.Params("taskName"))
	childName := strings.TrimSpace(c.Params("child"))
	if namespace == "" || taskName == "" || childName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, taskName, and child are required")
	}
	if len(requesterStampKey) == 0 {
		return fiber.NewError(fiber.StatusNotImplemented, "requester stamps are not enabled on this controller")
	}
	parent, err := h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}
	if !connectors.RequesterStampValid(requesterStampKey, parent) {
		return fiber.NewError(fiber.StatusForbidden, "the parent task carries no verified requester")
	}
	ctx := c.Context()
	reader := h.apiReader
	if reader == nil {
		reader = h.k8sClient
	}
	child := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: childName}, child); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "child task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read child task")
	}
	if err := controller.SealChildRequesterStamp(ctx, h.k8sClient, requesterStampKey, parent, child); err != nil {
		switch {
		case errors.Is(err, controller.ErrChildSealRefused):
			return fiber.NewError(fiber.StatusForbidden, err.Error())
		case apierrors.IsConflict(err):
			return fiber.NewError(fiber.StatusConflict, "the child task changed while it was being sealed; retry")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to seal the child task")
	}
	return c.JSON(fiber.Map{"sealed": true})
}
