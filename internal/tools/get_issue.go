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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetIssueTool fetches full details of a specific GitHub issue including comments.
type GetIssueTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// GetIssueArgs are the arguments for the get_issue tool.
type GetIssueArgs struct {
	// TaskName is the name of the task to read workspace config from (optional).
	TaskName string `json:"task_name"`
	// RepoURL is a direct GitHub repo URL (optional; falls back to ORKA_GIT_REPO).
	RepoURL string `json:"repo_url"`
	// IssueNumber is the GitHub issue number (required).
	IssueNumber int `json:"issue_number"`
}

// IssueComment represents a single comment on a GitHub issue.
type IssueComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// GetIssueResult is the result of fetching a GitHub issue.
type GetIssueResult struct {
	Number       int            `json:"number"`
	Title        string         `json:"title"`
	Body         string         `json:"body"`
	Author       string         `json:"author"`
	Labels       []string       `json:"labels"`
	Assignees    []string       `json:"assignees"`
	State        string         `json:"state"`
	CreatedAt    string         `json:"created_at"`
	HTMLURL      string         `json:"html_url"`
	CommentCount int            `json:"comment_count"`
	Comments     []IssueComment `json:"comments"`
}

// NewGetIssueTool creates a new get_issue tool.
func NewGetIssueTool(k8sClient client.Client) *GetIssueTool {
	return &GetIssueTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *GetIssueTool) Name() string {
	return "get_issue"
}

// Description returns the tool description.
func (t *GetIssueTool) Description() string {
	return "Fetch full details of a specific GitHub issue by number, including title, body, labels, " +
		"assignees, state, and the first page of comments. Use this to understand an issue's context " +
		"before working on it or creating related tasks."
}

// Parameters returns the JSON schema for tool parameters.
func (t *GetIssueTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: workspaceTaskDescription}, repoURLField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Direct GitHub repository URL (e.g. 'https://github.com/owner/repo'). Falls back to ORKA_GIT_REPO env var if not provided"}, githubIssueNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub issue number to fetch"}}, jsonSchemaRequiredField: []string{githubIssueNumberField}}
	data, _ := json.Marshal(schema)
	return data
}

// Execute fetches the issue details and comments from GitHub.
func (t *GetIssueTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args GetIssueArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.IssueNumber <= 0 {
		return "", fmt.Errorf("issue_number is required and must be positive")
	}

	owner, repo, token, baseURL, err := resolveRepoAndToken(ctx, t.k8sClient, args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repo and token: %w", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Fetch issue details
	issueResult, err := fetchIssueDetails(ctx, httpClient, baseURL, token, owner, repo, args.IssueNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch issue: %w", err)
	}

	// Fetch comments (non-fatal on failure)
	comments, err := fetchIssueComments(ctx, httpClient, baseURL, token, owner, repo, args.IssueNumber)
	if err == nil {
		issueResult.Comments = comments
	}

	resultJSON, _ := json.Marshal(issueResult)
	return string(resultJSON), nil
}

// fetchIssueDetails fetches a single issue from the GitHub API.
func fetchIssueDetails(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, issueNumber int) (*GetIssueResult, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", baseURL, owner, repo, issueNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var issueResp struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		State    string `json:"state"`
		HTMLURL  string `json:"html_url"`
		Comments int    `json:"comments"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(respBody, &issueResp); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	labels := make([]string, len(issueResp.Labels))
	for i, l := range issueResp.Labels {
		labels[i] = l.Name
	}

	assignees := make([]string, len(issueResp.Assignees))
	for i, a := range issueResp.Assignees {
		assignees[i] = a.Login
	}

	return &GetIssueResult{
		Number:       issueResp.Number,
		Title:        issueResp.Title,
		Body:         issueResp.Body,
		Author:       issueResp.User.Login,
		Labels:       labels,
		Assignees:    assignees,
		State:        issueResp.State,
		CreatedAt:    issueResp.CreatedAt,
		HTMLURL:      issueResp.HTMLURL,
		CommentCount: issueResp.Comments,
		Comments:     []IssueComment{},
	}, nil
}

// fetchIssueComments fetches the first page of comments for a GitHub issue.
func fetchIssueComments(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, issueNumber int) ([]IssueComment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=30", baseURL, owner, repo, issueNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var commentsResp []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(respBody, &commentsResp); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	comments := make([]IssueComment, len(commentsResp))
	for i, c := range commentsResp {
		comments[i] = IssueComment{
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		}
	}

	return comments, nil
}
