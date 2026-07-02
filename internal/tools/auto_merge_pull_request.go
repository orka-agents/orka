/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// AutoMergePullRequestTool polls GitHub CI checks and auto-merges a PR when all checks pass.
type AutoMergePullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// AutoMergePullRequestArgs are the arguments for the auto_merge_pull_request tool.
type AutoMergePullRequestArgs struct {
	// TaskName is the name of the child task to read workspace config from.
	TaskName string `json:"task_name"`
	// PRNumber is the GitHub PR number to merge.
	PRNumber int `json:"pr_number"`
	// MergeMethod is the merge method: "merge", "squash", "rebase". Defaults to "squash".
	MergeMethod string `json:"merge_method,omitempty"`
	// CommitTitle is the custom merge commit title.
	CommitTitle string `json:"commit_title,omitempty"`
	// CommitMessage is the custom merge commit message.
	CommitMessage string `json:"commit_message,omitempty"`
	// Timeout is the maximum time to wait for CI checks (e.g. "30m"). Defaults to "30m".
	Timeout string `json:"timeout,omitempty"`
}

// AutoMergePullRequestResult is the result of the auto-merge operation.
type AutoMergePullRequestResult struct {
	Merged        bool   `json:"merged"`
	ChecksPassed  bool   `json:"checks_passed"`
	SHA           string `json:"sha,omitempty"`
	Message       string `json:"message"`
	Outcome       string `json:"outcome"`
	ChecksDetails string `json:"checks_details,omitempty"`
}

// CICheckResult categorizes CI check status into passed, failed, or pending.
type CICheckResult struct {
	Passed  bool
	Failed  bool
	Pending bool
	Details string
}

var errNoCIChecksConfigured = errors.New("no CI checks configured for this PR")

// NewAutoMergePullRequestTool creates a new auto_merge_pull_request tool.
func NewAutoMergePullRequestTool(k8sClient client.Client) *AutoMergePullRequestTool {
	return &AutoMergePullRequestTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *AutoMergePullRequestTool) Name() string {
	return autoMergePullRequestToolName
}

// Description returns the tool description.
func (t *AutoMergePullRequestTool) Description() string {
	return "Poll GitHub CI checks and automatically merge a pull request when all checks pass. " +
		"Waits up to the specified timeout, retrying on transient errors."
}

// Parameters returns the JSON schema for tool parameters.
func (t *AutoMergePullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: childWorkspaceTaskDescription}, githubPRNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub pull request number to merge"}, mergeMethodField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Merge method: 'merge', 'squash', or 'rebase'. Defaults to 'squash'",
		jsonSchemaEnumField: []string{mergeMethodMerge, defaultMergeMethod, mergeMethodRebase},
	}, githubCommitTitleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Custom merge commit title"},
		githubCommitMessageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Custom merge commit message"}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Maximum time to wait for CI checks (e.g. '30m', '1h'). Defaults to '30m'"},
	}, jsonSchemaRequiredField: []string{taskNameField, githubPRNumberField},
	}
	data, _ := json.Marshal(schema)
	return data
}

// Execute polls CI checks and merges the PR when all checks pass.
func (t *AutoMergePullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args AutoMergePullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.PRNumber == 0 {
		return "", fmt.Errorf("task_name and pr_number are required")
	}

	if args.MergeMethod == "" {
		args.MergeMethod = defaultMergeMethod
	}

	if args.Timeout == "" {
		args.Timeout = "30m"
	}

	timeout, err := time.ParseDuration(args.Timeout)
	if err != nil {
		return "", fmt.Errorf("invalid timeout %q: %w", args.Timeout, err)
	}

	// Determine namespace from environment
	ns := os.Getenv(envOrkaTaskNamespace)
	if ns == "" {
		ns = defaultNamespace
	}

	// Look up the child task to get workspace config
	var task corev1alpha1.Task
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: args.TaskName, Namespace: ns}, &task); err != nil {
		return "", fmt.Errorf("failed to get task %s: %w", args.TaskName, err)
	}

	// Extract repo URL and git secret from the task's workspace config
	ws := taskWorkspace(&task)
	if ws == nil {
		return "", fmt.Errorf("task %s does not have workspace configuration", args.TaskName)
	}

	repoURL := ws.GitRepo
	if repoURL == "" {
		return "", fmt.Errorf("task %s workspace has no gitRepo configured", args.TaskName)
	}

	// Parse owner/repo from the git URL
	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
	}

	// Get the git token from the referenced secret
	if ws.GitSecretRef == nil {
		return "", fmt.Errorf("task %s workspace has no gitSecretRef configured", args.TaskName)
	}

	var secret corev1.Secret
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: ws.GitSecretRef.Name, Namespace: ns}, &secret); err != nil {
		return "", fmt.Errorf("failed to get git secret %s: %w", ws.GitSecretRef.Name, err)
	}

	token := ""
	for _, key := range []string{tokenKey, passwordKey} {
		if v, ok := secret.Data[key]; ok {
			token = strings.TrimSpace(string(v))
			break
		}
	}
	if token == "" {
		return "", fmt.Errorf("git secret %s does not contain a 'token' or 'password' key", ws.GitSecretRef.Name)
	}

	baseURL := githubAPIBaseURL
	if t.apiBaseURL != "" {
		baseURL = t.apiBaseURL
	}

	logger := log.FromContext(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	deadline := time.After(timeout)

	pc := &pollContext{
		token: token, owner: owner, repo: repo,
		prNumber: args.PRNumber, mergeMethod: args.MergeMethod,
		commitTitle: args.CommitTitle, commitMessage: args.CommitMessage,
		baseURL: baseURL, logger: logger,
	}

	// Run the first check immediately, then poll on ticker
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return marshalResult(AutoMergePullRequestResult{
					Message: "context cancelled while waiting for CI checks",
					Outcome: timeoutField,
				}), nil
			case <-deadline:
				return marshalResult(AutoMergePullRequestResult{
					Message: fmt.Sprintf("timed out after %s waiting for CI checks to pass", args.Timeout),
					Outcome: timeoutField,
				}), nil
			case <-ticker.C:
			}
		}

		result, done, err := pc.pollOnce(ctx)
		if err != nil {
			return "", err
		}
		if done {
			return marshalResult(*result), nil
		}
	}
}

// pollContext holds the parameters needed for each poll iteration.
type pollContext struct {
	token, owner, repo                      string
	prNumber                                int
	mergeMethod, commitTitle, commitMessage string
	baseURL                                 string
	logger                                  logr.Logger
}

// pollOnce performs a single poll iteration. It returns a result and done=true
// if the poll loop should stop, or done=false if polling should continue.
func (pc *pollContext) pollOnce(ctx context.Context) (*AutoMergePullRequestResult, bool, error) {
	headSHA, state, merged, err := getGitHubPRDetails(ctx, pc.token, pc.owner, pc.repo, pc.prNumber, pc.baseURL)
	if err != nil {
		if isTransientHTTPError(err) {
			pc.logger.Info("transient GitHub API error, will retry", "error", err)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get PR details: %w", err)
	}

	if merged {
		return &AutoMergePullRequestResult{
			Merged: true, ChecksPassed: true,
			Message: "Pull request was already merged", Outcome: "already_merged",
		}, true, nil
	}

	if state == githubPRStateClosed {
		return &AutoMergePullRequestResult{
			Message: "Pull request is closed", Outcome: githubPRStateClosed,
		}, true, nil
	}

	ciResult, err := checkCIStatusDetailed(ctx, pc.token, pc.owner, pc.repo, headSHA, pc.baseURL)
	if err != nil {
		if isTransientHTTPError(err) {
			pc.logger.Info("transient GitHub API error, will retry", "error", err)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to check CI status: %w", err)
	}

	if ciResult.Failed {
		return &AutoMergePullRequestResult{
			ChecksDetails: ciResult.Details,
			Message:       fmt.Sprintf("CI checks failed: %s", ciResult.Details),
			Outcome:       "ci_failed",
		}, true, nil
	}

	if ciResult.Passed {
		sha, err := mergeGitHubPR(ctx, pc.token, pc.owner, pc.repo, pc.prNumber, pc.mergeMethod, pc.commitTitle, pc.commitMessage, pc.baseURL)
		if err != nil {
			return nil, false, fmt.Errorf("failed to merge pull request: %w", err)
		}
		return &AutoMergePullRequestResult{
			Merged: true, ChecksPassed: true, SHA: sha,
			Message: pullRequestMergedMessage, Outcome: mergedStatusString,
		}, true, nil
	}

	pc.logger.Info("CI checks still pending, will retry", "pr", pc.prNumber, "details", ciResult.Details)
	return nil, false, nil
}

// marshalResult serializes an AutoMergePullRequestResult to JSON string.
func marshalResult(r AutoMergePullRequestResult) string {
	data, _ := json.Marshal(r)
	return string(data)
}

// getGitHubPRDetails retrieves the head SHA, state, and merged status for a pull request.
func getGitHubPRDetails(ctx context.Context, token, owner, repo string, prNumber int, baseURL string) (headSHA, state string, merged bool, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", false, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", false, &githubAPIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var prResp struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return "", "", false, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return prResp.Head.SHA, prResp.State, prResp.Merged, nil
}

// checkCIStatusDetailed checks CI status and categorizes checks into passed, failed, or pending.
func checkCIStatusDetailed(ctx context.Context, token, owner, repo, sha, baseURL string) (CICheckResult, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", baseURL, owner, repo, sha)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CICheckResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return CICheckResult{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CICheckResult{}, &githubAPIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var checkResp struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(respBody, &checkResp); err != nil {
		return CICheckResult{}, fmt.Errorf("failed to parse check runs response: %w", err)
	}

	if checkResp.TotalCount == 0 {
		return CICheckResult{}, errNoCIChecksConfigured
	}

	var failed, pending []string
	for _, check := range checkResp.CheckRuns {
		if check.Status != "completed" {
			// queued, in_progress, etc.
			pending = append(pending, fmt.Sprintf("%s (status=%s)", check.Name, check.Status))
			continue
		}
		switch check.Conclusion {
		case successStatusString, "neutral", "skipped":
			// passed — nothing to report
		case "failure", cancelledStatusString, "timed_out", "action_required", "stale":
			failed = append(failed, fmt.Sprintf("%s (conclusion=%s)", check.Name, check.Conclusion))
		default:
			failed = append(failed, fmt.Sprintf("%s (conclusion=%s)", check.Name, check.Conclusion))
		}
	}

	if len(failed) > 0 {
		return CICheckResult{
			Failed:  true,
			Details: strings.Join(failed, "; "),
		}, nil
	}

	if len(pending) > 0 {
		return CICheckResult{
			Pending: true,
			Details: strings.Join(pending, "; "),
		}, nil
	}

	return CICheckResult{Passed: true}, nil
}

// githubAPIError represents an HTTP error from the GitHub API, carrying the status code.
type githubAPIError struct {
	StatusCode int
	Body       string
}

func (e *githubAPIError) Error() string {
	return fmt.Sprintf("GitHub API returned %d: %s", e.StatusCode, e.Body)
}

// isTransientHTTPError returns true if the error represents a transient GitHub API error (429 or 5xx).
func isTransientHTTPError(err error) bool {
	var apiErr *githubAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == 429 || apiErr.StatusCode >= 500
	}
	return false
}

var _ Tool = (*AutoMergePullRequestTool)(nil)
