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
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// ReviewPullRequestTool fetches a GitHub PR's diff and file changes for LLM review.
type ReviewPullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// ReviewPullRequestArgs are the arguments for the review_pull_request tool.
type ReviewPullRequestArgs struct {
	// TaskName is the name of the child task to read workspace config from.
	TaskName string `json:"task_name"`
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
}

// NewReviewPullRequestTool creates a new review_pull_request tool.
func NewReviewPullRequestTool(k8sClient client.Client) *ReviewPullRequestTool {
	return &ReviewPullRequestTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *ReviewPullRequestTool) Name() string {
	return "review_pull_request"
}

// Description returns the tool description.
func (t *ReviewPullRequestTool) Description() string {
	return "Fetch the diff and file changes of a GitHub pull request for code review. " +
		"Returns the full unified diff, individual file patches, PR metadata (title, body, author, branches), " +
		"and change statistics. Use this to analyze code changes before approving or requesting changes."
}

// Parameters returns the JSON schema for tool parameters.
func (t *ReviewPullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_name": map[string]any{
				"type":        "string",
				"description": "Name of the child task whose workspace config has the repo and git credentials",
			},
			"pr_number": map[string]any{
				"type":        "integer",
				"description": "GitHub pull request number to review",
			},
		},
		"required": []string{"task_name", "pr_number"},
	}
	data, _ := json.Marshal(schema)
	return data
}

// Execute fetches a pull request's diff and file changes from GitHub.
func (t *ReviewPullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args ReviewPullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.PRNumber == 0 {
		return "", fmt.Errorf("task_name and pr_number are required")
	}

	// Determine namespace from environment
	ns := os.Getenv("ORKA_TASK_NAMESPACE")
	if ns == "" {
		ns = defaultNamespace
	}

	// Look up the child task to get workspace config
	var task corev1alpha1.Task
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: args.TaskName, Namespace: ns}, &task); err != nil {
		return "", fmt.Errorf("failed to get task %s: %w", args.TaskName, err)
	}

	// Extract repo URL and git secret from the task's workspace config
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		return "", fmt.Errorf("task %s does not have workspace configuration", args.TaskName)
	}
	ws := task.Spec.AgentRuntime.Workspace

	repoURL := ws.GitRepo
	if repoURL == "" {
		return "", fmt.Errorf("task %s workspace has no gitRepo configured", args.TaskName)
	}

	// Parse owner/repo from the git URL
	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
	}

	// Get the git token from the referenced secret
	if ws.GitSecretRef == nil {
		return "", fmt.Errorf("task %s workspace has no gitSecretRef configured", args.TaskName)
	}

	var secret corev1.Secret
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: ws.GitSecretRef.Name, Namespace: ns}, &secret); err != nil {
		return "", fmt.Errorf("failed to get git secret %s: %w", ws.GitSecretRef.Name, err)
	}

	token := ""
	for _, key := range []string{"token", "password"} {
		if v, ok := secret.Data[key]; ok {
			token = strings.TrimSpace(string(v))
			break
		}
	}
	if token == "" {
		return "", fmt.Errorf("git secret %s does not contain a 'token' or 'password' key", ws.GitSecretRef.Name)
	}

	baseURL := githubAPIBaseURL
	if t.apiBaseURL != "" {
		baseURL = t.apiBaseURL
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Fetch PR details
	prTitle, prBody, prAuthor, baseBranch, headBranch, err := fetchPRDetails(httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR details: %w", err)
	}

	// Fetch PR diff
	diff, err := fetchPRDiff(httpClient, baseURL, token, owner, repo, args.PRNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR diff: %w", err)
	}

	// Fetch PR files
	files, err := fetchPRFiles(httpClient, baseURL, token, owner, repo, args.PRNumber)
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
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// fetchPRDetails fetches PR metadata from the GitHub API.
func fetchPRDetails(httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) (title, body, author, baseBranch, headBranch string, err error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequest(http.MethodGet, url, nil)
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

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

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
func fetchPRDiff(httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, owner, repo, prNumber)

	req, err := http.NewRequest(http.MethodGet, url, nil)
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
func fetchPRFiles(httpClient *http.Client, baseURL, token, owner, repo string, prNumber int) ([]FileChange, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files", baseURL, owner, repo, prNumber)

	req, err := http.NewRequest(http.MethodGet, url, nil)
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
