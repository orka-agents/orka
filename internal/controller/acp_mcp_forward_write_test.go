package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tools"
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
	for _, denied := range []string{"azure-arm", write} {
		task := forwardingWriteTask()
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{denied}}
		reader := fake.NewClientBuilder().WithScheme(newTestScheme()).
			WithObjects(writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalRequired)).Build()
		task, agent, err := adjustInputsForForwardingWrites(context.Background(), reader, task, forwardingWriteAgent())
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(effectiveACPAllowedTools(task, agent), write) || agent.Spec.Coordination != nil {
			t.Errorf("a Task that denies %q still got the write tool", denied)
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

// TestForwardingWriteToolIsCheckedAgainstItsTool covers the broker's drift
// check before an approval is spent: the write tool is held to its
// forwarding Tool, so it passes while the Tool still allows writes and fails
// once they are withdrawn.
func TestForwardingWriteToolIsCheckedAgainstItsTool(t *testing.T) {
	tool := writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalRequired)
	read, err := customACPMCPToolDescriptor(tool)
	if err != nil {
		t.Fatal(err)
	}
	write, err := forwardingWriteDescriptor(tool, read)
	if err != nil {
		t.Fatal(err)
	}
	request := harnessv2.MCPBrokerCallRequest{Namespace: "tenant"}
	executor := RegistryACPMCPToolExecutor{Reader: fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(tool).Build()}
	if err := executor.ValidateACPMCPTool(context.Background(), request, write); err == nil {
		t.Fatal("a write the Tool requires approval for passed under a policy without it")
	}
	request.Authorization.ApprovalPolicy = harnessv2.MCPApprovalPolicy{RequiredTools: []string{write.Name}}
	if err := executor.ValidateACPMCPTool(context.Background(), request, write); err != nil {
		t.Fatalf("a current write tool failed revalidation: %v", err)
	}
	withdrawn := forwardingDescriptorTool()
	executor.Reader = fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(withdrawn).Build()
	if err := executor.ValidateACPMCPTool(context.Background(), request, write); err == nil {
		t.Fatal("a write tool whose Tool no longer allows writes passed revalidation")
	}
}

// TestConnectionFreezeSkipsForwardingWriteTools covers binding a Task whose
// policy carries a write tool: the freeze covers the forwarding Tool and
// does not look for a Tool named after the write tool.
func TestConnectionFreezeSkipsForwardingWriteTools(t *testing.T) {
	tool := writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalRequired)
	read, err := customACPMCPToolDescriptor(tool)
	if err != nil {
		t.Fatal(err)
	}
	write, err := forwardingWriteDescriptor(tool, read)
	if err != nil {
		t.Fatal(err)
	}
	configuration := harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{Tools: []harnessv2.MCPToolDescriptor{read, write}}}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: "tenant"},
		Spec:       corev1alpha1.OutboundAccessPolicySpec{AgentIdentity: &corev1alpha1.AgentIdentityOutboundAccess{}},
	}
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(tool, policy).Build()
	_, _, names, err := freezeRequesterConnectionsClassified(context.Background(), reader, nil, forwardingWriteTask(), configuration)
	if err != nil {
		t.Fatalf("freeze with a write tool: %v", err)
	}
	if !slices.Equal(names, []string{"azure-arm"}) {
		t.Fatalf("frozen tools = %v, want the forwarding Tool only", names)
	}
}

// TestForwardingWritesPlanOnABuiltInRuntime covers the planning path a Codex
// Task with a write-enabled forwarding Tool takes: the adjusted inputs plan,
// build a policy whose only approval-required tool is the write tool, and
// freeze without looking for a Tool named after it.
func TestForwardingWritesPlanOnABuiltInRuntime(t *testing.T) {
	tool := writeForwardingTool(corev1alpha1.HTTPForwardWriteApprovalRequired)
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: "tenant"},
		Spec:       corev1alpha1.OutboundAccessPolicySpec{AgentIdentity: &corev1alpha1.AgentIdentityOutboundAccess{}},
	}
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(tool, policy).Build()
	agent := forwardingWriteAgent()
	agent.UID, agent.Generation = "agent-uid", 1
	agent.Spec.Model = &corev1alpha1.ModelConfig{Name: "model"}
	agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV2)
	agent.Spec.Runtime.DefaultAllowedTools = append(acp.BuiltInRuntimeNativeToolNames("codex"), "azure-arm")
	task := forwardingWriteTask()
	task.UID = "task-uid"
	task, agent, err := adjustInputsForForwardingWrites(context.Background(), reader, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfigurationWithRegistry(context.Background(), reader, task, agent, plan.Profile, tools.NewRegistry())
	if err != nil {
		t.Fatalf("policy for a write-enabled forwarding Tool: %v", err)
	}
	write, ok := configuration.ToolPolicy.Descriptor("azure-arm:write")
	if !ok || write.Forward == nil || !write.Forward.Write || !configuration.ApprovalPolicy.Requires("azure-arm:write") ||
		configuration.ApprovalPolicy.NeedsRuntimeApprovals(configuration.ToolPolicy) {
		t.Fatalf("write tool = %#v, approval = %#v", write, configuration.ApprovalPolicy)
	}
	if _, _, _, err := freezeRequesterConnectionsClassified(context.Background(), reader, nil, task, configuration); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	// A policy that drops the write tool's approval, as a registered
	// external runtime's could, is refused while the Tool requires it.
	unapproved := agent.DeepCopy()
	unapproved.Spec.Coordination = nil
	plan, err = PlanACPRuntime(task, unapproved, ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeSessionMCPConfigurationWithRegistry(context.Background(), reader, task, unapproved, plan.Profile, tools.NewRegistry()); err == nil ||
		!strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("a policy without the write tool's approval: %v", err)
	}
}
