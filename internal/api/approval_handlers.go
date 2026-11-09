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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// ListTaskApprovalsResponse is one read of a Task and the approvals filtered
// against it. TaskUID, TaskPhase, and TaskDeleting come from that same read,
// so a client polling the list can tell whether a pending request still
// belongs to a live Task that DecideTaskApproval would accept, without a
// second, separately timed lookup by name.
type ListTaskApprovalsResponse struct {
	Namespace string `json:"namespace"`
	TaskName  string `json:"taskName"`
	TaskUID   string `json:"taskUID,omitempty"`
	TaskPhase string `json:"taskPhase,omitempty"`
	// TaskDeleting is true when the Task has a deletion timestamp; decisions
	// on its requests are refused even while its phase is not terminal.
	TaskDeleting bool                 `json:"taskDeleting,omitempty"`
	Approvals    []approvals.Approval `json:"approvals"`
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
	task, err := h.taskAccess().loadReadable(c, "listTaskApprovals", namespace, taskName)
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
	return c.JSON(ListTaskApprovalsResponse{
		Namespace:    namespace,
		TaskName:     taskName,
		TaskUID:      string(task.UID),
		TaskPhase:    string(task.Status.Phase),
		TaskDeleting: !task.DeletionTimestamp.IsZero(),
		Approvals:    deriveTaskApprovalState(filterTaskApprovalEvents(listed, task)),
	})
}

// DecideTaskApproval handles POST /api/v1/tasks/{id}/approvals/{approvalID}/decision.
//
//nolint:gocyclo // Decision validation, authorization, and replay checks stay together so none can be skipped.
func (h *Handlers) DecideTaskApproval(c fiber.Ctx) error {
	taskName := c.Params("id")
	approvalID, err := url.PathUnescape(c.Params("approvalID"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "approvalID is invalid")
	}
	approvalID = strings.TrimSpace(approvalID)
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
	task, err := h.taskAccess().loadAuthorized(c, "decideTaskApproval", namespace, taskName)
	if err != nil {
		return err
	}
	if err := h.taskAccess().authorizeGatewayTaskOperate(c, "decideTaskApproval", task); err != nil {
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
	switch decision {
	case approvalDecisionApprove, "approved":
		decision = approvalDecisionApprove
	case approvalDecisionDecline, "declined":
		decision = approvalDecisionDecline
	default:
		return fiber.NewError(fiber.StatusBadRequest, "decision must be approve or decline")
	}
	decided, err := h.decideApproval(c.Context(), GetUserInfo(c), task, approvalID, decision, req.Reason, false)
	if err != nil {
		return err
	}
	return c.JSON(decided)
}

// DecideTaskApprovalAs decides, for a person signed in with OIDC or a context
// token, a permission request an agent runtime is holding for them as the
// requester of the Task. It is the in-process form of the decision route for
// callers inside the API server and applies the same checks for those
// identities; it decides no other kind of approval.
func (h *Handlers) DecideTaskApprovalAs(ctx context.Context, ui *UserInfo, namespace, taskName, approvalID string, approve bool) error {
	if ui == nil || (ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken) {
		return fiber.NewError(fiber.StatusForbidden, "a verified OIDC or context-token identity is required")
	}
	approvalID = strings.TrimSpace(approvalID)
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(taskName) == "" || approvalID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, task and approval are required")
	}
	if h.watchNamespace != "" && namespace != h.watchNamespace {
		return fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}
	if h.enforceNamespaceIsolation && (ui.Namespace == "" || namespace != ui.Namespace) {
		return fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	token := contextTokenFromUserInfo(ui)
	contextTokenChecks := h.contextTokenAuthorization.Enabled() && token != nil
	if contextTokenChecks {
		var failures []string
		if !hasAnyScope(token.Scopes, h.contextTokenAuthorization.TaskUpdateScopes) {
			failures = append(failures, fmt.Sprintf("missing one of required scopes %q",
				strings.Join(h.contextTokenAuthorization.TaskUpdateScopes, ",")))
		}
		if want, ok := contextString(token.TransactionContext, "namespace"); ok && want != namespace {
			failures = append(failures, fmt.Sprintf("namespace %q does not match token context %q", namespace, want))
		}
		if len(failures) > 0 {
			if err := h.handleContextTokenAuthorizationFailures(token, "decideTaskApproval", failures); err != nil {
				return err
			}
		}
	}
	task := &corev1alpha1.Task{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if contextTokenChecks {
		failures, err := h.contextTokenLoadedTaskContextFailures(ctx, token, task, true)
		if err != nil {
			return err
		}
		if len(failures) > 0 {
			if err := h.handleContextTokenAuthorizationFailures(token, "decideTaskApproval", failures); err != nil {
				return err
			}
		}
	}
	// A gateway's Task is operated through the gateway's own authorization,
	// which needs a request; nothing here holds permissions for it.
	if _, gatewayOwned, err := gatewayTaskIdentity(ctx, h.gatewayEventStore, task); err != nil {
		return err
	} else if gatewayOwned {
		return fiber.NewError(fiber.StatusForbidden, "gateway tasks are decided through the decision route")
	}
	decision := approvalDecisionDecline
	if approve {
		decision = approvalDecisionApprove
	}
	_, err := h.decideApproval(ctx, ui, task, approvalID, decision, "", true)
	return err
}

// decideApproval records a decision on one of the Task's approvals for an
// authorized caller and returns the approval as stored. A permission request
// held for the Task's requester may be decided only by that requester.
func (h *Handlers) decideApproval(
	ctx context.Context,
	ui *UserInfo,
	task *corev1alpha1.Task,
	approvalID, decision, reason string,
	permissionsOnly bool,
) (approvals.Approval, error) {
	namespace, taskName := task.Namespace, task.Name
	eventType := events.ExecutionEventTypeApprovalDeclined
	if decision == approvalDecisionApprove {
		eventType = events.ExecutionEventTypeApprovalApproved
	}
	listed, err := h.listTaskApprovalEvents(ctx, namespace, taskName)
	if err != nil {
		return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", err))
	}
	listed = filterTaskApprovalEvents(listed, task)
	current, found := findApproval(deriveTaskApprovalState(listed), approvalID)
	if !found {
		return approvals.Approval{}, fiber.NewError(fiber.StatusNotFound, "approval not found")
	}
	if current.Kind == approvals.KindPermission {
		if !requestedByCaller(task, ui) {
			return approvals.Approval{}, fiber.NewError(fiber.StatusForbidden, "only the task's requester may decide this permission request")
		}
	} else if permissionsOnly {
		return approvals.Approval{}, fiber.NewError(fiber.StatusForbidden, "only permission requests can be decided here")
	}
	if current.Binding != nil {
		if err := validateV2ApprovalTask(current, task); err != nil {
			return approvals.Approval{}, err
		}
	}
	if current.Status != approvals.StatusPending {
		if approvalStatusMatchesDecision(current.Status, decision) {
			if err := h.patchTaskApprovalDecisionAnnotation(ctx, namespace, taskName, current); err != nil {
				return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to nudge task after approval decision: %v", err))
			}
			return current, nil
		}
		return approvals.Approval{}, fiber.NewError(fiber.StatusConflict, fmt.Sprintf("approval is already %s", current.Status))
	}
	if !task.DeletionTimestamp.IsZero() {
		return approvals.Approval{}, fiber.NewError(fiber.StatusGone, "task is deleting")
	}
	if isTerminalInternalTaskPhase(task.Status.Phase) {
		return approvals.Approval{}, fiber.NewError(fiber.StatusConflict, "task is complete")
	}

	sessionName, err := h.existingSessionNameForTask(ctx, namespace, task)
	if err != nil {
		return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}
	content, err := json.Marshal(map[string]string{
		"approvalID": approvalID,
		"decision":   decision,
		"reason":     strings.TrimSpace(reason),
		"actor":      approvalDecisionActor(ui),
		"taskUID":    string(task.UID),
	})
	if err != nil {
		return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode approval decision: %v", err))
	}
	appended, err := h.executionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
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
			refreshed, listErr := h.listTaskApprovalEvents(ctx, namespace, taskName)
			if listErr != nil {
				return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list execution events: %v", listErr))
			}
			refreshed = filterTaskApprovalEvents(refreshed, task)
			if current, found := findApproval(deriveTaskApprovalState(refreshed), approvalID); found {
				if approvalStatusMatchesDecision(current.Status, decision) {
					if patchErr := h.patchTaskApprovalDecisionAnnotation(ctx, namespace, taskName, current); patchErr != nil {
						return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to nudge task after approval decision: %v", patchErr))
					}
					return current, nil
				}
				return approvals.Approval{}, fiber.NewError(fiber.StatusConflict, fmt.Sprintf("approval is already %s", current.Status))
			}
		}
		return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to append approval decision: %v", err))
	}
	approvals.RecordDecision(current, appended)
	listed = append(listed, *appended)
	updated, _ := findApproval(deriveTaskApprovalState(listed), approvalID)
	if err := h.patchTaskApprovalDecisionAnnotation(ctx, namespace, taskName, updated); err != nil {
		return approvals.Approval{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to nudge task after approval decision: %v", err))
	}
	return updated, nil
}

// requestedByCaller reports whether ui is the Task's verified requester.
func requestedByCaller(task *corev1alpha1.Task, ui *UserInfo) bool {
	if task == nil || ui == nil || task.Spec.RequestedBy == nil ||
		task.Annotations[labels.AnnotationRequestedBySource] != labels.RequestedBySourceAPI ||
		(ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken) {
		return false
	}
	requester := task.Spec.RequestedBy
	return strings.TrimSpace(requester.Issuer) != "" && strings.TrimSpace(requester.Subject) != "" &&
		requester.Issuer == ui.Issuer && requester.Subject == ui.Subject
}

func filterTaskApprovalEvents(input []store.ExecutionEvent, task *corev1alpha1.Task) []store.ExecutionEvent {
	if task == nil {
		return input
	}
	return approvals.FilterEventsForTaskUID(input, string(task.UID))
}

func deriveTaskApprovalState(input []store.ExecutionEvent) []approvals.Approval {
	// Preserve AI-worker parking semantics. Only broker-owned v2 reviews have
	// an enforced wait deadline, including when their original runtime is lost.
	values := approvals.Derive(input, time.Time{})
	now := time.Now().UTC()
	for i := range values {
		if values[i].Binding != nil && values[i].Status == approvals.StatusPending && values[i].ExpiresAt != nil && !now.Before(*values[i].ExpiresAt) {
			values[i].Status = approvals.StatusExpired
		}
	}
	return values
}

func validateV2ApprovalTask(approval approvals.Approval, task *corev1alpha1.Task) error {
	binding := approval.Binding
	execution := task.Status.Execution
	if approval.TaskUID == "" || approval.TaskUID != string(task.UID) || execution == nil ||
		execution.Attempt != int32(binding.TaskAttempt) || execution.PromptID != binding.PromptID ||
		execution.RuntimeSessionUID != binding.RuntimeSessionUID || execution.RuntimeSessionGeneration != int64(binding.RuntimeSessionGeneration) ||
		store.CanonicalBytesDigest([]byte(execution.RuntimeInstanceID)) != binding.RuntimeInstanceIDDigest ||
		store.CanonicalBytesDigest([]byte(execution.RuntimeSessionSupervisorBootID)) != binding.SupervisorBootIDDigest ||
		execution.ControllerEpoch != int64(binding.ControllerEpoch) ||
		(execution.State != corev1alpha1.TaskExecutionStateAccepted && execution.State != corev1alpha1.TaskExecutionStateRunning) {
		return fiber.NewError(fiber.StatusConflict, "approval belongs to a task run that is no longer active")
	}
	return nil
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
		if approval.TaskUID != "" && approval.TaskUID != string(current.UID) {
			return errors.New("task identity changed after approval decision")
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
