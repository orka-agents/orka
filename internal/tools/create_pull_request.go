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

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// CreatePullRequestTool creates a GitHub pull request from a pushed branch.
type CreatePullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// CreatePullRequestArgs are the arguments for the create_pull_request tool.
type CreatePullRequestArgs struct {
	// TaskName is the name of the completed child task whose branch to use.
	// The tool reads the task's workspace config to determine repo and git secret.
	TaskName string `json:"task_name"`
	// HeadBranch is the remote branch that contains the changes.
	HeadBranch string `json:"head_branch"`
	// BaseBranch is the target branch to merge into (e.g. "main").
	BaseBranch string `json:"base_branch"`
	// Title is the pull request title.
	Title string `json:"title"`
	// Body is the pull request body (Markdown).
	Body string `json:"body"`
}

// CreatePullRequestResult is the result of creating a pull request.
type CreatePullRequestResult struct {
	PRURL    string `json:"pr_url"`
	PRNumber int    `json:"pr_number"`
	Status   string `json:"status"`
}

// NewCreatePullRequestTool creates a new create_pull_request tool.
func NewCreatePullRequestTool(k8sClient client.Client) *CreatePullRequestTool {
	return &CreatePullRequestTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *CreatePullRequestTool) Name() string {
	return "create_pull_request"
}

// Description returns the tool description.
func (t *CreatePullRequestTool) Description() string {
	return "Create a GitHub pull request from a branch that was pushed by a completed agent task. " +
		"Use this after a coder task with pushBranch has succeeded and been reviewed."
}

// Parameters returns the JSON schema for tool parameters.
func (t *CreatePullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_name": map[string]any{
				"type":        "string",
				"description": "Name of the completed child task whose workspace config has the repo and git credentials",
			},
			"head_branch": map[string]any{
				"type":        "string",
				"description": "The branch containing the changes (the pushBranch used by the coder)",
			},
			"base_branch": map[string]any{
				"type":        "string",
				"description": "The target branch to merge into (e.g. 'main')",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Pull request title",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Pull request body in Markdown format",
			},
		},
		"required": []string{"task_name", "head_branch", "base_branch", "title"},
	}
	data, _ := json.Marshal(schema)
	return data
}

// Execute creates a pull request on GitHub.
func (t *CreatePullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args CreatePullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.HeadBranch == "" || args.BaseBranch == "" || args.Title == "" {
		return "", fmt.Errorf("task_name, head_branch, base_branch, and title are required")
	}

	// Determine namespace from environment
	ns := os.Getenv("MERCAN_TASK_NAMESPACE")
	if ns == "" {
		ns = "default"
	}

	// Look up the child task to get workspace config
	var task corev1alpha1.Task
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: args.TaskName, Namespace: ns}, &task); err != nil {
		return "", fmt.Errorf("failed to get task %s: %w", args.TaskName, err)
	}

	// Extract repo URL and git secret from the task's workspace config
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		return "", fmt.Errorf("task %s does not have workspace configuration", args.TaskName)
	}
	ws := task.Spec.AgentRuntime.Workspace

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
	for _, key := range []string{"token", "password"} {
		if v, ok := secret.Data[key]; ok {
			token = strings.TrimSpace(string(v))
			break
		}
	}
	if token == "" {
		return "", fmt.Errorf("git secret %s does not contain a 'token' or 'password' key", ws.GitSecretRef.Name)
	}

	// Create the pull request via GitHub API
	prURL, prNumber, err := createGitHubPR(token, owner, repo, args.HeadBranch, args.BaseBranch, args.Title, args.Body, t.apiBaseURL)
	if err != nil {
		return "", fmt.Errorf("failed to create pull request: %w", err)
	}

	result := CreatePullRequestResult{
		PRURL:    prURL,
		PRNumber: prNumber,
		Status:   "created",
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// parseGitHubRepo extracts owner and repo name from a GitHub URL.
// Supports https://github.com/owner/repo.git and similar formats.
func parseGitHubRepo(repoURL string) (string, string, error) {
	// Normalize
	repoURL = strings.TrimSuffix(repoURL, ".git")
	repoURL = strings.TrimSuffix(repoURL, "/")

	// Try HTTPS format: https://github.com/owner/repo
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if after, ok := strings.CutPrefix(repoURL, prefix); ok {
			path := after
			parts := strings.SplitN(path, "/", 3)
			if len(parts) >= 2 {
				return parts[0], parts[1], nil
			}
		}
	}

	// Try SSH format: git@github.com:owner/repo
	if after, ok := strings.CutPrefix(repoURL, "git@github.com:"); ok {
		path := after
		parts := strings.SplitN(path, "/", 3)
		if len(parts) >= 2 {
			return parts[0], parts[1], nil
		}
	}

	return "", "", fmt.Errorf("unsupported GitHub URL format: %s", repoURL)
}

// createGitHubPR creates a pull request via the GitHub REST API.
// An optional apiBaseURL can be provided for testing; if empty, uses https://api.github.com.
func createGitHubPR(token, owner, repo, head, base, title, body string, apiBaseURL ...string) (string, int, error) {
	baseURL := githubAPIBaseURL
	if len(apiBaseURL) > 0 && apiBaseURL[0] != "" {
		baseURL = apiBaseURL[0]
	}
	url := fmt.Sprintf("%s/repos/%s/%s/pulls", baseURL, owner, repo)

	payload := map[string]string{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var prResp struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return "", 0, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return prResp.HTMLURL, prResp.Number, nil
}
