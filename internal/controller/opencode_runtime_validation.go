/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// ValidateOpenCodeAgentSpec validates the reviewed OpenCode runtime configuration.
func ValidateOpenCodeAgentSpec(agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeOpencode {
		return nil
	}
	if strings.TrimSpace(agent.Spec.Runtime.DefaultReasoningEffort) != "" {
		return fmt.Errorf("agent %q opencode runtime does not support spec.runtime.defaultReasoningEffort", agent.Name)
	}
	if agent.Spec.SystemPrompt != nil &&
		(strings.TrimSpace(agent.Spec.SystemPrompt.Inline) != "" || agent.Spec.SystemPrompt.ConfigMapRef != nil) {
		return fmt.Errorf("agent %q opencode runtime does not support spec.systemPrompt", agent.Name)
	}
	if agent.Spec.ProviderRef != nil {
		return fmt.Errorf("agent %q opencode runtime does not accept spec.providerRef; provider identity is derived from spec.model.name", agent.Name)
	}
	if agent.Spec.Model == nil || strings.TrimSpace(agent.Spec.Model.Name) == "" {
		return fmt.Errorf("agent %q opencode runtime requires spec.model.name", agent.Name)
	}
	if strings.ContainsAny(agent.Spec.Model.Name, "{}") {
		return fmt.Errorf("agent %q opencode runtime model name must not contain substitution braces", agent.Name)
	}
	provider, model, ok := strings.Cut(strings.TrimSpace(agent.Spec.Model.Name), "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return fmt.Errorf("agent %q opencode runtime model name must use provider/model form", agent.Name)
	}
	if strings.TrimSpace(agent.Spec.Model.Provider) != "" {
		return fmt.Errorf("agent %q opencode runtime does not accept model.provider", agent.Name)
	}
	if agent.Spec.Model.Temperature != nil && *agent.Spec.Model.Temperature != legacyDefaultACPTemperature {
		return fmt.Errorf("agent %q opencode runtime does not support spec.model.temperature values other than the legacy default %.1f", agent.Name, legacyDefaultACPTemperature)
	}
	if len(agent.Spec.Model.Fallbacks) > 0 {
		return fmt.Errorf("agent %q opencode runtime does not support spec.model.fallbacks", agent.Name)
	}
	if agent.Spec.Model.ContextWindow == nil || *agent.Spec.Model.ContextWindow <= 0 {
		return fmt.Errorf("agent %q opencode runtime requires a positive spec.model.contextWindow", agent.Name)
	}
	if agent.Spec.Model.MaxTokens == nil || *agent.Spec.Model.MaxTokens <= 0 {
		return fmt.Errorf("agent %q opencode runtime requires a positive spec.model.maxTokens", agent.Name)
	}
	if *agent.Spec.Model.ContextWindow <= *agent.Spec.Model.MaxTokens {
		return fmt.Errorf("agent %q opencode runtime model.contextWindow must exceed model.maxTokens", agent.Name)
	}
	if agent.Spec.SecretRef != nil && strings.TrimSpace(agent.Spec.SecretRef.Name) != "" {
		return fmt.Errorf("agent %q opencode runtime does not accept spec.secretRef; provider access is controller-proxied", agent.Name)
	}
	return nil
}
