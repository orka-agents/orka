/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"

	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

type ListTaskApprovalsResponse struct {
	Namespace string               `json:"namespace"`
	TaskName  string               `json:"taskName"`
	Approvals []approvals.Approval `json:"approvals"`
}

const (
	approvalDecisionApprove = "approve"
	approvalDecisionDecline = "decline"
)

type ApprovalDecisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// ListTaskApprovals handles GET /api/v1/tasks/{id}/approvals.
func (h *Handlers) ListTaskApprovals(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "listTaskApprovals", namespace, taskName); err != nil {
		return err
	}
	task, err := h.loadReadableTask(c, namespace, taskName, "listTaskApprovals")
	if err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	listed, err := h.listTaskApprovalEvents(c.Context(), namespace, taskName)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	return c.JSON(ListTaskApprovalsResponse{Namespace: namespace, TaskName: taskName, Approvals: deriveTaskApprovalState(filterTaskApprovalEvents(listed, task))})
}

// DecideTaskApproval handles POST /api/v1/tasks/{id}/approvals/{approvalID}/decision.
func (h *Handlers) DecideTaskApproval(c fiber.Ctx) error {
	taskName := c.Params("id")
	approvalID := strings.TrimSpace(c.Params("approvalID"))
	if approvalID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "approvalID is required")
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "decideTaskApproval", h.contextTokenAuthorization.TaskUpdateScopes); err != nil {
		return err
	}
	if err := authorizeKubernetesApprovalDecision(c.Context(), h.clientset, GetUserInfo(c), namespace, taskName); err != nil {
		return err
	}
	task, err := h.loadReadableTask(c, namespace, taskName, "decideTaskApproval")
	if err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}

	var req ApprovalDecisionRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	decision := strings.ToLower(strings.TrimSpace(req.Decision))
	var eventType string
	switch decision {
	case approvalDecisionApprove, "approved":
		decision = approvalDecisionApprove
		eventType = events.ExecutionEventTypeApprovalApproved
	case approvalDecisionDecline, "declined":
		decision = approvalDecisionDecline
		eventType = events.ExecutionEventTypeApprovalDeclined
	default:
		return fiber.NewError(fiber.StatusBadRequest, "decision must be approve or decline")
	}

	listed, err := h.listTaskApprovalEvents(c.Context(), namespace, taskName)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	listed = filterTaskApprovalEvents(listed, task)
	current, found := findApproval(deriveTaskApprovalState(listed), approvalID)
	if !found {
		return fiber.NewError(fiber.StatusNotFound, "approval not found")
	}
	if current.Status != approvals.StatusPending {
		if (current.Status == approvals.StatusApproved && decision == approvalDecisionApprove) || (current.Status == approvals.StatusDeclined && decision == approvalDecisionDecline) {
			if err := h.patchTaskApprovalDecisionAnnotation(c.Context(), namespace, taskName, current); err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to nudge task after approval decision: %v", err))
			}
			return c.JSON(current)
		}
		return fiber.NewError(fiber.StatusConflict, fmt.Sprintf("approval is already %s", current.Status))
	}
	if !task.DeletionTimestamp.IsZero() {
		return fiber.NewError(fiber.StatusGone, "task is deleting")
	}
	if isTerminalInternalTaskPhase(task.Status.Phase) {
		return fiber.NewError(fiber.StatusConflict, "task is complete")
	}

	sessionName, err := h.existingSessionNameForTask(c.Context(), namespace, task)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}
	actor := approvalDecisionActor(GetUserInfo(c))
	content, err := json.Marshal(map[string]string{
		"approvalID": approvalID,
		"decision":   decision,
		"reason":     strings.TrimSpace(req.Reason),
		"actor":      actor,
		"taskUID":    string(task.UID),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode approval decision: %v", err))
	}
	appended, err := h.executionEventStore.AppendExecutionEvent(c.Context(), &store.ExecutionEvent{
		Namespace:   namespace,
		StreamType:  events.ExecutionEventStreamTypeTask,
		StreamID:    taskName,
		TaskName:    taskName,
		SessionName: sessionName,
		Type:        eventType,
		Severity:    events.ExecutionEventSeverityInfo,
		// Stamp the canonical approval ID into the stable top-level field (not
		// current.ToolCallID, which is empty for harness-emitted approvals). This
		// keeps approvals.Derive and the terminal-conflict guard able to match the
		// decision to its pending approval even if a large reason truncates the
		// JSON content (where approvalID would otherwise be the only copy).
		ToolCallID: approvalID,
		Summary:    fmt.Sprintf("approval %s", decision),
		Content:    content,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			refreshed, listErr := h.listTaskApprovalEvents(c.Context(), namespace, taskName)
			if listErr != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", listErr))
			}
			refreshed = filterTaskApprovalEvents(refreshed, task)
			if current, found := findApproval(deriveTaskApprovalState(refreshed), approvalID); found {
				if approvalStatusMatchesDecision(current.Status, decision) {
					if patchErr := h.patchTaskApprovalDecisionAnnotation(c.Context(), namespace, taskName, current); patchErr != nil {
						return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to nudge task after approval decision: %v", patchErr))
					}
					return c.JSON(current)
				}
				return fiber.NewError(fiber.StatusConflict, fmt.Sprintf("approval is already %s", current.Status))
			}
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append approval decision: %v", err))
	}
	listed = append(listed, *appended)
	updated, _ := findApproval(deriveTaskApprovalState(listed), approvalID)
	if err := h.patchTaskApprovalDecisionAnnotation(c.Context(), namespace, taskName, updated); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to nudge task after approval decision: %v", err))
	}
	return c.JSON(updated)
}

func filterTaskApprovalEvents(input []store.ExecutionEvent, task *corev1alpha1.Task) []store.ExecutionEvent {
	if task == nil {
		return input
	}
	return approvals.FilterEventsForTaskUID(input, string(task.UID))
}

func deriveTaskApprovalState(input []store.ExecutionEvent) []approvals.Approval {
	// V1 does not passively expire approvals. Until an expiry producer appends
	// explicit ApprovalExpired events, pending approvals must remain human-
	// decidable and must match controller parking semantics.
	return approvals.Derive(input, time.Time{})
}

func (h *Handlers) patchTaskApprovalDecisionAnnotation(ctx context.Context, namespace, taskName string, approval approvals.Approval) error {
	if h == nil || h.client == nil || strings.TrimSpace(approval.ID) == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, current); err != nil {
			return err
		}
		base := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[labels.AnnotationApprovalDecidedAt] = time.Now().UTC().Format(time.RFC3339Nano)
		current.Annotations[labels.AnnotationApprovalDecisionID] = approval.ID
		current.Annotations[labels.AnnotationApprovalDecisionStatus] = approval.Status
		if approval.DecisionSeq > 0 {
			current.Annotations[labels.AnnotationApprovalDecisionSeq] = strconv.FormatInt(approval.DecisionSeq, 10)
		}
		return h.client.Patch(ctx, current, client.MergeFrom(base))
	})
}

func (h *Handlers) listTaskApprovalEvents(ctx context.Context, namespace, taskName string) ([]store.ExecutionEvent, error) {
	return newTaskTimelineReader(h.executionEventStore, namespace, taskName).listMatching(ctx, approvals.EventTypes())
}

func approvalDecisionActor(userInfo *UserInfo) string {
	if userInfo == nil {
		return ""
	}
	if strings.TrimSpace(userInfo.Username) != "" {
		return strings.TrimSpace(userInfo.Username)
	}
	if strings.TrimSpace(userInfo.Subject) != "" {
		return strings.TrimSpace(userInfo.Subject)
	}
	return ""
}

func approvalStatusMatchesDecision(status string, decision string) bool {
	return (status == approvals.StatusApproved && decision == approvalDecisionApprove) ||
		(status == approvals.StatusDeclined && decision == approvalDecisionDecline)
}

func findApproval(values []approvals.Approval, id string) (approvals.Approval, bool) {
	for _, approval := range values {
		if approval.ID == id {
			return approval, true
		}
	}
	return approvals.Approval{}, false
}
