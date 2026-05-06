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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CommentOnIssueTool posts a comment on a GitHub issue.
type CommentOnIssueTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// CommentOnIssueArgs are the arguments for the comment_on_issue tool.
type CommentOnIssueArgs struct {
	// TaskName is the name of a task whose workspace config provides the repo and git credentials.
	TaskName string `json:"task_name"`
	// RepoURL is a direct GitHub repo URL (falls back to ORKA_GIT_REPO env var).
	RepoURL string `json:"repo_url"`
	// IssueNumber is the GitHub issue number to comment on.
	IssueNumber int `json:"issue_number"`
	// Body is the comment text (Markdown supported).
	Body string `json:"body"`
}

// CommentOnIssueResult is the result of posting a comment on an issue.
type CommentOnIssueResult struct {
	CommentID int    `json:"comment_id"`
	HTMLURL   string `json:"html_url"`
	Status    string `json:"status"`
}

// NewCommentOnIssueTool creates a new comment_on_issue tool.
func NewCommentOnIssueTool(k8sClient client.Client) *CommentOnIssueTool {
	return &CommentOnIssueTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *CommentOnIssueTool) Name() string {
	return "comment_on_issue"
}

// Description returns the tool description.
func (t *CommentOnIssueTool) Description() string {
	return "Post a comment on a GitHub issue. " +
		"Use this to post status updates, progress reports, or agent activity notes on an issue " +
		"(e.g. '🤖 Agent is working on this issue')."
}

// Parameters returns the JSON schema for tool parameters.
func (t *CommentOnIssueTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: workspaceTaskDescription}, repoURLField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Direct GitHub repository URL (e.g. 'https://github.com/owner/repo'). Falls back to ORKA_GIT_REPO env var if not provided."}, githubIssueNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub issue number to comment on"}, githubBodyField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Comment text to post on the issue (Markdown supported)"}}, jsonSchemaRequiredField: []string{githubIssueNumberField, githubBodyField}}
	data, _ := json.Marshal(schema)
	return data
}

// Execute posts a comment on a GitHub issue.
func (t *CommentOnIssueTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args CommentOnIssueArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.IssueNumber <= 0 {
		return "", fmt.Errorf("issue_number is required and must be positive")
	}
	if args.Body == "" {
		return "", fmt.Errorf("body is required")
	}

	// Resolve repo and token using the shared helper
	owner, repo, token, baseURL, err := resolveRepoAndToken(ctx, t.k8sClient, args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", err
	}

	// Post the comment via GitHub API
	commentID, htmlURL, err := postIssueComment(ctx, token, owner, repo, args.IssueNumber, args.Body, baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to post comment: %w", err)
	}

	result := CommentOnIssueResult{
		CommentID: commentID,
		HTMLURL:   htmlURL,
		Status:    GitHubPullRequestStatusCreated,
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// postIssueComment posts a comment on a GitHub issue via the REST API.
func postIssueComment(ctx context.Context, token, owner, repo string, issueNumber int, body, apiBaseURL string) (int, string, error) {
	baseURL := githubAPIBaseURL
	if apiBaseURL != "" {
		baseURL = apiBaseURL
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", baseURL, owner, repo, issueNumber)

	payload := map[string]string{
		githubBodyField: body,
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var commentResp struct {
		ID      int    `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &commentResp); err != nil {
		return 0, "", fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return commentResp.ID, commentResp.HTMLURL, nil
}
