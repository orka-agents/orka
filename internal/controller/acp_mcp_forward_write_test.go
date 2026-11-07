package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// writeForwardingTool is the descriptor test Tool with writes allowed.
func writeForwardingTool(approval corev1alpha1.HTTPForwardWriteApproval) *corev1alpha1.Tool {
	tool := forwardingDescriptorTool()
	tool.Spec.HTTP.Forward.Writes = &corev1alpha1.HTTPForwardWrites{Approval: approval}
	return tool
}

func forwardingWriteAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: "tenant"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			Type: corev1alpha1.AgentRuntimeCodex, DefaultAllowedTools: []string{"azure-arm"},
		}},
	}
}

func forwardingWriteTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: "tenant"}}
}

// TestForwardingWritesJoinThePlanningInputs covers a forwarding Tool that
// allows writes: its write tool joins the allowed tools, and is
// approval-required unless the Tool waives approval. A Tool without writes
// adds nothing, and naming a write tool directly is refused.
func TestForwardingWritesJoinThePlanningInputs(t *testing.T) {
	write := harnessv2.MCPForwardWriteToolName("azure-arm")
	for name, tc := range map[string]struct {
		tool     *corev1alpha1.Tool
		allowed  bool
		approved bool
	}{
		"approval required": {writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalRequired), true, true},
		"approval default":  {writeForwardingTool(""), true, true},
		"approval waived":   {writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalNone), true, false},
		"reads only":        {forwardingDescriptorTool(), false, false},
	} {
		reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(tc.tool).Build()
		task, agent, err := adjustInputsForForwardingWrites(context.Background(), reader, forwardingWriteTask(), forwardingWriteAgent())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := slices.Contains(effectiveACPAllowedTools(task, agent), write); got != tc.allowed {
			t.Errorf("%s: write tool allowed = %t, want %t", name, got, tc.allowed)
		}
		approved := agent.Spec.Coordination != nil && slices.Contains(agent.Spec.Coordination.ApprovalRequiredTools, write)
		if approved != tc.approved {
			t.Errorf("%s: write tool approval-required = %t, want %t", name, approved, tc.approved)
		}
	}
	named := forwardingWriteAgent()
	named.Spec.Runtime.DefaultAllowedTools = []string{write}
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	if _, _, err := adjustInputsForForwardingWrites(context.Background(), reader, forwardingWriteTask(), named); err == nil ||
		!isPermanentACPAgentConfigurationError(err) {
		t.Fatalf("a directly named write tool: err = %v, want a permanent refusal", err)
	}
}

// TestForwardingWriteDescriptorIsConsequential covers the derived write tool:
// named after its Tool, consequential, with the write schema, and refused for
// a Tool that does not allow writes.
func TestForwardingWriteDescriptorIsConsequential(t *testing.T) {
	tool := writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalRequired)
	read, err := customACPMCPToolDescriptor(tool)
	if err != nil {
		t.Fatal(err)
	}
	write, err := forwardingWriteDescriptor(tool, read)
	if err != nil {
		t.Fatal(err)
	}
	if write.Name != "azure-arm:write" || write.Effect != harnessv2.MCPToolEffectConsequential || write.Forward == nil || !write.Forward.Write ||
		!strings.Contains(string(write.InputSchema), `"PUT"`) || read.Forward.Write {
		t.Fatalf("write descriptor = %#v", write)
	}
	if _, err := forwardingWriteDescriptor(forwardingDescriptorTool(), read); err == nil {
		t.Fatal("a Tool without writes must not get a write tool")
	}
}

// TestOnlyForwardingWritesNeedApprovalOnBuiltInRuntimes covers the approval
// gate: a built-in runtime may carry approval-required forwarding writes,
// whose gateway waits for the decision, but no other approval-required tool.
func TestOnlyForwardingWritesNeedApprovalOnBuiltInRuntimes(t *testing.T) {
	if !onlyForwardingWritesNeedApproval(harnessv2.MCPApprovalPolicy{RequiredTools: []string{"azure-arm:write"}}) {
		t.Fatal("forwarding writes alone must be accepted")
	}
	if onlyForwardingWritesNeedApproval(harnessv2.MCPApprovalPolicy{RequiredTools: []string{"azure-arm:write", "gh_comment"}}) {
		t.Fatal("another approval-required tool must still need an approval-capable runtime")
	}
	if onlyForwardingWritesNeedApproval(harnessv2.MCPApprovalPolicy{}) {
		t.Fatal("an empty approval policy is not a forwarding-writes policy")
	}
}
