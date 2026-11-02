/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/capabilitygap"
)

const capabilityGapTaskAction = "listTasks"

// GetCapabilityGaps handles GET /api/v1/capability-gaps. It returns the
// namespace's capability-gap counts for the current UTC day and the six
// before it: categories, closed reasons, runtimes, catalog tool names and the
// names of recent Tasks the caller may list. It never returns Task content.
func (h *Handlers) GetCapabilityGaps(c fiber.Ctx) error {
	recorder := capabilitygap.Default()
	if recorder == nil {
		return fiber.NewError(fiber.StatusNotFound, "capability-gap signals are not enabled")
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if err := h.authorizeContextTokenAction(c, capabilityGapTaskAction, h.contextTokenAuthorization.TaskListScopes); err != nil {
		return err
	}

	summary := recorder.Summary(namespace)
	gatewayAuthorizations := map[gatewayTaskAuthorizationKey]bool{}
	for i := range summary.Signals {
		visible := make([]string, 0, len(summary.Signals[i].Tasks))
		for _, name := range summary.Signals[i].Tasks {
			ok, err := h.capabilityGapTaskVisible(c, namespace, name, gatewayAuthorizations)
			if err != nil {
				return err
			}
			if ok {
				visible = append(visible, name)
			}
		}
		summary.Signals[i].Tasks = visible
	}
	return c.JSON(summary)
}

// capabilityGapTaskVisible applies the per-Task checks ListTasks applies, so
// a Task reference appears only to callers who could list that Task. Deleted
// Tasks are dropped; their counts remain.
func (h *Handlers) capabilityGapTaskVisible(
	c fiber.Ctx, namespace, name string, gatewayAuthorizations map[gatewayTaskAuthorizationKey]bool,
) (bool, error) {
	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: name}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fiber.NewError(fiber.StatusInternalServerError, "failed to load capability-gap Task reference")
	}
	if h.contextTokenAuthorization.Enabled() {
		allowed, err := h.contextTokenAllowsLoadedTask(c, capabilityGapTaskAction, task)
		if err != nil || !allowed {
			return false, err
		}
	}
	return h.taskAccess().gatewayTaskReadableCached(c, capabilityGapTaskAction, task, gatewayAuthorizations)
}
