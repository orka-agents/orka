package controller

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestPlannedAgentTaskSoulRuntimeCompatibility(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	for _, provider := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
	} {
		for _, contract := range []corev1alpha1.AgentRuntimeContractVersion{corev1alpha1.AgentRuntimeContractVersion("orka.harness.v1"), corev1alpha1.AgentRuntimeContractHarnessV2} {
			t.Run(string(provider)+"/"+string(contract), func(t *testing.T) {
				agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: provider, ContractVersion: new(contract)},
					Soul:    &corev1alpha1.SoulSource{Inline: "declared persona"},
				}}
				plan := agentACPPlan()
				err := validatePlannedRuntimeRefAgentTaskRestrictions(task, agent, plan)
				if (err == nil) != (contract == corev1alpha1.AgentRuntimeContractHarnessV2) {
					t.Fatalf("soul route compatibility error = %v", err)
				}
				agent.Spec.Soul = nil
				if err := validatePlannedRuntimeRefAgentTaskRestrictions(task, agent, plan); err != nil {
					t.Fatalf("no-soul route behavior changed: %v", err)
				}
			})
		}
	}
}
