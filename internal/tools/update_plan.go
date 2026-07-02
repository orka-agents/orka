/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/orka-agents/orka/internal/workerenv"
)

// UpdatePlanTool allows the LLM to update the autonomous plan state.
type UpdatePlanTool struct{}

// NewUpdatePlanTool creates a new UpdatePlanTool.
func NewUpdatePlanTool() *UpdatePlanTool {
	return &UpdatePlanTool{}
}

// Name returns the tool name.
func (t *UpdatePlanTool) Name() string { return updatePlanToolName }

// Description returns the tool description for the LLM.
func (t *UpdatePlanTool) Description() string {
	return "Update the autonomous execution plan. Call this to save progress, track completed phases, and signal when the goal is complete. Must be called at least once per iteration."
}

// Parameters returns the JSON Schema for the tool parameters.
func (t *UpdatePlanTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {
				"type": "string",
				"description": "Brief human-readable summary of current progress (1-2 sentences)"
			},
			"progress_pct": {
				"type": "integer",
				"description": "Estimated progress percentage (0-100)",
				"minimum": 0,
				"maximum": 100
			},
			"goal_complete": {
				"type": "boolean",
				"description": "Set to true when the overall goal has been fully achieved or cannot be progressed further"
			},
			"plan_document": {
				"type": "string",
				"description": "Full markdown plan document. Include completed phases, current work, and remaining tasks. This replaces the previous plan document entirely."
			}
		},
		"required": ["summary", "plan_document"]
	}`)
}

// updatePlanArgs are the arguments for the update_plan tool.
type updatePlanArgs struct {
	Summary      string `json:"summary"`
	ProgressPct  int    `json:"progress_pct"`
	GoalComplete bool   `json:"goal_complete"`
	PlanDocument string `json:"plan_document"`
}

// Execute saves the plan state via the controller's internal API.
func (t *UpdatePlanTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a updatePlanArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if a.Summary == "" {
		return "", fmt.Errorf("summary is required")
	}
	if a.PlanDocument == "" {
		return "", fmt.Errorf("plan_document is required")
	}

	controllerURL := os.Getenv(envOrkaControllerURL)
	taskName := os.Getenv(envOrkaTaskName)
	taskNamespace := os.Getenv(envOrkaTaskNamespace)
	saToken := os.Getenv(workerenv.ServiceAccountToken)

	if controllerURL == "" || taskName == "" || taskNamespace == "" {
		return "", errors.New(missingControllerTaskEnvMessage)
	}

	// Read SA token from file if not in env
	if saToken == "" {
		data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err == nil {
			saToken = string(data)
		}
	}

	// Build request payload
	payload, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("failed to marshal plan: %w", err)
	}

	url := fmt.Sprintf("%s/internal/v1/plans/%s/%s", controllerURL, taskNamespace, taskName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to save plan: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to save plan: HTTP %d", resp.StatusCode)
	}

	result := fmt.Sprintf("Plan updated: %s (progress: %d%%", a.Summary, a.ProgressPct)
	if a.GoalComplete {
		result += ", goal marked as COMPLETE"
	}
	result += ")"

	return result, nil
}
