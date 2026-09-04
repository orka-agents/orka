package agentruntimepolicy

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestMaterializeRuntimeRefAllowedToolsPreservesExplicitEmpty(t *testing.T) {
	task := &corev1alpha1.Task{}
	if err := MaterializeRuntimeRefAllowedTools(task, &RuntimeRefPolicy{AllowedTools: []string{}}); err != nil {
		t.Fatal(err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.AllowedTools == nil || len(task.Spec.AgentRuntime.AllowedTools) != 0 {
		t.Fatalf("agentRuntime = %#v, want explicit empty allowedTools", task.Spec.AgentRuntime)
	}
}

func TestPolicyForRuntimeCopiesRegisteredPolicy(t *testing.T) {
	contract := corev1alpha1.AgentRuntimeContractHarnessV2
	runtimeObject := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "external-runtime"},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: &contract,
			Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
				Profile: &corev1alpha1.AgentRuntimeProfileSpec{ProviderKind: "codex", Model: "gpt-5.6"},
				MCPPolicy: &corev1alpha1.AgentRuntimeMCPPolicySpec{
					AllowedTools:    []string{"read_evidence"},
					DisallowedTools: []string{},
				},
			},
		},
	}
	policy, err := PolicyForRuntime(runtimeObject)
	if err != nil {
		t.Fatal(err)
	}
	runtimeObject.Spec.Capabilities.MCPPolicy.AllowedTools[0] = "changed"
	if policy == nil || len(policy.AllowedTools) != 1 || policy.AllowedTools[0] != "read_evidence" || policy.DisallowedTools == nil {
		t.Fatalf("resolved policy = %#v, want copied explicit lists", policy)
	}
}
