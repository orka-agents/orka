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

// ListIssuesTool lists open GitHub issues in a repository.
type ListIssuesTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// ListIssuesArgs are the arguments for the list_issues tool.
type ListIssuesArgs struct {
	// TaskName is the name of a task whose workspace config provides the repo and git credentials.
	TaskName string `json:"task_name"`
	// RepoURL is a direct GitHub repo URL (falls back to ORKA_GIT_REPO env var).
	RepoURL string `json:"repo_url"`
	// UnassignedOnly filters to issues with no assignee. Default is true.
	UnassignedOnly *bool `json:"unassigned_only"`
	// PerPage is the number of results per page (default: 30, max: 100).
	PerPage int `json:"per_page"`
	// Page is the page number for pagination.
	Page int `json:"page"`
}

// IssueSummary represents a single GitHub issue in the result.
type IssueSummary struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	HTMLURL   string   `json:"html_url"`
}

// ListIssuesResult is the result of listing issues.
type ListIssuesResult struct {
	Issues      []IssueSummary `json:"issues"`
	Count       int            `json:"count"`         // number of issues on this page (after filtering)
	HasNextPage bool           `json:"has_next_page"` // true if more pages may exist
}

// NewListIssuesTool creates a new list_issues tool.
func NewListIssuesTool(k8sClient client.Client) *ListIssuesTool {
	return &ListIssuesTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *ListIssuesTool) Name() string {
	return "list_issues"
}

// Description returns the tool description.
func (t *ListIssuesTool) Description() string {
	return "List open GitHub issues in a repository. " +
		"Use this to scan a repo for open issues to triage, prioritize, or assign to agents. " +
		"By default only returns unassigned issues. Returns issue number, title, body, labels, and author."
}

// Parameters returns the JSON schema for tool parameters.
func (t *ListIssuesTool) Parameters() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_name": map[string]any{
				"type":        "string",
				"description": "Name of a task whose workspace config has the repo and git credentials",
			},
			"repo_url": map[string]any{
				"type":        "string",
				"description": "Direct GitHub repository URL (e.g. 'https://github.com/owner/repo'). Falls back to ORKA_GIT_REPO env var if not provided.",
			},
			"unassigned_only": map[string]any{
				"type":        "boolean",
				"description": "If true, only return issues with no assignee. Defaults to true.",
			},
			"per_page": map[string]any{
				"type":        "integer",
				"description": "Number of results per page (default: 30, max: 100)",
			},
			"page": map[string]any{
				"type":        "integer",
				"description": "Page number for pagination (default: 1)",
			},
		},
	}
	data, _ := json.Marshal(schema)
	return data
}

const maxIssueBodyLength = 500

// Execute lists open issues from a GitHub repository.
func (t *ListIssuesTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args ListIssuesArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	// Resolve repo and token using the shared helper
	owner, repo, token, baseURL, err := resolveRepoAndToken(ctx, t.k8sClient, args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", err
	}

	// Apply defaults
	perPage := args.PerPage
	if perPage <= 0 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}

	page := args.Page
	if page <= 0 {
		page = 1
	}

	unassignedOnly := true
	if args.UnassignedOnly != nil {
		unassignedOnly = *args.UnassignedOnly
	}

	// Build GitHub API URL
	url := fmt.Sprintf("%s/repos/%s/%s/issues?state=open&per_page=%d&page=%d", baseURL, owner, repo, perPage, page)
	if unassignedOnly {
		url += "&assignee=none"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
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

	// Parse the response — GitHub issues API returns PRs too, so we filter them out
	var rawIssues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		CreatedAt   string `json:"created_at"`
		HTMLURL     string `json:"html_url"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(respBody, &rawIssues); err != nil {
		return "", fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	rawCount := len(rawIssues)

	issues := make([]IssueSummary, 0, len(rawIssues))
	for _, raw := range rawIssues {
		// Filter out pull requests
		if raw.PullRequest != nil {
			continue
		}

		body := raw.Body
		runes := []rune(body)
		if len(runes) > maxIssueBodyLength {
			body = string(runes[:maxIssueBodyLength]) + "..."
		}

		labels := make([]string, len(raw.Labels))
		for i, l := range raw.Labels {
			labels[i] = l.Name
		}

		issues = append(issues, IssueSummary{
			Number:    raw.Number,
			Title:     raw.Title,
			Body:      body,
			Author:    raw.User.Login,
			Labels:    labels,
			CreatedAt: raw.CreatedAt,
			HTMLURL:   raw.HTMLURL,
		})
	}

	result := ListIssuesResult{
		Issues:      issues,
		Count:       len(issues),
		HasNextPage: rawCount >= perPage,
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}
