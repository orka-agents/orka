/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type resolvedRuntimeRefPolicy struct {
	providerKind string
	model        string
	allowedTools []string
}

func resolveRuntimeRefPolicy(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	agent *corev1alpha1.Agent,
) (*resolvedRuntimeRefPolicy, error) {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef == nil {
		return nil, nil
	}
	runtimeName := strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
	if runtimeName == "" {
		return nil, nil
	}
	if reader == nil {
		return nil, fmt.Errorf("resolve external AgentRuntime %q: Kubernetes reader is required", runtimeName)
	}

	runtime := &corev1alpha1.AgentRuntime{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: runtimeName}, runtime); err != nil {
		return nil, fmt.Errorf("resolve external AgentRuntime %q: %w", runtimeName, err)
	}
	if runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil, nil
	}
	if runtime.Spec.Capabilities == nil || runtime.Spec.Capabilities.MCPPolicy == nil {
		return nil, fmt.Errorf("external AgentRuntime %q is missing capabilities.mcpPolicy", runtimeName)
	}
	if runtime.Spec.Capabilities.Profile == nil {
		return nil, fmt.Errorf("external AgentRuntime %q is missing capabilities.profile", runtimeName)
	}
	allowed := runtime.Spec.Capabilities.MCPPolicy.AllowedTools
	if allowed == nil {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.mcpPolicy.allowedTools must be an explicit list", runtimeName)
	}
	providerKind := runtime.Spec.Capabilities.Profile.ProviderKind
	if strings.TrimSpace(providerKind) == "" {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.profile.providerKind is required", runtimeName)
	}
	model := runtime.Spec.Capabilities.Profile.Model
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.profile.model is required", runtimeName)
	}
	return &resolvedRuntimeRefPolicy{
		providerKind: providerKind,
		model:        model,
		allowedTools: append([]string{}, allowed...),
	}, nil
}

// materializeRuntimeRefAllowedTools copies the registered harness-v2 allowlist
// into a generated Task. The AgentRuntime remains the policy authority; the
// Task copy makes the exact requested broker exposure explicit for binding.
func materializeRuntimeRefAllowedTools(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) error {
	if task == nil {
		return nil
	}
	policy, err := resolveRuntimeRefPolicy(ctx, reader, task.Namespace, agent)
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}
	if task.Spec.AgentRuntime == nil {
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	// Start from a non-nil slice so an explicit deny-all policy survives JSON
	// serialization instead of becoming an omitted task override.
	task.Spec.AgentRuntime.AllowedTools = append([]string{}, policy.allowedTools...)
	return nil
}
