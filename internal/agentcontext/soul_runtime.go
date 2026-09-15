package agentcontext

import (
	"errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
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
