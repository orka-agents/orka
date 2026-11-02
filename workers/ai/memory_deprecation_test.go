/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestAgentLoopMemoryDeprecationWarnings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    []string
		deprecated []string
		calls      []string
		want       []string
	}{
		{name: "legacy calls warn", enabled: aitools.MemoryToolNames(), deprecated: aitools.MemoryToolNames(),
			calls: aitools.MemoryToolNames(), want: aitools.MemoryToolNames()},
		{name: "explicit tool does not warn", enabled: []string{"recall_memory"},
			deprecated: []string{"remember", "propose_memory", "search_transcript"}, calls: []string{"recall_memory"}},
		{name: "coordination does not warn", enabled: aitools.MemoryToolNames(), calls: []string{"remember"}},
		{name: "rejected call does not warn", deprecated: aitools.MemoryToolNames(), calls: []string{"recall_memory"}},
		{name: "unused tools do not warn", enabled: aitools.MemoryToolNames(), deprecated: aitools.MemoryToolNames()},
		{name: "non-memory tool does not warn", enabled: []string{"web_search"},
			deprecated: []string{"web_search"}, calls: []string{"web_search"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := replaceDefaultToolRegistryForTest(t)
			defer restore()
			for _, name := range append(aitools.MemoryToolNames(), "web_search") {
				tools.DefaultRegistry.Register(staticTestTool{name: name})
			}
			t.Setenv(workerenv.AIDeprecatedMemoryTools, workerenv.JoinCSV(tc.deprecated))
			logs := captureNativeReplyStderr(t)
			provider := &mockProvider{responses: []*llm.CompletionResponse{}}
			if len(tc.calls) > 0 {
				response := &llm.CompletionResponse{StopReason: "tool_use"}
				for _, name := range tc.calls {
					response.ToolCalls = append(response.ToolCalls, llm.ToolCall{
						ID: name, Name: name, Arguments: json.RawMessage(`{}`),
					})
				}
				provider.responses = append(provider.responses, response)
			}
			provider.responses = append(provider.responses, &llm.CompletionResponse{Content: "done", StopReason: "end_turn"})
			result, err := executeAgentLoopWithEvents(t.Context(), provider,
				[]llm.Message{{Role: "user", Content: "work"}}, "", "test-model", modelSettings{maxTokens: 4096},
				buildLLMTools(tc.enabled, nil), nil, nil, common.NewFakeEventRecorder())
			require.NoError(t, err)
			require.Equal(t, "done", result)
			output := strings.TrimSpace(logs())
			if len(tc.want) == 0 {
				require.Empty(t, output)
				return
			}
			lines := strings.Split(output, "\n")
			require.Len(t, lines, len(tc.want))
			for i, line := range lines {
				var warning map[string]string
				require.NoError(t, json.Unmarshal([]byte(line), &warning))
				require.Len(t, warning, 3, "warnings must not include arguments or memory contents")
				require.Equal(t, "warning", warning["level"])
				require.Equal(t, tc.want[i], warning["tool"])
				require.Contains(t, warning["message"], "next release")
				require.Contains(t, warning["message"], "spec.tools")
				require.Contains(t, warning["message"], "#680")
			}
		})
	}
}
