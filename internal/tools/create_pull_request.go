/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreatePullRequestTool creates a GitHub pull request from a pushed branch.
type CreatePullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string // override for testing; empty uses https://api.github.com
}

// CreatePullRequestArgs are the arguments for the create_pull_request tool.
type CreatePullRequestArgs struct {
	// TaskName is the name of the completed child task whose branch to use.
	// The tool reads the task's workspace config to determine repo and git secret.
	TaskName string `json:"task_name"`
	// HeadBranch is the remote branch that contains the changes.
	HeadBranch string `json:"head_branch"`
	// BaseBranch is the target branch to merge into (e.g. "main").
	BaseBranch string `json:"base_branch"`
	// Title is the pull request title.
	Title string `json:"title"`
	// Body is the pull request body (Markdown).
	Body string `json:"body"`
}

// CreatePullRequestResult is the result of creating a pull request.
type CreatePullRequestResult struct {
	PRURL    string `json:"pr_url"`
	PRNumber int    `json:"pr_number"`
	Status   string `json:"status"`
}

// NewCreatePullRequestTool creates a new create_pull_request tool.
func NewCreatePullRequestTool(k8sClient client.Client) *CreatePullRequestTool {
	return &CreatePullRequestTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name.
func (t *CreatePullRequestTool) Name() string {
	return createPullRequestToolName
}

// Description returns the tool description.
func (t *CreatePullRequestTool) Description() string {
	return "Create a GitHub pull request from a branch that was pushed by a completed agent task. " +
		"Use this after a coder task with pushBranch has succeeded and been reviewed."
}

// Parameters returns the JSON schema for tool parameters.
func (t *CreatePullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Name of the completed child task whose workspace config has the repo and git credentials"}, "head_branch": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "The branch containing the changes (the pushBranch used by the coder)"},
		"base_branch": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "The target branch to merge into (e.g. 'main')"}, titleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Pull request title"}, githubBodyField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Pull request body in Markdown format"},
	}, jsonSchemaRequiredField: []string{taskNameField, "head_branch", "base_branch", titleField},
	}
	data, _ := json.Marshal(schema)
	return data
}

// Execute creates a pull request on GitHub.
func (t *CreatePullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args CreatePullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.TaskName == "" || args.HeadBranch == "" || args.BaseBranch == "" || args.Title == "" {
		return "", fmt.Errorf("task_name, head_branch, base_branch, and title are required")
	}
	target, err := resolveCreatePullRequestApprovalTarget(ctx, t.k8sClient, args, t.apiBaseURL)
	if err != nil {
		return "", err
	}
	approvalTaskName := strings.TrimSpace(args.TaskName)
	if tc := GetToolContext(ctx); tc != nil && strings.TrimSpace(tc.TaskID) != "" {
		approvalTaskName = strings.TrimSpace(tc.TaskID)
	}
	approval, err := requireToolApproval(ctx, toolApprovalRequest{
		Action:           createPullRequestApprovalAction,
		ToolName:         createPullRequestToolName,
		RiskSummary:      createPullRequestRiskSummary(args, target),
		SafeSummary:      "approval required to create pull request",
		Seed:             createPullRequestApprovalSeed(args, target),
		ApprovalTaskName: approvalTaskName,
		SafeContent: map[string]any{
			"targetTaskName": args.TaskName,
			"headBranch":     args.HeadBranch,
			"baseBranch":     args.BaseBranch,
			"title":          args.Title,
			"repository":     target.Repository(),
			"apiBaseURL":     target.BaseURL,
		},
	})
	if err != nil {
		return "", err
	}
	if !approval.Approved {
		return approval.Result, nil
	}
	latestTarget, err := resolveCreatePullRequestApprovalTarget(ctx, t.k8sClient, args, t.apiBaseURL)
	if err != nil {
		return "", err
	}
	if latestTarget != target {
		return "", fmt.Errorf("approved pull request target changed from %s to %s", target.Repository(), latestTarget.Repository())
	}

	owner, repo, token, baseURL, err := resolveRepoAndToken(ctx, t.k8sClient, args.TaskName, "", t.apiBaseURL)
	if err != nil {
		return "", err
	}

	// Create the pull request via GitHub API
	prURL, prNumber, status, err := createGitHubPR(ctx, token, owner, repo, args.HeadBranch, args.BaseBranch, args.Title, args.Body, baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to create pull request: %w", err)
	}

	result := CreatePullRequestResult{
		PRURL:    prURL,
		PRNumber: prNumber,
		Status:   status,
	}
	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

func createPullRequestApprovalSeed(args CreatePullRequestArgs, target createPullRequestApprovalTarget) string {
	bodyDigest := sha256.Sum256([]byte(args.Body))
	seed := map[string]string{
		"taskName":   strings.TrimSpace(args.TaskName),
		"headBranch": strings.TrimSpace(args.HeadBranch),
		"baseBranch": strings.TrimSpace(args.BaseBranch),
		"title":      strings.TrimSpace(args.Title),
		"bodySHA256": hex.EncodeToString(bodyDigest[:]),
		"repository": target.Repository(),
		"apiBaseURL": target.BaseURL,
	}
	data, err := json.Marshal(seed)
	if err != nil {
		return strings.Join([]string{seed["taskName"], seed["headBranch"], seed["baseBranch"], seed["title"], seed["bodySHA256"], seed["repository"], seed["apiBaseURL"]}, "|")
	}
	return string(data)
}

func createPullRequestRiskSummary(args CreatePullRequestArgs, target createPullRequestApprovalTarget) string {
	return fmt.Sprintf(
		"Create a GitHub pull request in %s from branch %q into %q using completed task %q.",
		target.Repository(),
		strings.TrimSpace(args.HeadBranch),
		strings.TrimSpace(args.BaseBranch),
		strings.TrimSpace(args.TaskName),
	)
}

type createPullRequestApprovalTarget struct {
	Owner   string
	Repo    string
	BaseURL string
}

func (t createPullRequestApprovalTarget) Repository() string {
	return strings.TrimSpace(t.Owner) + "/" + strings.TrimSpace(t.Repo)
}

func resolveCreatePullRequestApprovalTarget(
	ctx context.Context,
	k8sClient client.Client,
	args CreatePullRequestArgs,
	overrideBaseURL string,
) (createPullRequestApprovalTarget, error) {
	baseURL := githubAPIBaseURL
	if strings.TrimSpace(overrideBaseURL) != "" {
		baseURL = strings.TrimSpace(overrideBaseURL)
	}
	taskContext, err := loadGitHubTaskContext(ctx, k8sClient, args.TaskName)
	if err != nil {
		return createPullRequestApprovalTarget{}, err
	}
	if len(taskContext.scopes) == 0 {
		return createPullRequestApprovalTarget{}, fmt.Errorf("task %s workspace has no GitHub repository scope", strings.TrimSpace(args.TaskName))
	}
	return createPullRequestApprovalTarget{
		Owner:   taskContext.scopes[0].owner,
		Repo:    taskContext.scopes[0].repo,
		BaseURL: baseURL,
	}, nil
}

// parseGitHubRepo extracts owner and repo name from a GitHub URL.
// Supports https://github.com/owner/repo.git and similar formats.
func parseGitHubRepo(repoURL string) (string, string, error) {
	// Normalize
	repoURL = strings.TrimSuffix(repoURL, ".git")
	repoURL = strings.TrimSuffix(repoURL, "/")

	// Try HTTPS format: https://github.com/owner/repo
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if after, ok := strings.CutPrefix(repoURL, prefix); ok {
			path := after
			parts := strings.SplitN(path, "/", 3)
			if len(parts) >= 2 {
				return parts[0], parts[1], nil
			}
		}
	}

	// Try SSH format: git@github.com:owner/repo
	if after, ok := strings.CutPrefix(repoURL, "git@github.com:"); ok {
		path := after
		parts := strings.SplitN(path, "/", 3)
		if len(parts) >= 2 {
			return parts[0], parts[1], nil
		}
	}

	return "", "", fmt.Errorf("unsupported GitHub URL format: %s", repoURL)
}

// createGitHubPR creates a pull request via the GitHub REST API.
// If the pull request already exists, it resolves and returns the existing PR.
func createGitHubPR(ctx context.Context, token, owner, repo, head, base, title, body, apiBaseURL string) (string, int, string, error) {
	pr, err := CreateOrGetGitHubPullRequest(ctx, token, owner, repo, head, base, title, body, apiBaseURL)
	if err != nil {
		return "", 0, "", err
	}

	return pr.HTMLURL, pr.Number, pr.Status, nil
}
