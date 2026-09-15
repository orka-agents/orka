package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestFullNativeToolsCannotFitFiniteTransactionAuthority(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(provider), func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: provider, ToolPolicy: corev1alpha1.AgentToolPolicyFull, DefaultAllowedTools: []string{"delegate_task"},
			}}}
			for _, override := range []*corev1alpha1.AgentRuntimeSpec{nil, {AllowedTools: []string{}}, {AllowedTools: []string{"Read"}, AllowBash: new(false)}} {
				req := CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: override}
				allowed, bash := contextTokenTaskCreateEffectiveRuntimePolicy(req, agent)
				require.Nil(t, allowed)
				require.True(t, bash)
				token := &ContextToken{TransactionContext: map[string]any{"allowedTools": []any{"Read", "Bash", "delegate_task"}}}
				failures := contextTokenTaskToolFailures(token, contextTokenTaskCreateAuthorizationContext{
					Request: req, Agent: agent, RuntimeAllowedTools: allowed, RuntimeAllowBash: bash,
				})
				require.Contains(t, strings.Join(failures, "\n"), "unrestricted")
			}
			allowed, bash := contextTokenAgentRuntimeAuthorizationPolicy(agent)
			require.Nil(t, allowed)
			require.True(t, bash)
		})
	}
}

func TestRestrictedNativeToolAuthorizationUsesEffectiveCanonicalNames(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(provider), func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: provider, ToolPolicy: corev1alpha1.AgentToolPolicyRestricted, DefaultAllowedTools: []string{"Read", "gLoB"},
			}}}
			req := CreateTaskRequest{Type: corev1alpha1.TaskTypeAgent, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"rEaD"}}}
			allowed, bash := contextTokenTaskCreateEffectiveRuntimePolicy(req, agent)
			require.False(t, bash)
			require.Len(t, allowed, 1)
			require.Equal(t, "glob", strings.ToLower(allowed[0]))
			token := &ContextToken{TransactionContext: map[string]any{"allowedTools": []any{"gLoB"}}}
			require.Empty(t, contextTokenTaskToolFailures(token, contextTokenTaskCreateAuthorizationContext{
				Request: req, Agent: agent, RuntimeAllowedTools: allowed, RuntimeAllowBash: bash,
			}))
		})
	}
}
