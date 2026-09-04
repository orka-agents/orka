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

// materializeRuntimeRefAllowedTools copies the registered harness-v2 allowlist
// into a generated Task. The AgentRuntime remains the policy authority; the
// Task copy makes the exact requested broker exposure explicit for binding.
func materializeRuntimeRefAllowedTools(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) error {
	if task == nil || agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef == nil {
		return nil
	}
	runtimeName := strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
	if runtimeName == "" {
		return nil
	}
	if reader == nil {
		return fmt.Errorf("resolve external AgentRuntime %q: Kubernetes reader is required", runtimeName)
	}

	runtime := &corev1alpha1.AgentRuntime{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: runtimeName}, runtime); err != nil {
		return fmt.Errorf("resolve external AgentRuntime %q: %w", runtimeName, err)
	}
	if runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil
	}
	if runtime.Spec.Capabilities == nil || runtime.Spec.Capabilities.MCPPolicy == nil {
		return fmt.Errorf("external AgentRuntime %q is missing capabilities.mcpPolicy", runtimeName)
	}
	allowed := runtime.Spec.Capabilities.MCPPolicy.AllowedTools
	if allowed == nil {
		return fmt.Errorf("external AgentRuntime %q capabilities.mcpPolicy.allowedTools must be an explicit list", runtimeName)
	}
	if task.Spec.AgentRuntime == nil {
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	// Start from a non-nil slice so an explicit deny-all policy survives JSON
	// serialization instead of becoming an omitted task override.
	task.Spec.AgentRuntime.AllowedTools = append([]string{}, allowed...)
	return nil
}
