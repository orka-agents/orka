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
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// PostReviewCommentTool posts a review on a GitHub pull request.
type PostReviewCommentTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// PostReviewCommentArgs are the arguments for the post_review_comment tool.
type PostReviewCommentArgs struct {
	// TaskName is the name of the child task to read workspace config from.
	TaskName string `json:"task_name"`
	// PRNumber is the GitHub PR number.
	PRNumber int `json:"pr_number"`
	// Body is the top-level review body text.
	Body string `json:"body"`
	// Event is one of reviewEventApprove, reviewEventRequestChanges, reviewEventComment.
	Event string `json:"event"`
	// Comments is an optional list of line-level review comments.
	Comments []ReviewComment `json:"comments,omitempty"`
}

// ReviewComment represents a line-level review comment on a PR diff.
type ReviewComment struct {
	// Path is the file path relative to the repo root.
	Path string `json:"path"`
	// Line is the line number in the diff (new file line number).
	Line int `json:"line"`
	// Body is the comment text.
	Body string `json:"body"`
}

// PostReviewCommentResult is the result of posting a review.
type PostReviewCommentResult struct {
	ReviewID int    `json:"review_id"`
	Status   string `json:"status"`
	HTMLURL  string `json:"html_url"`
}

// NewPostReviewCommentTool creates a new post_review_comment tool.
func NewPostReviewCommentTool(k8sClient client.Client) *PostReviewCommentTool {
	return &PostReviewCommentTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *PostReviewCommentTool) Name() string {
	return postReviewCommentToolName
}

// Description returns the tool description.
func (t *PostReviewCommentTool) Description() string {
	return "Post a review on a GitHub pull request with an optional verdict (APPROVE, REQUEST_CHANGES, COMMENT) " +
		"and line-level comments. Use this after analyzing a PR diff to submit your review feedback."
}

// Parameters returns the JSON schema for tool parameters.
func (t *PostReviewCommentTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: childWorkspaceTaskDescription}, githubPRNumberField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "GitHub pull request number"}, githubBodyField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Top-level review body text"},
		githubReviewEventField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaEnumField: []string{reviewEventApprove, reviewEventRequestChanges, reviewEventComment},
			jsonSchemaDescriptionField: "Review verdict: APPROVE, REQUEST_CHANGES, or COMMENT",
		},
		"comments": map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, jsonSchemaDescriptionField: "Optional line-level review comments", itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
			"path":          map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "File path relative to repo root"},
			"line":          map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Line number in the diff (new file line number)"},
			githubBodyField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Comment text"},
		}, jsonSchemaRequiredField: []string{"path", "line", githubBodyField},
		},
		},
	}, jsonSchemaRequiredField: []string{taskNameField, githubPRNumberField, githubBodyField, githubReviewEventField},
	}
	data, _ := json.Marshal(schema)
	return data
}

// Execute posts a review on a GitHub pull request.
func (t *PostReviewCommentTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args PostReviewCommentArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.PRNumber == 0 || args.Body == "" || args.Event == "" {
		return "", fmt.Errorf("task_name, pr_number, body, and event are required")
	}

	// Validate event value
	switch args.Event {
	case reviewEventApprove, reviewEventRequestChanges, reviewEventComment:
		// valid
	default:
		return "", fmt.Errorf("invalid event value %q: must be APPROVE, REQUEST_CHANGES, or COMMENT", args.Event)
	}

	// Determine namespace from environment
	ns := os.Getenv(envOrkaTaskNamespace)
	if ns == "" {
		ns = defaultNamespace
	}

	// Look up the child task to get workspace config
	var task corev1alpha1.Task
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: args.TaskName, Namespace: ns}, &task); err != nil {
		return "", fmt.Errorf("failed to get task %s: %w", args.TaskName, err)
	}

	// Extract repo URL and git secret from the task's workspace config
	ws := taskWorkspace(&task)
	if ws == nil {
		return "", fmt.Errorf("task %s does not have workspace configuration", args.TaskName)
	}

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
	for _, key := range []string{tokenKey, passwordKey} {
		if v, ok := secret.Data[key]; ok {
			token = strings.TrimSpace(string(v))
			break
		}
	}
	if token == "" {
		return "", fmt.Errorf("git secret %s does not contain a 'token' or 'password' key", ws.GitSecretRef.Name)
	}

	// Post the review via GitHub API
	reviewID, htmlURL, err := postGitHubReview(ctx, token, owner, repo, args.PRNumber, args.Body, args.Event, args.Comments, t.apiBaseURL)
	if err != nil {
		return "", fmt.Errorf("failed to post review: %w", err)
	}

	result := PostReviewCommentResult{
		ReviewID: reviewID,
		Status:   "submitted",
		HTMLURL:  htmlURL,
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// postGitHubReview posts a pull request review via the GitHub REST API.
func postGitHubReview(ctx context.Context, token, owner, repo string, prNumber int, body, event string, comments []ReviewComment, apiBaseURL string) (int, string, error) {
	baseURL := githubAPIBaseURL
	if apiBaseURL != "" {
		baseURL = apiBaseURL
	}
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", baseURL, owner, repo, prNumber)

	payload := map[string]any{
		githubBodyField:        body,
		githubReviewEventField: event,
	}
	if len(comments) > 0 {
		payload["comments"] = comments
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var reviewResp struct {
		ID      int    `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &reviewResp); err != nil {
		return 0, "", fmt.Errorf("failed to parse GitHub response: %w", err)
	}

	return reviewResp.ID, reviewResp.HTMLURL, nil
}
