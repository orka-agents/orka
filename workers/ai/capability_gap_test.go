/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/workers/common"
)

// A tool call the Task did not enable carries a closed errorType so the
// controller can count it as a capability gap without reading free text.
func TestToolCallFailedCarriesTypedRejection(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(recordingTool{name: "enabled_tool"})

	provider := &mockProvider{responses: []*llm.CompletionResponse{
		{
			ToolCalls: []llm.ToolCall{{
				ID: "missing-call", Name: "deploy_to_prod", Arguments: json.RawMessage(`{"target":"prod"}`),
			}},
			StopReason: "tool_use",
		},
		{Content: doneResult, StopReason: "end_turn"},
	}}
	recorder := common.NewFakeEventRecorder()
	if _, err := executeAgentLoopWithEvents(
		context.Background(), provider, []llm.Message{{Role: "user", Content: "deploy"}}, "", "test-model",
		modelSettings{maxTokens: 4096}, toolspkg.DefaultRegistry.ToLLMTools([]string{"enabled_tool"}), nil, nil, recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "gap-task", TaskUID: "gap-task-uid"},
	); err != nil {
		t.Fatalf("agent loop: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, event := range recorder.Events() {
			if event.Type != events.ExecutionEventTypeToolCallFailed {
				continue
			}
			var content map[string]any
			if err := json.Unmarshal(event.Content, &content); err != nil {
				t.Fatalf("ToolCallFailed content = %s: %v", event.Content, err)
			}
			if content[events.ToolCallErrorTypeContentKey] != events.ToolCallErrorTypeNotEnabled {
				t.Fatalf("errorType = %v, want %q", content[events.ToolCallErrorTypeContentKey], events.ToolCallErrorTypeNotEnabled)
			}
			if _, ok := content["arguments"]; ok {
				t.Fatalf("rejection content must not carry arguments: %v", content)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no ToolCallFailed event recorded; got %v", recorder.EventTypes())
}
