/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResponsesCoordinatorUsageCounts(t *testing.T) {
	for _, test := range []struct {
		name   string
		rounds []llm.CompletionResponse
		want   llm.CompletionResponse
	}{
		{
			name: "exclusive cache counts",
			rounds: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, InputExcludesCache: true, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
				{InputTokens: 20, OutputTokens: 5, InputExcludesCache: true, CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2)), UsageReported: true},
			},
			want: llm.CompletionResponse{InputTokens: 45, OutputTokens: 8, CachedInputTokens: new(int64(10)), CacheWriteInputTokens: new(int64(3)), UsageReported: true},
		},
		{
			name: "inclusive cache counts",
			rounds: []llm.CompletionResponse{
				{InputTokens: 17, OutputTokens: 3, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
				{InputTokens: 28, OutputTokens: 5, CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2)), UsageReported: true},
			},
			want: llm.CompletionResponse{InputTokens: 45, OutputTokens: 8, CachedInputTokens: new(int64(10)), CacheWriteInputTokens: new(int64(3)), UsageReported: true},
		},
		{
			name: "exclusive then inclusive",
			rounds: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, InputExcludesCache: true, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
				{InputTokens: 28, OutputTokens: 5, CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2)), UsageReported: true},
			},
			want: llm.CompletionResponse{InputTokens: 45, OutputTokens: 8, CachedInputTokens: new(int64(10)), CacheWriteInputTokens: new(int64(3)), UsageReported: true},
		},
		{
			name: "inclusive then exclusive",
			rounds: []llm.CompletionResponse{
				{InputTokens: 17, OutputTokens: 3, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
				{InputTokens: 20, OutputTokens: 5, InputExcludesCache: true, CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2)), UsageReported: true},
			},
			want: llm.CompletionResponse{InputTokens: 45, OutputTokens: 8, CachedInputTokens: new(int64(10)), CacheWriteInputTokens: new(int64(3)), UsageReported: true},
		},
		{
			name: "explicit zero is reported without estimates",
			rounds: []llm.CompletionResponse{
				{UsageReported: true, CachedInputTokens: new(int64), CacheWriteInputTokens: new(int64)},
				{UsageReported: true, CachedInputTokens: new(int64), CacheWriteInputTokens: new(int64)},
			},
			want: llm.CompletionResponse{UsageReported: true, CachedInputTokens: new(int64), CacheWriteInputTokens: new(int64)},
		},
		{
			name: "final zero does not erase earlier usage",
			rounds: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, InputExcludesCache: true, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
				{UsageReported: true, CachedInputTokens: new(int64), CacheWriteInputTokens: new(int64)},
			},
			want: llm.CompletionResponse{InputTokens: 17, OutputTokens: 3, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
		},
		{
			name: "missing breakdown retains known counts",
			rounds: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3, InputExcludesCache: true, CachedInputTokens: new(int64(4)), UsageReported: true},
				{InputTokens: 20, OutputTokens: 5, CacheWriteInputTokens: new(int64(2)), UsageReported: true},
			},
			want: llm.CompletionResponse{InputTokens: 36, OutputTokens: 8, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(2)), UsageReported: true},
		},
		{
			name: "missing final usage retains reported counts",
			rounds: []llm.CompletionResponse{
				{InputTokens: 17, OutputTokens: 3, CachedInputTokens: new(int64(4)), UsageReported: true},
				{},
			},
			want: llm.CompletionResponse{InputTokens: 17, OutputTokens: 3, CachedInputTokens: new(int64(4)), UsageReported: true},
		},
		{
			name: "cache-only usage",
			rounds: []llm.CompletionResponse{
				{InputExcludesCache: true, CachedInputTokens: new(int64(4))},
				{InputExcludesCache: true, CacheWriteInputTokens: new(int64(2))},
			},
			want: llm.CompletionResponse{InputTokens: 6, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(2))},
		},
		{
			name: "legacy positive counts without reported flag",
			rounds: []llm.CompletionResponse{
				{InputTokens: 12, OutputTokens: 3},
				{InputTokens: 20, OutputTokens: 5},
			},
			want: llm.CompletionResponse{InputTokens: 32, OutputTokens: 8},
		},
		{name: "absent usage stays absent", rounds: []llm.CompletionResponse{{}, {}}},
	} {
		for _, responsesInput := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/responses=%t", test.name, responsesInput), func(t *testing.T) {
				// The first successful completion is a billable coordinator
				// re-prompt, unlike a rejected provider attempt or API probe.
				first, final := test.rounds[0], test.rounds[1]
				first.Content, first.StopReason = "Still working.", "end_turn"
				final.Content, final.StopReason = goalStateSentinel+"\nDone.", "end_turn"
				final.ID, final.Model, final.Provider = "final-id", "final-model", "final-provider"
				final.OutputItems = []llm.AssistantOutputItem{{Content: goalStateSentinel + "\n"}, {Content: "Done.", Status: "completed"}}
				before := []llm.CompletionResponse{first, final}
				for i := range before {
					if before[i].CachedInputTokens != nil {
						before[i].CachedInputTokens = new(*before[i].CachedInputTokens)
					}
					if before[i].CacheWriteInputTokens != nil {
						before[i].CacheWriteInputTokens = new(*before[i].CacheWriteInputTokens)
					}
				}
				provider := &mockAnthropicProvider{responses: []*llm.CompletionResponse{&first, &final}}
				var progress []string
				result, err := runToolLoopWithObserver(t.Context(), provider,
					&llm.CompletionRequest{ResponsesInput: responsesInput}, "test-model",
					ChatConfig{MaxIterations: 3, MaxPrematureEndRetries: 1}, nil,
					&toolLoopObserver{OnPrematureEndRetry: func() { progress = append(progress, "retry") }, OnFinalContent: func(content string) { progress = append(progress, content) }},
					toolLoopOptions{requireFinalCompletion: true, allowEmptyTokenBudget: true})
				require.NoError(t, err)
				require.Equal(t, 2, provider.callIdx)
				require.Equal(t, []string{"retry", final.Content}, progress)
				want := final
				if responsesInput {
					want.InputTokens, want.OutputTokens = test.want.InputTokens, test.want.OutputTokens
					want.CachedInputTokens, want.CacheWriteInputTokens = test.want.CachedInputTokens, test.want.CacheWriteInputTokens
					want.InputExcludesCache, want.UsageReported = false, test.want.UsageReported
				} else {
					require.Same(t, &final, result, "other compatibility APIs keep the provider response")
				}
				require.Equal(t, &want, result, "usage changes must preserve final content, identity and item order")
				require.Equal(t, before, []llm.CompletionResponse{first, final}, "provider-owned usage must not change")
			})
		}
	}
}

func TestResponsesCoordinatorUsageTerminalPaths(t *testing.T) {
	for _, path := range []string{"normal", "iteration limit", "stream required", "iteration limit stream required", "context retry", "context retry stream required"} {
		for _, stop := range []string{"end_turn", "max_tokens", "length", "empty budget", "zero usage"} {
			t.Run(path+"/"+stop, func(t *testing.T) {
				first := &llm.CompletionResponse{
					Content: "Reading.", StopReason: "tool_use", ToolCalls: []llm.ToolCall{{ID: "call", Name: "unexposed", Arguments: []byte(`{}`)}},
					InputTokens: 12, OutputTokens: 3, InputExcludesCache: true, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true,
				}
				final := &llm.CompletionResponse{
					Content: "Done.", StopReason: stop, ID: "final-id", Model: "final-model", Provider: "final-provider",
					InputTokens: 28, OutputTokens: 5, CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2)), UsageReported: true,
					OutputItems: []llm.AssistantOutputItem{{Content: "Done.", Status: "completed"}},
				}
				if stop == "empty budget" {
					final.Content, final.StopReason, final.OutputItems = "", "max_tokens", []llm.AssistantOutputItem{}
				}
				wantInput, wantOutput, wantCached, wantWrite := 45, 8, int64(10), int64(3)
				if stop == "zero usage" {
					final.StopReason = "end_turn"
					final.InputTokens, final.OutputTokens = 0, 0
					final.CachedInputTokens, final.CacheWriteInputTokens = new(int64), new(int64)
					wantInput, wantOutput, wantCached, wantWrite = 17, 3, 4, 1
				}
				provider := &mockAnthropicProvider{responses: []*llm.CompletionResponse{first, final}, errors: []error{nil, nil}}
				config := ChatConfig{MaxIterations: 3}
				if strings.HasPrefix(path, "iteration limit") {
					config.MaxIterations = 1
				}
				if strings.HasPrefix(path, "context retry") {
					provider.responses = append(provider.responses[:1], nil, final)
					provider.errors = []error{nil, &llm.ProviderError{StatusCode: 400, Message: "context length exceeded"}, nil}
				}
				if strings.HasSuffix(path, "stream required") {
					provider.errors[len(provider.errors)-1] = fmt.Errorf("streaming is required")
					provider.streamChunks = []llm.StreamChunk{
						{Content: final.Content, Model: final.Model, Provider: final.Provider},
						// These are snapshots of one call, not additive chunks.
						{InputTokens: 21, OutputTokens: 1, CachedInputTokens: new(int64(2)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
						{InputTokens: final.InputTokens, OutputTokens: final.OutputTokens, CachedInputTokens: final.CachedInputTokens, CacheWriteInputTokens: final.CacheWriteInputTokens, UsageReported: true},
						{Done: true, StopReason: final.StopReason},
					}
				}
				result, err := runNonStreamingToolLoop(t.Context(), provider,
					&llm.CompletionRequest{ResponsesInput: true, Tools: []llm.Tool{{Name: "another_tool"}}}, "test-model", config, nil,
					toolLoopOptions{requireFinalCompletion: true, allowEmptyTokenBudget: true})
				require.NoError(t, err)
				require.Equal(t, len(provider.responses), provider.callIdx, "partials must not trigger another completion")
				require.Equal(t, final.Content, result.Content)
				require.Equal(t, final.StopReason, result.StopReason)
				require.Equal(t, final.Model, result.Model)
				require.Equal(t, final.Provider, result.Provider)
				if !strings.HasSuffix(path, "stream required") {
					require.Equal(t, final.ID, result.ID)
					require.Equal(t, final.OutputItems, result.OutputItems)
				}
				if config.MaxIterations == 1 {
					require.Empty(t, provider.requests[len(provider.requests)-1].Tools)
				}
				require.Equal(t, wantInput, result.InputTokens)
				require.Equal(t, wantOutput, result.OutputTokens)
				require.Equal(t, wantCached, *result.CachedInputTokens)
				require.Equal(t, wantWrite, *result.CacheWriteInputTokens)
				require.False(t, result.InputExcludesCache)
				require.True(t, result.UsageReported)
				var response ResponsesResponse
				require.NoError(t, response.setCompletion(result))
				require.Equal(t, wantInput+wantOutput, response.Usage.TotalTokens, "serialization must not add cache counts twice")
				if final.StopReason != "end_turn" {
					require.Equal(t, "incomplete", response.Status)
					require.Equal(t, "max_output_tokens", response.IncompleteDetails.Reason)
				}
			})
		}
	}
}

func TestResponsesCoordinatorUsageErrors(t *testing.T) {
	for _, limit := range []int{1, 3} {
		for _, test := range []struct {
			name     string
			response *llm.CompletionResponse
			err      error
			wantErr  string
		}{
			{name: "provider error", response: &llm.CompletionResponse{InputTokens: 999, UsageReported: true}, err: fmt.Errorf("provider failed"), wantErr: "provider failed"},
			{name: "refusal", response: &llm.CompletionResponse{StopReason: "refusal"}, wantErr: "refused completion"},
			{name: "failed outcome", response: &llm.CompletionResponse{StopReason: "failed"}, wantErr: "failed completion"},
			{name: "truncated tool", response: &llm.CompletionResponse{StopReason: "max_tokens", ToolCalls: []llm.ToolCall{{ID: "call", Name: "unexposed", Arguments: []byte(`{}`)}}}, wantErr: "truncated tool calls"},
		} {
			t.Run(fmt.Sprintf("limit=%d/%s", limit, test.name), func(t *testing.T) {
				provider := &mockAnthropicProvider{
					responses: []*llm.CompletionResponse{
						{Content: "Working.", StopReason: "end_turn", InputTokens: 12, OutputTokens: 3, UsageReported: true},
						test.response,
					},
					errors: []error{nil, test.err},
				}
				result, err := runNonStreamingToolLoop(t.Context(), provider, &llm.CompletionRequest{ResponsesInput: true}, "test-model",
					ChatConfig{MaxIterations: limit, MaxPrematureEndRetries: 1}, nil, toolLoopOptions{requireFinalCompletion: true, allowEmptyTokenBudget: true})
				require.Nil(t, result, "earlier successful usage must not turn an error into a response")
				require.ErrorContains(t, err, test.wantErr)
				require.Equal(t, 2, provider.callIdx)
			})
		}
	}
}

func TestResponsesCoordinatorUsageHTTPAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, limit := range []int{1, 3} {
			for _, mode := range []string{"complete", "stream required", "context retry"} {
				t.Run(fmt.Sprintf("stream=%t/limit=%d/%s", stream, limit, mode), func(t *testing.T) {
					path := filepath.Join(responsesToolTempDir(t), "usage.txt")
					require.NoError(t, os.WriteFile(path, []byte("local tool result"), 0600))
					arguments, err := json.Marshal(map[string]string{"path": path})
					require.NoError(t, err)
					var rounds, probes, rejected atomic.Int32
					handler, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						require.Equal(t, false, request["store"])
						if request["max_output_tokens"] == float64(1) {
							probes.Add(1)
							probe := responsesFixtureResponse("probe")
							probe["usage"] = coordinatorUsageWire(101, 1, 3, 1)
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(probe))
							return
						}
						if request["stream"] != true && (mode == "stream required" || mode == "context retry" && rejected.Load() == 0) {
							rejected.Add(1)
							message := "streaming is required"
							if mode == "context retry" {
								message = "context length exceeded"
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}}))
							return
						}
						var response map[string]any
						if rounds.Add(1) == 1 {
							response = responsesFixtureResponse("Reading.", llm.ToolCall{ID: "read-call", Name: "file_read", Arguments: arguments})
							response["usage"] = coordinatorUsageWire(17, 3, 4, 1)
						} else {
							require.Contains(t, fmt.Sprint(request["input"]), "local tool result")
							if limit == 1 {
								require.Empty(t, request["tools"])
							}
							response, _ = orderedResponsesWire([]string{goalStateSentinel + "\nFirst.", "Second."}, "", "")
							response["usage"] = coordinatorUsageWire(28, 5, 6, 2)
						}
						if request["stream"] == true {
							upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
						} else {
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(response))
						}
					}, false)
					handler.config.MaxIterations = limit
					backend := newInternalExecutionEventStore(t)
					handler.resultStore = backend
					handler.apiReader = testInternalExecutionEventClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "usage-namespace"}})
					status, body := requestResponses(t, app, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read the file"}`, stream), false)
					require.Equal(t, http.StatusOK, status, string(body))
					require.NotContains(t, string(body), goalStateSentinel)
					var events []map[string]any
					if stream {
						events = parseResponsesSSE(t, body)
						terminal := events[len(events)-1]
						require.Equal(t, "response.completed", terminal["type"])
						body, err = json.Marshal(terminal["response"])
						require.NoError(t, err)
					}
					var response ResponsesResponse
					require.NoError(t, json.Unmarshal(body, &response))
					require.Equal(t, &responsesUsage{InputTokens: 45, OutputTokens: 8, TotalTokens: 53,
						InputTokensDetails: map[string]int{"cached_tokens": 10, "cache_write_tokens": 3}, OutputTokensDetails: map[string]int{"reasoning_tokens": 0}}, response.Usage)
					require.GreaterOrEqual(t, len(response.Output), 2)
					last := response.Output[len(response.Output)-2:]
					require.Equal(t, "First.", last[0].Content[0].Text)
					require.Equal(t, "Second.", last[1].Content[0].Text)
					for _, item := range response.Output {
						require.Equal(t, "message", item.Type, "internal tools must not become client function calls")
					}
					for i, event := range events {
						require.EqualValues(t, i, event["sequence_number"])
						if snapshot, ok := event["response"].(map[string]any); ok {
							require.Equal(t, response.ID, snapshot["id"])
						}
						if event["type"] == "response.output_item.done" {
							index := int(event["output_index"].(float64))
							require.Equal(t, response.Output[index].ID, event["item"].(map[string]any)["id"])
						}
					}
					wantProbes, wantRejected := 0, 0
					switch mode {
					case "stream required":
						wantProbes, wantRejected = 1, 2
					case "context retry":
						wantRejected = 1
					}
					require.EqualValues(t, wantProbes, probes.Load())
					require.EqualValues(t, wantRejected, rejected.Load())
					require.EqualValues(t, 2, rounds.Load())
					data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{defaultNamespace}})
					require.NoError(t, err)
					completed, failed := map[string]bool{}, map[string]bool{}
					for _, observation := range data.Observations {
						if observation.Status == store.UsageStatusFailed {
							failed[observation.CounterID] = true
						}
						if !observation.Complete {
							continue
						}
						completed[observation.CounterID] = true
						require.Equal(t, store.UsageScopeCall, observation.Scope)
						require.NotNil(t, observation.InputTokens)
						want := map[int64][]int64{101: {1, 3, 1}, 17: {3, 4, 1}, 28: {5, 6, 2}}[*observation.InputTokens]
						require.NotNil(t, want, "persisted usage must remain per-call, not request totals")
						require.Equal(t, new(want[0]), observation.OutputTokens)
						require.Equal(t, new(want[1]), observation.CachedInputTokens)
						require.Equal(t, new(want[2]), observation.CacheWriteInputTokens)
					}
					require.Len(t, completed, 2+wantProbes, "any probe is persisted separately, not added to request-visible usage")
					require.Len(t, failed, int(rejected.Load()), "rejected attempts stay in persisted accounting")
				})
			}
		}
	}
}

func coordinatorUsageWire(input, output, cached, write int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details":  map[string]any{"cached_tokens": cached, "cache_write_tokens": write},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0}}
}

func TestResponsesCoordinatorUsagePersistenceError(t *testing.T) {
	for _, limit := range []int{1, 3} {
		for _, message := range []string{"streaming is required", "context length exceeded"} {
			t.Run(fmt.Sprintf("limit=%d/%s", limit, message), func(t *testing.T) {
				mock := &mockAnthropicProvider{responses: []*llm.CompletionResponse{
					{Content: "Working.", StopReason: "end_turn", InputTokens: 17, OutputTokens: 3, UsageReported: true},
					{Content: "Done.", StopReason: "end_turn", InputTokens: 28, OutputTokens: 5, UsageReported: true},
				}}
				const providerType = "responses-coordinator-failed-usage-fixture"
				llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return mock, nil })
				provider, err := llm.NewProvider(providerType, llm.ProviderConfig{})
				require.NoError(t, err)
				writeErr := errors.New(message)
				ctx := llm.WithUsageRecorder(t.Context(), func(_ context.Context, observation store.UsageObservation) error {
					if observation.Complete && observation.InputTokens != nil && *observation.InputTokens == 28 {
						return writeErr
					}
					return nil
				})
				result, err := runNonStreamingToolLoop(ctx, provider, &llm.CompletionRequest{ResponsesInput: true}, "test-model",
					ChatConfig{MaxIterations: limit, MaxPrematureEndRetries: 1}, nil, toolLoopOptions{requireFinalCompletion: true, allowEmptyTokenBudget: true})
				require.Nil(t, result, "a response plus accounting error must not produce a successful usage aggregate")
				require.ErrorIs(t, err, writeErr)
				require.True(t, llm.IsUsagePersistenceError(err))
				require.Equal(t, 2, mock.callIdx, "accounting errors must not retry billable completions")
			})
		}
	}
}

func TestResponsesCoordinatorUsageRepeatedRounds(t *testing.T) {
	for _, limit := range []int{2, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			provider := &mockAnthropicProvider{responses: []*llm.CompletionResponse{
				{Content: "Working.", StopReason: "end_turn", InputTokens: 12, OutputTokens: 3, InputExcludesCache: true, CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1)), UsageReported: true},
				{Content: "Still working.", StopReason: "end_turn", InputTokens: 20, OutputTokens: 5, InputExcludesCache: true, CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2)), UsageReported: true},
				{Content: goalStateSentinel + "\nDone.", StopReason: "end_turn", InputTokens: 7, OutputTokens: 2, InputExcludesCache: true, CachedInputTokens: new(int64(2)), CacheWriteInputTokens: new(int64), UsageReported: true},
			}}
			result, err := runNonStreamingToolLoop(t.Context(), provider, &llm.CompletionRequest{ResponsesInput: true}, "test-model",
				ChatConfig{MaxIterations: limit, MaxPrematureEndRetries: 2}, nil, toolLoopOptions{requireFinalCompletion: true, allowEmptyTokenBudget: true})
			require.NoError(t, err)
			require.Equal(t, 3, provider.callIdx)
			var response ResponsesResponse
			require.NoError(t, response.setCompletion(result))
			require.Equal(t, &responsesUsage{InputTokens: 54, OutputTokens: 10, TotalTokens: 64,
				InputTokensDetails: map[string]int{"cached_tokens": 12, "cache_write_tokens": 3}, OutputTokensDetails: map[string]int{"reasoning_tokens": 0}}, response.Usage,
				"earlier cache counts must not be normalized again when a third round is added")
		})
	}
}
