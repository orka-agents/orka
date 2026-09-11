package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestSessionContextRedactsConfiguredSecretsInToolArguments(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	const fixtureValue = "context-placeholder-value"
	t.Setenv("CONTEXT_TEST_SECRET", fixtureValue)
	t.Setenv("CONTEXT_TEST_NUMERIC_SECRET", "3141592653589793")
	state, _, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: "Inspect the selected record."}})
	require.NoError(t, err)
	arguments := json.RawMessage(`{"plain":"context-placeholder-value","selection":3141592653589793,` +
		`"nested":[{"escaped":"\u0063ontext-placeholder-value","record_id":9007199254740993}]}`)
	message := llm.Message{Role: "assistant",
		ToolCalls: []llm.ToolCall{{ID: "call-" + fixtureValue, Name: "inspect", Arguments: arguments}}}
	saved, err := state.persist(context.Background(), message)
	require.NoError(t, err)
	require.Equal(t, arguments, message.ToolCalls[0].Arguments,
		"saving a redacted source must not change execution arguments")
	page, err := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
		Namespace: f.write.Namespace, SessionName: f.write.SessionName, MessageID: saved.ID, Limit: 4096,
	})
	require.NoError(t, err)
	require.NotContains(t, page.Data, fixtureValue)
	require.NotContains(t, page.Data, "3141592653589793")
	require.Contains(t, page.Data, "[REDACTED]")
	require.Contains(t, page.Data, "9007199254740993")
	require.Len(t, saved.ToolCalls, 1)
	require.JSONEq(t, `{"plain":"[REDACTED]","selection":"[REDACTED]",`+
		`"nested":[{"escaped":"[REDACTED]","record_id":9007199254740993}]}`, string(saved.ToolCalls[0].Arguments))
	require.Contains(t, string(saved.ToolCalls[0].Arguments), "9007199254740993")
	result, err := state.persist(context.Background(), llm.Message{
		Role: "tool", ToolCallID: message.ToolCalls[0].ID, Name: "inspect", Content: "Found the record.",
	})
	require.NoError(t, err)
	require.Equal(t, saved.ToolCalls[0].ID, result.ToolCallID)
	_, err = llm.FitMessages([]llm.Message{saved, result}, 1000)
	require.NoError(t, err, "redaction must preserve the saved call/result relationship")
}

func TestSessionCheckpointRejectsEscapedSecretsBeforeCommit(t *testing.T) {
	for _, field := range []string{"goal", "constraint", "finding", "remaining", "question", "source", "quoted value"} {
		t.Run(field, func(t *testing.T) {
			f := newWorkerContextFixture(t, 32768)
			fixtureValue := "context-placeholder-value"
			if field == "quoted value" {
				fixtureValue += "\"\nextra"
			}
			t.Setenv("CONTEXT_TEST_SECRET", fixtureValue)
			state, active, err := newWorkerSessionContext(context.Background(),
				[]llm.Message{{Role: "user", Content: "Continue the investigation."}})
			require.NoError(t, err)
			prior, err := state.persist(context.Background(),
				llm.Message{Role: "assistant", Content: "The parser is the next step."})
			require.NoError(t, err)
			active = append(active, prior)
			draft := checkpointDraft{Goal: checkpointClaim{
				Text: "Continue the investigation.", Sources: []string{state.current.ID},
			}}
			switch field {
			case "goal", "quoted value":
				draft.Goal.Text = fixtureValue
			case "constraint":
				draft.Constraints = []checkpointClaim{{Text: fixtureValue, Sources: []string{state.current.ID}}}
			case "finding":
				draft.Findings = []checkpointClaim{{Text: fixtureValue, Sources: []string{prior.ID}}}
			case "remaining":
				draft.Remaining = []string{fixtureValue}
			case "question":
				draft.Questions = []string{fixtureValue}
			case "source":
				draft.Goal.Sources = []string{fixtureValue}
			}
			data, err := json.Marshal(draft)
			require.NoError(t, err)
			escaped := strings.ReplaceAll(string(data), "context-placeholder", `\u0063ontext-placeholder`)
			require.Equal(t, escaped, sanitizeCheckpointText(escaped), "fixture must exercise decoded-string validation")
			provider := contextFixtureProvider{complete: func(context.Context,
				*llm.CompletionRequest) (*llm.CompletionResponse, error) {
				return &llm.CompletionResponse{Content: escaped, StopReason: "stop"}, nil
			}}
			_, err = state.makeCheckpoint(context.Background(), provider, &llm.CompletionRequest{
				Model: "fixture", Messages: active, ContextWindow: 32768, MaxTokens: 512,
			}, common.NoopEventRecorder{})
			require.ErrorContains(t, err, "secret content")
			_, err = f.store.LoadSessionCheckpoint(context.Background(), f.write.Namespace, f.write.SessionName, "")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
	_, _, err := parseCheckpointNote(`{"goal":{"text":"\u0043ontinue work.","sources":["source"]}}`,
		map[string]bool{"source": true})
	require.NoError(t, err, "safe JSON escapes remain valid")
}

func TestSessionContextBootstrapPreservesNumericArguments(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	arguments := json.RawMessage(`{"record_id":9007199254740993}`)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-call", Role: "assistant",
			ToolCalls: []llm.ToolCall{{ID: "numeric-call", Name: "inspect", Arguments: arguments}}},
		{ID: "prior-result", Role: "tool", ToolCallID: "numeric-call", Name: "inspect", Content: "Found the record."},
	})
	require.NoError(t, err)
	_, active, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: "Continue the investigation."}})
	require.NoError(t, err)
	require.Len(t, active, 3)
	require.Len(t, active[0].ToolCalls, 1)
	require.Equal(t, arguments, active[0].ToolCalls[0].Arguments)
}

func TestSessionCheckpointIncludesCurrentRequestOnce(t *testing.T) {
	f := newWorkerContextFixture(t, 8192)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-user", Role: "user", Content: strings.Repeat("u", 4000)},
		{ID: "prior-assistant", Role: "assistant", Content: strings.Repeat("a", 8000)},
	})
	require.NoError(t, err)
	current := strings.Repeat("c", 12000)
	state, active, err := newWorkerSessionContext(context.Background(), []llm.Message{{Role: "user", Content: current}})
	require.NoError(t, err)
	recent, err := state.persist(context.Background(), llm.Message{Role: "assistant", Content: strings.Repeat("r", 8000)})
	require.NoError(t, err)
	req := &llm.CompletionRequest{Model: "fixture", Messages: append(active, recent),
		SystemPrompt: "Instructions", ContextWindow: 8192, MaxTokens: 512}
	require.ErrorIs(t, llm.CheckContextWindow(req), llm.ErrContextLimit)
	calls := 0
	provider := contextFixtureProvider{complete: func(_ context.Context,
		request *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		calls++
		require.NoError(t, llm.CheckContextWindow(request))
		var payload struct {
			CurrentRequestID string                 `json:"currentRequestMessageID"`
			Sources          []store.SessionMessage `json:"sources"`
		}
		require.NoError(t, json.Unmarshal([]byte(request.Messages[0].Content), &payload))
		require.Equal(t, state.current.ID, payload.CurrentRequestID)
		require.Equal(t, 1, strings.Count(request.Messages[0].Content, current))
		require.True(t, slices.ContainsFunc(payload.Sources, func(source store.SessionMessage) bool {
			return source.ID == state.current.ID && source.Role == "user" && source.Content == current
		}))
		return fixtureCheckpoint(request), nil
	}}
	require.NoError(t, state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{}))
	require.Equal(t, 1, calls)
	require.NoError(t, llm.CheckContextWindow(req))
	require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.ID == state.current.ID && message.Content == current
	}))
}

func TestSessionHistoryPagesReachModelWithCompleteDataAndCursor(t *testing.T) {
	for _, window := range []int{32768, 10000} {
		for _, padded := range []bool{false, true} {
			t.Run(fmt.Sprintf("window-%d-padded-%t", window, padded), func(t *testing.T) {
				name := readSessionHistoryTool
				if padded {
					name = " " + name + "\t"
				}
				testSessionHistoryPagesReachModel(t, window, name)
			})
		}
	}
}

func testSessionHistoryPagesReachModel(t *testing.T, window int, toolName string) {
	t.Helper()
	f := newWorkerContextFixture(t, window)
	content := strings.Repeat("evidence \n", 4000)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "prior-source", Role: "assistant", Content: content},
	})
	require.NoError(t, err)
	var assembled strings.Builder
	pages := 0
	checkpoints := 0
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		require.NoError(t, llm.CheckContextWindow(req))
		if req.SystemPrompt == checkpointInstructions {
			checkpoints++
			return fixtureCheckpoint(req), nil
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			var page store.SessionHistoryResult
			require.NoError(t, json.Unmarshal([]byte(last.Content), &page))
			require.Equal(t, "prior-source", page.MessageID)
			require.Equal(t, assembled.Len(), page.Offset)
			require.Equal(t, page.Offset+len(page.Data), page.NextOffset)
			require.LessOrEqual(t, len(page.Data), store.MaxSessionHistoryReadBytes)
			assembled.WriteString(page.Data)
			pages++
			if page.NextOffset == page.TotalBytes {
				var original store.SessionMessage
				require.NoError(t, json.Unmarshal([]byte(assembled.String()), &original))
				require.Equal(t, content, original.Content)
				return &llm.CompletionResponse{Content: "Read all saved evidence.", StopReason: "stop"}, nil
			}
			require.Greater(t, len(last.Content), store.MaxSessionContextPreviewBytes)
		}
		arguments, marshalErr := json.Marshal(map[string]any{
			"message_id": "prior-source", "offset": assembled.Len(), "limit": store.MaxSessionHistoryReadBytes,
		})
		require.NoError(t, marshalErr)
		return &llm.CompletionResponse{StopReason: "tool_calls", ToolCalls: []llm.ToolCall{{
			ID: fmt.Sprintf("history-%d", assembled.Len()), Name: toolName, Arguments: arguments,
		}}}, nil
	}}
	result, err := executeAgentLoopWithEvents(context.Background(), provider,
		[]llm.Message{{Role: "user", Content: "Read all earlier evidence."}}, "Instructions", "fixture",
		modelSettings{maxTokens: 512}, nil, nil, nil, common.NoopEventRecorder{})
	require.NoError(t, err)
	require.Equal(t, "Read all saved evidence.", result)
	require.Greater(t, pages, 1)
	if window == 10000 {
		require.Positive(t, checkpoints, "already consumed pages must remain eligible for compaction")
	}
	transcript, err := f.store.LoadTranscript(context.Background(), f.write.Namespace, f.write.SessionName, 0)
	require.NoError(t, err)
	for _, message := range transcript {
		require.LessOrEqual(t, len(message.Content), store.MaxSessionContextPreviewBytes)
	}
}

func TestSessionHistoryRedactedPagesKeepSourceAndEnvelope(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	content := strings.Repeat("evidence ", 4000) + "token is [REDACTED]"
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "redacted-source", Role: "assistant", Content: content},
	})
	require.NoError(t, err)
	state, _, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: "Read saved evidence."}})
	require.NoError(t, err)
	var assembled strings.Builder
	for {
		arguments, marshalErr := json.Marshal(map[string]any{
			"message_id": "redacted-source", "offset": assembled.Len(), "limit": store.MaxSessionHistoryReadBytes,
		})
		require.NoError(t, marshalErr)
		content, readErr := state.readHistory(context.Background(), arguments)
		require.NoError(t, readErr)
		var original store.SessionHistoryResult
		require.NoError(t, json.Unmarshal([]byte(content), &original))
		saved, saveErr := state.persist(context.Background(), llm.Message{
			Role: "tool", Name: readSessionHistoryTool, ToolCallID: "history-call", Content: content,
		})
		require.NoError(t, saveErr)
		var delivered store.SessionHistoryResult
		require.NoError(t, json.Unmarshal([]byte(saved.Content), &delivered))
		require.Equal(t, original, delivered, "redaction must leave the saved fragment and its byte cursor intact")
		var savedSource strings.Builder
		for {
			page, pageErr := f.store.ReadSessionHistory(context.Background(), store.SessionHistoryRead{
				Namespace: f.write.Namespace, SessionName: f.write.SessionName,
				MessageID: saved.ID, Offset: savedSource.Len(), Limit: store.MaxSessionHistoryReadBytes,
			})
			require.NoError(t, pageErr)
			savedSource.WriteString(page.Data)
			if page.NextOffset == page.TotalBytes {
				break
			}
		}
		var archived store.SessionMessage
		require.NoError(t, json.Unmarshal([]byte(savedSource.String()), &archived))
		require.JSONEq(t, content, archived.Content)
		assembled.WriteString(delivered.Data)
		if delivered.NextOffset == delivered.TotalBytes {
			break
		}
	}
	var recovered store.SessionMessage
	require.NoError(t, json.Unmarshal([]byte(assembled.String()), &recovered))
	require.Equal(t, content, recovered.Content)
}

func TestSessionCheckpointDoesNotEvictUnreadHistoryPage(t *testing.T) {
	f := newWorkerContextFixture(t, 32768)
	_, err := f.store.AppendContextMessages(context.Background(), f.write, []store.SessionMessage{
		{ID: "read-source", Role: "assistant", Content: strings.Repeat("evidence ", 2000)},
	})
	require.NoError(t, err)
	state, _, err := newWorkerSessionContext(context.Background(),
		[]llm.Message{{Role: "user", Content: strings.Repeat("current request ", 256)}})
	require.NoError(t, err)
	arguments := json.RawMessage(`{"message_id":"read-source","limit":16384}`)
	call, err := state.persist(context.Background(), llm.Message{
		Role:      "assistant",
		ToolCalls: []llm.ToolCall{{ID: "history-call", Name: readSessionHistoryTool, Arguments: arguments}},
	})
	require.NoError(t, err)
	content, err := state.readHistory(context.Background(), arguments)
	require.NoError(t, err)
	result, err := state.persist(context.Background(), llm.Message{
		Role: "tool", ToolCallID: "history-call", Name: readSessionHistoryTool, Content: content,
	})
	require.NoError(t, err)
	req := &llm.CompletionRequest{Model: "fixture", SystemPrompt: "Instructions", MaxTokens: 512,
		Tools: []llm.Tool{sessionHistoryToolDefinition()}, Messages: []llm.Message{state.current, call, result}}
	req.ContextWindow = llm.EstimateRequestTokens(req) + llm.ResponseTokenReserve(req) + 32
	provider := contextFixtureProvider{complete: func(_ context.Context,
		req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
		return fixtureCheckpoint(req), nil
	}}
	require.NoError(t, state.fit(context.Background(), provider, req, false, common.NoopEventRecorder{}))
	require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.ID == result.ID && message.Content == content
	}), "the normal model must receive the complete requested page before compaction may remove it")
	require.NoError(t, llm.CheckContextWindow(req))
	require.False(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.Name == sessioncontext.CheckpointName
	}))

	// Required reduction may discard older history while preserving the entire
	// unread call/result. Its framing and data also remain fixed during recovery.
	older, err := sessioncontext.ModelMessage(state.sources["read-source"])
	require.NoError(t, err)
	req.Messages = append([]llm.Message{older}, req.Messages...)
	req.ContextWindow += 1024
	require.ErrorIs(t, llm.CheckContextWindow(req), llm.ErrContextLimit)
	recoveryWindow := contextRecoveryWindow(req, fmt.Errorf("context length exceeded"),
		state.currentIndex(req.Messages), state.requiredHistoryIndices(req.Messages)...)
	require.Greater(t, recoveryWindow, llm.EstimateRequestTokens(&llm.CompletionRequest{
		Messages: []llm.Message{state.current, call, result},
	}))
	require.NoError(t, state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{}))
	require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
		return message.ID == result.ID && message.Content == content
	}))
	require.False(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool { return message.ID == older.ID }))
	require.NoError(t, llm.CheckContextWindow(req))

	original := slices.Clone(req.Messages)
	req.ContextWindow = llm.EstimateRequestTokens(req) + llm.ResponseTokenReserve(req) - 128
	require.Error(t, state.fit(context.Background(), provider, req, true, common.NoopEventRecorder{}))
	require.Equal(t, original, req.Messages, "an impossible fit must not silently discard the unread page")
}

func TestContextRecoveryPreservesRequiredInputs(t *testing.T) {
	for _, fixedInput := range []string{"system prompt", "system message", "tool schema", "current request"} {
		t.Run(fixedInput, func(t *testing.T) {
			t.Setenv(workerenv.SessionCheckpointsEnabled, "false")
			const providerWindow = 9000
			large := strings.Repeat("s", 24000)
			messages := []llm.Message{
				{Role: "user", Content: strings.Repeat("u", 8000)},
				{Role: "assistant", Content: strings.Repeat("a", 8000)},
				{ID: "current", Role: "user", Content: "Continue investigating."},
			}
			var system string
			var requestTools []llm.Tool
			switch fixedInput {
			case "system prompt":
				system = large
			case "system message":
				messages = append([]llm.Message{{Role: "system", Content: large}}, messages...)
			case "tool schema":
				requestTools = []llm.Tool{{Name: "inspect",
					Parameters: json.RawMessage(`{"type":"object","description":"` + large + `"}`)}}
			case "current request":
				messages[len(messages)-1].Content = large
			}
			current := messages[len(messages)-1]
			calls := 0
			provider := contextFixtureProvider{complete: func(_ context.Context,
				req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				calls++
				require.Equal(t, system, req.SystemPrompt)
				require.Equal(t, requestTools, req.Tools)
				require.True(t, slices.ContainsFunc(req.Messages, func(message llm.Message) bool {
					return message.ID == current.ID && message.Role == current.Role && message.Content == current.Content
				}))
				if fixedInput == "system message" {
					require.Equal(t, messages[0], req.Messages[0])
				}
				if llm.EstimateRequestTokens(req)+llm.ResponseTokenReserve(req) > providerWindow {
					return nil, &llm.ProviderError{StatusCode: http.StatusBadRequest, Message: "context window too long"}
				}
				return &llm.CompletionResponse{Content: "Finished the investigation.", StopReason: "stop"}, nil
			}}
			result, err := executeAgentLoopWithEvents(context.Background(), provider, messages, system, "fixture",
				modelSettings{maxTokens: 512}, requestTools, nil, nil, common.NoopEventRecorder{})
			require.NoError(t, err)
			require.Equal(t, "Finished the investigation.", result)
			require.Equal(t, 2, calls)
		})
	}
}
