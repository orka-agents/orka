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

const defaultPRReviewMarkerPrefix = "<!-- orka:pr-review"

// CheckPRReviewMarkerTool checks whether Orka has already reviewed a PR head SHA.
type CheckPRReviewMarkerTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// CheckPRReviewMarkerArgs are the arguments for check_pr_review_marker.
type CheckPRReviewMarkerArgs struct {
	TaskName string `json:"task_name,omitempty"`
	RepoURL  string `json:"repo_url,omitempty"`
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha,omitempty"`
}

// CheckPRReviewMarkerResult is the result of checking review markers.
type CheckPRReviewMarkerResult struct {
	Found    bool   `json:"found"`
	PRNumber int    `json:"pr_number"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Marker   string `json:"marker,omitempty"`
	Source   string `json:"source,omitempty"`
	HTMLURL  string `json:"html_url,omitempty"`
	Author   string `json:"author,omitempty"`
	Message  string `json:"message"`
}

// NewCheckPRReviewMarkerTool creates a new check_pr_review_marker tool.
func NewCheckPRReviewMarkerTool(k8sClient client.Client) *CheckPRReviewMarkerTool {
	return &CheckPRReviewMarkerTool{k8sClient: k8sClient}
}

func (t *CheckPRReviewMarkerTool) Name() string { return checkPRReviewMarkerToolName }

func (t *CheckPRReviewMarkerTool) Description() string {
	return "Check whether a pull request already has an Orka review marker for its current head SHA. " +
		"Use this before reviewing a PR to avoid duplicate reviews. If head_sha is omitted, the tool fetches the current PR head SHA."
}

func (t *CheckPRReviewMarkerTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		jsonSchemaTypeField: jsonSchemaTypeObject,
		jsonSchemaPropertiesField: map[string]any{
			taskNameField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional task whose workspace config has the repo and git credentials",
			},
			repoURLField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional GitHub repository URL. Falls back to ORKA_GIT_REPO when task_name is empty",
			},
			githubPRNumberField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeInteger,
				jsonSchemaDescriptionField: "GitHub pull request number to inspect",
			},
			headSHAField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional PR head SHA to check. If omitted, the current PR head SHA is fetched from GitHub",
			},
		},
		jsonSchemaRequiredField: []string{githubPRNumberField},
	})
}

func (t *CheckPRReviewMarkerTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args CheckPRReviewMarkerArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}
	if args.PRNumber == 0 {
		return "", fmt.Errorf("pr_number is required")
	}

	owner, repo, token, baseURL, err := resolveRepoAndToken(ctx, t.k8sClient, args.TaskName, args.RepoURL, t.apiBaseURL)
	if err != nil {
		return "", err
	}

	headSHA := strings.TrimSpace(args.HeadSHA)
	if headSHA == "" {
		var state string
		var merged bool
		headSHA, state, merged, err = getGitHubPRDetails(ctx, token, owner, repo, args.PRNumber, baseURL)
		if err != nil {
			return "", fmt.Errorf("failed to get PR details: %w", err)
		}
		if merged {
			return marshalCheckPRReviewMarkerResult(CheckPRReviewMarkerResult{
				PRNumber: args.PRNumber,
				HeadSHA:  headSHA,
				Message:  "pull request is already merged",
			}), nil
		}
		if state == githubPRStateClosed {
			return marshalCheckPRReviewMarkerResult(CheckPRReviewMarkerResult{
				PRNumber: args.PRNumber,
				HeadSHA:  headSHA,
				Message:  "pull request is closed",
			}), nil
		}
	}

	marker := formatPRReviewMarker(headSHA)
	match, err := findPRReviewMarker(ctx, token, owner, repo, args.PRNumber, headSHA, baseURL)
	if err != nil {
		return "", err
	}
	if match != nil {
		return marshalCheckPRReviewMarkerResult(CheckPRReviewMarkerResult{
			Found:    true,
			PRNumber: args.PRNumber,
			HeadSHA:  headSHA,
			Marker:   marker,
			Source:   match.Source,
			HTMLURL:  match.HTMLURL,
			Author:   match.Author,
			Message:  "review marker found for this PR head SHA",
		}), nil
	}

	return marshalCheckPRReviewMarkerResult(CheckPRReviewMarkerResult{
		Found:    false,
		PRNumber: args.PRNumber,
		HeadSHA:  headSHA,
		Marker:   marker,
		Message:  "no review marker found for this PR head SHA",
	}), nil
}

type prReviewMarkerMatch struct {
	Source  string
	HTMLURL string
	Author  string
}

func findPRReviewMarker(ctx context.Context, token, owner, repo string, prNumber int, headSHA, baseURL string) (*prReviewMarkerMatch, error) {
	if match, err := findPRReviewMarkerInIssueComments(ctx, token, owner, repo, prNumber, headSHA, baseURL); err != nil || match != nil {
		return match, err
	}
	return findPRReviewMarkerInReviews(ctx, token, owner, repo, prNumber, headSHA, baseURL)
}

func findPRReviewMarkerInIssueComments(ctx context.Context, token, owner, repo string, prNumber int, headSHA, baseURL string) (*prReviewMarkerMatch, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=100", baseURL, owner, repo, prNumber)
	body, err := githubGet(ctx, token, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to list PR comments: %w", err)
	}
	var comments []struct {
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &comments); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub comments response: %w", err)
	}
	for _, c := range comments {
		if containsPRReviewMarker(c.Body, headSHA) {
			return &prReviewMarkerMatch{Source: "issue_comment", HTMLURL: c.HTMLURL, Author: c.User.Login}, nil
		}
	}
	return nil, nil
}

func findPRReviewMarkerInReviews(ctx context.Context, token, owner, repo string, prNumber int, headSHA, baseURL string) (*prReviewMarkerMatch, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", baseURL, owner, repo, prNumber)
	body, err := githubGet(ctx, token, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to list PR reviews: %w", err)
	}
	var reviews []struct {
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &reviews); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub reviews response: %w", err)
	}
	for _, r := range reviews {
		if containsPRReviewMarker(r.Body, headSHA) {
			return &prReviewMarkerMatch{Source: "review", HTMLURL: r.HTMLURL, Author: r.User.Login}, nil
		}
	}
	return nil, nil
}

func githubGet(ctx context.Context, token, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, githubAPIResponseLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func containsPRReviewMarker(body, headSHA string) bool {
	body = strings.TrimSpace(body)
	headSHA = strings.TrimSpace(headSHA)
	return strings.Contains(body, defaultPRReviewMarkerPrefix) && (headSHA == "" || strings.Contains(body, headSHA))
}

func formatPRReviewMarker(headSHA string) string {
	return fmt.Sprintf("%s head_sha=%s -->", defaultPRReviewMarkerPrefix, strings.TrimSpace(headSHA))
}

func marshalCheckPRReviewMarkerResult(result CheckPRReviewMarkerResult) string {
	data, _ := json.Marshal(result)
	return string(data)
}
