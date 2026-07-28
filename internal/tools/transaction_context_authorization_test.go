/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/labels"
)

func TestValidateChildTaskAgainstParentTransactionUsesAllowedAgentsForDelegation(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"agent":         "coordinator",
		"allowedAgents": `["coordinator","researcher"]`,
	}
	child := childTaskForResearcherAgent()

	if err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(researcherAgent()), parent, child, testResearcherAgentName); err != nil {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsDisallowedAgent(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"agent":         "coordinator",
		"allowedAgents": `["coordinator"]`,
	}
	child := childTaskForResearcherAgent()

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(researcherAgent()), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), "is not allowed by transaction context") {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want allowedAgents denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsDisallowedProviderModelAndTool(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":        defaultNamespace,
		"allowedAgents":    `["researcher"]`,
		"allowedProviders": `["approved-provider"]`,
		"allowedModels":    `["approved-provider/approved-model"]`,
		"allowedTools":     `["file_read"]`,
	}
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "disallowed-provider", Namespace: defaultNamespace},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "disallowed-model",
		},
	}
	agent := researcherAgent()
	agent.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: provider.Name}
	agent.Spec.Tools = []corev1alpha1.ToolReference{{Name: "web_search"}}
	child := childTaskForResearcherAgent()

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(provider, agent), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want provider denial", err)
	}

	parent.Spec.Transaction.Context["allowedProviders"] = `["disallowed-provider"]`
	parent.Spec.Transaction.Context["allowedModels"] = `["disallowed-provider/disallowed-model"]`
	err = validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(provider, agent), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), `tool "web_search"`) {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want tool denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsProviderlessChildUnderProviderConstraints(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":        defaultNamespace,
		"allowedProviders": `["approved-provider"]`,
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "alpine:3.20",
		},
	}

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(), parent, child, "")
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want provider denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsUnrestrictedAgentRuntimeTools(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"allowedAgents": `["researcher"]`,
		"allowedTools":  `["Read"]`,
	}
	agent := researcherAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}
	child := childTaskForResearcherAgent()
	child.Spec.Type = corev1alpha1.TaskTypeAgent

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), "agent runtime tools are unrestricted") {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want unrestricted runtime tools denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsBlankAgentRuntimeTools(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"allowedAgents": `["researcher"]`,
		"allowedTools":  `["Read"]`,
	}
	agent := researcherAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
		Type:                corev1alpha1.AgentRuntimeCodex,
		DefaultAllowedTools: []string{" "},
	}
	child := childTaskForResearcherAgent()
	child.Spec.Type = corev1alpha1.TaskTypeAgent

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), "agent runtime tools are unrestricted") {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want unrestricted runtime tools denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsEnabledBashOutsideAllowedTools(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"allowedAgents": `["researcher"]`,
		"allowedTools":  `["Read"]`,
	}
	agent := researcherAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
		Type:                corev1alpha1.AgentRuntimeCodex,
		DefaultAllowedTools: []string{"Read"},
	}
	child := childTaskForResearcherAgent()
	child.Spec.Type = corev1alpha1.TaskTypeAgent

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), `tool "Bash"`) {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want bash tool denial", err)
	}
}

func TestChildTransactionEffectiveAIToolsSkipsDisabledCoordinationInjection(t *testing.T) {
	agent := researcherAgent()
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true}
	child := childTaskForResearcherAgent()
	child.Labels = map[string]string{labels.LabelParentTask: "parent-task"}
	child.Annotations = map[string]string{labels.AnnotationDisableCoordinationToolInject: "true"}
	child.Spec.Type = corev1alpha1.TaskTypeAI
	child.Spec.AI = &corev1alpha1.AISpec{
		Tools: []string{"list_pull_requests", "check_pr_review_marker"},
	}

	got := strings.Join(childTransactionEffectiveAITools(child, agent), ",")
	for _, tool := range []string{"list_pull_requests", "check_pr_review_marker"} {
		if !strings.Contains(got, tool) {
			t.Fatalf("expected explicit tool %q in %q", tool, got)
		}
	}
	for _, tool := range []string{"recall_memory", "remember", "propose_memory", "search_transcript"} {
		if !strings.Contains(got, tool) {
			t.Fatalf("expected memory tool %q in %q", tool, got)
		}
	}
	for _, tool := range []string{"delegate_task", "send_message", "check_messages", "request_approval", "merge_pull_request", "auto_merge_pull_request"} {
		if strings.Contains(got, tool) {
			t.Fatalf("unexpected coordination tool %q in %q", tool, got)
		}
	}
}

func TestChildTransactionEffectiveAIToolsIncludesPRReviewCoordinationTools(t *testing.T) {
	agent := researcherAgent()
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true}
	child := childTaskForResearcherAgent()
	child.Spec.Type = corev1alpha1.TaskTypeAI

	got := strings.Join(childTransactionEffectiveAITools(child, agent), ",")
	for _, tool := range []string{"list_pull_requests", "check_pr_review_marker"} {
		if !strings.Contains(got, tool) {
			t.Fatalf("expected PR review coordination tool %q in %q", tool, got)
		}
	}
}

func TestChildTransactionEffectiveAIToolsMatchesSharedResolver(t *testing.T) {
	agent := researcherAgent()
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true}
	child := childTaskForResearcherAgent()
	child.Labels = map[string]string{labels.LabelParentTask: "parent-task"}
	child.Spec.AI = &corev1alpha1.AISpec{Tools: []string{"task_tool"}}

	want := aitools.Resolve(child, agent)
	got := childTransactionEffectiveAITools(child, agent)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("child authorization tools = %#v, shared resolver = %#v", got, want)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsOmittedAutonomousApprovalTool(t *testing.T) {
	agent := researcherAgent()
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true}
	child := childTaskForResearcherAgent()
	parent := parentTaskWithAllowedTools(t, childTransactionEffectiveAITools(child, agent), "request_approval")

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName)
	if err == nil || !strings.Contains(err.Error(), `tool "request_approval"`) {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want request_approval denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionAllowsAutonomousApprovalTool(t *testing.T) {
	agent := researcherAgent()
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true}
	child := childTaskForResearcherAgent()
	parent := parentTaskWithAllowedTools(t, childTransactionEffectiveAITools(child, agent))

	if err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName); err != nil {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionRejectsOmittedChildMessagingTools(t *testing.T) {
	agent := researcherAgent()
	child := childTaskForResearcherAgent()
	child.Labels = map[string]string{labels.LabelParentTask: "parent-task"}
	parent := parentTaskWithAllowedTools(t, childTransactionEffectiveAITools(child, agent), "send_message", "check_messages")

	err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName)
	if err == nil || (!strings.Contains(err.Error(), `tool "send_message"`) && !strings.Contains(err.Error(), `tool "check_messages"`)) {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v, want child messaging denial", err)
	}
}

func TestValidateChildTaskAgainstParentTransactionAllowsChildMessagingTools(t *testing.T) {
	agent := researcherAgent()
	child := childTaskForResearcherAgent()
	child.Labels = map[string]string{labels.LabelParentTask: "parent-task"}
	parent := parentTaskWithAllowedTools(t, childTransactionEffectiveAITools(child, agent))

	if err := validateChildTaskAgainstParentTransaction(context.Background(), newFakeClient(agent), parent, child, testResearcherAgentName); err != nil {
		t.Fatalf("validateChildTaskAgainstParentTransaction() error = %v", err)
	}
}

func parentTaskWithAllowedTools(t *testing.T, tools []string, omitted ...string) *corev1alpha1.Task {
	t.Helper()
	omit := make(map[string]struct{}, len(omitted))
	for _, tool := range omitted {
		omit[tool] = struct{}{}
	}
	allowed := make([]string, 0, len(tools))
	for _, tool := range tools {
		if _, found := omit[tool]; !found {
			allowed = append(allowed, tool)
		}
	}
	encoded, err := json.Marshal(allowed)
	if err != nil {
		t.Fatalf("marshal allowed tools: %v", err)
	}
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"allowedAgents": `["researcher"]`,
		"allowedTools":  string(encoded),
	}
	return parent
}

func childTaskForResearcherAgent() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{
				Name: testResearcherAgentName,
			},
		},
	}
}
