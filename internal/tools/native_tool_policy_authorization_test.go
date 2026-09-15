package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestDelegationCannotNarrowFullToolsToFitTransaction(t *testing.T) {
	parent := parentTask()
	parent.Spec.Transaction.Context = map[string]string{
		"namespace": defaultNamespace, "allowedAgents": `["researcher"]`, "allowedTools": `["Read","Bash","delegate_task"]`,
	}
	agent := researcherAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
		Type: corev1alpha1.AgentRuntimeOpencode, ToolPolicy: corev1alpha1.AgentToolPolicyFull,
		DefaultAllowedTools: []string{"delegate_task"},
	}
	child := childTaskForResearcherAgent()
	child.Spec.Type = corev1alpha1.TaskTypeAgent
	child.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Read"}, AllowBash: new(false)}
	err := validateChildTaskAgainstParentTransaction(t.Context(), newFakeClient(agent), parent, child, testResearcherAgentName)
	require.ErrorContains(t, err, "agent runtime tools are unrestricted")
}

func TestRestrictedNativeToolDelegationUsesCanonicalDenials(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(provider), func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: provider, ToolPolicy: corev1alpha1.AgentToolPolicyRestricted, DefaultAllowedTools: []string{"Read", "gLoB"},
			}}}
			child := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"rEaD"}},
			}}
			allowed, bash := childTransactionEffectiveRuntimePolicy(child, agent)
			require.False(t, bash)
			require.Len(t, allowed, 1)
			require.Equal(t, "glob", strings.ToLower(allowed[0]))
			require.NoError(t, validateChildToolConstraints(map[string]string{"allowedTools": `["gLoB"]`}, childTransactionContext{
				agent: agent, childType: corev1alpha1.TaskTypeAgent, runtimeTools: allowed, runtimeBash: bash,
			}))
		})
	}
}

func TestCreateAgentToolCannotSelectFullNativeTools(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	kubeClient := newFakeClient(parentTask())
	tool := NewCreateAgentTool(kubeClient, executionmode.HarnessV2)
	result, err := tool.Execute(t.Context(), json.RawMessage(`{
		"role":"researcher",
		"model":{"provider":"openai","name":"test-model","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode","toolPolicy":"full"}
	}`))
	require.NoError(t, err)
	var created CreateAgentResult
	require.NoError(t, json.Unmarshal([]byte(result), &created))
	var agent corev1alpha1.Agent
	require.NoError(t, kubeClient.Get(t.Context(), types.NamespacedName{Name: created.AgentName, Namespace: created.Namespace}, &agent))
	require.NotNil(t, agent.Spec.Runtime)
	require.Empty(t, agent.Spec.Runtime.ToolPolicy)
	require.NotContains(t, agent.Spec.Runtime.DefaultAllowedTools, "WebFetch")
}
