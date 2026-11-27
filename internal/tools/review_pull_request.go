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

type ReviewPullRequestTool struct {
	k8sClient      client.Client
	apiBaseURL     string
	maxResultBytes int
}

type ReviewPullRequestArgs struct {
	TaskName string `json:"task_name,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	PRNumber int    `json:"pr_number"`
}

type FileChange struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

type ReviewPullRequestResult struct {
	PRTitle        string       `json:"pr_title"`
	PRBody         string       `json:"pr_body"`
	PRAuthor       string       `json:"pr_author"`
	BaseBranch     string       `json:"base_branch"`
	HeadBranch     string       `json:"head_branch"`
	Diff           string       `json:"diff"`
	Files          []FileChange `json:"files"`
	Status         string       `json:"status"`
	Truncated      bool         `json:"truncated,omitempty"`
	TruncationNote string       `json:"truncation_note,omitempty"`
}

func (t *ReviewPullRequestTool) WithMaxResultBytes(limit int) *ReviewPullRequestTool {
	t.maxResultBytes = limit
	return t
}

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

func NewReviewPullRequestTool(k8sClient client.Client) *ReviewPullRequestTool {
	return &ReviewPullRequestTool{
		k8sClient: k8sClient,
	}
}

func (t *ReviewPullRequestTool) Name() string {
	return reviewPullRequestToolName
}

func (t *ReviewPullRequestTool) Description() string {
	return "Fetch the diff and file changes of a GitHub pull request for code review. " +
		"Returns the full unified diff, individual file patches, PR metadata (title, body, author, branches), " +
		"and change statistics. Use this to analyze code changes before approving or requesting changes."
}

func (t *ReviewPullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional task whose workspace config has the repo and git credentials"}, repoURLField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: scopedRepositoryURLDescription}, githubPRNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub pull request number to review"}}, jsonSchemaRequiredField: []string{githubPRNumberField}}
	data, _ := json.Marshal(schema)
	return data
}

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

	prTitle, prBody, prAuthor, baseBranch, headBranch, err := fetchPRDetails(ctx, httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR details: %w", err)
	}

	diff, err := fetchPRDiff(ctx, httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR diff: %w", err)
	}

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

	respBody, err := readGitHubResponse(resp.Body, prDiffResponseLimit)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
	}

	return string(respBody), nil
}

const prDiffResponseLimit int64 = 10 << 20

const (
	prFilesPerPage = 100
	maxPRFilePages = 10
)

var maxPRFilesBytes = maxPRFilePages * githubResponseLimit

var prFilesSplitSizes = []int{25, 5, 1}

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
