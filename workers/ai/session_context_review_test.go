package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/sessioncontext"
	toolspkg "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestSessionCheckpointProactiveNoteAdmissionFailureKeepsFittingRequest(t *testing.T) {
	newWorkerContextFixture(t, 6000)
	state, active, err := newWorkerSessionContext(context.Background(), []llm.Message{{Role: "user",
		Content: "Keep the current requirements."}})
	require.NoError(t, err)
	message, err := state.persist(context.Background(), llm.Message{Role: "assistant",
		Content: strings.Repeat("x", 100)})
	require.NoError(t, err)
	active = append(active, message)
	req := &llm.CompletionRequest{Messages: active, SystemPrompt: strings.Repeat("i", 18000),
		ContextWindow: 6000, MaxTokens: 512}
	require.NoError(t, llm.CheckContextWindow(req))
	require.Greater(t, llm.EstimateRequestTokens(req)+llm.ResponseTokenReserve(req), req.ContextWindow*4/5)
	before := slices.Clone(req.Messages)
	checkpointCalls := 0
	provider := contextFixtureProvider{complete: func(_ context.Context,
		_ *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		checkpointCalls++
		data, marshalErr := json.Marshal(checkpointDraft{Goal: checkpointClaim{
			Text: strings.Repeat("evidence ", 400), Sources: []string{active[0].ID},
		}})
		require.NoError(t, marshalErr)
		return &llm.CompletionResponse{Content: string(data), StopReason: "stop"}, nil
	}}
	err = state.fit(context.Background(), provider, req, false, common.NoopEventRecorder{})
	require.NoError(t, err, "an optional checkpoint that cannot fit must not stop the fitting request")
	require.Equal(t, 1, checkpointCalls)
	require.Equal(t, before, req.Messages)
	require.NoError(t, llm.CheckContextWindow(req))
}

func TestSessionCheckpointParkedApprovalKeepsCompleteExchange(t *testing.T) {
	f := newWorkerContextFixture(t, 10000)
	defer replaceDefaultToolRegistryForTest(t)()
	executions := 0
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions++
		},
	})
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: "read_incident",
		onExecute: func(json.RawMessage) {
			executions++
		},
	})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	provider := &mockProvider{response: &llm.CompletionResponse{
		Content: "Need an approved action.",
		ToolCalls: []llm.ToolCall{
			{ID: "call-dispatch", Name: gatedDispatchTool, Arguments: json.RawMessage(`{"incident":"inc-1"}`)},
			{ID: "call-read", Name: "read_incident", Arguments: json.RawMessage(`{}`)},
		},
		StopReason: "tool_use",
	}}
	result, err := executeAgentLoopWithEvents(context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "Handle the incident."}}, "", "fixture", modelSettings{maxTokens: 512},
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool, "read_incident"}), nil, nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: f.write.Namespace, TaskID: f.write.OwnerName, TaskUID: f.write.OwnerUID})
	require.NoError(t, err)
	require.Contains(t, result, "approval requested")
	require.Zero(t, executions)
	saved, err := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
	require.NoError(t, err)
	messages := make([]llm.Message, 0, len(saved))
	for _, source := range saved {
		message, convertErr := sessioncontext.ModelMessage(source)
		require.NoError(t, convertErr)
		messages = append(messages, message)
	}
	_, err = llm.FitMessagesKeeping(messages, 10000, 0)
	require.NoError(t, err, "parking a known unexecuted action must leave a complete exchange for a later Task")
	var calls []llm.ToolCall
	for _, message := range messages {
		calls = append(calls, message.ToolCalls...)
	}
	require.Len(t, calls, 2)
	for _, call := range calls {
		require.True(t, slices.ContainsFunc(messages, func(message llm.Message) bool {
			return message.Role == "tool" && message.ToolCallID == call.ID && strings.TrimSpace(message.Content) != ""
		}))
	}
}
