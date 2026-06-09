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

// ListPullRequestsTool lists open pull requests in a GitHub repository.
type ListPullRequestsTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// ListPullRequestsArgs are the arguments for the list_pull_requests tool.
type ListPullRequestsArgs struct {
	TaskName string `json:"task_name"` // optional: resolve repo from a task's workspace
	RepoURL  string `json:"repo_url"`  // optional: direct repo URL; with task context it must match that task's repository scope
	PerPage  int    `json:"per_page"`  // results per page (default: 30, max: 100)
	Page     int    `json:"page"`      // page number (default: 1)
}

// PRSummary represents a single pull request in the listing.
type PRSummary struct {
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Author     string   `json:"author"`
	BaseBranch string   `json:"base_branch"`
	HeadBranch string   `json:"head_branch"`
	Labels     []string `json:"labels"`
	CreatedAt  string   `json:"created_at"`
	HTMLURL    string   `json:"html_url"`
	Draft      bool     `json:"draft"`
}

// ListPullRequestsResult is the result of listing pull requests.
type ListPullRequestsResult struct {
	PullRequests []PRSummary `json:"pull_requests"`
	TotalCount   int         `json:"total_count"`
}

// NewListPullRequestsTool creates a new list_pull_requests tool.
func NewListPullRequestsTool(k8sClient client.Client) *ListPullRequestsTool {
	return &ListPullRequestsTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *ListPullRequestsTool) Name() string {
	return "list_pull_requests"
}

// Description returns the tool description.
func (t *ListPullRequestsTool) Description() string {
	return "List open pull requests in a GitHub repository. " +
		"Returns PR numbers, titles, authors, branches, labels, and URLs. " +
		"Use this to scan a repo for PRs that need review or to find specific pull requests."
}

// Parameters returns the JSON schema for tool parameters.
func (t *ListPullRequestsTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Name of the task whose workspace config has the repo and git credentials"}, repoURLField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "GitHub repository URL (for example, https://github.com/owner/repo). Requires task_name or current task context and must match that task's repository scope."}, perPageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Number of results per page (default: 30, max: 100)"}, pageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Page number for pagination (default: 1)"}}}
	data, _ := json.Marshal(schema)
	return data
}

// Execute lists open pull requests from GitHub.
func (t *ListPullRequestsTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args ListPullRequestsArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	// Apply defaults and caps
	if args.PerPage <= 0 {
		args.PerPage = 30
	}
	if args.PerPage > 100 {
		args.PerPage = 100
	}
	if args.Page <= 0 {
		args.Page = 1
	}

	owner, repo, token, baseURL, err := resolveScopedRepoAndToken(ctx, t.k8sClient, args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=%d&page=%d", baseURL, owner, repo, args.PerPage, args.Page)

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

	var pullsResp []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Draft   bool   `json:"draft"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(respBody, &pullsResp); err != nil {
		return "", fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	summaries := make([]PRSummary, len(pullsResp))
	for i, pr := range pullsResp {
		body := pr.Body
		runes := []rune(body)
		if len(runes) > 500 {
			body = string(runes[:500]) + "..."
		}

		labels := make([]string, len(pr.Labels))
		for j, l := range pr.Labels {
			labels[j] = l.Name
		}

		summaries[i] = PRSummary{
			Number:     pr.Number,
			Title:      pr.Title,
			Body:       body,
			Author:     pr.User.Login,
			BaseBranch: pr.Base.Ref,
			HeadBranch: pr.Head.Ref,
			Labels:     labels,
			CreatedAt:  pr.CreatedAt,
			HTMLURL:    pr.HTMLURL,
			Draft:      pr.Draft,
		}
	}

	result := ListPullRequestsResult{
		PullRequests: summaries,
		TotalCount:   len(summaries),
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}
