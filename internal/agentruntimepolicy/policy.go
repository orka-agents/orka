/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package agentruntimepolicy resolves registered external runtime policy for
// Task producers that must materialize the exact brokered tool request.
package agentruntimepolicy

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// RuntimeRefPolicy is the registered harness-v2 policy a generated Task must
// explicitly request. The AgentRuntime remains the policy authority.
type RuntimeRefPolicy struct {
	ProviderKind    string
	Model           string
	AllowedTools    []string
	DisallowedTools []string
	AllowBash       bool
}

// ResolveRuntimeRefPolicy resolves an Agent's external harness-v2 policy.
// Built-in and harness-v1 Agents return a nil policy.
func ResolveRuntimeRefPolicy(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	agent *corev1alpha1.Agent,
) (*RuntimeRefPolicy, error) {
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
	return PolicyForRuntime(runtime)
}

// PolicyForRuntime returns the registered harness-v2 policy from an already
// resolved AgentRuntime. Other contracts return a nil policy.
func PolicyForRuntime(runtime *corev1alpha1.AgentRuntime) (*RuntimeRefPolicy, error) {
	if runtime == nil {
		return nil, fmt.Errorf("external AgentRuntime is required")
	}
	if runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil, nil
	}
	runtimeName := strings.TrimSpace(runtime.Name)
	if runtime.Spec.Capabilities == nil || runtime.Spec.Capabilities.MCPPolicy == nil {
		return nil, fmt.Errorf("external AgentRuntime %q is missing capabilities.mcpPolicy", runtimeName)
	}
	if runtime.Spec.Capabilities.Profile == nil {
		return nil, fmt.Errorf("external AgentRuntime %q is missing capabilities.profile", runtimeName)
	}
	policy := runtime.Spec.Capabilities.MCPPolicy
	if policy.AllowedTools == nil {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.mcpPolicy.allowedTools must be an explicit list", runtimeName)
	}
	if policy.DisallowedTools == nil {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.mcpPolicy.disallowedTools must be an explicit list", runtimeName)
	}
	providerKind := runtime.Spec.Capabilities.Profile.ProviderKind
	if strings.TrimSpace(providerKind) == "" {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.profile.providerKind is required", runtimeName)
	}
	model := runtime.Spec.Capabilities.Profile.Model
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("external AgentRuntime %q capabilities.profile.model is required", runtimeName)
	}
	return &RuntimeRefPolicy{
		ProviderKind:    providerKind,
		Model:           model,
		AllowedTools:    append([]string{}, policy.AllowedTools...),
		DisallowedTools: append([]string{}, policy.DisallowedTools...),
		AllowBash:       policy.AllowBash,
	}, nil
}

// MaterializeRuntimeRefAllowedTools copies the registered harness-v2 allowlist
// into a generated Task while preserving an explicit empty list.
func MaterializeRuntimeRefAllowedTools(task *corev1alpha1.Task, policy *RuntimeRefPolicy) error {
	if task == nil || policy == nil {
		return nil
	}
	if task.Spec.AgentRuntime != nil {
		if task.Spec.AgentRuntime.MaxTurns != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support maxTurns; iteration limits are fixed by the registered runtime profile")
		}
		if task.Spec.AgentRuntime.AllowBash != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support allowBash policy metadata")
		}
	}
	if task.Spec.AgentRuntime == nil {
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	task.Spec.AgentRuntime.AllowedTools = append([]string{}, policy.AllowedTools...)
	return nil
}

// ResolveAndMaterializeRuntimeRefAllowedTools resolves an Agent's registered
// policy and materializes its exact allowlist into a generated Task.
func ResolveAndMaterializeRuntimeRefAllowedTools(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) error {
	if task == nil {
		return nil
	}
	policy, err := ResolveRuntimeRefPolicy(ctx, reader, task.Namespace, agent)
	if err != nil {
		return err
	}
	return MaterializeRuntimeRefAllowedTools(task, policy)
}
