package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// Reapplying an Agent manifest that no longer spells out the only supported
// contract deletes the stored selector. The Agent CRD rule alone, with no
// admission webhook, must allow that; the controller writes the value back.
func TestAgentContractSelectorMayBeClearedOnAPIServer(t *testing.T) {
	apiClient := newArchivedSessionAPIClient(t)
	ctx := t.Context()

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coder", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: "gpt-5.2-codex"},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeCodex,
				ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			},
		},
	}
	if err := apiClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}

	base := agent.DeepCopy()
	agent.Spec.Runtime.ContractVersion = nil
	if err := apiClient.Patch(ctx, agent, client.MergeFrom(base)); err != nil {
		t.Fatalf("clearing the selector was rejected: %v", err)
	}
	stored := &corev1alpha1.Agent{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(agent), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Runtime.ContractVersion != nil {
		t.Fatalf("selector = %q, want cleared", *stored.Spec.Runtime.ContractVersion)
	}

	base = stored.DeepCopy()
	stored.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV2)
	if err := apiClient.Patch(ctx, stored, client.MergeFrom(base)); err != nil {
		t.Fatalf("restamping the selector was rejected: %v", err)
	}
}
