/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// SendMessageTool implements inter-agent messaging
type SendMessageTool struct{}

// SendMessageArgs are the arguments for the send_message tool
type SendMessageArgs struct {
	ToTask  string `json:"to_task"`
	Content string `json:"content"`
}

// NewSendMessageTool creates a new send message tool
func NewSendMessageTool() *SendMessageTool {
	return &SendMessageTool{}
}

// Name returns the tool name
func (t *SendMessageTool) Name() string {
	return "send_message"
}

// Description returns the tool description
func (t *SendMessageTool) Description() string {
	return "Send a message to a sibling task (another child of the same parent coordinator). " +
		"Use this to share findings, coordinate work, challenge hypotheses, or avoid duplicated effort. " +
		"Use to_task=\"*\" to broadcast to all siblings. Messages are delivered asynchronously — " +
		"the recipient will see them next time they call check_messages."
}

// Parameters returns the JSON Schema for the tool parameters
func (t *SendMessageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"to_task": {
				"type": "string",
				"description": "Name of the sibling task to message, or \"*\" to broadcast to all siblings"
			},
			"content": {
				"type": "string",
				"description": "Message content to send"
			}
		},
		"required": ["to_task", "content"]
	}`)
}

// Execute sends a message to a sibling task via the controller's internal API
func (t *SendMessageTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a SendMessageArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if a.ToTask == "" || a.Content == "" {
		return "", fmt.Errorf("to_task and content are required")
	}

	taskName := os.Getenv("ORKA_TASK_NAME")
	namespace := os.Getenv("ORKA_TASK_NAMESPACE")
	parentTask := os.Getenv("ORKA_PARENT_TASK")
	controllerURL := strings.TrimRight(os.Getenv("ORKA_CONTROLLER_URL"), "/")

	if controllerURL == "" || taskName == "" || namespace == "" || parentTask == "" {
		return "", fmt.Errorf("messaging requires ORKA_CONTROLLER_URL, ORKA_TASK_NAME, ORKA_TASK_NAMESPACE, and ORKA_PARENT_TASK")
	}

	body, _ := json.Marshal(map[string]string{
		"fromTask":   taskName,
		"toTask":     a.ToTask,
		"parentTask": parentTask,
		"content":    a.Content,
	})

	endpoint := fmt.Sprintf("%s/internal/v1/messages/%s", controllerURL, namespace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	token, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if len(token) > 0 {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		target := a.ToTask
		if target == "*" {
			target = "all siblings"
		}
		return fmt.Sprintf("Message sent to %s", target), nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return "", fmt.Errorf("failed to send message: HTTP %d: %s", resp.StatusCode, string(respBody))
}
