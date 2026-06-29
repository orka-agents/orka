/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

const (
	approvalRequiredErrorType       = "approval_required"
	approvalDeniedErrorType         = "approval_denied"
	approvalUnavailableErrorType    = "approval_unavailable"
	defaultToolApprovalTimeout      = time.Hour
	createPullRequestApprovalAction = "create_pull_request"
)

type toolApprovalRequest struct {
	Action           string
	ToolName         string
	RiskSummary      string
	SafeSummary      string
	Seed             string
	SafeContent      map[string]any
	ApprovalTaskName string
	Timeout          time.Duration
}

type toolApprovalCheck struct {
	Approved bool
	Result   string
}

type approvalRequiredToolResult struct {
	Success    bool   `json:"success"`
	ErrorType  string `json:"errorType"`
	Error      string `json:"error"`
	Suggestion string `json:"suggestion,omitempty"`
	ApprovalID string `json:"approvalID"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Namespace  string `json:"namespace"`
	TaskName   string `json:"taskName"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
}

func requireToolApproval(ctx context.Context, req toolApprovalRequest) (toolApprovalCheck, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		result, err := marshalApprovalRequiredResult(approvalRequiredToolResult{
			Success:    false,
			ErrorType:  approvalUnavailableErrorType,
			Error:      fmt.Sprintf("approval gate for %s requires a tool context", req.Action),
			Suggestion: "Retry from a running Orka task so an approval can be attached to the task event stream.",
			Action:     req.Action,
			Status:     approvalUnavailableErrorType,
		})
		return toolApprovalCheck{Result: result}, err
	}
	namespace := strings.TrimSpace(tc.Namespace)
	if namespace == "" {
		namespace = defaultNamespace
	}
	if tc.ExecutionEventStore == nil {
		result, err := marshalApprovalRequiredResult(approvalRequiredToolResult{
			Success:    false,
			ErrorType:  approvalUnavailableErrorType,
			Error:      fmt.Sprintf("approval gate for %s requires an execution event store", req.Action),
			Suggestion: "Configure an approval-capable task event store, then retry the tool call.",
			Action:     req.Action,
			Status:     approvalUnavailableErrorType,
			Namespace:  namespace,
			TaskName:   strings.TrimSpace(tc.TaskID),
		})
		return toolApprovalCheck{Result: result}, err
	}
	approvalTaskName := strings.TrimSpace(tc.TaskID)
	if approvalTaskName == "" {
		approvalTaskName = strings.TrimSpace(req.ApprovalTaskName)
	}
	if approvalTaskName == "" {
		result, err := marshalApprovalRequiredResult(approvalRequiredToolResult{
			Success:    false,
			ErrorType:  approvalUnavailableErrorType,
			Error:      fmt.Sprintf("approval gate for %s requires a current task context", req.Action),
			Suggestion: "Retry from a running Orka task so an approval can be attached to the task event stream.",
			Action:     req.Action,
			Status:     approvalUnavailableErrorType,
			Namespace:  namespace,
		})
		return toolApprovalCheck{Result: result}, err
	}

	now := time.Now().UTC()
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultToolApprovalTimeout
	}
	expiresAt := now.Add(timeout).UTC()

	listed, err := listApprovalEvents(ctx, tc.ExecutionEventStore, namespace, approvalTaskName)
	if err != nil {
		return toolApprovalCheck{}, err
	}
	derivedApprovals := approvals.Derive(listed, time.Time{})
	approvalID := nextApprovalIDForRequest(listed, derivedApprovals, namespace, approvalTaskName, req)
	if approval, ok := findDerivedApproval(derivedApprovals, approvalID); ok {
		switch approval.Status {
		case approvals.StatusApproved:
			if !approvalRequestMatches(listed, approvalID, req) {
				result, err := marshalApprovalRequiredResult(approvalRequiredToolResult{
					Success:    false,
					ErrorType:  approvalDeniedErrorType,
					Error:      "approved request does not match current tool arguments",
					Suggestion: "Create a fresh approval request for the current tool arguments.",
					ApprovalID: approvalID,
					Action:     req.Action,
					Status:     approvalDeniedErrorType,
					Namespace:  namespace,
					TaskName:   approvalTaskName,
				})
				return toolApprovalCheck{Result: result}, err
			}
			return toolApprovalCheck{Approved: true}, nil
		case approvals.StatusPending:
			result, err := marshalApprovalRequiredResult(approvalResultFromState(approval, namespace, approvalTaskName, approvalRequiredErrorType, fmt.Sprintf("approval is required before %s can run", req.Action)))
			return toolApprovalCheck{Result: result}, err
		case approvals.StatusExpired, approvals.StatusCancelled:
			// Re-issue a fresh deterministic retry approval below.
		default:
			result, err := marshalApprovalRequiredResult(approvalResultFromState(approval, namespace, approvalTaskName, approvalDeniedErrorType, fmt.Sprintf("approval is %s", approval.Status)))
			return toolApprovalCheck{Result: result}, err
		}
	}

	content := map[string]any{
		"approvalID":    approvalID,
		"action":        req.Action,
		"tool":          req.ToolName,
		"toolCallID":    strings.TrimSpace(tc.ToolCallID),
		"riskSummary":   req.RiskSummary,
		"requestDigest": approvalRequestDigest(req),
		"timeout":       timeout.String(),
		"expiresAt":     expiresAt.Format(time.RFC3339),
	}
	maps.Copy(content, req.SafeContent)
	contentBytes, err := json.Marshal(content)
	if err != nil {
		return toolApprovalCheck{}, err
	}
	if len(contentBytes) > events.MaxExecutionEventContentJSONBytes {
		content = map[string]any{
			"approvalID":           approvalID,
			"action":               req.Action,
			"tool":                 req.ToolName,
			"toolCallID":           truncateForkContextTextForApproval(tc.ToolCallID, 1024),
			"riskSummary":          truncateForkContextTextForApproval(req.RiskSummary, 1024),
			"requestDigest":        approvalRequestDigest(req),
			"timeout":              timeout.String(),
			"expiresAt":            expiresAt.Format(time.RFC3339),
			"safeContentTruncated": true,
		}
		contentBytes, err = json.Marshal(content)
		if err != nil {
			return toolApprovalCheck{}, err
		}
	}

	if _, err := tc.ExecutionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   namespace,
		StreamType:  events.ExecutionEventStreamTypeTask,
		StreamID:    approvalTaskName,
		TaskName:    approvalTaskName,
		SessionName: strings.TrimSpace(tc.SessionID),
		ToolName:    req.ToolName,
		ToolCallID:  approvalID,
		Type:        events.ExecutionEventTypeApprovalRequested,
		Severity:    events.ExecutionEventSeverityWarning,
		Summary:     req.SafeSummary,
		Content:     contentBytes,
		CreatedAt:   now,
	}); err != nil {
		return toolApprovalCheck{}, err
	}

	result, err := marshalApprovalRequiredResult(approvalRequiredToolResult{
		Success:    false,
		ErrorType:  approvalRequiredErrorType,
		Error:      fmt.Sprintf("approval is required before %s can run", req.Action),
		Suggestion: "Approve or decline the request through the task approval API, then retry the tool call.",
		ApprovalID: approvalID,
		Action:     req.Action,
		Status:     approvals.StatusPending,
		Namespace:  namespace,
		TaskName:   approvalTaskName,
		ExpiresAt:  expiresAt.Format(time.RFC3339),
	})
	return toolApprovalCheck{Result: result}, err
}

func nextApprovalIDForRequest(
	approvalEvents []store.ExecutionEvent,
	derived []approvals.Approval,
	namespace string,
	taskName string,
	req toolApprovalRequest,
) string {
	matchingIDs := matchingApprovalRequestIDs(approvalEvents, req)
	if len(matchingIDs) == 0 {
		return deterministicApprovalID(namespace, taskName, req.Action, req.Seed)
	}
	latestID := matchingIDs[len(matchingIDs)-1]
	approval, ok := findDerivedApproval(derived, latestID)
	if !ok {
		return latestID
	}
	switch approval.Status {
	case approvals.StatusExpired, approvals.StatusCancelled:
		return deterministicApprovalID(namespace, taskName, req.Action, fmt.Sprintf("%s|retry:%d", req.Seed, len(matchingIDs)))
	default:
		return latestID
	}
}

func matchingApprovalRequestIDs(approvalEvents []store.ExecutionEvent, req toolApprovalRequest) []string {
	ids := []string{}
	for _, event := range approvalEvents {
		if !approvalRequestEventMatches(event, req) {
			continue
		}
		id := store.ApprovalIDFromExecutionEvent(event)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func approvalRequestEventMatches(event store.ExecutionEvent, req toolApprovalRequest) bool {
	if event.Type != events.ExecutionEventTypeApprovalRequested {
		return false
	}
	var content map[string]any
	if err := json.Unmarshal(event.Content, &content); err != nil {
		return false
	}
	if stringField(content, "action") != strings.TrimSpace(req.Action) {
		return false
	}
	if stringField(content, "tool") != strings.TrimSpace(req.ToolName) {
		return false
	}
	return stringField(content, "requestDigest") == approvalRequestDigest(req)
}

func deterministicApprovalID(namespace, taskName, action, seed string) string {
	parts := []string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(taskName),
		strings.TrimSpace(action),
		strings.TrimSpace(seed),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "apr-" + hex.EncodeToString(sum[:])[:20]
}

func truncateForkContextTextForApproval(value string, maxChars int) string {
	runes := []rune(strings.TrimSpace(value))
	if maxChars <= 0 || len(runes) <= maxChars {
		return string(runes)
	}
	return string(runes[:maxChars]) + "…"
}

func approvalRequestDigest(req toolApprovalRequest) string {
	content := map[string]any{
		"action":      strings.TrimSpace(req.Action),
		"tool":        strings.TrimSpace(req.ToolName),
		"riskSummary": strings.TrimSpace(req.RiskSummary),
		"seed":        strings.TrimSpace(req.Seed),
		"safeContent": req.SafeContent,
	}
	data, err := json.Marshal(content)
	if err != nil {
		data = []byte(strings.Join([]string{req.Action, req.ToolName, req.RiskSummary, req.Seed}, "|"))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func approvalRequestMatches(approvalEvents []store.ExecutionEvent, approvalID string, req toolApprovalRequest) bool {
	wantDigest := approvalRequestDigest(req)
	for _, event := range approvalEvents {
		if event.Type != events.ExecutionEventTypeApprovalRequested {
			continue
		}
		var content map[string]any
		if err := json.Unmarshal(event.Content, &content); err != nil {
			continue
		}
		if stringField(content, "approvalID") != approvalID {
			continue
		}
		if stringField(content, "action") != strings.TrimSpace(req.Action) {
			continue
		}
		if stringField(content, "tool") != strings.TrimSpace(req.ToolName) {
			continue
		}
		if stringField(content, "requestDigest") != wantDigest {
			continue
		}
		return true
	}
	return false
}

func stringField(content map[string]any, key string) string {
	value, _ := content[key].(string)
	return strings.TrimSpace(value)
}

func listApprovalEvents(ctx context.Context, eventStore store.ExecutionEventStore, namespace, taskName string) ([]store.ExecutionEvent, error) {
	out := []store.ExecutionEvent{}
	after := int64(0)
	for {
		batch, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  namespace,
			StreamType: events.ExecutionEventStreamTypeTask,
			StreamID:   taskName,
			EventTypes: []string{
				events.ExecutionEventTypeApprovalRequested,
				events.ExecutionEventTypeApprovalApproved,
				events.ExecutionEventTypeApprovalDeclined,
				events.ExecutionEventTypeApprovalExpired,
				events.ExecutionEventTypeApprovalCancelled,
			},
			AfterSeq: after,
			Limit:    store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
		if len(batch) < store.MaxExecutionEventLimit {
			return out, nil
		}
	}
}

func findDerivedApproval(values []approvals.Approval, approvalID string) (approvals.Approval, bool) {
	for _, approval := range values {
		if approval.ID == approvalID {
			return approval, true
		}
	}
	return approvals.Approval{}, false
}

func approvalResultFromState(approval approvals.Approval, namespace, taskName, errorType, message string) approvalRequiredToolResult {
	result := approvalRequiredToolResult{
		Success:    false,
		ErrorType:  errorType,
		Error:      message,
		Suggestion: "Resolve the approval state, then retry only if it is approved.",
		ApprovalID: approval.ID,
		Action:     approval.Action,
		Status:     approval.Status,
		Namespace:  namespace,
		TaskName:   taskName,
	}
	if approval.ExpiresAt != nil {
		result.ExpiresAt = approval.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return result
}

func marshalApprovalRequiredResult(result approvalRequiredToolResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
