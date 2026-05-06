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
	"net/url"
	"strings"
	"time"
)

const (
	GitHubPullRequestStatusCreated  = "created"
	GitHubPullRequestStatusExisting = "existing"
	githubAPIResponseLimit          = 1 << 20
)

// GitHubPullRequest captures the canonical URL, number, and resolution status.
type GitHubPullRequest struct {
	HTMLURL string
	Number  int
	Status  string
}

type gitHubPullRequestResponse struct {
	HTMLURL string `json:"html_url"`
	Number  int    `json:"number"`
}

type gitHubAPIErrorResponse struct {
	Message string `json:"message"`
	Errors  []struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Field   string `json:"field"`
	} `json:"errors"`
}

// CreateOrGetGitHubPullRequest creates a pull request via the GitHub REST API.
// If GitHub reports that the pull request already exists, it resolves and returns
// the existing open pull request instead.
func CreateOrGetGitHubPullRequest(ctx context.Context, token, owner, repo, head, base, title, body, apiBaseURL string) (GitHubPullRequest, error) {
	baseURL := githubAPIBaseURL
	if apiBaseURL != "" {
		baseURL = apiBaseURL
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", baseURL, owner, repo)
	payload := map[string]string{
		"title": title,
		"body":  body,
		"head":  head,
		"base":  base,
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payloadBytes))
	if err != nil {
		return GitHubPullRequest{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return GitHubPullRequest{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, githubAPIResponseLimit))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		pr, err := parseGitHubPullRequestResponse(respBody)
		if err != nil {
			return GitHubPullRequest{}, err
		}
		pr.Status = GitHubPullRequestStatusCreated
		return pr, nil
	}

	if resp.StatusCode == http.StatusUnprocessableEntity && isExistingPullRequestError(respBody) {
		pr, lookupErr := findExistingGitHubPullRequest(ctx, token, owner, repo, head, base, baseURL)
		if lookupErr != nil {
			return GitHubPullRequest{}, fmt.Errorf("GitHub reported an existing pull request, but lookup failed: %w", lookupErr)
		}
		pr.Status = GitHubPullRequestStatusExisting
		return pr, nil
	}

	return GitHubPullRequest{}, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
}

func findExistingGitHubPullRequest(ctx context.Context, token, owner, repo, head, base, baseURL string) (GitHubPullRequest, error) {
	query := url.Values{}
	query.Set("state", "open")
	query.Set("head", qualifyGitHubHead(owner, head))
	if strings.TrimSpace(base) != "" {
		query.Set("base", base)
	}
	query.Set("per_page", "10")

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", baseURL, owner, repo, query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return GitHubPullRequest{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return GitHubPullRequest{}, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, githubAPIResponseLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GitHubPullRequest{}, fmt.Errorf("GitHub API returned %d while resolving existing pull request: %s", resp.StatusCode, string(respBody))
	}

	var pulls []gitHubPullRequestResponse
	if err := json.Unmarshal(respBody, &pulls); err != nil {
		return GitHubPullRequest{}, fmt.Errorf("failed to parse GitHub response: %w", err)
	}
	if len(pulls) == 0 {
		return GitHubPullRequest{}, fmt.Errorf("no open pull request found for head %q and base %q", head, base)
	}

	return GitHubPullRequest{
		HTMLURL: pulls[0].HTMLURL,
		Number:  pulls[0].Number,
	}, nil
}

func parseGitHubPullRequestResponse(respBody []byte) (GitHubPullRequest, error) {
	var prResp gitHubPullRequestResponse
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return GitHubPullRequest{}, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return GitHubPullRequest{
		HTMLURL: prResp.HTMLURL,
		Number:  prResp.Number,
	}, nil
}

func isExistingPullRequestError(respBody []byte) bool {
	var apiErr gitHubAPIErrorResponse
	if err := json.Unmarshal(respBody, &apiErr); err == nil {
		if containsExistingPullRequestText(apiErr.Message) {
			return true
		}
		for _, item := range apiErr.Errors {
			if containsExistingPullRequestText(item.Message) {
				return true
			}
		}
	}

	return containsExistingPullRequestText(string(respBody))
}

func containsExistingPullRequestText(message string) bool {
	return strings.Contains(strings.ToLower(message), "pull request already exists")
}

func qualifyGitHubHead(owner, head string) string {
	if strings.Contains(head, ":") {
		return head
	}
	return owner + ":" + head
}
