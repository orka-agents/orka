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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// resolveRepoAndToken resolves GitHub owner/repo, auth token, and API base URL.
// Resolution priority for owner/repo:
//  1. repoURL arg → parse owner/repo directly
//  2. taskName arg → look up that task's workspace config in K8s
//  3. ORKA_GIT_REPO env var → parse owner/repo
//
// Token resolution (in order):
//  1. If taskName used → read from that task's workspace gitSecretRef
//  2. /secrets/git/token file (mounted in pod)
//  3. /secrets/git/password file (mounted in pod)
//  4. GITHUB_TOKEN env var
func resolveRepoAndToken(ctx context.Context, k8sClient client.Client, taskName, repoURL, overrideBaseURL string) (owner, repo, token, baseURL string, err error) {
	// --- Resolve base URL ---
	baseURL = githubAPIBaseURL
	if overrideBaseURL != "" {
		baseURL = overrideBaseURL
	}

	// --- Resolve owner/repo ---
	var tokenFromTask string

	switch {
	case repoURL != "":
		// Priority 1: explicit repoURL arg
		owner, repo, err = parseGitHubRepo(repoURL)
		if err != nil {
			return "", "", "", "", fmt.Errorf("failed to parse GitHub repo from %s: %w", repoURL, err)
		}

	case taskName != "":
		// Priority 2: look up Task CR in K8s
		owner, repo, tokenFromTask, err = resolveFromTask(ctx, k8sClient, taskName)
		if err != nil {
			return "", "", "", "", err
		}

	default:
		// Priority 3: ORKA_GIT_REPO env var
		envRepo := os.Getenv("ORKA_GIT_REPO")
		if envRepo == "" {
			return "", "", "", "", fmt.Errorf("no repo_url, task_name, or ORKA_GIT_REPO provided")
		}
		owner, repo, err = parseGitHubRepo(envRepo)
		if err != nil {
			return "", "", "", "", fmt.Errorf("failed to parse GitHub repo from ORKA_GIT_REPO: %w", err)
		}
	}

	// --- Resolve token ---
	if tokenFromTask != "" {
		token = tokenFromTask
	} else {
		token = resolveToken()
	}

	if token == "" {
		return "", "", "", "", fmt.Errorf("could not resolve GitHub token from task secret, /secrets/git/token, /secrets/git/password, or GITHUB_TOKEN")
	}

	return owner, repo, token, baseURL, nil
}

// resolveFromTask looks up a Task CR in K8s and extracts owner/repo and token
// from its workspace configuration and gitSecretRef.
func resolveFromTask(ctx context.Context, k8sClient client.Client, taskName string) (owner, repo, token string, err error) {
	ns := os.Getenv("ORKA_TASK_NAMESPACE")
	if ns == "" {
		ns = defaultNamespace
	}

	var task corev1alpha1.Task
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: taskName, Namespace: ns}, &task); err != nil {
		return "", "", "", fmt.Errorf("failed to get task %s: %w", taskName, err)
	}

	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		return "", "", "", fmt.Errorf("task %s does not have workspace configuration", taskName)
	}
	ws := task.Spec.AgentRuntime.Workspace

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

	for _, key := range []string{"token", "password"} {
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
	return os.Getenv("GITHUB_TOKEN")
}
