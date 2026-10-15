/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// An agent with its own shell off and sandbox_exec on runs every command in a
// micro-VM: the descriptor is runtime-local, so the broker never serves it.
func TestBuildRuntimeSessionMCPConfigurationServesSandboxExecLocally(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	allowBash := false
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: []string{providerNativeToolRead, providerNativeToolEdit, acp.SandboxExecToolName}, AllowBash: &allowBash,
			},
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: "model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeClaude,
				ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			},
		},
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfiguration(context.Background(), reader, task, agent, plan.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := configuration.ValidateProfile(plan.Profile); err != nil {
		t.Fatalf("ValidateProfile() error = %v", err)
	}
	descriptor, ok := configuration.ToolPolicy.Descriptor(acp.SandboxExecToolName)
	if !ok {
		t.Fatalf("sandbox_exec has no descriptor: %#v", configuration.ToolPolicy.Tools)
	}
	if descriptor.Source != harnessv2.MCPToolSourceRuntimeLocal || descriptor.Effect != harnessv2.MCPToolEffectConsequential {
		t.Fatalf("sandbox_exec descriptor = %#v, want runtime-local and consequential", descriptor)
	}
	if string(descriptor.InputSchema) != acp.SandboxExecInputSchema {
		t.Fatalf("sandbox_exec schema = %s", descriptor.InputSchema)
	}
	if configuration.ToolPolicy.Allows("Bash") {
		t.Fatal("the provider's own shell is still allowed")
	}

	// The broker refuses it: a runtime-local tool is never executed there.
	call := harnessv2.MCPToolCall{CallID: "call-1", ToolName: acp.SandboxExecToolName, Arguments: []byte(`{"code":"true"}`)}
	if _, err := call.ValidateAt(harnessv2.PromptMCPAuthorization{ToolPolicy: configuration.ToolPolicy}, metav1.Now().Time); err == nil {
		t.Fatal("the broker would accept a runtime-local sandbox_exec call")
	}
	// It cannot be put behind an Orka approval, which only the broker grants.
	approval := harnessv2.MCPApprovalPolicy{RequiredTools: []string{acp.SandboxExecToolName}}
	if err := approval.Validate(configuration.ToolPolicy); err == nil {
		t.Fatal("an approval policy accepted the runtime-local sandbox_exec")
	}
}
