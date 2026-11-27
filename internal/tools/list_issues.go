/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const listIssuesToolName = "list_issues"

type ListIssuesTool struct {
	k8sClient  client.Client
	apiBaseURL string
}

type ListIssuesArgs struct {
	TaskName       string `json:"task_name"`
	RepoURL        string `json:"repo_url"`
	UnassignedOnly *bool  `json:"unassigned_only"`
	PerPage        int    `json:"per_page"`
	Page           int    `json:"page"`
}

type IssueSummary struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	HTMLURL   string   `json:"html_url"`
}

type ListIssuesResult struct {
	Issues      []IssueSummary `json:"issues"`
	Count       int            `json:"count"`
	HasNextPage bool           `json:"has_next_page"`
}

func NewListIssuesTool(k8sClient client.Client) *ListIssuesTool {
	return &ListIssuesTool{
		k8sClient: k8sClient,
	}
}

func (t *ListIssuesTool) Name() string {
	return listIssuesToolName
}

func (t *ListIssuesTool) Description() string {
	return "List open GitHub issues in a repository. " +
		"Use this to scan a repo for open issues to triage, prioritize, or assign to agents. " +
		"By default only returns unassigned issues. Returns issue number, title, body, labels, and author."
}

func (t *ListIssuesTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: workspaceTaskDescription}, repoURLField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Direct GitHub repository URL (e.g. 'https://github.com/owner/repo'). Falls back to ORKA_GIT_REPO env var if not provided."}, "unassigned_only": map[string]any{jsonSchemaTypeField: jsonSchemaTypeBoolean, jsonSchemaDescriptionField: "If true, only return issues with no assignee. Defaults to true."}, perPageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Number of results per page", jsonSchemaDefaultField: 30, jsonSchemaMinimumField: 1, jsonSchemaMaximumField: 100}, pageField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Page number for pagination (default: 1)"}}}
	data, _ := json.Marshal(schema)
	return data
}

const maxIssueBodyLength = 500

func (t *ListIssuesTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args ListIssuesArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	owner, repo, token, baseURL, err := resolveReadRepoAndToken(ctx, t.k8sClient, t.Name(), args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", err
	}

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

	respBody, err := readGitHubResponse(resp.Body, githubResponseLimit)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
	}

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
