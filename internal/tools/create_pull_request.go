/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
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

	owner, repo, token, baseURL, err := resolveRepoAndToken(ctx, t.k8sClient, args.TaskName, "", t.apiBaseURL)
	if err != nil {
		return "", err
	}

	// Create the pull request via GitHub API
	prURL, prNumber, status, err := createGitHubPR(ctx, token, owner, repo, args.HeadBranch, args.BaseBranch, args.Title, args.Body, baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to create pull request: %w", err)
	}

	result := CreatePullRequestResult{
		PRURL:    prURL,
		PRNumber: prNumber,
		Status:   status,
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
// If the pull request already exists, it resolves and returns the existing PR.
func createGitHubPR(ctx context.Context, token, owner, repo, head, base, title, body, apiBaseURL string) (string, int, string, error) {
	pr, err := CreateOrGetGitHubPullRequest(ctx, token, owner, repo, head, base, title, body, apiBaseURL)
	if err != nil {
		return "", 0, "", err
	}

	return pr.HTMLURL, pr.Number, pr.Status, nil
}
