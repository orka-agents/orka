package agentcontext

import (
	"errors"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
)

// ValidateSoulRuntime permits souls only for AI workers and the four built-in
// harness v2 runtimes. Agents without a soul retain their existing behavior.
func ValidateSoulRuntime(agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.Soul == nil || agent.Spec.Runtime == nil {
		return nil
	}
	runtime := agent.Spec.Runtime
	if runtime.RuntimeRef == nil && runtime.ContractVersion != nil && *runtime.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV2 {
		switch runtime.Type {
		case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
			corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode:
			return nil
		}
	}
	return errors.New("agent.spec.soul requires an AI worker or a built-in harness v2 runtime")
}

// ValidateInlineCopilotInstructions checks only known inline portions of the
// completed Agent. It does not resolve ConfigMaps; the controller must still
// validate the resolved configuration before dispatch.
func ValidateInlineCopilotInstructions(agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.Runtime == nil {
		return nil
	}
	runtime := agent.Spec.Runtime
	if runtime.Type != corev1alpha1.AgentRuntimeCopilot || runtime.RuntimeRef != nil ||
		runtime.ContractVersion == nil || *runtime.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil
	}
	if role := agent.Spec.SystemPrompt; role != nil {
		if err := acp.ValidateCopilotInstructions(role.Inline); err != nil {
			return fmt.Errorf("agent.spec.systemPrompt.inline: %w", err)
		}
	}
	if soul := agent.Spec.Soul; soul != nil {
		if err := acp.ValidateCopilotInstructions(soul.Inline); err != nil {
			return fmt.Errorf("agent.spec.soul.inline: %w", err)
		}
	}
	return nil
}

// ValidateAgentWrite checks the soul and instruction fields an Agent write can
// validate without resolving ConfigMaps, so an API write cannot persist an
// Agent that its reconciler would immediately reject.
func ValidateAgentWrite(agent *corev1alpha1.Agent) error {
	if err := ValidateSoulRuntime(agent); err != nil {
		return err
	}
	if agent != nil {
		if err := ValidateSource(agent.Spec.Soul); err != nil {
			return err
		}
	}
	return ValidateInlineCopilotInstructions(agent)
}
