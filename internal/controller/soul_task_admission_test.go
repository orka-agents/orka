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
		for _, contract := range []corev1alpha1.AgentRuntimeContractVersion{corev1alpha1.AgentRuntimeContractHarnessV1, corev1alpha1.AgentRuntimeContractHarnessV2} {
			t.Run(string(provider)+"/"+string(contract), func(t *testing.T) {
				agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: provider, ContractVersion: new(contract)},
					Soul:    &corev1alpha1.SoulSource{Inline: "declared persona"},
				}}
				plan := agentACPPlan()
				if contract == corev1alpha1.AgentRuntimeContractHarnessV1 {
					plan = agentHarnessV1Plan("")
				}
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

func TestNewHarnessV1CandidateRejectsDeclaredSoul(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude} {
		t.Run(string(provider), func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: provider, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV1)},
				Soul:    &corev1alpha1.SoulSource{Inline: "must not be omitted"},
			}}
			if err := validateNewHarnessV1Workload(task, agent); err == nil {
				t.Fatal("new v1 candidate silently ignored a declared soul")
			}
			agent.Spec.Soul = nil
			if err := validateNewHarnessV1Workload(task, agent); err != nil {
				t.Fatalf("legacy no-soul candidate was rejected: %v", err)
			}
		})
	}
}
