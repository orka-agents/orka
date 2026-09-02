package executionmode

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNamespaceMode(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant", Labels: map[string]string{NamespaceLabel: string(HarnessV1)},
	}}
	if mode, err := FromNamespace(namespace); err != nil || mode != HarnessV1 {
		t.Fatalf("FromNamespace() = %q, %v", mode, err)
	}
	if err := ValidateNamespace(namespace, HarnessV2); err == nil {
		t.Fatal("mode mismatch must fail")
	}
	for _, raw := range []string{"", "auto", "dual", "harness-v3"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded", raw)
		}
	}
}

func TestDefaultBuiltInAgentContract(t *testing.T) {
	v1 := corev1alpha1.AgentRuntimeContractHarnessV1
	v2 := corev1alpha1.AgentRuntimeContractHarnessV2
	tests := []struct {
		name      string
		agent     *corev1alpha1.Agent
		mode      Mode
		want      corev1alpha1.AgentRuntimeContractVersion
		wantError bool
	}{
		{name: "nil agent", mode: HarnessV2},
		{name: "no runtime", agent: &corev1alpha1.Agent{}, mode: HarnessV2},
		{
			name: "external runtime remains derived",
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"},
			}}},
			mode: HarnessV2,
		},
		{
			name:  "v1 omission defaults to v1",
			agent: builtInAgent(),
			mode:  HarnessV1,
			want:  v1,
		},
		{
			name:  "v2 omission defaults to v2",
			agent: builtInAgent(),
			mode:  HarnessV2,
			want:  v2,
		},
		{
			name: "matching explicit contract is preserved",
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: &v1,
			}}},
			mode: HarnessV1,
			want: v1,
		},
		{
			name: "opposite explicit contract fails closed",
			agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: &v2,
			}}},
			mode:      HarnessV1,
			want:      v2,
			wantError: true,
		},
		{name: "missing authoritative mode fails closed", agent: builtInAgent(), wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := DefaultBuiltInAgentContract(test.agent, test.mode)
			if (err != nil) != test.wantError {
				t.Fatalf("DefaultBuiltInAgentContract() error = %v, wantError %t", err, test.wantError)
			}
			if test.agent == nil || test.agent.Spec.Runtime == nil {
				return
			}
			if got := test.agent.BuiltInContractVersion(); got != test.want {
				t.Fatalf("contractVersion = %q, want %q", got, test.want)
			}
		})
	}
}

func builtInAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
		Type: corev1alpha1.AgentRuntimeCodex,
	}}}
}
