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
	ParkedApprovalAction   = "Agent sign-off (parked turn)"
	ParkedApprovalToolName = "park_until"
	parkedApprovalIDKind   = "acp-parked-approval-v1"
)

func ParkedApprovalID(namespace, taskUID string, attempt int32, promptID, operationDigest string) string {
	return store.CanonicalControlID(parkedApprovalIDKind, namespace, taskUID, fmt.Sprint(attempt), promptID, operationDigest)
}

type parkedApprovalPayload struct {
	ApprovalID  string `json:"approvalID"`
	Action      string `json:"action"`
	RiskSummary string `json:"riskSummary"`
	TaskUID     string `json:"taskUID"`
	Severity    string `json:"severity"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	Parked      bool   `json:"parked"`
}

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
	case corev1alpha1.TaskWaitStateRequested:
		return task.Status.Phase == corev1alpha1.TaskPhaseSucceeded
	case corev1alpha1.TaskWaitStateWaiting:
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
