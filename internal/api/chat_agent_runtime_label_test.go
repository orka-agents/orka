package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAgentRuntimeLabelNamesExternalRuntimes(t *testing.T) {
	require.Equal(t, "opencode", agentRuntimeLabel(&corev1alpha1.AgentCLIRuntime{Type: "opencode"}))
	require.Equal(t, "external AgentRuntime fibey", agentRuntimeLabel(&corev1alpha1.AgentCLIRuntime{
		RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey"},
	}))
	require.Equal(t, "unspecified", agentRuntimeLabel(&corev1alpha1.AgentCLIRuntime{}))
}
