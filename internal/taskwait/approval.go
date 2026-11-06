/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwait

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// ParkedApprovalAction is the approval action shown to reviewers. A parked
	// approval is a sign-off checkpoint; it never authorizes a gated tool call.
	ParkedApprovalAction = "Agent sign-off (parked turn)"
	// ParkedApprovalToolName attributes the approval to the parking tool.
	ParkedApprovalToolName = "park_until"
	parkedApprovalIDKind   = "acp-parked-approval-v1"
)

// ParkedApprovalID is the stable approval ID for the parked approval one exact
// brokered tool call requested.
func ParkedApprovalID(namespace, taskUID string, attempt int32, promptID, operationDigest string) string {
	return store.CanonicalControlID(parkedApprovalIDKind, namespace, taskUID, fmt.Sprint(attempt), promptID, operationDigest)
}

// parkedApprovalPayload is the ApprovalRequested content consumed by
// approvals.Derive. It carries no binding: the decision is delivered to a
// later turn, which revalidates every policy itself.
type parkedApprovalPayload struct {
	ApprovalID  string `json:"approvalID"`
	Action      string `json:"action"`
	RiskSummary string `json:"riskSummary"`
	TaskUID     string `json:"taskUID"`
	Severity    string `json:"severity"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	Parked      bool   `json:"parked"`
}

// ParkedApprovalRequestEvent builds the idempotent ApprovalRequested event for
// a parked approval wait.
func ParkedApprovalRequestEvent(task *corev1alpha1.Task, wait *corev1alpha1.TaskWaitStatus) (*store.ExecutionEvent, string, error) {
	if task == nil || wait == nil || wait.Kind != corev1alpha1.TaskWaitKindApproval || len(wait.References) != 1 {
		return nil, "", fmt.Errorf("parked approval requires one approval reference")
	}
	approvalID := wait.References[0]
	content, err := json.Marshal(parkedApprovalPayload{
		ApprovalID:  approvalID,
		Action:      ParkedApprovalAction,
		RiskSummary: wait.Reason,
		TaskUID:     string(task.UID),
		Severity:    "info",
		ExpiresAt:   wait.Deadline.UTC().Format(time.RFC3339),
		Parked:      true,
	})
	if err != nil {
		return nil, "", err
	}
	return &store.ExecutionEvent{
		Namespace:   task.Namespace,
		StreamType:  events.ExecutionEventStreamTypeTask,
		StreamID:    task.Name,
		TaskName:    task.Name,
		SessionName: sessionName(task),
		Type:        events.ExecutionEventTypeApprovalRequested,
		Severity:    events.ExecutionEventSeverityInfo,
		ToolName:    ParkedApprovalToolName,
		ToolCallID:  approvalID,
		Summary:     "Agent requested sign-off and parked its turn",
		Content:     content,
	}, approvalID, nil
}

// EnsureParkedApprovalRequested appends the parked approval request once.
func EnsureParkedApprovalRequested(
	ctx context.Context,
	eventStore store.DeduplicatingExecutionEventStore,
	task *corev1alpha1.Task,
	wait *corev1alpha1.TaskWaitStatus,
) error {
	if eventStore == nil {
		return fmt.Errorf("parked approvals require execution event storage")
	}
	event, approvalID, err := ParkedApprovalRequestEvent(task, wait)
	if err != nil {
		return err
	}
	_, _, err = eventStore.AppendExecutionEventIfAbsent(ctx, event, approvalID)
	return err
}

// ParkedApprovalTerminalEvent builds the terminal event that closes a parked
// approval which was never decided: expired when the wait deadline passed, or
// cancelled otherwise.
func ParkedApprovalTerminalEvent(
	task *corev1alpha1.Task,
	wait *corev1alpha1.TaskWaitStatus,
	expired bool,
	reason string,
) (*store.ExecutionEvent, string, error) {
	if task == nil || wait == nil || wait.Kind != corev1alpha1.TaskWaitKindApproval || len(wait.References) != 1 {
		return nil, "", fmt.Errorf("parked approval requires one approval reference")
	}
	approvalID := wait.References[0]
	eventType := events.ExecutionEventTypeApprovalCancelled
	summary := "approval cancelled"
	if expired {
		eventType = events.ExecutionEventTypeApprovalExpired
		summary = "approval expired"
	}
	content, err := json.Marshal(map[string]string{
		"approvalID": approvalID,
		"reason":     reason,
		"actor":      "orka-controller",
		"taskUID":    string(task.UID),
	})
	if err != nil {
		return nil, "", err
	}
	return &store.ExecutionEvent{
		Namespace:   task.Namespace,
		StreamType:  events.ExecutionEventStreamTypeTask,
		StreamID:    task.Name,
		TaskName:    task.Name,
		SessionName: sessionName(task),
		Type:        eventType,
		Severity:    events.ExecutionEventSeverityInfo,
		ToolName:    ParkedApprovalToolName,
		ToolCallID:  approvalID,
		Summary:     summary,
		Content:     content,
	}, approvalID + ":terminal", nil
}

// AcceptsApprovalDecision reports whether task is parked on approvalID, so a
// human decision is accepted even though the parked turn itself has ended.
func AcceptsApprovalDecision(task *corev1alpha1.Task, approvalID string) bool {
	if task == nil || !task.DeletionTimestamp.IsZero() {
		return false
	}
	wait := task.Status.Wait
	if wait == nil || wait.Kind != corev1alpha1.TaskWaitKindApproval || len(wait.References) != 1 ||
		strings.TrimSpace(approvalID) == "" || wait.References[0] != approvalID {
		return false
	}
	switch wait.State {
	case corev1alpha1.TaskWaitStateRequested, corev1alpha1.TaskWaitStateWaiting:
		return true
	default:
		return false
	}
}

func sessionName(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.SessionRef == nil {
		return ""
	}
	return strings.TrimSpace(task.Spec.SessionRef.Name)
}
