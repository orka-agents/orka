/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// CheckMessagesTool implements checking for inter-agent messages
type CheckMessagesTool struct{}

// CheckMessagesArgs are the arguments for the check_messages tool
type CheckMessagesArgs struct {
	MarkRead *bool `json:"mark_read,omitempty"`
}

// NewCheckMessagesTool creates a new check messages tool
func NewCheckMessagesTool() *CheckMessagesTool {
	return &CheckMessagesTool{}
}

// Name returns the tool name
func (t *CheckMessagesTool) Name() string {
	return checkMessagesToolName
}

// Description returns the tool description
func (t *CheckMessagesTool) Description() string {
	return "Check for messages from sibling tasks. Returns all unread messages sent to you or broadcast to all siblings. " +
		"Call this periodically during long-running tasks to stay coordinated with your siblings. " +
		"Messages are marked as read by default so you won't see them again."
}

// Parameters returns the JSON Schema for the tool parameters
func (t *CheckMessagesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"mark_read": {
				"type": "boolean",
				"description": "Whether to mark returned messages as read (default: true)"
			}
		}
	}`)
}

// Execute checks for messages from sibling tasks via the controller's internal API
func (t *CheckMessagesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a CheckMessagesArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("failed to parse arguments: %w", err)
		}
	}

	markRead := trueStr
	if a.MarkRead != nil && !*a.MarkRead {
		markRead = falseStr
	}
	if toolCtx := GetToolContext(ctx); toolCtx != nil && toolCtx.MessageStore != nil {
		taskName := strings.TrimSpace(toolCtx.TaskID)
		namespace := strings.TrimSpace(toolCtx.Namespace)
		parentTask := strings.TrimSpace(toolCtx.ParentTaskID)
		if taskName == "" || namespace == "" || parentTask == "" {
			return "", fmt.Errorf("messaging requires task, namespace, and parent task context")
		}
		messages, err := toolCtx.MessageStore.GetMessages(ctx, namespace, taskName, parentTask, markRead == trueStr)
		if err != nil {
			return "", fmt.Errorf("failed to check messages: %w", err)
		}
		if len(messages) == 0 {
			return noNewMessagesText, nil
		}
		body, err := json.Marshal(messages)
		if err != nil {
			return "", fmt.Errorf("failed to marshal messages: %w", err)
		}
		return string(body), nil
	}

	taskName := os.Getenv(envOrkaTaskName)
	namespace := os.Getenv(envOrkaTaskNamespace)
	parentTask := os.Getenv(envOrkaParentTask)
	controllerURL := strings.TrimRight(os.Getenv(envOrkaControllerURL), "/")

	if controllerURL == "" || taskName == "" || namespace == "" || parentTask == "" {
		return "", fmt.Errorf("messaging requires ORKA_CONTROLLER_URL, ORKA_TASK_NAME, ORKA_TASK_NAMESPACE, and ORKA_PARENT_TASK")
	}

	endpoint := fmt.Sprintf("%s/internal/v1/messages/%s/%s?parentTask=%s&markRead=%s",
		controllerURL, namespace, taskName, parentTask, markRead)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	token, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to check messages: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("failed to check messages: HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse to count messages
	var messages []json.RawMessage
	if err := json.Unmarshal(body, &messages); err != nil {
		return string(body), nil
	}

	if len(messages) == 0 {
		return noNewMessagesText, nil
	}

	return string(body), nil
}
