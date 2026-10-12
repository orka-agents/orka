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
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const getIssueToolName = "get_issue"

// GetIssueTool fetches full details of a specific GitHub issue including comments.
type GetIssueTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
	// maxResultBytes bounds the encoded result when positive.
	maxResultBytes int
}

// WithMaxResultBytes bounds the encoded result: the oldest comments are
// dropped first, then the body is cut, so a long discussion returns usable
// partial data instead of a result too big for its transport.
func (t *GetIssueTool) WithMaxResultBytes(limit int) *GetIssueTool {
	t.maxResultBytes = limit
	return t
}

// boundGetIssueResult trims result until its JSON encoding fits limit; a
// nonpositive limit leaves it unchanged. comment_count keeps the true total.
func boundGetIssueResult(result GetIssueResult, limit int) GetIssueResult {
	if limit <= 0 {
		return result
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) <= limit {
		return result
	}
	result.Truncated = true
	result.Comments = append([]IssueComment(nil), result.Comments...)
	for len(encoded) > limit && len(result.Comments) > 0 {
		result.Comments = result.Comments[1:]
		result.TruncationNote = fmt.Sprintf("the oldest comments were omitted to fit the result size limit; %d of %d comments remain", len(result.Comments), result.CommentCount)
		encoded, _ = json.Marshal(result)
	}
	for len(encoded) > limit && result.Body != "" {
		excess := len(encoded) - limit
		cut := excess + excess/8 + 64
		if cut >= len(result.Body) {
			result.Body = ""
		} else {
			result.Body = strings.ToValidUTF8(result.Body[:len(result.Body)-cut], "")
		}
		result.TruncationNote = "comments omitted and the body cut to fit the result size limit; read the issue directly for the rest"
		encoded, _ = json.Marshal(result)
	}
	return result
}

// maxGitHubErrorNoteBytes bounds a GitHub error quoted in a result or an
// error: it can carry a response body of up to the response limit, which a
// result's size bound never trims.
const maxGitHubErrorNoteBytes = 512

// boundedNote cuts text to at most maxGitHubErrorNoteBytes of valid UTF-8,
// marking a cut.
func boundedNote(text string) string {
	if len(text) <= maxGitHubErrorNoteBytes {
		return text
	}
	return strings.ToValidUTF8(text[:maxGitHubErrorNoteBytes], "") + "…"
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
	// Truncated reports that the result was cut to fit a size budget; the
	// note says what was dropped.
	Truncated      bool   `json:"truncated,omitempty"`
	TruncationNote string `json:"truncation_note,omitempty"`
}

// NewGetIssueTool creates a new get_issue tool.
func NewGetIssueTool(k8sClient client.Client) *GetIssueTool {
	return &GetIssueTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *GetIssueTool) Name() string {
	return getIssueToolName
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

	owner, repo, token, baseURL, err := resolveReadRepoAndToken(ctx, t.k8sClient, t.Name(), args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", fmt.Errorf("failed to resolve repo and token: %w", err)
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Fetch issue details
	issueResult, err := fetchIssueDetails(ctx, httpClient, baseURL, token, owner, repo, args.IssueNumber)
	if err != nil {
		return "", fmt.Errorf("failed to fetch issue: %w", err)
	}

	// Fetch comments (non-fatal on failure, but never silent)
	comments, err := fetchIssueComments(ctx, httpClient, baseURL, token, owner, repo, args.IssueNumber, issueResult.CommentCount)
	if err == nil {
		issueResult.Comments = comments
		if len(comments) < issueResult.CommentCount {
			issueResult.Truncated = true
			issueResult.TruncationNote = fmt.Sprintf("only the newest %d of %d comments were fetched", len(comments), issueResult.CommentCount)
		}
	} else {
		issueResult.Truncated = true
		issueResult.TruncationNote = "comments could not be fetched: " + boundedNote(err.Error())
	}

	resultJSON, _ := json.Marshal(boundGetIssueResult(*issueResult, t.maxResultBytes))
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

	respBody, err := readGitHubResponse(resp.Body, githubResponseLimit)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
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
// issueCommentsPerPage and maxIssueCommentPages bound the comment pages one
// get_issue call reads: the newest pages first, since a size-bounded result
// keeps the newest comments.
const (
	issueCommentsPerPage = 100
	maxIssueCommentPages = 5
)

// fetchIssueComments reads up to maxIssueCommentPages pages of an issue's
// comments, ending at the newest page for total comments.
func fetchIssueComments(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, issueNumber, total int) ([]IssueComment, error) {
	lastPage := max(1, (total+issueCommentsPerPage-1)/issueCommentsPerPage)
	firstPage := max(1, lastPage-maxIssueCommentPages+1)
	var comments []IssueComment
	for page := firstPage; page <= lastPage; page++ {
		pageComments, err := fetchIssueCommentsPage(ctx, httpClient, baseURL, token, owner, repo, issueNumber, page)
		if err != nil {
			return nil, err
		}
		comments = append(comments, pageComments...)
		if len(pageComments) < issueCommentsPerPage {
			break
		}
	}
	return comments, nil
}

func fetchIssueCommentsPage(ctx context.Context, httpClient *http.Client, baseURL, token, owner, repo string, issueNumber, page int) ([]IssueComment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=%d&page=%d", baseURL, owner, repo, issueNumber, issueCommentsPerPage, page)

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

	respBody, err := readGitHubResponse(resp.Body, githubResponseLimit)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, boundedNote(string(respBody)))
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
