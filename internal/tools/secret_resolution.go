/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

var (
	copilotRuntimeSecretCandidates = []string{"copilot-token"}
	claudeRuntimeSecretCandidates  = []string{"claude-credentials", "claude-api-key"}
	codexRuntimeSecretCandidates   = []string{"codex-credentials", "codex-api-key", "openai-api-key"}
	gitCredentialSecretCandidates  = []string{"git-credentials", "github-credentials", "copilot-token", "github-token", "git-token"}
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
		if !slices.Contains(candidates, requested) {
			return nil, fmt.Errorf("runtime secretRef %q is not allowed for %s runtime; supported names: %s", requested, runtimeType, strings.Join(candidates, ", "))
		}
		exists, err := secretExists(ctx, k8sClient, namespace, requested)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("runtime secretRef %q not found in namespace %q", requested, namespace)
		}
		return &corev1.LocalObjectReference{Name: requested}, nil
	}

	name, err := firstExistingSecretName(ctx, k8sClient, namespace, candidates)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("no supported %s runtime credentials found in namespace %q; expected one of %s", runtimeType, namespace, strings.Join(candidates, ", "))
	}
	return &corev1.LocalObjectReference{Name: name}, nil
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
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get secret %q: %w", name, err)
	}
	return true, nil
}
