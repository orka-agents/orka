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
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReviewPullRequestTool fetches a GitHub PR's diff and file changes for LLM review.
type ReviewPullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
	// maxResultBytes bounds the encoded result when positive.
	maxResultBytes int
}

// ReviewPullRequestArgs are the arguments for the review_pull_request tool.
type ReviewPullRequestArgs struct {
	// TaskName is an optional task to read workspace config and credentials from.
	TaskName string `json:"task_name,omitempty"`
	// RepoURL is an optional GitHub repository URL.
	RepoURL string `json:"repo_url,omitempty"`
	// PRNumber is the GitHub PR number to review.
	PRNumber int `json:"pr_number"`
}

// FileChange represents a changed file in a pull request.
type FileChange struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// ReviewPullRequestResult is the result of fetching a pull request for review.
type ReviewPullRequestResult struct {
	PRTitle    string       `json:"pr_title"`
	PRBody     string       `json:"pr_body"`
	PRAuthor   string       `json:"pr_author"`
	BaseBranch string       `json:"base_branch"`
	HeadBranch string       `json:"head_branch"`
	Diff       string       `json:"diff"`
	Files      []FileChange `json:"files"`
	Status     string       `json:"status"`
	// Truncated reports that the result was cut to fit a size budget; the
	// note says what was dropped.
	Truncated      bool   `json:"truncated,omitempty"`
	TruncationNote string `json:"truncation_note,omitempty"`
}

// WithMaxResultBytes bounds the encoded result: file patches are dropped
// first, then the unified diff is cut, so a large pull request returns
// usable partial data instead of a result too big for its transport.
func (t *ReviewPullRequestTool) WithMaxResultBytes(limit int) *ReviewPullRequestTool {
	t.maxResultBytes = limit
	return t
}

// boundReviewPullRequestResult trims result until its JSON encoding fits
// limit; a nonpositive limit leaves it unchanged.
func boundReviewPullRequestResult(result ReviewPullRequestResult, limit int) ReviewPullRequestResult {
	if limit <= 0 {
		return result
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) <= limit {
		return result
	}
	result.Truncated = true
	result.TruncationNote = "file patches omitted to fit the result size limit; the unified diff carries the changes"
	files := make([]FileChange, len(result.Files))
	for i, file := range result.Files {
		file.Patch = ""
		files[i] = file
	}
	result.Files = files
	encoded, _ = json.Marshal(result)
	for len(encoded) > limit && result.Diff != "" {
		excess := len(encoded) - limit
		cut := excess + excess/8 + 64
		if cut >= len(result.Diff) {
			result.Diff = ""
		} else {
			result.Diff = strings.ToValidUTF8(result.Diff[:len(result.Diff)-cut], "")
		}
		result.TruncationNote = "file patches omitted and the unified diff cut to fit the result size limit; fetch the remaining hunks separately"
		encoded, _ = json.Marshal(result)
	}
	return result
}

// NewReviewPullRequestTool creates a new review_pull_request tool.
func NewReviewPullRequestTool(k8sClient client.Client) *ReviewPullRequestTool {
	return &ReviewPullRequestTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *ReviewPullRequestTool) Name() string {
	return reviewPullRequestToolName
}

// Description returns the tool description.
func (t *ReviewPullRequestTool) Description() string {
	return "Fetch the diff and file changes of a GitHub pull request for code review. " +
		"Returns the full unified diff, individual file patches, PR metadata (title, body, author, branches), " +
		"and change statistics. Use this to analyze code changes before approving or requesting changes."
}

// Parameters returns the JSON schema for tool parameters.
func (t *ReviewPullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional task whose workspace config has the repo and git credentials"}, repoURLField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: scopedRepositoryURLDescription}, githubPRNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub pull request number to review"}}, jsonSchemaRequiredField: []string{githubPRNumberField}}
	data, _ := json.Marshal(schema)
	return data
}

// Execute fetches a pull request's diff and file changes from GitHub.
func (t *ReviewPullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args ReviewPullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.PRNumber == 0 {
		return "", fmt.Errorf("pr_number is required")
	}

	owner, repo, token, baseURL, err := resolveScopedReadRepoAndToken(ctx, t.k8sClient, t.Name(), args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", err
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Fetch PR details
	prTitle, prBody, prAuthor, baseBranch, headBranch, err := fetchPRDetails(ctx, httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR details: %w", err)
	}

	// Fetch PR diff
	diff, err := fetchPRDiff(ctx, httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR diff: %w", err)
	}

	// Fetch PR files
	files, err := fetchPRFiles(ctx, httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR files: %w", err)
	}

	result := ReviewPullRequestResult{
		PRTitle:    prTitle,
		PRBody:     prBody,
		PRAuthor:   prAuthor,
		BaseBranch: baseBranch,
		HeadBranch: headBranch,
		Diff:       diff,
		Files:      files,
		Status:     "fetched",
	}
	resultJSON, _ := json.Marshal(boundReviewPullRequestResult(result, t.maxResultBytes))
	return string(resultJSON), nil
}

// fetchPRDetails fetches PR metadata from the GitHub API.
func fetchPRDetails(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) (title, body, author, baseBranch, headBranch string, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := readGitHubResponse(resp.Body, githubResponseLimit)
	if err != nil {
		return "", "", "", "", "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", "", "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var prResp struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return "", "", "", "", "", fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return prResp.Title, prResp.Body, prResp.User.Login, prResp.Base.Ref, prResp.Head.Ref, nil
}

// fetchPRDiff fetches the unified diff of a PR.
func fetchPRDiff(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	return string(respBody), nil
}

// fetchPRFiles fetches the list of changed files in a PR.
func fetchPRFiles(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) ([]FileChange, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files", baseURL, owner, repo, prNumber)

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

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var filesResp []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Patch     string `json:"patch"`
	}
	if err := json.Unmarshal(respBody, &filesResp); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	files := make([]FileChange, len(filesResp))
	for i, f := range filesResp {
		files[i] = FileChange{
			Filename:  f.Filename,
			Status:    f.Status,
			Additions: f.Additions,
			Deletions: f.Deletions,
			Patch:     f.Patch,
		}
	}

	return files, nil
}
