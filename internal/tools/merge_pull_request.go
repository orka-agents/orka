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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// MergePullRequestTool merges a GitHub pull request after verifying CI checks pass.
type MergePullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// MergePullRequestArgs are the arguments for the merge_pull_request tool.
type MergePullRequestArgs struct {
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
}

// MergePullRequestResult is the result of merging a pull request.
type MergePullRequestResult struct {
	SHA          string `json:"sha"`
	Merged       bool   `json:"merged"`
	Message      string `json:"message"`
	ChecksPassed bool   `json:"checks_passed"`
}

// NewMergePullRequestTool creates a new merge_pull_request tool.
func NewMergePullRequestTool(k8sClient client.Client) *MergePullRequestTool {
	return &MergePullRequestTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *MergePullRequestTool) Name() string {
	return mergePullRequestToolName
}

// Description returns the tool description.
func (t *MergePullRequestTool) Description() string {
	return "Merge a GitHub pull request after verifying all CI checks have passed. " +
		"Takes a PR number and task name to read git credentials from the task's workspace configuration."
}

// Parameters returns the JSON schema for tool parameters.
func (t *MergePullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: childWorkspaceTaskDescription}, githubPRNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub pull request number to merge"}, mergeMethodField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Merge method: 'merge', 'squash', or 'rebase'. Defaults to 'squash'",
		jsonSchemaEnumField: []string{mergeMethodMerge, defaultMergeMethod, mergeMethodRebase},
	}, githubCommitTitleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Custom merge commit title"},
		githubCommitMessageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Custom merge commit message"},
	}, jsonSchemaRequiredField: []string{taskNameField, githubPRNumberField},
	}
	data, _ := json.Marshal(schema)
	return data
}

// Execute merges a pull request on GitHub after verifying CI checks.
func (t *MergePullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args MergePullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.PRNumber == 0 {
		return "", fmt.Errorf("task_name and pr_number are required")
	}

	if args.MergeMethod == "" {
		args.MergeMethod = defaultMergeMethod
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

	// Get PR details to find the head SHA
	headSHA, err := getGitHubPRHeadSHA(ctx, token, owner, repo, args.PRNumber, baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to get PR head SHA: %w", err)
	}

	// Check CI status
	passed, checkDetails, err := checkGitHubCIStatus(ctx, token, owner, repo, headSHA, baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to check CI status: %w", err)
	}

	if !passed {
		result := MergePullRequestResult{
			Merged:       false,
			ChecksPassed: false,
			Message:      fmt.Sprintf("CI checks have not all passed: %s", checkDetails),
		}
		resultJSON, _ := json.Marshal(result)
		return string(resultJSON), nil
	}

	// Merge the PR
	sha, err := mergeGitHubPR(ctx, token, owner, repo, args.PRNumber, args.MergeMethod, args.CommitTitle, args.CommitMessage, baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to merge pull request: %w", err)
	}

	result := MergePullRequestResult{
		SHA:          sha,
		Merged:       true,
		ChecksPassed: true,
		Message:      pullRequestMergedMessage,
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// getGitHubPRHeadSHA retrieves the head SHA for a pull request.
func getGitHubPRHeadSHA(ctx context.Context, token, owner, repo string, prNumber int, baseURL string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var prResp struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return "", fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return prResp.Head.SHA, nil
}

// checkGitHubCIStatus checks whether all CI checks have passed for a commit.
func checkGitHubCIStatus(ctx context.Context, token, owner, repo, sha, baseURL string) (bool, string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", baseURL, owner, repo, sha)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
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
		return false, "", fmt.Errorf("failed to parse check runs response: %w", err)
	}

	if checkResp.TotalCount == 0 {
		return false, "no CI checks configured", nil
	}

	var failures []string
	for _, check := range checkResp.CheckRuns {
		if check.Status != "completed" || check.Conclusion != successStatusString {
			failures = append(failures, fmt.Sprintf("%s (status=%s, conclusion=%s)", check.Name, check.Status, check.Conclusion))
		}
	}

	if len(failures) > 0 {
		return false, strings.Join(failures, "; "), nil
	}

	return true, "", nil
}

// mergeGitHubPR merges a pull request via the GitHub REST API.
func mergeGitHubPR(ctx context.Context, token, owner, repo string, prNumber int, mergeMethod, commitTitle, commitMessage, baseURL string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", baseURL, owner, repo, prNumber)

	payload := map[string]any{mergeMethodField: mergeMethod}
	if commitTitle != "" {
		payload[githubCommitTitleField] = commitTitle
	}
	if commitMessage != "" {
		payload[githubCommitMessageField] = commitMessage
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var mergeResp struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(respBody, &mergeResp); err != nil {
		return "", fmt.Errorf("failed to parse merge response: %w", err)
	}

	return mergeResp.SHA, nil
}
