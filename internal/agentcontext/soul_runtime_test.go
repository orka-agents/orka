package agentcontext

import (
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestValidateSoulRuntime(t *testing.T) {
	type testCase struct {
		name    string
		agent   *corev1alpha1.Agent
		allowed bool
	}
	withSoul := func(runtime *corev1alpha1.AgentCLIRuntime) *corev1alpha1.Agent {
		return &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
			Soul: &corev1alpha1.SoulSource{Inline: "persona"}, Runtime: runtime,
		}}
	}
	cases := make([]testCase, 0, 32)
	cases = append(cases,
		testCase{name: "nil Agent", allowed: true},
		testCase{name: "no soul", agent: &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"}},
		}}, allowed: true},
		testCase{name: "AI worker", agent: withSoul(nil), allowed: true},
		testCase{name: "empty runtime", agent: withSoul(&corev1alpha1.AgentCLIRuntime{})},
		testCase{name: "unknown runtime", agent: withSoul(&corev1alpha1.AgentCLIRuntime{
			Type: "unknown", ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
		})},
		testCase{name: "unknown contract", agent: withSoul(&corev1alpha1.AgentCLIRuntime{
			Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractVersion("unknown")),
		})},
		testCase{name: "runtimeRef with built-in type", agent: withSoul(&corev1alpha1.AgentCLIRuntime{
			Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external"},
		})},
		testCase{name: "empty runtimeRef", agent: withSoul(&corev1alpha1.AgentCLIRuntime{
			Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{},
		})},
	)
	for _, kind := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
	} {
		for _, contract := range []*corev1alpha1.AgentRuntimeContractVersion{
			new(corev1alpha1.AgentRuntimeContractHarnessV2),
			new(corev1alpha1.AgentRuntimeContractHarnessV1), nil,
		} {
			version := "missing contract"
			allowed := false
			if contract != nil {
				version = string(*contract)
				allowed = *contract == corev1alpha1.AgentRuntimeContractHarnessV2
			}
			agent := withSoul(&corev1alpha1.AgentCLIRuntime{Type: kind, ContractVersion: contract})
			cases = append(cases, testCase{string(kind) + "/" + version, agent, allowed})
			withoutSoul := agent.DeepCopy()
			withoutSoul.Spec.Soul = nil
			cases = append(cases, testCase{string(kind) + "/" + version + "/no soul", withoutSoul, true})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.agent.DeepCopy()
			err := ValidateSoulRuntime(tc.agent)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, error=%v", tc.allowed, err)
			}
			if err != nil && !strings.Contains(err.Error(), "built-in harness v2") {
				t.Fatalf("missing runtime restriction in error: %v", err)
			}
			if !reflect.DeepEqual(before, tc.agent) {
				t.Fatal("runtime validation mutated the Agent")
			}
		})
	}
}
