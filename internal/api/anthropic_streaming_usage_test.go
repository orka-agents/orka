/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
)

func TestAnthropicStreamingProxyReportsInputUsage(t *testing.T) {
	count := func(value int64) *int64 { return &value }
	tests := []struct {
		name   string
		chunks []llm.StreamChunk
		want   map[string]int64
	}{
		{
			name: "Anthropic counts arrive before content and again at completion",
			chunks: []llm.StreamChunk{
				{InputTokens: 100, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
				{Content: "hello"},
				{Done: true, StopReason: "end_turn", InputTokens: 100, OutputTokens: 20, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
			},
			want: map[string]int64{"input_tokens": 100, "output_tokens": 20, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 10},
		},
		{
			name: "terminal provider counts without cache breakdown",
			chunks: []llm.StreamChunk{
				{Content: "hello"},
				{Done: true, StopReason: "end_turn", InputTokens: 16, OutputTokens: 34, UsageReported: true},
			},
			want: map[string]int64{"input_tokens": 16, "output_tokens": 34},
		},
		{
			name: "usage snapshots are cumulative",
			chunks: []llm.StreamChunk{
				{InputTokens: 12, OutputTokens: 1, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(2), CacheWriteInputTokens: count(1)},
				{Content: "hello"},
				{Done: true, StopReason: "end_turn", InputTokens: 16, OutputTokens: 34, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(4), CacheWriteInputTokens: count(2)},
			},
			want: map[string]int64{"input_tokens": 16, "output_tokens": 34, "cache_read_input_tokens": 4, "cache_creation_input_tokens": 2},
		},
		{
			name: "explicit zero remains reported",
			chunks: []llm.StreamChunk{
				{Content: "provider reported zero consumption"},
				{Done: true, StopReason: "end_turn", UsageReported: true, CachedInputTokens: count(0), CacheWriteInputTokens: count(0)},
			},
			want: map[string]int64{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
		},
		{
			name: "missing input and cache counts stay absent",
			chunks: []llm.StreamChunk{
				{Content: "no usage was reported"},
				{Done: true, StopReason: "end_turn"},
			},
			want: map[string]int64{"output_tokens": 0},
		},
		{
			name: "early input survives a terminal output-only snapshot",
			chunks: []llm.StreamChunk{
				{InputTokens: 16, InputExcludesCache: true, CachedInputTokens: count(0)},
				{Content: "hello"},
				{Done: true, StopReason: "end_turn", OutputTokens: 34},
			},
			want: map[string]int64{"input_tokens": 16, "output_tokens": 34, "cache_read_input_tokens": 0},
		},
		{
			name: "inclusive provider input excludes the emitted cache breakdown",
			chunks: []llm.StreamChunk{
				{Content: "hello"},
				{Done: true, StopReason: "end_turn", InputTokens: 100, OutputTokens: 20, UsageReported: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
			},
			want: map[string]int64{"input_tokens": 50, "output_tokens": 20, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 10},
		},
		{
			name: "reported zero replaces an earlier snapshot",
			chunks: []llm.StreamChunk{
				{InputTokens: 12, OutputTokens: 3, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(5), CacheWriteInputTokens: count(2)},
				{Content: "hello"},
				{Done: true, StopReason: "end_turn", UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(0), CacheWriteInputTokens: count(0)},
			},
			want: map[string]int64{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := runAnthropicUsageStream(t, &mockAnthropicProvider{streamChunks: tt.chunks}, false, 1)
			assertAnthropicStreamUsage(t, body, tt.want)
		})
	}
}

func TestAnthropicStreamingFallbackReportsInputUsage(t *testing.T) {
	zero := int64(0)
	tests := []struct {
		name     string
		response llm.CompletionResponse
		want     map[string]int64
	}{
		{
			name:     "reported counts",
			response: llm.CompletionResponse{InputTokens: 16, OutputTokens: 34, UsageReported: true},
			want:     map[string]int64{"input_tokens": 16, "output_tokens": 34},
		},
		{
			name:     "zero counts",
			response: llm.CompletionResponse{UsageReported: true, CachedInputTokens: &zero, CacheWriteInputTokens: &zero},
			want:     map[string]int64{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
		},
		{
			name: "missing counts",
			want: map[string]int64{"output_tokens": 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.response.Content = "fallback reply"
			tt.response.StopReason = "end_turn"
			provider := &mockAnthropicProvider{responses: []*llm.CompletionResponse{&tt.response}}
			body := runAnthropicUsageStream(t, provider, false, 1)
			assertAnthropicStreamUsage(t, body, tt.want)
			require.Equal(t, 1, provider.callIdx)
		})
	}
}

func TestAnthropicStreamingToolLoopReportsCumulativeInputUsage(t *testing.T) {
	count := func(value int64) *int64 { return &value }
	tests := []struct {
		name          string
		responses     []llm.CompletionResponse
		maxIterations int
		want          map[string]int64
	}{
		{
			name: "two provider turns",
			responses: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(4), CacheWriteInputTokens: count(1)},
				{InputTokens: 20, OutputTokens: 5, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(6), CacheWriteInputTokens: count(2)},
			},
			want: map[string]int64{"input_tokens": 32, "output_tokens": 8, "cache_read_input_tokens": 10, "cache_creation_input_tokens": 3},
		},
		{
			name: "zero counts are not estimated from generated text",
			responses: []llm.CompletionResponse{
				{UsageReported: true, CachedInputTokens: count(0), CacheWriteInputTokens: count(0)},
				{UsageReported: true, CachedInputTokens: count(0), CacheWriteInputTokens: count(0)},
			},
			want: map[string]int64{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
		},
		{
			name: "a missing turn makes the input aggregate unavailable",
			responses: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(4), CacheWriteInputTokens: count(1)},
				{OutputTokens: 5},
			},
			want: map[string]int64{"output_tokens": 8},
		},
		{
			name: "later usage cannot fill an earlier missing input count",
			responses: []llm.CompletionResponse{
				{OutputTokens: 3},
				{InputTokens: 20, OutputTokens: 5, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(6), CacheWriteInputTokens: count(2)},
			},
			want: map[string]int64{"output_tokens": 8},
		},
		{
			name: "a missing cache field does not invent a complete subtotal",
			responses: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(4), CacheWriteInputTokens: count(1)},
				{InputTokens: 20, OutputTokens: 5, UsageReported: true, InputExcludesCache: true, CacheWriteInputTokens: count(2)},
			},
			want: map[string]int64{"input_tokens": 32, "output_tokens": 8, "cache_creation_input_tokens": 3},
		},
		{
			name: "inclusive input with complete cache coverage",
			responses: []llm.CompletionResponse{
				{InputTokens: 100, OutputTokens: 3, UsageReported: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
				{InputTokens: 80, OutputTokens: 5, UsageReported: true, CachedInputTokens: count(20), CacheWriteInputTokens: count(10)},
			},
			want: map[string]int64{"input_tokens": 100, "output_tokens": 8, "cache_read_input_tokens": 60, "cache_creation_input_tokens": 20},
		},
		{
			name: "inclusive input loses read coverage on the second turn",
			responses: []llm.CompletionResponse{
				{InputTokens: 100, OutputTokens: 3, UsageReported: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
				{InputTokens: 80, OutputTokens: 5, UsageReported: true, CacheWriteInputTokens: count(20)},
			},
			want: map[string]int64{"output_tokens": 8, "cache_creation_input_tokens": 30},
		},
		{
			name: "inclusive input has missing read coverage on the first turn",
			responses: []llm.CompletionResponse{
				{InputTokens: 80, OutputTokens: 5, UsageReported: true, CacheWriteInputTokens: count(20)},
				{InputTokens: 100, OutputTokens: 3, UsageReported: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
			},
			want: map[string]int64{"output_tokens": 8, "cache_creation_input_tokens": 30},
		},
		{
			name: "inclusive input loses write coverage on the second turn",
			responses: []llm.CompletionResponse{
				{InputTokens: 100, OutputTokens: 3, UsageReported: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
				{InputTokens: 80, OutputTokens: 5, UsageReported: true, CachedInputTokens: count(20)},
			},
			want: map[string]int64{"output_tokens": 8, "cache_read_input_tokens": 60},
		},
		{
			name: "inclusive input has missing write coverage on the first turn",
			responses: []llm.CompletionResponse{
				{InputTokens: 80, OutputTokens: 5, UsageReported: true, CachedInputTokens: count(20)},
				{InputTokens: 100, OutputTokens: 3, UsageReported: true, CachedInputTokens: count(40), CacheWriteInputTokens: count(10)},
			},
			want: map[string]int64{"output_tokens": 8, "cache_read_input_tokens": 60},
		},
		{
			name: "missing zero-cache breakdown does not discard known input",
			responses: []llm.CompletionResponse{
				{InputTokens: 100, OutputTokens: 3, UsageReported: true, CachedInputTokens: count(0), CacheWriteInputTokens: count(0)},
				{InputTokens: 80, OutputTokens: 5, UsageReported: true},
			},
			want: map[string]int64{"input_tokens": 180, "output_tokens": 8},
		},
		{
			name: "iteration limit retains completed-turn input",
			responses: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, UsageReported: true, InputExcludesCache: true, CachedInputTokens: count(4), CacheWriteInputTokens: count(1)},
			},
			maxIterations: 1,
			want:          map[string]int64{"input_tokens": 12, "output_tokens": 3, "cache_read_input_tokens": 4, "cache_creation_input_tokens": 1},
		},
	}
	for _, tt := range tests {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fallback=%t", tt.name, fallback), func(t *testing.T) {
				provider := &anthropicUsageTurnsProvider{fallback: fallback}
				for i, response := range tt.responses {
					if i == 0 {
						response.StopReason = "tool_use"
						// This tool is not exposed, so execution only returns an error
						// result to the next model turn without invoking a real tool.
						response.ToolCalls = []llm.ToolCall{{ID: "usage-tool", Name: "unavailable", Arguments: json.RawMessage(`{}`)}}
					} else {
						response.Content = goalStateSentinel + "\nDone."
						response.StopReason = "end_turn"
					}
					provider.responses = append(provider.responses, response)
				}
				maxIterations := tt.maxIterations
				if maxIterations == 0 {
					maxIterations = 2
				}
				body := runAnthropicUsageStream(t, provider, true, maxIterations)
				assertAnthropicStreamUsage(t, body, tt.want)
				require.Equal(t, len(tt.responses), provider.index)
			})
		}
	}
}

func TestAnthropicStreamingToolLoopRejectsOutOfRangeTotals(t *testing.T) {
	for _, field := range []string{"input", "output", "cache read", "cache write"} {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fallback=%t", field, fallback), func(t *testing.T) {
				provider := &anthropicUsageTurnsProvider{fallback: fallback}
				for i, count := range []int64{store.MaxUsageTokenCount, 1} {
					response := llm.CompletionResponse{UsageReported: true, InputExcludesCache: true}
					switch field {
					case "input":
						response.InputTokens = int(count)
					case "output":
						response.OutputTokens = int(count)
					case "cache read":
						response.CachedInputTokens = &count
					case "cache write":
						response.CacheWriteInputTokens = &count
					}
					if i == 0 {
						response.StopReason = "tool_use"
						response.ToolCalls = []llm.ToolCall{{ID: "usage-tool", Name: "unavailable", Arguments: json.RawMessage(`{}`)}}
					} else {
						response.Content = goalStateSentinel + "\nDone."
						response.StopReason = "end_turn"
					}
					provider.responses = append(provider.responses, response)
				}
				body := runAnthropicUsageStream(t, provider, true, 2)
				require.Contains(t, string(body), "event: error")
				require.Contains(t, string(body), "usage_out_of_range")
				require.NotContains(t, string(body), "event: message_delta")
				require.NotContains(t, string(body), "event: message_stop")
				require.Equal(t, 2, provider.index)
			})
		}
	}
}

type anthropicUsageTurnsProvider struct {
	responses []llm.CompletionResponse
	index     int
	fallback  bool
}

func (p *anthropicUsageTurnsProvider) Name() string { return "anthropic-usage-turns" }

func (p *anthropicUsageTurnsProvider) Complete(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if !p.fallback || p.index >= len(p.responses) {
		return nil, fmt.Errorf("unexpected completion")
	}
	response := p.responses[p.index]
	p.index++
	return &response, nil
}

func (p *anthropicUsageTurnsProvider) Stream(context.Context, *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	if p.fallback || p.index >= len(p.responses) {
		return nil, fmt.Errorf("stream unavailable")
	}
	response := p.responses[p.index]
	p.index++
	chunks := make(chan llm.StreamChunk, 4)
	chunks <- llm.StreamChunk{InputTokens: response.InputTokens, UsageReported: response.UsageReported,
		InputExcludesCache: response.InputExcludesCache, CachedInputTokens: response.CachedInputTokens,
		CacheWriteInputTokens: response.CacheWriteInputTokens}
	if response.Content != "" {
		chunks <- llm.StreamChunk{Content: response.Content}
	}
	for i := range response.ToolCalls {
		chunks <- llm.StreamChunk{ToolCall: &response.ToolCalls[i]}
	}
	chunks <- llm.StreamChunk{Done: true, StopReason: response.StopReason, InputTokens: response.InputTokens,
		OutputTokens: response.OutputTokens, UsageReported: response.UsageReported,
		InputExcludesCache: response.InputExcludesCache, CachedInputTokens: response.CachedInputTokens,
		CacheWriteInputTokens: response.CacheWriteInputTokens}
	close(chunks)
	return chunks, nil
}

func runAnthropicUsageStream(t *testing.T, provider llm.Provider, toolLoop bool, maxIterations int) []byte {
	t.Helper()
	handler, app := setupTestAnthropicHandler()
	handler.config.MaxIterations = maxIterations
	handler.config.MaxPrematureEndRetries = 0
	app.Post("/test", func(c fiber.Ctx) error {
		req := &llm.CompletionRequest{Model: "usage-model"}
		if toolLoop {
			return handler.handleStreamingMessages(c, context.Background(), provider, req, req.Model, nil)
		}
		return handler.handleStreamingProxy(c, context.Background(), provider, req, req.Model)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/test", nil))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return body
}

func assertAnthropicStreamUsage(t *testing.T, body []byte, want map[string]int64) {
	t.Helper()
	require.NotContains(t, string(body), "event: error")
	var message anthropic.Message
	var finalUsage map[string]json.RawMessage
	stopped := false
	for line := range strings.SplitSeq(string(body), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event anthropic.MessageStreamEventUnion
		require.NoError(t, json.Unmarshal([]byte(data), &event))
		require.NoError(t, message.Accumulate(event))
		if event.Type == "message_delta" {
			var envelope struct {
				Usage map[string]json.RawMessage `json:"usage"`
			}
			require.NoError(t, json.Unmarshal([]byte(data), &envelope))
			finalUsage = envelope.Usage
		}
		stopped = stopped || event.Type == "message_stop"
	}
	require.True(t, stopped, "missing message_stop: %s", body)
	require.Len(t, finalUsage, len(want), "final usage: %s", body)
	for field, expected := range want {
		raw, found := finalUsage[field]
		require.True(t, found, "missing %s in final usage: %s", field, body)
		require.NotEqual(t, "null", string(raw), "reported %s became unavailable", field)
		var actual int64
		require.NoError(t, json.Unmarshal(raw, &actual))
		require.Equal(t, expected, actual, field)
	}
	// Exercise the real client accumulator, which replaces the initial zero
	// input count with the cumulative provider count from message_delta.
	require.Equal(t, want["input_tokens"], message.Usage.InputTokens)
	require.Equal(t, want["output_tokens"], message.Usage.OutputTokens)
	require.Equal(t, want["cache_read_input_tokens"], message.Usage.CacheReadInputTokens)
	require.Equal(t, want["cache_creation_input_tokens"], message.Usage.CacheCreationInputTokens)
}
