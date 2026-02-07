/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// WaitForTasksTool implements waiting for child tasks to complete
type WaitForTasksTool struct {
	k8sClient client.Client
}

// WaitForTasksArgs are the arguments for the wait_for_tasks tool
type WaitForTasksArgs struct {
	Tasks   []string `json:"tasks"`
	Timeout string   `json:"timeout,omitempty"`
}

// WaitForTasksResult represents the aggregated result
type WaitForTasksResult struct {
	Completed bool             `json:"completed"`
	Results   []TaskResultInfo `json:"results"`
}

// TaskResultInfo holds individual task result information
type TaskResultInfo struct {
	Task   string `json:"task"`
	Agent  string `json:"agent,omitempty"`
	Phase  string `json:"phase"`
	Result string `json:"result,omitempty"`
}

// NewWaitForTasksTool creates a new wait_for_tasks tool
func NewWaitForTasksTool(k8sClient client.Client) *WaitForTasksTool {
	return &WaitForTasksTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name
func (t *WaitForTasksTool) Name() string {
	return "wait_for_tasks"
}

// Description returns the tool description
func (t *WaitForTasksTool) Description() string {
	return "Wait for one or more child tasks to complete and return their results. Use after delegating tasks to check completion status."
}

// Parameters returns the JSON Schema for parameters
func (t *WaitForTasksTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Child task names to wait for"
			},
			"timeout": {
				"type": "string",
				"description": "Max wait duration, e.g. '5m' (default: '10m')"
			}
		},
		"required": ["tasks"]
	}`)
}

// Execute waits for the specified tasks to complete and returns their results
func (t *WaitForTasksTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var waitArgs WaitForTasksArgs
	if err := json.Unmarshal(args, &waitArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if len(waitArgs.Tasks) == 0 {
		return "", fmt.Errorf("at least one task name is required")
	}

	// Parse timeout
	timeoutStr := waitArgs.Timeout
	if timeoutStr == "" {
		timeoutStr = "10m"
	}
	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return "", fmt.Errorf("invalid timeout %q: %w", timeoutStr, err)
	}

	ns := os.Getenv("MERCAN_TASK_NAMESPACE")
	if ns == "" {
		return "", fmt.Errorf("MERCAN_TASK_NAMESPACE environment variable is not set")
	}

	deadline := time.Now().Add(timeout)
	pollInterval := 5 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	results := make(map[string]*TaskResultInfo)
	for _, taskName := range waitArgs.Tasks {
		results[taskName] = &TaskResultInfo{
			Task:  taskName,
			Phase: "Unknown",
		}
	}

	allTerminal := false
	for {
		allTerminal = true
		for _, taskName := range waitArgs.Tasks {
			var task corev1alpha1.Task
			err := t.k8sClient.Get(ctx, types.NamespacedName{Name: taskName, Namespace: ns}, &task)
			if err != nil {
				results[taskName].Phase = "Error"
				results[taskName].Result = fmt.Sprintf("error: %v", err)
				continue
			}

			phase := task.Status.Phase
			results[taskName].Phase = string(phase)

			if task.Spec.AgentRef != nil {
				results[taskName].Agent = task.Spec.AgentRef.Name
			}

			if phase != corev1alpha1.TaskPhaseSucceeded && phase != corev1alpha1.TaskPhaseFailed {
				allTerminal = false
				continue
			}

			// Fetch result if available via HTTP GET to controller
			if task.Status.ResultRef != nil && task.Status.ResultRef.Available {
				resultStr, fetchErr := fetchTaskResult(taskName)
				if fetchErr == nil {
					results[taskName].Result = resultStr
				} else {
					results[taskName].Result = fmt.Sprintf("error reading result: %v", fetchErr)
				}
			} else if task.Status.Message != "" {
				results[taskName].Result = task.Status.Message
			}
		}

		if allTerminal {
			break
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// Wait for the shorter of poll interval or remaining time
		wait := min(remaining, pollInterval)

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}

	// Build ordered results
	resultList := make([]TaskResultInfo, 0, len(waitArgs.Tasks))
	for _, taskName := range waitArgs.Tasks {
		resultList = append(resultList, *results[taskName])
	}

	output := WaitForTasksResult{
		Completed: allTerminal,
		Results:   resultList,
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}

	return string(data), nil
}

// Ensure WaitForTasksTool implements Tool
var _ Tool = (*WaitForTasksTool)(nil)

const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// fetchTaskResult fetches a task result from the controller via HTTP GET.
func fetchTaskResult(taskName string) (string, error) {
	controllerURL := os.Getenv("MERCAN_CONTROLLER_URL")
	if controllerURL == "" {
		return "", fmt.Errorf("MERCAN_CONTROLLER_URL is not set")
	}

	controllerURL = strings.TrimRight(controllerURL, "/")
	url := fmt.Sprintf("%s/api/v1/tasks/%s/result", controllerURL, taskName)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Add SA token for auth
	if token, readErr := os.ReadFile(saTokenPath); readErr == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// The public endpoint returns JSON: {"result": "..."}
	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse result JSON: %w", err)
	}

	return result.Result, nil
}
