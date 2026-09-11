package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tools"
)

func TestBrokeredCoordinationRegistrationBuildsACPDescriptors(t *testing.T) {
	registry := tools.NewRegistry()

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := tools.RegisterBrokeredCoordinationTools(registry, k8sClient); err != nil {
		t.Fatal(err)
	}

	allowed := []string{"delegate_task", "propose_memory", "recall_memory", "remember", "search_transcript"}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid"},
		Spec:       corev1alpha1.TaskSpec{AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: allowed}},
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
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{
		Claude: "docker.io/example/claude@sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfigurationWithRegistry(
		context.Background(), k8sClient, task, agent, plan.Profile, registry,
	)
	if err != nil {
		t.Fatalf("buildRuntimeSessionMCPConfiguration() error = %v", err)
	}

	got := make([]string, 0, len(configuration.ToolPolicy.Tools))
	for _, descriptor := range configuration.ToolPolicy.Tools {
		if descriptor.Source != harnessv2.MCPToolSourceBrokeredBuiltin {
			t.Fatalf("descriptor %q source = %q, want brokered_builtin", descriptor.Name, descriptor.Source)
		}
		got = append(got, descriptor.Name)
	}
	if !slices.Equal(got, allowed) {
		t.Fatalf("brokered descriptors = %v, want %v", got, allowed)
	}
}
