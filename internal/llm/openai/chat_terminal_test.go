/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
)

func chatTerminalDelta(t *testing.T, w http.ResponseWriter, delta any, finish string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"id": "chat-terminal", "model": "reported-model", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	require.NoError(t, err)
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	require.NoError(t, err)
}

func chatTerminalProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "local-fixture-only", BaseURL: server.URL})
	require.NoError(t, err)
	provider.mode.Store(int32(apiModeChatCompletions))
	return provider
}

func TestResponsesChatTerminalValidatesWholeCallSet(t *testing.T) {
	for _, tt := range []struct {
		name      string
		second    string
		finish    string
		wantCalls int
	}{
		{name: "parallel", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool","arguments":"{}"}}`, finish: "tool_calls", wantCalls: 2},
		{name: "malformed later arguments", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool","arguments":"{"}}`, finish: "tool_calls"},
		{name: "empty later arguments", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool","arguments":""}}`, finish: "tool_calls"},
		{name: "null arguments", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool","arguments":"null"}}`, finish: "tool_calls"},
		{name: "array arguments", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool","arguments":"[]"}}`, finish: "tool_calls"},
		{name: "missing name", second: `{"index":1,"id":"second","type":"function","function":{"arguments":"{}"}}`, finish: "tool_calls"},
		{name: "missing ID", second: `{"index":1,"type":"function","function":{"name":"second_tool","arguments":"{}"}}`, finish: "tool_calls"},
		{name: "duplicate ID", second: `{"index":1,"id":"first","type":"function","function":{"name":"second_tool","arguments":"{}"}}`, finish: "tool_calls"},
		{name: "unfinished later call", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool"}}`, finish: "length"},
		{name: "missing finish", second: `{"index":1,"id":"second","type":"function","function":{"name":"second_tool","arguments":"{}"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider := chatTerminalProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Include both calls in one delta, which JustFinishedToolCall
				// cannot reliably emit. No transition is required to complete them.
				chatTerminalDelta(t, w, json.RawMessage(`{"tool_calls":[{"index":0,"id":"first","type":"function","function":{"name":"first_tool","arguments":"{}"}},`+tt.second+`]}`), tt.finish)
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			})
			chunks, err := provider.Stream(t.Context(), &llm.CompletionRequest{Model: "test-model", ResponsesInput: true})
			require.NoError(t, err)
			var calls []llm.ToolCall
			var terminal llm.StreamChunk
			for chunk := range chunks {
				if chunk.ToolCall != nil {
					calls = append(calls, *chunk.ToolCall)
				}
				if chunk.Done {
					terminal = chunk
				}
			}
			require.True(t, terminal.Done)
			require.Len(t, calls, tt.wantCalls)
			if tt.wantCalls == 0 {
				require.Error(t, terminal.Error)
			} else {
				require.NoError(t, terminal.Error)
				require.Equal(t, "tool_calls", terminal.StopReason)
				require.Equal(t, "first", calls[0].ID)
				require.Equal(t, "second", calls[1].ID)
			}
		})
	}
}

func TestResponsesChatTerminalRejectsUnfinishedLegacyCalls(t *testing.T) {
	for _, delta := range []string{`{"function_call":{}}`, `{"function_call":{"name":"tool"}}`, `{"function_call":{"arguments":"{}"}}`} {
		t.Run(delta, func(t *testing.T) {
			provider := chatTerminalProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				chatTerminalDelta(t, w, map[string]any{"content": "text must not mask unfinished calls"}, "")
				chatTerminalDelta(t, w, json.RawMessage(delta), "stop")
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			})
			chunks, err := provider.Stream(t.Context(), &llm.CompletionRequest{Model: "test-model", ResponsesInput: true})
			require.NoError(t, err)
			var terminal llm.StreamChunk
			for chunk := range chunks {
				require.Nil(t, chunk.ToolCall)
				terminal = chunk
			}
			require.True(t, terminal.Done)
			require.Error(t, terminal.Error)
		})
	}
}

func TestChatTerminalPreservesTextUsageAndCancellation(t *testing.T) {
	for _, responsesInput := range []bool{false, true} {
		for _, cancelStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%t/cancel=%t", responsesInput, cancelStream), func(t *testing.T) {
				release := make(chan struct{})
				provider := chatTerminalProvider(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					chatTerminalDelta(t, w, json.RawMessage(`{"tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"client_tool","arguments":"{}"}}]}`), "")
					chatTerminalDelta(t, w, map[string]any{"content": "still streaming"}, "")
					_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":2,\"cache_write_tokens\":1}}}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-release:
						chatTerminalDelta(t, w, map[string]any{}, "tool_calls")
						_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
					case <-r.Context().Done():
					}
				})
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				chunks, err := provider.Stream(ctx, &llm.CompletionRequest{Model: "test-model", ResponsesInput: responsesInput})
				require.NoError(t, err)
				var text string
				var calls int
				for {
					chunk, ok := <-chunks
					require.True(t, ok, "stream closed before usage while waiting for terminal release")
					require.NoError(t, chunk.Error)
					text += chunk.Content
					if chunk.ToolCall != nil {
						calls++
					}
					if chunk.UsageReported {
						require.Equal(t, 7, chunk.InputTokens)
						require.Equal(t, 3, chunk.OutputTokens)
						require.EqualValues(t, 2, *chunk.CachedInputTokens)
						require.EqualValues(t, 1, *chunk.CacheWriteInputTokens)
						break
					}
				}
				require.Equal(t, "still streaming", text)
				if responsesInput {
					require.Zero(t, calls, "Responses calls must wait for a validated terminal")
				} else {
					require.Equal(t, 1, calls, "existing Chat callers must still receive calls immediately")
				}
				if cancelStream {
					cancel()
				} else {
					close(release)
				}
				var terminal llm.StreamChunk
				for chunk := range chunks {
					if chunk.ToolCall != nil {
						calls++
					}
					terminal = chunk
				}
				if cancelStream {
					if responsesInput {
						require.Zero(t, calls)
					}
					return
				}
				require.NoError(t, terminal.Error)
				require.True(t, terminal.Done)
				require.Equal(t, "tool_calls", terminal.StopReason)
				require.Equal(t, 1, calls)
				require.True(t, terminal.UsageReported)
				require.Equal(t, "reported-model", terminal.Model)
				require.Equal(t, "openai", terminal.Provider)
				require.Equal(t, 7, terminal.InputTokens)
				require.Equal(t, 3, terminal.OutputTokens)
			})
		}
	}
}

func TestResponsesChatTerminalTransportErrorAfterFinish(t *testing.T) {
	provider := chatTerminalProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000000")
		chatTerminalDelta(t, w, json.RawMessage(`{"tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"client_tool","arguments":"{}"}}]}`), "")
		chatTerminalDelta(t, w, map[string]any{}, "tool_calls")
		// No [DONE], and the declared HTTP body is incomplete.
	})
	chunks, err := provider.Stream(t.Context(), &llm.CompletionRequest{Model: "test-model", ResponsesInput: true})
	require.NoError(t, err)
	var terminal llm.StreamChunk
	for chunk := range chunks {
		require.Nil(t, chunk.ToolCall)
		terminal = chunk
	}
	require.True(t, terminal.Done)
	require.Error(t, terminal.Error)
}
