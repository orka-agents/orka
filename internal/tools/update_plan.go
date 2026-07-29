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
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	updatePlanRequestTimeout         = 10 * time.Second
	updatePlanResponseHeaderTimeout  = 10 * time.Second
	updatePlanResponseBodyTimeout    = 100 * time.Millisecond
	updatePlanErrorBodyLimit         = 4 << 10
	updatePlanResponseBodyDrainLimit = 64 << 10
)

// UpdatePlanTool allows the LLM to update the autonomous plan state.
type UpdatePlanTool struct {
	client              *http.Client
	requestTimeout      time.Duration
	responseBodyTimeout time.Duration
}

// NewUpdatePlanTool creates a new UpdatePlanTool.
func NewUpdatePlanTool() *UpdatePlanTool {
	return &UpdatePlanTool{
		client:              newUpdatePlanHTTPClient(updatePlanResponseHeaderTimeout),
		requestTimeout:      updatePlanRequestTimeout,
		responseBodyTimeout: updatePlanResponseBodyTimeout,
	}
}

// newUpdatePlanHTTPClient preserves the standard dial, TLS, and idle-connection
// safeguards while adding a bounded wait for controller response headers.
func newUpdatePlanHTTPClient(responseHeaderTimeout time.Duration) *http.Client {
	transport := cloneDefaultUpdatePlanTransport()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: transport}
}

func cloneDefaultUpdatePlanTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}

	const defaultDialTimeout = 30 * time.Second
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: defaultDialTimeout}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
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

	requestTimeout := t.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = updatePlanRequestTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/internal/v1/plans/%s/%s", controllerURL, taskNamespace, taskName)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if saToken != "" {
		req.Header.Set("Authorization", "Bearer "+saToken)
	}

	client := t.client
	if client == nil {
		client = newUpdatePlanHTTPClient(updatePlanResponseHeaderTimeout)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to save plan: %w", err)
	}
	if resp.Body == nil {
		return "", fmt.Errorf("failed to save plan: controller returned an empty response body handle")
	}

	responseBodyTimeout := t.responseBodyTimeout
	if responseBodyTimeout <= 0 {
		responseBodyTimeout = updatePlanResponseBodyTimeout
	}
	bodyTimer := time.AfterFunc(responseBodyTimeout, cancel)
	defer func() {
		bodyTimer.Stop()
		cancel()
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		preview, consumed, readErr := readUpdatePlanErrorBody(resp.Body)
		drainUpdatePlanResponseBody(resp.Body, updatePlanResponseBodyDrainLimit-consumed)
		if preview != "" {
			return "", fmt.Errorf("failed to save plan: HTTP %d: %s", resp.StatusCode, preview)
		}
		if readErr != nil {
			return "", fmt.Errorf("failed to save plan: HTTP %d: read error response: %w", resp.StatusCode, readErr)
		}
		return "", fmt.Errorf("failed to save plan: HTTP %d", resp.StatusCode)
	}
	drainUpdatePlanResponseBody(resp.Body, updatePlanResponseBodyDrainLimit)

	result := fmt.Sprintf("Plan updated: %s (progress: %d%%", a.Summary, a.ProgressPct)
	if a.GoalComplete {
		result += ", goal marked as COMPLETE"
	}
	result += ")"

	return result, nil
}

func readUpdatePlanErrorBody(body io.Reader) (string, int64, error) {
	limited := &io.LimitedReader{R: body, N: updatePlanErrorBodyLimit + 1}
	data, err := io.ReadAll(limited)
	consumed := int64(len(data))
	truncated := len(data) > updatePlanErrorBodyLimit
	if truncated {
		data = data[:updatePlanErrorBodyLimit]
	}
	preview := strings.TrimSpace(string(data))
	if truncated {
		preview += " [truncated]"
	}
	return preview, consumed, err
}

func drainUpdatePlanResponseBody(body io.Reader, limit int64) {
	if limit <= 0 {
		return
	}
	_, _ = io.CopyN(io.Discard, body, limit)
}
