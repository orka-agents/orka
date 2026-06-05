/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultPRReviewMarkerPrefix      = "<!-- orka:pr-review"
	prReviewMarkerSecretEnv          = "ORKA_PR_REVIEW_MARKER_SECRET"
	prReviewMarkerPreviousSecretsEnv = "ORKA_PR_REVIEW_MARKER_PREVIOUS_SECRETS"
	prReviewMarkerTrustedAuthorEnv   = "ORKA_PR_REVIEW_MARKER_TRUSTED_AUTHOR"
)

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

	if err := validateRepoURLScope(ctx, t.k8sClient, args.TaskName, args.RepoURL); err != nil {
		return "", err
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

	markerKeys := prReviewMarkerSigningKeys(token)
	marker := formatPRReviewMarker(owner, repo, args.PRNumber, headSHA, markerKeys[0])
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
	return findPRReviewMarkerInReviews(ctx, token, owner, repo, prNumber, headSHA, baseURL)
}

func findPRReviewMarkerInReviews(ctx context.Context, token, owner, repo string, prNumber int, headSHA, baseURL string) (*prReviewMarkerMatch, error) {
	const perPage = 100
	markerKeys := prReviewMarkerSigningKeys(token)
	trustedAuthor := trustedPRReviewMarkerAuthor(ctx, token, baseURL)
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=%d&page=%d", baseURL, owner, repo, prNumber, perPage, page)
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
			if containsPRReviewMarker(r.Body, owner, repo, prNumber, headSHA, markerKeys, r.User.Login, trustedAuthor) {
				return &prReviewMarkerMatch{Source: "review", HTMLURL: r.HTMLURL, Author: r.User.Login}, nil
			}
		}
		if len(reviews) < perPage {
			return nil, nil
		}
	}
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

func containsPRReviewMarker(body, owner, repo string, prNumber int, headSHA string, markerKeys []string, author, trustedAuthor string) bool {
	body = strings.TrimSpace(body)
	for _, key := range markerKeys {
		if key != "" && strings.Contains(body, formatPRReviewMarker(owner, repo, prNumber, headSHA, key)) {
			return true
		}
	}
	if trustedAuthor != "" && strings.EqualFold(strings.TrimSpace(author), trustedAuthor) {
		return containsAnyPRReviewMarkerForHead(body, headSHA)
	}
	return false
}

func formatPRReviewMarker(owner, repo string, prNumber int, headSHA, markerKey string) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.ToLower(strings.TrimSpace(repo))
	headSHA = strings.TrimSpace(headSHA)
	return fmt.Sprintf("%s repo=%s/%s pr=%d head_sha=%s sig=%s -->",
		defaultPRReviewMarkerPrefix,
		owner,
		repo,
		prNumber,
		headSHA,
		prReviewMarkerSignature(owner, repo, prNumber, headSHA, markerKey),
	)
}

func prReviewMarkerSigningKeys(token string) []string {
	primary := strings.TrimSpace(os.Getenv(prReviewMarkerSecretEnv))
	if primary == "" {
		primary = strings.TrimSpace(token)
	}
	keys := []string{primary}
	for previous := range strings.SplitSeq(os.Getenv(prReviewMarkerPreviousSecretsEnv), ",") {
		if previous = strings.TrimSpace(previous); previous != "" {
			keys = append(keys, previous)
		}
	}
	if token = strings.TrimSpace(token); token != "" && token != primary {
		keys = append(keys, token)
	}
	return keys
}

func trustedPRReviewMarkerAuthor(ctx context.Context, token, baseURL string) string {
	if author := strings.TrimSpace(os.Getenv(prReviewMarkerTrustedAuthorEnv)); author != "" {
		return author
	}
	login, err := githubAuthenticatedLogin(ctx, token, baseURL)
	if err != nil {
		return ""
	}
	return login
}

func githubAuthenticatedLogin(ctx context.Context, token, baseURL string) (string, error) {
	body, err := githubGet(ctx, token, baseURL+"/user")
	if err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return "", fmt.Errorf("failed to parse GitHub user response: %w", err)
	}
	return strings.TrimSpace(user.Login), nil
}

func containsAnyPRReviewMarkerForHead(body, headSHA string) bool {
	body = strings.TrimSpace(body)
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" || !strings.Contains(body, defaultPRReviewMarkerPrefix) {
		return false
	}
	legacyMarker := fmt.Sprintf("%s head_sha=%s -->", defaultPRReviewMarkerPrefix, headSHA)
	if strings.Contains(body, legacyMarker) {
		return true
	}
	return strings.Contains(body, "head_sha="+headSHA) && strings.Contains(body, "sig=")
}

func prReviewMarkerSignature(owner, repo string, prNumber int, headSHA, markerKey string) string {
	payload := fmt.Sprintf("%s/%s\n%d\n%s",
		strings.ToLower(strings.TrimSpace(owner)),
		strings.ToLower(strings.TrimSpace(repo)),
		prNumber,
		strings.TrimSpace(headSHA),
	)
	mac := hmac.New(sha256.New, []byte(markerKey))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func marshalCheckPRReviewMarkerResult(result CheckPRReviewMarkerResult) string {
	data, _ := json.Marshal(result)
	return string(data)
}
