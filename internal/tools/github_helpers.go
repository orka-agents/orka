/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sozercan/orka/internal/workerenv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const githubPRStateClosed = "closed"

type githubRepoScope struct {
	source string
	owner  string
	repo   string
}

// resolveRepoAndToken resolves GitHub owner/repo, auth token, and API base URL.
// Repository resolution prefers an explicit repoURL, then task workspace config,
// then ORKA_GIT_REPO. If taskName is provided alongside repoURL, repoURL still
// selects the repository while the task workspace can supply credentials.
//
// Token resolution (in order):
//  1. task workspace gitSecretRef, when taskName is provided
//  2. /secrets/git/token file (mounted in pod)
//  3. /secrets/git/password file (mounted in pod)
//  4. GITHUB_TOKEN env var
func resolveRepoAndToken(ctx context.Context, k8sClient client.Client, taskName, repoURL, overrideBaseURL string) (owner, repo, token, baseURL string, err error) {
	baseURL = githubAPIBaseURL
	if overrideBaseURL != "" {
		baseURL = overrideBaseURL
	}

	if repoURL != "" {
		owner, repo, err = parseGitHubRepo(repoURL)
		if err != nil {
			return "", "", "", "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
		}
	}

	if taskName != "" {
		taskOwner, taskRepo, taskToken, err := resolveFromTask(ctx, k8sClient, taskName)
		if err != nil {
			if owner == "" || repo == "" {
				return "", "", "", "", err
			}
		} else {
			if owner == "" || repo == "" {
				owner, repo = taskOwner, taskRepo
			} else if repoURL != "" {
				scopes, err := taskRepoScopes(ctx, k8sClient, taskName)
				if err != nil {
					return "", "", "", "", err
				}
				if !githubRepoAllowed(owner, repo, scopes) {
					return "", "", "", "", fmt.Errorf(
						"repo_url repository %s/%s does not match task repository scope %s",
						owner,
						repo,
						formatGitHubRepoScopes(scopes),
					)
				}
			}
			token = taskToken
		}
	}

	if owner == "" || repo == "" {
		envRepo := os.Getenv(workerenv.GitRepo)
		if envRepo == "" {
			return "", "", "", "", fmt.Errorf("no repo_url, task_name, or ORKA_GIT_REPO provided")
		}
		owner, repo, err = parseGitHubRepo(envRepo)
		if err != nil {
			return "", "", "", "", fmt.Errorf("failed to parse GitHub repo from ORKA_GIT_REPO: %w", err)
		}
	}

	if token == "" {
		token = resolveToken()
	}

	if token == "" {
		return "", "", "", "", fmt.Errorf("could not resolve GitHub token from task secret, /secrets/git/token, /secrets/git/password, or GITHUB_TOKEN")
	}

	return owner, repo, token, baseURL, nil
}

func validateRepoURLScope(ctx context.Context, k8sClient client.Client, taskName, repoURL string) error {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return nil
	}
	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return fmt.Errorf("failed to parse repo_url: %w", err)
	}

	scopes, err := taskRepoScopes(ctx, k8sClient, taskName)
	if err != nil {
		return err
	}
	if envRepo := strings.TrimSpace(os.Getenv(workerenv.GitRepo)); envRepo != "" {
		envOwner, envRepoName, err := parseGitHubRepo(envRepo)
		if err != nil {
			return fmt.Errorf("failed to parse %s for repo_url scope: %w", workerenv.GitRepo, err)
		}
		scopes = append(scopes, githubRepoScope{source: workerenv.GitRepo, owner: envOwner, repo: envRepoName})
	}
	if len(scopes) == 0 {
		return fmt.Errorf("repo_url repository %s/%s requires a permitted repository scope", owner, repo)
	}

	for _, scope := range scopes {
		if githubRepoMatches(owner, repo, scope.owner, scope.repo) {
			return nil
		}
	}
	return fmt.Errorf(
		"repo_url repository %s/%s does not match permitted repository scope %s",
		owner,
		repo,
		formatGitHubRepoScopes(scopes),
	)
}

func githubRepoAllowed(owner, repo string, scopes []githubRepoScope) bool {
	for _, scope := range scopes {
		if githubRepoMatches(owner, repo, scope.owner, scope.repo) {
			return true
		}
	}
	return false
}

func taskRepoScopes(ctx context.Context, k8sClient client.Client, taskName string) ([]githubRepoScope, error) {
	if strings.TrimSpace(taskName) == "" {
		return nil, nil
	}
	if k8sClient == nil {
		return nil, fmt.Errorf("task_name %q requires a Kubernetes client for repo_url scope validation", taskName)
	}

	ns := os.Getenv(envOrkaTaskNamespace)
	if ns == "" {
		ns = defaultNamespace
	}

	var task corev1alpha1.Task
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: taskName, Namespace: ns}, &task); err != nil {
		return nil, fmt.Errorf("failed to get task %s: %w", taskName, err)
	}

	var scopes []githubRepoScope
	ws := task.Spec.Workspace
	if ws == nil && task.Spec.AgentRuntime != nil {
		ws = task.Spec.AgentRuntime.Workspace
	}
	if ws != nil && strings.TrimSpace(ws.GitRepo) != "" {
		owner, repo, err := parseGitHubRepo(ws.GitRepo)
		if err != nil {
			return nil, fmt.Errorf("failed to parse GitHub repo from %s: %w", ws.GitRepo, err)
		}
		scopes = append(scopes, githubRepoScope{source: "task workspace", owner: owner, repo: repo})
	}
	if task.Spec.Transaction != nil {
		if txRepo := strings.TrimSpace(task.Spec.Transaction.Context["repo"]); txRepo != "" {
			owner, repo, err := parseGitHubRepo(txRepo)
			if err != nil {
				return nil, fmt.Errorf("failed to parse transaction repo context: %w", err)
			}
			scopes = append(scopes, githubRepoScope{source: "transaction repo context", owner: owner, repo: repo})
		}
	}
	return scopes, nil
}

func githubRepoMatches(owner, repo, wantOwner, wantRepo string) bool {
	return strings.EqualFold(owner, wantOwner) && strings.EqualFold(repo, wantRepo)
}

func formatGitHubRepoScopes(scopes []githubRepoScope) string {
	parts := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		parts = append(parts, fmt.Sprintf("%s=%s/%s", scope.source, scope.owner, scope.repo))
	}
	return strings.Join(parts, ", ")
}

// resolveFromTask looks up a Task CR in K8s and extracts owner/repo and token
// from its workspace configuration and gitSecretRef.
func resolveFromTask(ctx context.Context, k8sClient client.Client, taskName string) (owner, repo, token string, err error) {
	ns := os.Getenv(envOrkaTaskNamespace)
	if ns == "" {
		ns = defaultNamespace
	}

	var task corev1alpha1.Task
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: taskName, Namespace: ns}, &task); err != nil {
		return "", "", "", fmt.Errorf("failed to get task %s: %w", taskName, err)
	}

	ws := task.Spec.Workspace
	if ws == nil && task.Spec.AgentRuntime != nil {
		ws = task.Spec.AgentRuntime.Workspace
	}
	if ws == nil {
		return "", "", "", fmt.Errorf("task %s does not have workspace configuration", taskName)
	}

	repoURL := ws.GitRepo
	if repoURL == "" {
		return "", "", "", fmt.Errorf("task %s workspace has no gitRepo configured", taskName)
	}

	owner, repo, err = parseGitHubRepo(repoURL)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
	}

	// Read token from the task's gitSecretRef
	if ws.GitSecretRef == nil {
		return "", "", "", fmt.Errorf("task %s workspace has no gitSecretRef configured", taskName)
	}

	var secret corev1.Secret
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: ws.GitSecretRef.Name, Namespace: ns}, &secret); err != nil {
		return "", "", "", fmt.Errorf("failed to get git secret %s: %w", ws.GitSecretRef.Name, err)
	}

	for _, key := range []string{tokenKey, passwordKey} {
		if v, ok := secret.Data[key]; ok {
			token = strings.TrimSpace(string(v))
			break
		}
	}
	if token == "" {
		return "", "", "", fmt.Errorf("git secret %s does not contain a 'token' or 'password' key", ws.GitSecretRef.Name)
	}

	return owner, repo, token, nil
}

// resolveToken attempts to read a GitHub token from well-known file paths
// and environment variables.
func resolveToken() string {
	// Try mounted secret files
	for _, path := range []string{"/secrets/git/token", "/secrets/git/password"} {
		if data, err := os.ReadFile(path); err == nil {
			if t := strings.TrimSpace(string(data)); t != "" {
				return t
			}
		}
	}

	// Fall back to GITHUB_TOKEN env var
	return os.Getenv(workerenv.GitHubToken)
}
