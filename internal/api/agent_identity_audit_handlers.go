/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

const (
	defaultIdentityAuditLimit = 100
	maxIdentityAuditLimit     = 500
)

// ListAgentIdentityAudit serves GET /api/v1/audit/identity, returning redacted
// agent-identity-audit records scoped to the caller's authorized namespace.
// Records never carry tokens, headers, bodies, full URLs, or raw error text.
func (h *Handlers) ListAgentIdentityAudit(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listAgentIdentityAudit", h.contextTokenAuthorization.TaskListScopes); err != nil {
		return err
	}

	taskName := strings.TrimSpace(c.Query("task", ""))
	taskUID := strings.TrimSpace(c.Query("taskUID", ""))
	agentName := strings.TrimSpace(c.Query("agent", ""))
	outcome := strings.TrimSpace(c.Query("outcome", ""))
	if outcome != "" && outcome != "issued" && outcome != "refused" {
		return fiber.NewError(fiber.StatusBadRequest, "outcome must be issued or refused")
	}

	limit := defaultIdentityAuditLimit
	if rawLimit := strings.TrimSpace(c.Query("limit", "")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			return fiber.NewError(fiber.StatusBadRequest, "limit must be a positive integer")
		}
		if parsed > maxIdentityAuditLimit {
			parsed = maxIdentityAuditLimit
		}
		limit = parsed
	}

	if taskName != "" {
		if h.client == nil {
			return fiber.NewError(fiber.StatusServiceUnavailable, "task lookup is unavailable")
		}
		task := &corev1alpha1.Task{}
		if err := h.client.Get(c.Context(), types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
			if apierrors.IsNotFound(err) {
				return fiber.NewError(fiber.StatusNotFound, "task not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, "failed to load task")
		}
		if err := h.taskAccess().authorizeGatewayTask(c, "listAgentIdentityAudit", task, false); err != nil {
			return err
		}
		if taskUID != "" && taskUID != string(task.UID) {
			return c.JSON(fiber.Map{apiFieldItems: []outboundaccess.AgentIdentityAuditRecord{}})
		}
		taskUID = string(task.UID)
	}

	if h.agentIdentityAudit == nil {
		return c.JSON(fiber.Map{apiFieldItems: []outboundaccess.AgentIdentityAuditRecord{}})
	}

	records := h.agentIdentityAudit.List(outboundaccess.AgentIdentityAuditFilter{
		Namespace: namespace,
		TaskUID:   taskUID,
		AgentName: agentName,
		Outcome:   outcome,
		Limit:     limit,
	})
	if records == nil {
		records = []outboundaccess.AgentIdentityAuditRecord{}
	}
	return c.JSON(fiber.Map{apiFieldItems: records})
}
