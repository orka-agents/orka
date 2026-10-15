/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentSpecToolsJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []ToolReference
		wire  string
	}{
		{name: "nil omitted"},
		{name: "empty preserved", tools: []ToolReference{}, wire: `[]`},
		{name: "nonempty unchanged", tools: []ToolReference{{Name: "web_search"}, {Name: "reply_in_conversation", Enabled: new(false)}}, wire: `[{"name":"web_search"},{"name":"reply_in_conversation","enabled":false}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := AgentSpec{
				ProviderRef:  &ProviderReference{Name: "provider"},
				Model:        &ModelConfig{Name: "model"},
				Tools:        tc.tools,
				Skills:       []SkillReference{{Name: "skill"}},
				Coordination: &CoordinationConfig{Enabled: true},
			}
			encoded, err := json.Marshal(original)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &fields))
			require.Equal(t, tc.wire, string(fields["tools"]))
			var decoded AgentSpec
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			require.Equal(t, original, decoded)
			if tc.tools == nil || len(tc.tools) > 0 {
				// Byte equality protects existing field order and fingerprints, not just JSON meaning.
				type plainAgentSpec AgentSpec
				previous, err := json.Marshal(plainAgentSpec(original))
				require.NoError(t, err)
				require.Equal(t, previous, encoded)
			}
		})
	}
}

func TestAISpecToolsJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
		wire  string
	}{
		{name: "nil omitted"},
		{name: "empty preserved", tools: []string{}, wire: `[]`},
		{name: "nonempty unchanged", tools: []string{"web_search", "reply_in_conversation"}, wire: `["web_search","reply_in_conversation"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := AISpec{
				ProviderRef: &ProviderReference{Name: "provider"},
				Provider:    "anthropic", Model: "model", Prompt: "prompt", SystemPrompt: "system",
				Temperature: new(0.5), MaxTokens: new(int32(1024)),
				Skills: []SkillReference{{Name: "skill"}}, Tools: tc.tools,
			}
			encoded, err := json.Marshal(original)
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &fields))
			require.Equal(t, tc.wire, string(fields["tools"]))
			var decoded AISpec
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			require.Equal(t, original, decoded)
			if tc.tools == nil || len(tc.tools) > 0 {
				type plainAISpec AISpec
				previous, err := json.Marshal(plainAISpec(original))
				require.NoError(t, err)
				require.Equal(t, previous, encoded)
			}
		})
	}
}
