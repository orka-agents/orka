package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentToolPolicyRoundTripsWithoutDefaultingExistingAgents(t *testing.T) {
	for _, mode := range []AgentToolPolicyMode{"", AgentToolPolicyFull, AgentToolPolicyRestricted} {
		original := AgentCLIRuntime{Type: AgentRuntimeOpencode, ToolPolicy: mode, DefaultAllowedTools: []string{}}
		encoded, err := json.Marshal(original)
		require.NoError(t, err)
		var restored AgentCLIRuntime
		require.NoError(t, json.Unmarshal(encoded, &restored))
		require.Equal(t, mode, restored.ToolPolicy)
		require.NotNil(t, restored.DefaultAllowedTools)
		if mode == "" {
			require.NotContains(t, string(encoded), "toolPolicy")
		}
	}
}

func TestTaskRuntimeCannotSelectFullNativeTools(t *testing.T) {
	var task Task
	require.NoError(t, json.Unmarshal([]byte(`{"spec":{"type":"agent","agentRuntime":{"toolPolicy":"full"}}}`), &task))
	encoded, err := json.Marshal(task.Spec.AgentRuntime)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "toolPolicy")
}
