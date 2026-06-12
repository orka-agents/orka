package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
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
	unknownApprovalActor    = "unknown"
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
	if err := h.ensureTaskReadable(c, namespace, taskName, "listTaskApprovals"); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	listed, err := h.listTaskApprovalEvents(c.Context(), namespace, taskName)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	return c.JSON(ListTaskApprovalsResponse{Namespace: namespace, TaskName: taskName, Approvals: approvals.Derive(listed, time.Now().UTC())})
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
	task := &corev1alpha1.Task{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Name: taskName, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "decideTaskApproval", task); err != nil {
		return err
	}
	sessionName := sessionNameFromTask(task)
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

	h.approvalDecisionMu.Lock()
	defer h.approvalDecisionMu.Unlock()

	current, found, err := h.getTaskApproval(c.Context(), namespace, taskName, approvalID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	if !found {
		return fiber.NewError(fiber.StatusNotFound, "approval not found")
	}
	if current.Status != approvals.StatusPending {
		if approvalStatusMatchesDecision(current.Status, decision) {
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

	content, err := json.Marshal(map[string]string{
		"approvalID": approvalID,
		"decision":   decision,
		"reason":     strings.TrimSpace(req.Reason),
		"actor":      approvalDecisionActor(GetUserInfo(c)),
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
		ToolCallID:  current.ToolCallID,
		Summary:     fmt.Sprintf("approval %s", decision),
		Content:     content,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			current, found, listErr := h.getTaskApproval(c.Context(), namespace, taskName, approvalID)
			if listErr != nil {
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", listErr))
			}
			if found {
				if approvalStatusMatchesDecision(current.Status, decision) {
					return c.JSON(current)
				}
				return fiber.NewError(fiber.StatusConflict, fmt.Sprintf("approval is already %s", current.Status))
			}
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append approval decision: %v", err))
	}
	updated, found, err := h.getTaskApproval(c.Context(), namespace, taskName, approvalID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	if !found {
		return c.JSON(approvals.Approval{
			ID:           approvalID,
			Status:       statusForApprovalDecision(decision),
			DecisionSeq:  appended.Seq,
			DecisionTime: &appended.CreatedAt,
		})
	}
	return c.JSON(updated)
}

func (h *Handlers) getTaskApproval(ctx context.Context, namespace, taskName, approvalID string) (approvals.Approval, bool, error) {
	listed, err := h.listTaskApprovalEvents(ctx, namespace, taskName)
	if err != nil {
		return approvals.Approval{}, false, err
	}
	current, found := findApproval(approvals.Derive(listed, time.Now().UTC()), approvalID)
	return current, found, nil
}

func approvalDecisionActor(userInfo *UserInfo) string {
	if userInfo == nil {
		return unknownApprovalActor
	}
	for _, value := range []string{userInfo.Username, userInfo.Subject, userInfo.Email} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return unknownApprovalActor
}

func approvalStatusMatchesDecision(status, decision string) bool {
	switch decision {
	case approvalDecisionApprove:
		return status == approvals.StatusApproved
	case approvalDecisionDecline:
		return status == approvals.StatusDeclined
	default:
		return false
	}
}

func statusForApprovalDecision(decision string) string {
	switch decision {
	case approvalDecisionApprove:
		return approvals.StatusApproved
	case approvalDecisionDecline:
		return approvals.StatusDeclined
	default:
		return approvals.StatusPending
	}
}

func (h *Handlers) listTaskApprovalEvents(ctx context.Context, namespace, taskName string) ([]store.ExecutionEvent, error) {
	eventTypes := []string{
		events.ExecutionEventTypeApprovalRequested,
		events.ExecutionEventTypeApprovalApproved,
		events.ExecutionEventTypeApprovalDeclined,
		events.ExecutionEventTypeApprovalExpired,
		events.ExecutionEventTypeApprovalCancelled,
	}
	out := []store.ExecutionEvent{}
	after := int64(0)
	for {
		batch, err := h.executionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  namespace,
			StreamType: events.ExecutionEventStreamTypeTask,
			StreamID:   taskName,
			EventTypes: eventTypes,
			AfterSeq:   after,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
		if len(batch) < store.MaxExecutionEventLimit {
			break
		}
	}
	return out, nil
}

func findApproval(values []approvals.Approval, id string) (approvals.Approval, bool) {
	for _, approval := range values {
		if approval.ID == id {
			return approval, true
		}
	}
	return approvals.Approval{}, false
}

func sessionNameFromTask(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.SessionRef == nil {
		return ""
	}
	return strings.TrimSpace(task.Spec.SessionRef.Name)
}
