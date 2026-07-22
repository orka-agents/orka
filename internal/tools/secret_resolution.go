/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

var (
	copilotRuntimeSecretCandidates  = []string{"copilot-token"}
	claudeRuntimeSecretCandidates   = []string{claudeCredentialsSecretName, claudeAPIKeySecretName}
	codexRuntimeSecretCandidates    = []string{codexRuntimeCopilotSecretName, "codex-runtime-openai", "codex-credentials", "codex-api-key", codexProxyTokenSecretName, "openai-api-key"}
	opencodeRuntimeSecretCandidates = []string{"opencode-credentials", "opencode-api-key"}
	gitCredentialSecretCandidates   = []string{"git-credentials", "github-credentials", "copilot-token", "github-token", "git-token"}
)

// RuntimeSecretCandidates returns the supported secret names for the given runtime.
func RuntimeSecretCandidates(runtimeType corev1alpha1.AgentRuntimeType) []string {
	switch runtimeType {
	case corev1alpha1.AgentRuntimeCopilot:
		return append([]string(nil), copilotRuntimeSecretCandidates...)
	case corev1alpha1.AgentRuntimeClaude:
		return append([]string(nil), claudeRuntimeSecretCandidates...)
	case corev1alpha1.AgentRuntimeCodex:
		return append([]string(nil), codexRuntimeSecretCandidates...)
	case corev1alpha1.AgentRuntimeOpencode:
		return append([]string(nil), opencodeRuntimeSecretCandidates...)
	default:
		return nil
	}
}

// FirstPresentSecretName returns the first candidate found in the present map.
func FirstPresentSecretName(present map[string]bool, candidates []string) string {
	for _, name := range candidates {
		if present[name] {
			return name
		}
	}
	return ""
}

func resolveRuntimeSecretRef(ctx context.Context, k8sClient client.Reader, namespace string, runtimeType corev1alpha1.AgentRuntimeType, requested string) (*corev1.LocalObjectReference, error) {
	candidates := RuntimeSecretCandidates(runtimeType)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("unsupported runtime type %q", runtimeType)
	}

	if requested != "" {
		secret, exists, err := getSecret(ctx, k8sClient, namespace, requested)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("runtime secretRef %q not found in namespace %q", requested, namespace)
		}
		if err := validateRuntimeSecret(runtimeType, secret); err != nil {
			return nil, fmt.Errorf("runtime secretRef %q in namespace %q %w", requested, namespace, err)
		}
		return &corev1.LocalObjectReference{Name: requested}, nil
	}

	name, err := firstUsableRuntimeSecretName(ctx, k8sClient, namespace, runtimeType, candidates)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("no supported %s runtime credentials found in namespace %q; expected one of %s", runtimeType, namespace, strings.Join(candidates, ", "))
	}
	return &corev1.LocalObjectReference{Name: name}, nil
}

func FirstUsableRuntimeSecretName(secrets []corev1.Secret, runtimeType corev1alpha1.AgentRuntimeType) string {
	for _, candidate := range RuntimeSecretCandidates(runtimeType) {
		for i := range secrets {
			if secrets[i].Name != candidate {
				continue
			}
			if validateRuntimeSecret(runtimeType, &secrets[i]) == nil {
				return candidate
			}
		}
	}
	return ""
}

func firstUsableRuntimeSecretName(ctx context.Context, k8sClient client.Reader, namespace string, runtimeType corev1alpha1.AgentRuntimeType, candidates []string) (string, error) {
	invalid := make([]string, 0)
	for _, name := range candidates {
		secret, exists, err := getSecret(ctx, k8sClient, namespace, name)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		if err := validateRuntimeSecret(runtimeType, secret); err != nil {
			invalid = append(invalid, fmt.Sprintf("%q %v", name, err))
			continue
		}
		return name, nil
	}
	if len(invalid) > 0 {
		return "", fmt.Errorf("no valid %s runtime credentials found in namespace %q: %s", runtimeType, namespace, strings.Join(invalid, "; "))
	}
	return "", nil
}

func validateRuntimeSecret(runtimeType corev1alpha1.AgentRuntimeType, secret *corev1.Secret) error {
	if runtimeType != corev1alpha1.AgentRuntimeOpencode {
		return nil
	}
	if secret == nil || strings.TrimSpace(string(secret.Data[workerenv.OpenAIBaseURL])) == "" {
		return fmt.Errorf("must contain non-empty %s", workerenv.OpenAIBaseURL)
	}
	return nil
}

func resolveWorkspaceGitSecretRef(ctx context.Context, k8sClient client.Reader, namespace string, agent *corev1alpha1.Agent, requested string) (*corev1.LocalObjectReference, error) {
	if requested != "" {
		return &corev1.LocalObjectReference{Name: requested}, nil
	}

	if agent != nil &&
		agent.Spec.Runtime != nil &&
		agent.Spec.Runtime.Type == corev1alpha1.AgentRuntimeCopilot &&
		agent.Spec.SecretRef != nil &&
		agent.Spec.SecretRef.Name != "" {
		return &corev1.LocalObjectReference{Name: agent.Spec.SecretRef.Name}, nil
	}

	name, err := firstExistingSecretName(ctx, k8sClient, namespace, append([]string(nil), gitCredentialSecretCandidates...))
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, nil
	}
	return &corev1.LocalObjectReference{Name: name}, nil
}

func validateGitCredentialSecret(ctx context.Context, k8sClient client.Reader, namespace, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if k8sClient == nil {
		return fmt.Errorf("git secretRef %q requires a Kubernetes client", name)
	}
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("git secretRef %q not found in namespace %q", name, namespace)
		}
		return fmt.Errorf("failed to get git secretRef %q in namespace %q: %w", name, namespace, err)
	}
	if !gitCredentialSecretHasToken(secret) {
		return fmt.Errorf("git secretRef %q in namespace %q must contain a non-empty token, password, or %s key", name, namespace, workerenv.GitHubToken)
	}
	return nil
}

func gitCredentialSecretHasToken(secret *corev1.Secret) bool {
	if secret == nil {
		return false
	}
	for _, key := range []string{tokenKey, passwordKey, workerenv.GitHubToken} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			return true
		}
	}
	return false
}

func taskWorkspace(task *corev1alpha1.Task) *corev1alpha1.WorkspaceConfig {
	if task == nil {
		return nil
	}
	if task.Spec.Workspace != nil {
		return task.Spec.Workspace
	}
	if task.Spec.AgentRuntime != nil {
		return task.Spec.AgentRuntime.Workspace
	}
	return nil
}

func loadAgent(ctx context.Context, k8sClient client.Reader, namespace, agentName string) (*corev1alpha1.Agent, error) {
	if agentName == "" {
		return nil, nil
	}

	agent := &corev1alpha1.Agent{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: agentName, Namespace: namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get agent %q: %w", agentName, err)
	}
	return agent, nil
}

func firstExistingSecretName(ctx context.Context, k8sClient client.Reader, namespace string, candidates []string) (string, error) {
	for _, name := range candidates {
		exists, err := secretExists(ctx, k8sClient, namespace, name)
		if err != nil {
			return "", err
		}
		if exists {
			return name, nil
		}
	}
	return "", nil
}

func secretExists(ctx context.Context, k8sClient client.Reader, namespace, name string) (bool, error) {
	_, exists, err := getSecret(ctx, k8sClient, namespace, name)
	return exists, err
}

func getSecret(ctx context.Context, k8sClient client.Reader, namespace, name string) (*corev1.Secret, bool, error) {
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get secret %q: %w", name, err)
	}
	return secret, true, nil
}
