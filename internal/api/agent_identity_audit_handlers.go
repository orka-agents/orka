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
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

const (
	defaultIdentityAuditLimit = 100
	maxIdentityAuditLimit     = 500
)

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

	if h.client == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "task lookup is unavailable")
	}

	var matchedTask *corev1alpha1.Task
	if taskName != "" {
		task, err := h.resolveAuditedTask(c, namespace, taskName)
		if err != nil {
			return err
		}
		if taskUID != "" && taskUID != string(task.UID) {
			return c.JSON(fiber.Map{apiFieldItems: []outboundaccess.AgentIdentityAuditRecord{}})
		}
		taskUID = string(task.UID)
		matchedTask = task
	}

	if h.agentIdentityAudit == nil {
		return c.JSON(fiber.Map{apiFieldItems: []outboundaccess.AgentIdentityAuditRecord{}})
	}

	candidates := h.agentIdentityAudit.List(outboundaccess.AgentIdentityAuditFilter{
		Namespace: namespace,
		TaskUID:   taskUID,
		AgentName: agentName,
		Outcome:   outcome,
		Limit:     -1,
	})
	if len(candidates) == 0 {
		return c.JSON(fiber.Map{apiFieldItems: []outboundaccess.AgentIdentityAuditRecord{}})
	}

	tasksByUID := map[string]*corev1alpha1.Task{}
	if matchedTask != nil {
		tasksByUID[string(matchedTask.UID)] = matchedTask
	} else {
		taskList := &corev1alpha1.TaskList{}
		if err := h.client.List(c.Context(), taskList, client.InNamespace(namespace)); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to list tasks")
		}
		for i := range taskList.Items {
			t := &taskList.Items[i]
			tasksByUID[string(t.UID)] = t
		}
	}

	gatewayAuthorizations := map[gatewayTaskAuthorizationKey]bool{}
	visibleByUID := map[string]bool{}
	records := make([]outboundaccess.AgentIdentityAuditRecord, 0, min(limit, len(candidates)))
	for _, rec := range candidates {
		if rec.TaskUID == "" {
			continue
		}
		allowed, checked := visibleByUID[rec.TaskUID]
		if !checked {
			task, ok := tasksByUID[rec.TaskUID]
			if !ok {
				allowed = false
			} else {
				allowed, err = h.auditTaskVisible(c, task, gatewayAuthorizations)
				if err != nil {
					return err
				}
			}
			visibleByUID[rec.TaskUID] = allowed
		}
		if !allowed {
			continue
		}
		records = append(records, rec)
		if len(records) >= limit {
			break
		}
	}
	return c.JSON(fiber.Map{apiFieldItems: records})
}

func (h *Handlers) resolveAuditedTask(c fiber.Ctx, namespace, taskName string) (*corev1alpha1.Task, error) {
	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to load task")
	}
	if ahpTaskHidden(GetUserInfo(c), task) {
		return nil, fiber.NewError(fiber.StatusNotFound, "task not found")
	}
	allowed, err := h.auditTaskVisible(c, task, map[gatewayTaskAuthorizationKey]bool{})
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fiber.NewError(fiber.StatusForbidden, "not authorized to inspect audit records for this task")
	}
	return task, nil
}

func (h *Handlers) auditTaskVisible(
	c fiber.Ctx,
	task *corev1alpha1.Task,
	gatewayAuthorizations map[gatewayTaskAuthorizationKey]bool,
) (bool, error) {
	if h.contextTokenAuthorization.Enabled() {
		allowed, err := h.contextTokenAllowsLoadedTask(c, "listAgentIdentityAudit", task)
		if err != nil || !allowed {
			return false, err
		}
	}
	allowed, err := h.taskAccess().gatewayTaskReadableCached(c, "listAgentIdentityAudit", task, gatewayAuthorizations)
	if err != nil || !allowed {
		return false, err
	}
	return !ahpTaskHidden(GetUserInfo(c), task), nil
}
