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
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/workers/common"
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
	Task           string          `json:"task"`
	Agent          string          `json:"agent,omitempty"`
	Phase          string          `json:"phase"`
	Result         string          `json:"result,omitempty"`
	Summary        string          `json:"summary,omitempty"`
	Verdict        string          `json:"verdict,omitempty"`
	Feedback       string          `json:"feedback,omitempty"`
	Files          []string        `json:"files,omitempty"`
	BaseSHA        string          `json:"baseSHA,omitempty"`
	PushBranch     string          `json:"pushBranch,omitempty"`
	Iteration      string          `json:"iteration,omitempty"`
	FailureDetails *FailureDetails `json:"failureDetails,omitempty"`
	Retried        bool            `json:"retried,omitempty"`
	RetryTaskName  string          `json:"retryTaskName,omitempty"`
}

// FailureDetails provides structured information about a failed task
type FailureDetails struct {
	Message    string `json:"message"`
	RetryCount int    `json:"retryCount"`
	MaxRetries int    `json:"maxRetries"`
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

	ns := os.Getenv("ORKA_TASK_NAMESPACE")
	if ns == "" {
		return "", fmt.Errorf("ORKA_TASK_NAMESPACE environment variable is not set")
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

			// Handle auto-retry for failed tasks
			if phase == corev1alpha1.TaskPhaseFailed {
				retryTaskName, retried := t.tryAutoRetry(ctx, &task, ns)
				if retried {
					// Track the retry — replace this task with the new one
					results[taskName].Retried = true
					results[taskName].RetryTaskName = retryTaskName
					results[taskName].Phase = "Retried"
					results[taskName].Result = fmt.Sprintf("auto-retried as %s", retryTaskName)

					// Add failure details
					retryCount, maxRetries := getRetryInfo(&task)
					results[taskName].FailureDetails = &FailureDetails{
						Message:    task.Status.Message,
						RetryCount: retryCount,
						MaxRetries: maxRetries,
					}

					// Start tracking the retry task
					waitArgs.Tasks = append(waitArgs.Tasks, retryTaskName)
					results[retryTaskName] = &TaskResultInfo{
						Task:  retryTaskName,
						Agent: results[taskName].Agent,
						Phase: "Pending",
					}
					allTerminal = false
					continue
				}

				// Not retried — add failure details
				retryCount, maxRetries := getRetryInfo(&task)
				if task.Annotations[labels.AnnotationAutoRetry] == trueStr {
					results[taskName].FailureDetails = &FailureDetails{
						Message:    task.Status.Message,
						RetryCount: retryCount,
						MaxRetries: maxRetries,
					}
				}
			}

			// Fetch result if available via HTTP GET to controller
			if task.Status.ResultRef != nil && task.Status.ResultRef.Available {
				resultStr, fetchErr := fetchTaskResult(ctx, taskName)
				if fetchErr == nil {
					// Parse structured result and strip diff to avoid context bloat
					sr := common.ParseStructuredResult(resultStr)
					results[taskName].Summary = sr.Summary
					results[taskName].Verdict = sr.Verdict
					results[taskName].Feedback = sr.Feedback
					results[taskName].Files = sr.Files
					results[taskName].BaseSHA = sr.BaseSHA
					results[taskName].PushBranch = sr.PushBranch
					// Set Result to summary only (never include raw diff)
					results[taskName].Result = sr.Summary
				} else {
					results[taskName].Result = fmt.Sprintf("error reading result: %v", fetchErr)
				}
			} else if task.Status.Message != "" {
				results[taskName].Result = task.Status.Message
			}

			// Add iteration label if present
			if iterStr, ok := task.Labels[labels.LabelIteration]; ok {
				results[taskName].Iteration = iterStr
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

// tryAutoRetry checks if a failed task should be auto-retried and creates a new child task if so.
// Returns the new task name and true if a retry was created, or empty string and false otherwise.
func (t *WaitForTasksTool) tryAutoRetry(ctx context.Context, failedTask *corev1alpha1.Task, _ string) (string, bool) {
	if failedTask.Annotations[labels.AnnotationAutoRetry] != trueStr {
		return "", false
	}

	retryCount, maxRetries := getRetryInfo(failedTask)
	if retryCount >= maxRetries {
		return "", false
	}

	// Get the original prompt (stored at delegation time)
	originalPrompt := failedTask.Annotations[labels.AnnotationOriginalPrompt]
	if originalPrompt == "" {
		originalPrompt = failedTask.Spec.Prompt
	}

	// Build error context
	errorMsg := failedTask.Status.Message
	if errorMsg == "" {
		errorMsg = "task failed with no message"
	}

	// Create retry task with error feedback prepended
	retryTask := failedTask.DeepCopy()
	retryTask.Name = ""
	retryTask.GenerateName = failedTask.Name + "-retry-"
	retryTask.ResourceVersion = ""
	retryTask.UID = ""
	retryTask.Status = corev1alpha1.TaskStatus{}
	retryTask.Spec.Prompt = fmt.Sprintf("PREVIOUS ATTEMPT FAILED (attempt %d of %d):\n%s\n\nPlease retry the original task, avoiding the previous error:\n%s",
		retryCount+1, maxRetries, errorMsg, originalPrompt)

	// Update retry annotations
	retryTask.Annotations[labels.AnnotationRetryCount] = strconv.Itoa(retryCount + 1)
	retryTask.Annotations[labels.AnnotationRetriedFrom] = failedTask.Name

	if err := t.k8sClient.Create(ctx, retryTask); err != nil {
		return "", false
	}

	return retryTask.Name, true
}

// getRetryInfo extracts retry count and max retries from task annotations.
func getRetryInfo(task *corev1alpha1.Task) (retryCount, maxRetries int) {
	if countStr, ok := task.Annotations[labels.AnnotationRetryCount]; ok {
		retryCount, _ = strconv.Atoi(countStr)
	}
	maxRetries = 2 // default
	if maxStr, ok := task.Annotations[labels.AnnotationMaxRetries]; ok {
		maxRetries, _ = strconv.Atoi(maxStr)
	}
	return
}

const saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// fetchTaskResult fetches a task result from the controller via HTTP GET.
func fetchTaskResult(ctx context.Context, taskName string) (string, error) {
	controllerURL := os.Getenv("ORKA_CONTROLLER_URL")
	if controllerURL == "" {
		return "", fmt.Errorf("ORKA_CONTROLLER_URL is not set")
	}

	controllerURL = strings.TrimRight(controllerURL, "/")
	url := fmt.Sprintf("%s/api/v1/tasks/%s/result", controllerURL, taskName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
	defer resp.Body.Close() //nolint:errcheck

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
