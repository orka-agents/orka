/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"unicode/utf8"
)

// MaxCancelReasonLength bounds the cancel_task reason in characters; the
// reason becomes part of the child's terminal message.
const MaxCancelReasonLength = 512

// CancelTaskTool implements task cancellation for coordination workflows.
// Workers cannot write Task status, so the controller performs the
// cancellation after verifying that the target is a child of this Task.
type CancelTaskTool struct{}

// CancelTaskArgs are the arguments for the cancel_task tool
type CancelTaskArgs struct {
	TaskName  string `json:"task_name"`
	Namespace string `json:"namespace,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// CancelTaskResult represents the cancellation result
type CancelTaskResult struct {
	TaskName string `json:"taskName"`
	Status   string `json:"status"`
}

// NewCancelTaskTool creates a new cancel task tool
func NewCancelTaskTool() *CancelTaskTool {
	return &CancelTaskTool{}
}

// Name returns the tool name
func (t *CancelTaskTool) Name() string {
	return cancelTaskToolName
}

// Description returns the tool description
func (t *CancelTaskTool) Description() string {
	return "Cancel a pending or running child task. The controller then terminates its Job and marks it Cancelled. " +
		"Use this to stop a task that is going down the wrong path, taking too long, or is no longer needed. " +
		"You can only cancel tasks that are children of the current task, and not tasks running on an agent runtime."
}

// Parameters returns the JSON Schema for the tool parameters
func (t *CancelTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_name": {
				"type": "string",
				"description": "Name of the child task to cancel"
			},
			"namespace": {
				"type": "string",
				"description": "Namespace of the task (defaults to current namespace)"
			},
			"reason": {
				"type": "string",
				"description": "Reason for cancellation (optional, included in task status message)",
				"maxLength": 512
			}
		},
		"required": ["task_name"]
	}`)
}

// Execute asks the controller to cancel a running child task
func (t *CancelTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a CancelTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if a.TaskName == "" {
		return "", fmt.Errorf("task_name is required")
	}
	if utf8.RuneCountInString(a.Reason) > MaxCancelReasonLength {
		return "", fmt.Errorf("reason must be at most %d characters", MaxCancelReasonLength)
	}

	cfg, err := loadInternalControllerConfig()
	if err != nil {
		return "", err
	}
	// Children live in their parent's namespace.
	if a.Namespace != "" && a.Namespace != cfg.Namespace {
		return "", fmt.Errorf("task %q is not a child of the current task %q", a.TaskName, cfg.TaskName)
	}

	payload, err := json.Marshal(map[string]string{"reason": a.Reason})
	if err != nil {
		return "", fmt.Errorf("failed to encode cancellation: %w", err)
	}
	endpoint := cfg.url(fmt.Sprintf("/internal/v1/tasks/%s/%s/children/%s/cancel",
		url.PathEscape(cfg.Namespace), url.PathEscape(cfg.TaskName), url.PathEscape(a.TaskName)), nil)
	body, err := doInternalControllerRequest(ctx, cfg, http.MethodPost, endpoint, payload)
	if err != nil {
		return "", fmt.Errorf("failed to cancel task %q: %w", a.TaskName, err)
	}
	return body, nil
}
