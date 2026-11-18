/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
)

func TestResponsesRecordsUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, coordinator := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/coordinator=%t", stream, coordinator), func(t *testing.T) {
				backend := newInternalExecutionEventStore(t)
				reader := testInternalExecutionEventClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "original-namespace"}})
				var calls atomic.Int32
				handler, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						// Namespace ownership must be captured before even the API-mode probe.
						require.NoError(t, reader.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace}}))
						require.NoError(t, reader.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "replacement-namespace"}}))
					}
					var request struct {
						Stream bool `json:"stream"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					const text = "Done. <!-- GOAL_STATE:SATISFIED -->"
					if request.Stream {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": responsesFixtureResponse(text)}})
					} else {
						upstreamResponse(w, text)
					}
				}, false)
				handler.resultStore = backend
				handler.apiReader = reader
				// A stale informer identity must not override the uncached reader.
				require.NoError(t, handler.client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "stale-namespace"}}))
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
				status, response := requestResponses(t, app, body, !coordinator)
				require.Equal(t, http.StatusOK, status, string(response))
				if stream {
					events := parseResponsesSSE(t, response)
					require.Equal(t, "response.completed", events[len(events)-1]["type"])
				}

				data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{defaultNamespace}})
				require.NoError(t, err)
				require.NotEmpty(t, data.Observations, "Responses must persist usage independently of tracing")
				started, completed := map[string]bool{}, map[string]bool{}
				for _, observation := range data.Observations {
					require.Equal(t, defaultNamespace, observation.Namespace)
					require.Equal(t, "original-namespace", observation.NamespaceUID)
					require.Equal(t, store.UsageScopeCall, observation.Scope)
					require.Equal(t, store.UsageSourceProvider, observation.Source)
					require.Empty(t, observation.TaskUID)
					require.Empty(t, observation.SessionName)
					if observation.Status == store.UsageStatusStarted {
						started[observation.CounterID] = true
					}
					if observation.Complete {
						completed[observation.CounterID] = true
						require.Equal(t, store.UsageStatusCompleted, observation.Status)
						require.NotNil(t, observation.InputTokens)
						require.NotNil(t, observation.OutputTokens)
						require.EqualValues(t, 7, *observation.InputTokens)
						require.EqualValues(t, 3, *observation.OutputTokens)
					}
				}
				require.Len(t, started, int(calls.Load()), "each billed HTTP call, including probes, needs its own counter")
				require.Equal(t, started, completed)
				current, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{defaultNamespace}, NamespaceUIDs: map[string]string{defaultNamespace: "replacement-namespace"}})
				require.NoError(t, err)
				require.Empty(t, current.Observations)
			})
		}
	}
}

func TestResponsesUsageIdentityFailurePreventsProviderCall(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var calls atomic.Int32
			handler, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				upstreamResponse(w, "must not be called")
			}, false)
			handler.resultStore = newInternalExecutionEventStore(t)
			handler.apiReader = testInternalExecutionEventClient(t)
			// The cached Namespace still exists, but the authoritative lookup fails.
			require.NoError(t, handler.client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "stale-namespace"}}))
			body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
			status, response := requestResponses(t, app, body, true)
			require.Zero(t, calls.Load(), "failed ownership lookup must prevent billable provider calls")
			if stream {
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, response)
				require.Equal(t, "response.failed", events[len(events)-1]["type"])
			} else {
				require.Equal(t, http.StatusBadGateway, status, string(response))
			}
		})
	}
}

func TestResponsesCacheUsage(t *testing.T) {
	for _, usage := range []struct {
		name          string
		input         int
		excludesCache bool
		cached, write *int64
		wantInput     int
	}{
		{name: "inclusive", input: 100, cached: new(int64(40)), write: new(int64(10)), wantInput: 100},
		{name: "exclusive", input: 100, excludesCache: true, cached: new(int64(40)), write: new(int64(10)), wantInput: 150},
		{name: "no cache details", input: 100, wantInput: 100},
		{name: "explicit zero", excludesCache: true, cached: new(int64(0)), write: new(int64(0))},
	} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"direct", "fallback", "coordinator", "coordinator stream fallback"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", usage.name, stream, mode), func(t *testing.T) {
					completion := &llm.CompletionResponse{
						Content: "Done. <!-- GOAL_STATE:SATISFIED -->", StopReason: "end_turn", InputTokens: usage.input, OutputTokens: 3,
						CachedInputTokens: usage.cached, CacheWriteInputTokens: usage.write, InputExcludesCache: usage.excludesCache, UsageReported: true,
					}
					chunks := make(chan llm.StreamChunk, 3)
					chunks <- llm.StreamChunk{Content: completion.Content}
					// A terminal chunk need not repeat previously reported usage.
					chunks <- llm.StreamChunk{
						InputTokens: completion.InputTokens, OutputTokens: completion.OutputTokens, UsageReported: true,
						CachedInputTokens: usage.cached, CacheWriteInputTokens: usage.write, InputExcludesCache: usage.excludesCache,
					}
					chunks <- llm.StreamChunk{Done: true, StopReason: completion.StopReason}
					close(chunks)
					provider := &responsesUsageProvider{oaiMockProvider: &oaiMockProvider{resp: completion, streamCh: chunks}}
					coordinator := mode == "coordinator" || mode == "coordinator stream fallback"
					if mode == "fallback" && stream {
						provider.streamErr = fmt.Errorf("streaming unsupported")
					} else if mode == "fallback" || mode == "coordinator stream fallback" {
						provider.err = fmt.Errorf("streaming is required for operations that may take longer than 10 minutes")
					}
					const providerType = "responses-cache-usage-fixture"
					llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
					handler, app := setupTestOpenAIHandler(providerCRD("fixture", defaultNamespace, providerType, "test-model")...)
					handler.config.MaxDuration = 5 * time.Second
					handler.config.MaxPrematureEndRetries = 0
					app.Post(responsesPath, handler.HandleResponses)
					status, data := requestResponses(t, app, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), !coordinator)
					require.Equal(t, http.StatusOK, status, string(data))
					var response ResponsesResponse
					if stream {
						events := parseResponsesSSE(t, data)
						terminal := events[len(events)-1]
						require.Equal(t, "response.completed", terminal["type"], string(data))
						data, _ = json.Marshal(terminal["response"])
					}
					require.NoError(t, json.Unmarshal(data, &response))
					require.NotNil(t, response.Usage)
					wantCached, wantWrite := 0, 0
					if usage.cached != nil {
						wantCached = int(*usage.cached)
					}
					if usage.write != nil {
						wantWrite = int(*usage.write)
					}
					require.Equal(t, &responsesUsage{
						InputTokens: usage.wantInput, OutputTokens: 3, TotalTokens: usage.wantInput + 3,
						InputTokensDetails:  map[string]int{"cached_tokens": wantCached, "cache_write_tokens": wantWrite},
						OutputTokensDetails: map[string]int{"reasoning_tokens": 0},
					}, response.Usage)
					wantStreams, wantCompletes := int32(0), 1
					if mode == "fallback" || mode == "coordinator stream fallback" || stream && !coordinator {
						wantStreams = 1
					}
					if mode == "direct" && stream {
						wantCompletes = 0
					}
					require.Equal(t, wantStreams, provider.streamCalls.Load())
					require.Len(t, provider.requests, wantCompletes)
					require.Equal(t, usage.input, completion.InputTokens, "serialization must not normalize the provider response in place")
				})
			}
		}
	}
}

type responsesUsageProvider struct {
	*oaiMockProvider
	streamCalls atomic.Int32
}

func (p *responsesUsageProvider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	p.streamCalls.Add(1)
	return p.oaiMockProvider.Stream(ctx, req)
}

func TestResponsesSynthesizedTerminalPreservesUsage(t *testing.T) {
	for _, coordinator := range []bool{false, true} {
		for _, count := range []int64{0, 12} {
			t.Run(fmt.Sprintf("coordinator=%t/count=%d", coordinator, count), func(t *testing.T) {
				completion := &llm.CompletionResponse{
					Content: "Done. <!-- GOAL_STATE:SATISFIED -->", StopReason: "end_turn", InputTokens: int(count), OutputTokens: int(count),
					CachedInputTokens: &count, CacheWriteInputTokens: &count, InputExcludesCache: true, UsageReported: true,
				}
				provider := &oaiMockProvider{resp: completion, streamErr: fmt.Errorf("streaming unsupported")}
				handler := &OpenAICompatHandler{config: ChatConfig{MaxIterations: 1}}
				chunks := make(chan llm.StreamChunk, 8)
				handler.produceResponsesChunks(t.Context(), provider, &llm.CompletionRequest{Model: "test-model", ResponsesInput: true}, coordinator, nil, chunks)
				var terminal llm.StreamChunk
				for chunk := range chunks {
					require.NoError(t, chunk.Error)
					terminal = chunk
				}
				require.True(t, terminal.Done)
				require.True(t, terminal.UsageReported, "explicit zero usage must remain reported")
				require.True(t, terminal.InputExcludesCache)
				require.Equal(t, completion.InputTokens, terminal.InputTokens)
				require.Equal(t, completion.OutputTokens, terminal.OutputTokens)
				require.Equal(t, completion.CachedInputTokens, terminal.CachedInputTokens)
				require.Equal(t, completion.CacheWriteInputTokens, terminal.CacheWriteInputTokens)
			})
		}
	}
}

func TestResponsesStreamUsageSnapshots(t *testing.T) {
	for _, test := range []struct {
		name      string
		terminal  llm.StreamChunk
		wantUsage responsesUsage
	}{
		{
			name: "terminal replaces earlier usage",
			terminal: llm.StreamChunk{InputTokens: 20, OutputTokens: 5, UsageReported: true, InputExcludesCache: true,
				CachedInputTokens: new(int64(6)), CacheWriteInputTokens: new(int64(2))},
			wantUsage: responsesUsage{InputTokens: 28, OutputTokens: 5, TotalTokens: 33,
				InputTokensDetails: map[string]int{"cached_tokens": 6, "cache_write_tokens": 2}, OutputTokensDetails: map[string]int{"reasoning_tokens": 0}},
		},
		{
			name: "terminal reports explicit zero",
			terminal: llm.StreamChunk{UsageReported: true, InputExcludesCache: true,
				CachedInputTokens: new(int64(0)), CacheWriteInputTokens: new(int64(0))},
			wantUsage: responsesUsage{InputTokensDetails: map[string]int{"cached_tokens": 0, "cache_write_tokens": 0}, OutputTokensDetails: map[string]int{"reasoning_tokens": 0}},
		},
		{
			name:     "output-only terminal preserves exclusive input",
			terminal: llm.StreamChunk{OutputTokens: 5},
			wantUsage: responsesUsage{InputTokens: 17, OutputTokens: 5, TotalTokens: 22,
				InputTokensDetails: map[string]int{"cached_tokens": 4, "cache_write_tokens": 1}, OutputTokensDetails: map[string]int{"reasoning_tokens": 0}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			chunks := make(chan llm.StreamChunk, 2)
			chunks <- llm.StreamChunk{Content: "Done.", InputTokens: 12, OutputTokens: 3, UsageReported: true, InputExcludesCache: true,
				CachedInputTokens: new(int64(4)), CacheWriteInputTokens: new(int64(1))}
			test.terminal.Done, test.terminal.StopReason = true, "end_turn"
			chunks <- test.terminal
			close(chunks)
			provider := &oaiMockProvider{streamCh: chunks}
			const providerType = "responses-usage-snapshots-fixture"
			llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
			handler, app := setupTestOpenAIHandler(providerCRD("fixture", defaultNamespace, providerType, "test-model")...)
			app.Post(responsesPath, handler.HandleResponses)
			status, data := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			require.Equal(t, http.StatusOK, status, string(data))
			events := parseResponsesSSE(t, data)
			terminal := events[len(events)-1]
			require.Equal(t, "response.completed", terminal["type"], string(data))
			data, err := json.Marshal(terminal["response"])
			require.NoError(t, err)
			var response ResponsesResponse
			require.NoError(t, json.Unmarshal(data, &response))
			require.Equal(t, &test.wantUsage, response.Usage)
		})
	}
}
