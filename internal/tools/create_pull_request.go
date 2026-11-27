/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CreatePullRequestTool struct {
	k8sClient  client.Client
	apiBaseURL string
}

type CreatePullRequestArgs struct {
	TaskName   string `json:"task_name"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	Title      string `json:"title"`
	Body       string `json:"body"`
}

type CreatePullRequestResult struct {
	PRURL    string `json:"pr_url"`
	PRNumber int    `json:"pr_number"`
	Status   string `json:"status"`
}

func NewCreatePullRequestTool(k8sClient client.Client) *CreatePullRequestTool {
	return &CreatePullRequestTool{
		k8sClient: k8sClient,
	}
}

func (t *CreatePullRequestTool) Name() string {
	return createPullRequestToolName
}

func (t *CreatePullRequestTool) Description() string {
	return "Create a GitHub pull request from a branch that was pushed by a completed agent task. " +
		"Use this after a coder task with pushBranch has succeeded and been reviewed."
}

func (t *CreatePullRequestTool) Parameters() json.RawMessage {
	schema := map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{taskNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Name of the completed child task whose workspace config has the repo and git credentials"}, "head_branch": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "The branch containing the changes (the pushBranch used by the coder)"},
		"base_branch": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "The target branch to merge into (e.g. 'main')"}, titleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Pull request title"}, githubBodyField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Pull request body in Markdown format"},
	}, jsonSchemaRequiredField: []string{taskNameField, "head_branch", "base_branch", titleField},
	}
	data, _ := json.Marshal(schema)
	return data
}

func (t *CreatePullRequestTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	var args CreatePullRequestArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", notSent(fmt.Errorf("failed to parse arguments: %w", err))
	}

	if args.TaskName == "" || args.HeadBranch == "" || args.BaseBranch == "" || args.Title == "" {
		return "", notSent(fmt.Errorf("task_name, head_branch, base_branch, and title are required"))
	}

	owner, repo, token, baseURL, err := resolveForgeRepoAndToken(ctx, t.k8sClient, t.Name(), args.TaskName, "", t.apiBaseURL)
	if err != nil {
		return "", notSent(err)
	}

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

func ParseGitHubRepository(repoURL string) (owner, repo string, err error) {
	return parseGitHubRepo(repoURL)
}

func parseGitHubRepo(repoURL string) (string, string, error) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	repoURL = strings.TrimSuffix(repoURL, "/")

	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if after, ok := strings.CutPrefix(repoURL, prefix); ok {
			path := after
			parts := strings.SplitN(path, "/", 3)
			if len(parts) >= 2 {
				return parts[0], parts[1], nil
			}
		}
	}

	if after, ok := strings.CutPrefix(repoURL, "git@github.com:"); ok {
		path := after
		parts := strings.SplitN(path, "/", 3)
		if len(parts) >= 2 {
			return parts[0], parts[1], nil
		}
	}

	return "", "", fmt.Errorf("unsupported GitHub URL format: %s", repoURL)
}

func createGitHubPR(ctx context.Context, token, owner, repo, head, base, title, body, apiBaseURL string) (string, int, string, error) {
	pr, err := CreateOrGetGitHubPullRequest(ctx, token, owner, repo, head, base, title, body, apiBaseURL)
	if err != nil {
		return "", 0, "", err
	}

	return pr.HTMLURL, pr.Number, pr.Status, nil
}
