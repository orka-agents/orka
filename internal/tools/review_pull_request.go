/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	listNote := ""
	if result.Truncated {
		listNote = result.TruncationNote + "; "
	}
	result.Truncated = true
	result.TruncationNote = listNote + "file patches omitted to fit the result size limit; the unified diff carries the changes"
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
		result.TruncationNote = listNote + "file patches omitted and the unified diff cut to fit the result size limit; fetch the remaining hunks separately"
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
	files, complete, err := fetchPRFiles(ctx, httpClient, baseURL, token, owner, repo, args.PRNumber)
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
	if !complete {
		result.Truncated = true
		result.TruncationNote = fmt.Sprintf("only the first %d changed files are listed; the unified diff carries the changes", len(files))
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
		return "", "", "", "", "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
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

	// A diff past the limit is refused rather than silently cut: a cut
	// diff would read as the whole change. Below it, the result bound trims
	// the diff and says so.
	respBody, err := readGitHubResponse(resp.Body, prDiffResponseLimit)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
	}

	return string(respBody), nil
}

// prDiffResponseLimit bounds the unified diff read for a pull request.
const prDiffResponseLimit int64 = 10 << 20

// prFilesPerPage and maxPRFilePages bound the changed-file pages one
// review_pull_request call reads; maxPRFilesBytes bounds what those pages
// may hold together.
const (
	prFilesPerPage = 100
	maxPRFilePages = 10
)

var maxPRFilesBytes = maxPRFilePages * githubResponseLimit

// prFilesSplitSizes are the smaller page sizes a changed-file page past the
// document limit is re-read with, each dividing the one before it, so the
// smaller pages cover exactly the files of the page they replace.
var prFilesSplitSizes = []int{25, 5, 1}

// fetchPRFiles reads the pull request's changed files page by page, up to
// maxPRFilePages pages and maxPRFilesBytes in all; complete is false when
// either cap cut the list, so more files may exist. A page whose patches
// exceed the document limit is re-read as smaller pages rather than
// failing the call, and the result budget then trims those patches.
func fetchPRFiles(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) (files []FileChange, complete bool, err error) {
	budget := maxPRFilesBytes
	for page := 1; page <= maxPRFilePages; page++ {
		pageFiles, more, err := fetchPRFilesSpan(ctx, httpClient, baseURL, token, owner, repo, prNumber, prFilesPerPage, page, prFilesSplitSizes, &budget)
		if err != nil {
			return nil, false, err
		}
		files = append(files, pageFiles...)
		if !more {
			return files, true, nil
		}
		if budget <= 0 {
			return files, false, nil
		}
	}
	return files, false, nil
}

// fetchPRFilesSpan reads the changed files that page covers at perPage files
// per page, splitting it into smaller pages when it is past the document
// limit. more reports that the list may continue past what was read: the
// span was full, or the byte budget cut it short.
func fetchPRFilesSpan(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, prNumber, perPage, page int, smaller []int, budget *int64) (files []FileChange, more bool, err error) {
	files, read, err := fetchPRFilesPage(ctx, httpClient, baseURL, token, owner, repo, prNumber, perPage, page)
	if err == nil {
		*budget -= read
		return files, len(files) == perPage, nil
	}
	if !errors.Is(err, errGitHubResponseTooLarge) || len(smaller) == 0 {
		return nil, false, err
	}
	size := smaller[0]
	parts := perPage / size
	for part := range parts {
		partFiles, partMore, err := fetchPRFilesSpan(ctx, httpClient, baseURL, token, owner, repo, prNumber, size, (page-1)*parts+part+1, smaller[1:], budget)
		if err != nil {
			return nil, false, err
		}
		files = append(files, partFiles...)
		if !partMore {
			return files, false, nil
		}
		if *budget <= 0 {
			return files, true, nil
		}
	}
	return files, true, nil
}

func fetchPRFilesPage(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, prNumber, perPage, page int) ([]FileChange, int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=%d&page=%d", baseURL, owner, repo, prNumber, perPage, page)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// One page of file entries is a JSON document: an oversized page is
	// refused before decoding, never parsed from a cut prefix.
	respBody, err := readGitHubResponse(resp.Body, githubResponseLimit)
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
	}

	var filesResp []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Patch     string `json:"patch"`
	}
	if err := json.Unmarshal(respBody, &filesResp); err != nil {
		return nil, 0, fmt.Errorf("failed to parse GitHub response: %w", err)
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

	return files, int64(len(respBody)), nil
}
