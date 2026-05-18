/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestValidateChildTaskAgainstParentTransactionUsesAllowedAgentsForDelegation(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace":     defaultNamespace,
		"agent":         "coordinator",
		"allowedAgents": `["coordinator","researcher"]`,
	}
	child := childTaskForAgent(testResearcherAgentName)

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
	child := childTaskForAgent(testResearcherAgentName)

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
	child := childTaskForAgent(testResearcherAgentName)

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

func childTaskForAgent(agentName string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{
				Name: agentName,
			},
		},
	}
}
