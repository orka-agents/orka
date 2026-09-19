package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	_ "github.com/orka-agents/orka/internal/llm/anthropic"
	_ "github.com/orka-agents/orka/internal/llm/openai"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/usage"
)

func usageFixture(t *testing.T) (context.Context, func() usage.Report) {
	return usageFixtureWithRecorder(t, nil)
}

func usageFixtureWithRecorder(t *testing.T, beforeRecord func(store.UsageObservation) error) (context.Context, func() usage.Report) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	backend := sqlite.NewStore(db, "")
	start := time.Now().UTC().Add(-time.Minute)
	work := store.UsageWorkID("team", "monitor", "org/repo", "issue", 1)
	require.NoError(t, backend.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "team", ID: work, MonitorName: "monitor", MonitorUID: "monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: start}))
	require.NoError(t, backend.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "team", TaskUID: "task", TaskName: "task", WorkID: work, Phase: "Running", StartedAt: start}))
	ctx := llm.WithUsageRecorder(t.Context(), func(ctx context.Context, observation store.UsageObservation) error {
		if beforeRecord != nil {
			if err := beforeRecord(observation); err != nil {
				return err
			}
		}
		observation.Namespace, observation.TaskUID = "team", "task"
		return backend.RecordUsage(ctx, observation)
	})
	return ctx, func() usage.Report {
		filter := store.UsageFilter{Namespaces: []string{"team"}, From: start, Until: start.Add(time.Hour), AsOf: time.Now().UTC()}
		data, err := backend.LoadUsage(t.Context(), filter)
		require.NoError(t, err)
		report, err := usage.Build(data, filter)
		require.NoError(t, err)
		return report
	}
}

func usageRequest() *llm.CompletionRequest {
	return &llm.CompletionRequest{Model: "requested-model", MaxTokens: 100, Messages: []llm.Message{{Role: "user", Content: "fixture"}}}
}

func TestUsageProviderSDKRetriesAndCacheSemantics(t *testing.T) {
	for _, providerName := range []string{"openai", "anthropic"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", providerName, streaming), func(t *testing.T) {
				ctx, report := usageFixture(t)
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if requests.Add(1) == 1 {
						w.Header().Set("retry-after-ms", "1")
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = fmt.Fprint(w, `{"error":{"type":"api_error","message":"fixture failure"}}`)
						return
					}
					var body struct {
						Stream bool `json:"stream"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					if providerName == "openai" {
						response := `{"id":"response","object":"response","status":"completed","model":"served-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}}`
						if body.Stream {
							_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
						} else {
							_, _ = fmt.Fprint(w, response)
						}
					} else if body.Stream {
						_, _ = fmt.Fprint(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"message","type":"message","role":"assistant","model":"served-model","usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":40,"cache_creation_input_tokens":10}}}`+"\n\n")
						_, _ = fmt.Fprint(w, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`+"\n\n")
					} else {
						_, _ = fmt.Fprint(w, `{"id":"message","type":"message","role":"assistant","model":"served-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":40,"cache_creation_input_tokens":10}}`)
					}
				}))
				t.Cleanup(server.Close)
				provider, err := llm.NewProvider(providerName, llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
				require.NoError(t, err)
				if streaming {
					stream, err := provider.Stream(ctx, usageRequest())
					require.NoError(t, err)
					for chunk := range stream {
						require.NoError(t, chunk.Error)
					}
				} else {
					_, err = provider.Complete(ctx, usageRequest())
					require.NoError(t, err)
				}
				got := report()
				wantCalls, wantReported := 2, 1
				if providerName == "openai" && streaming {
					wantCalls, wantReported = 3, 2
				}
				require.EqualValues(t, wantCalls, requests.Load())
				require.Equal(t, wantCalls, got.Summary.Calls)
				require.Equal(t, 1, got.Summary.MissingMeasurements)
				require.Equal(t, wantReported, got.Summary.ReportedMeasurements)
				require.Equal(t, "partial", got.Summary.Completeness)
				want := int64(120)
				if providerName == "anthropic" {
					want = 170
				}
				if providerName == "openai" && streaming {
					want *= 2
				}
				require.Equal(t, want, got.Summary.TotalTokens)
				require.EqualValues(t, 40*wantReported, got.Summary.CachedInputTokens)
				require.EqualValues(t, 10*wantReported, got.Summary.CacheWriteInputTokens)
				require.Contains(t, got.Works[0].Models, "served-model")
			})
		}
	}
}

func TestUsageOpenAIChatCacheSemantics(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			ctx, report := usageFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/responses" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"error":{"message":"unsupported_api"}}`)
					return
				}
				const counts = `"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":10}}`
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"model\":\"served-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],%s}\n\ndata: [DONE]\n\n", counts)
				} else {
					_, _ = fmt.Fprintf(w, `{"id":"chat","object":"chat.completion","model":"served-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],%s}`, counts)
				}
			}))
			t.Cleanup(server.Close)
			provider, err := llm.NewProvider("openai", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
			require.NoError(t, err)
			if streaming {
				stream, err := provider.Stream(ctx, usageRequest())
				require.NoError(t, err)
				for chunk := range stream {
					require.NoError(t, chunk.Error)
				}
			} else {
				_, err = provider.Complete(ctx, usageRequest())
				require.NoError(t, err)
			}
			got := report()
			require.Equal(t, 2, got.Summary.Calls)
			require.Equal(t, 1, got.Summary.ReportedMeasurements)
			require.Equal(t, 1, got.Summary.MissingMeasurements)
			require.EqualValues(t, 120, got.Summary.TotalTokens)
			require.EqualValues(t, 40, got.Summary.CachedInputTokens)
			require.EqualValues(t, 10, got.Summary.CacheWriteInputTokens)
		})
	}
}

func TestUsageOpenAIResponsesTerminalAccounting(t *testing.T) {
	count := func(value int64) *int64 { return &value }
	cases := []struct {
		name         string
		usage        string
		input        *int64
		output       *int64
		cached       *int64
		cacheWrite   *int64
		completeness string
		invalid      bool
	}{
		{name: "reported", usage: `{"input_tokens":5,"output_tokens":3,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}`, input: count(5), output: count(3), cached: count(2), cacheWrite: count(1), completeness: "complete"},
		{name: "zero", usage: `{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, input: count(0), output: count(0), cached: count(0), cacheWrite: count(0), completeness: "complete"},
		{name: "partial", usage: `{"output_tokens":3,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}`, output: count(3), cached: count(2), cacheWrite: count(1), completeness: "partial"},
		{name: "missing", completeness: "unavailable"},
		{name: "invalid cache", usage: `{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":1}}`, completeness: "unavailable", invalid: true},
	}
	for _, status := range []string{"incomplete", "failed"} {
		for _, tt := range cases {
			t.Run(status+"/"+tt.name, func(t *testing.T) {
				ctx, report := usageFixture(t)
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var body struct {
						Stream bool `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "invalid fixture request", http.StatusBadRequest)
						return
					}
					if !body.Stream {
						// Discovery must succeed so the terminal event reaches the SDK stream.
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprint(w, `{"id":"probe","object":"response","status":"completed","model":"requested-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"probe"}]}]}`)
						return
					}
					response := map[string]any{"id": "terminal", "object": "response", "status": status, "model": "served-model", "output": []any{}}
					if status == "incomplete" {
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					}
					if tt.usage != "" {
						response["usage"] = json.RawMessage(tt.usage)
					}
					encoded, err := json.Marshal(response)
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
					_, _ = fmt.Fprintf(w, "event: response.%s\ndata: {\"type\":\"response.%s\",\"response\":%s}\n\ndata: [DONE]\n\n", status, status, encoded)
				}))
				t.Cleanup(server.Close)
				provider, err := llm.NewProvider("openai", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
				require.NoError(t, err)
				provider = llm.NewRetryProvider(provider)
				stream, err := provider.Stream(ctx, usageRequest())
				require.NoError(t, err)
				var terminal llm.StreamChunk
				var terminalCount int
				var content string
				for chunk := range stream {
					content += chunk.Content
					err = errors.Join(err, chunk.Error)
					require.Nil(t, chunk.ToolCall)
					if chunk.Done {
						terminal, terminalCount = chunk, terminalCount+1
					}
				}
				require.Equal(t, "partial", content)
				require.Equal(t, 1, terminalCount)
				if tt.invalid {
					require.ErrorIs(t, err, store.ErrValidation)
					require.True(t, llm.IsUsagePersistenceError(err))
					require.False(t, llm.ShouldRetry(err))
				} else {
					require.NoError(t, err)
					wantStop := "response.failed"
					if status == "incomplete" {
						wantStop = "length"
					}
					require.Equal(t, wantStop, terminal.StopReason)
					require.Equal(t, "served-model", terminal.Model)
					require.Equal(t, "openai", terminal.Provider)
					require.Equal(t, tt.completeness == "complete", terminal.UsageReported)
				}
				// One discovery call and one stream, including invalid accounting.
				require.EqualValues(t, 2, requests.Load())
				got := report()
				require.Equal(t, 2, got.Summary.Calls)
				require.Len(t, got.Works, 1)
				require.Len(t, got.Works[0].Tasks, 1)
				measurements := got.Works[0].Tasks[0].Measurements
				require.Len(t, measurements, 2)
				measurement := measurements[1]
				require.Equal(t, tt.input, measurement.InputTokens)
				require.Equal(t, tt.output, measurement.OutputTokens)
				require.Equal(t, tt.cached, measurement.CachedInputTokens)
				require.Equal(t, tt.cacheWrite, measurement.CacheWriteInputTokens)
				require.Equal(t, tt.completeness, measurement.Completeness)
				if !tt.invalid {
					require.Equal(t, "served-model", measurement.Model)
					require.Equal(t, "openai", measurement.Provider)
				}
			})
		}
	}
}

func TestUsageOpenAIRejectsCacheBreakdownsExceedingInput(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat=%v/stream=%v", chat, streaming), func(t *testing.T) {
				ctx, report := usageFixture(t)
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if chat && r.URL.Path == "/responses" {
						w.WriteHeader(http.StatusNotFound)
						_, _ = fmt.Fprint(w, `{"error":{"message":"unsupported_api"}}`)
						return
					}
					var body struct {
						Stream bool `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						http.Error(w, "invalid fixture request", http.StatusBadRequest)
						return
					}
					if body.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					if chat {
						const counts = `"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":100}}`
						if body.Stream {
							_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"model\":\"served-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],%s}\n\ndata: [DONE]\n\n", counts)
						} else {
							_, _ = fmt.Fprintf(w, `{"id":"chat","object":"chat.completion","model":"served-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],%s}`, counts)
						}
						return
					}
					response := map[string]any{"id": "response", "object": "response", "status": "completed", "model": "served-model",
						"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}}}
					// The Responses stream first probes support with a unary call.
					// Keep the probe unavailable so only the stream supplies bad counts.
					if !streaming || body.Stream {
						response["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 100}}
					}
					if body.Stream {
						encoded, err := json.Marshal(response)
						if err != nil {
							t.Error(err)
							return
						}
						_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", encoded)
					} else {
						if err := json.NewEncoder(w).Encode(response); err != nil {
							t.Error(err)
						}
					}
				}))
				t.Cleanup(server.Close)
				provider, err := llm.NewProvider("openai", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
				require.NoError(t, err)
				provider = llm.NewRetryProvider(provider)
				if streaming {
					var stream <-chan llm.StreamChunk
					stream, err = provider.Stream(ctx, usageRequest())
					if err == nil {
						for chunk := range stream {
							err = errors.Join(err, chunk.Error)
						}
					}
				} else {
					_, err = provider.Complete(ctx, usageRequest())
				}
				require.ErrorIs(t, err, store.ErrValidation)
				require.True(t, llm.IsUsagePersistenceError(err))
				require.False(t, llm.ShouldRetry(err))
				require.False(t, llm.ShouldFallback(err))
				wantCalls := 1
				if chat || streaming {
					wantCalls = 2
				}
				require.EqualValues(t, wantCalls, requests.Load())
				got := report()
				require.Equal(t, wantCalls, got.Summary.Calls)
				require.Equal(t, wantCalls, got.Summary.MissingMeasurements)
				require.Equal(t, "unavailable", got.Summary.Completeness)
				require.Zero(t, got.Summary.TotalTokens)
				require.Zero(t, got.Summary.CachedInputTokens)
				require.Zero(t, got.Summary.CacheWriteInputTokens)
			})
		}
	}
}

func TestUsageStreamFailureAndCancellationPreserveInput(t *testing.T) {
	for _, cancelStream := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelStream), func(t *testing.T) {
			base, report := usageFixture(t)
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"message","type":"message","role":"assistant","model":"served-model","usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":10}}}`+"\n\n")
				w.(http.Flusher).Flush()
				if cancelStream {
					<-r.Context().Done()
					return
				}
				_, _ = fmt.Fprint(w, "event: error\ndata: "+`{"type":"error","error":{"type":"overloaded_error","message":"fixture failure"}}`+"\n\n")
			}))
			t.Cleanup(server.Close)
			provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
			require.NoError(t, err)
			stream, err := provider.Stream(ctx, usageRequest())
			require.NoError(t, err)
			first := <-stream
			require.True(t, first.UsageReported)
			if cancelStream {
				cancel()
			}
			for range stream {
			}
			got := report()
			require.EqualValues(t, 110, got.Summary.TotalTokens)
			require.Equal(t, "partial", got.Summary.Completeness)
			wantStatus := store.UsageStatusFailed
			if cancelStream {
				wantStatus = store.UsageStatusCancelled
			}
			require.Equal(t, wantStatus, got.Works[0].Tasks[0].Measurements[0].Status)
		})
	}
}

func TestUsageStreamFallbackAfterEarlyInput(t *testing.T) {
	for _, outputBeforeFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(outputBeforeFailure), func(t *testing.T) {
			ctx, report := usageFixture(t)
			var fallbackRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallback := strings.HasPrefix(r.URL.Path, "/fallback/")
				model, input, cached := "primary-model", 100, 10
				if fallback {
					fallbackRequests.Add(1)
					model, input, cached = "fallback-model", 200, 20
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"usage\":{\"input_tokens\":%d,\"output_tokens\":0,\"cache_read_input_tokens\":%d}}}\n\n", model, input, cached)
				if fallback || outputBeforeFailure {
					_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", model)
				}
				if fallback {
					_, _ = fmt.Fprint(w, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`+"\n\n")
				} else {
					_, _ = fmt.Fprint(w, "event: error\ndata: "+`{"type":"error","error":{"type":"overloaded_error","message":"fixture failure"}}`+"\n\n")
				}
			}))
			t.Cleanup(server.Close)
			primary, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL + "/primary/"})
			require.NoError(t, err)
			fallback, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL + "/fallback/"})
			require.NoError(t, err)
			provider := llm.NewFallbackProvider(primary, []llm.FallbackEntry{{Provider: fallback, Model: "fallback-model"}})
			stream, err := provider.Stream(ctx, usageRequest())
			require.NoError(t, err)
			var output strings.Builder
			var streamErr error
			for chunk := range stream {
				output.WriteString(chunk.Content)
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			got := report()
			require.Equal(t, store.UsageStatusFailed, got.Works[0].Tasks[0].Measurements[0].Status)
			require.Equal(t, 1, got.Summary.PartialMeasurements)
			if outputBeforeFailure {
				require.Error(t, streamErr)
				require.Equal(t, "primary-model", output.String())
				require.Zero(t, fallbackRequests.Load())
				require.EqualValues(t, 110, got.Summary.TotalTokens)
				require.Equal(t, 1, got.Summary.Calls)
			} else {
				require.NoError(t, streamErr)
				require.Equal(t, "fallback-model", output.String())
				require.EqualValues(t, 1, fallbackRequests.Load())
				require.EqualValues(t, 340, got.Summary.TotalTokens)
				require.Equal(t, 2, got.Summary.Calls)
				require.Equal(t, 2, got.Summary.ReportedMeasurements)
			}
		})
	}
}

func TestUsageMissingIsDifferentFromReportedZero(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(fmt.Sprint(reported), func(t *testing.T) {
			ctx, report := usageFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				usageJSON := ""
				if reported {
					usageJSON = `,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`
				}
				_, _ = fmt.Fprintf(w, `{"id":"response","object":"response","model":"served-model","status":"completed","output":[]%s}`, usageJSON)
			}))
			t.Cleanup(server.Close)
			provider, err := llm.NewProvider("openai", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
			require.NoError(t, err)
			_, err = provider.Complete(ctx, usageRequest())
			require.NoError(t, err)
			got := report()
			want := "unavailable"
			if reported {
				want = "complete"
			}
			require.Equal(t, want, got.Summary.Completeness)
			require.EqualValues(t, 0, got.Summary.TotalTokens)
		})
	}
}

func TestUsageStartMustPersistBeforeProviderCall(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	provider, err := llm.NewProvider("openai", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
	require.NoError(t, err)
	ctx := llm.WithUsageRecorder(t.Context(), func(context.Context, store.UsageObservation) error { return errors.New("storage unavailable") })
	_, err = provider.Complete(ctx, usageRequest())
	require.ErrorContains(t, err, "persist model call start")
	require.Zero(t, requests.Load())
}

func TestUsagePersistenceFailureDoesNotRepeatProviderCall(t *testing.T) {
	for _, strategy := range []string{"retry", "fallback", "retry and fallback"} {
		for _, streaming := range []bool{false, true} {
			for _, failureStatus := range []string{store.UsageStatusStarted, store.UsageStatusCompleted} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", strategy, streaming, failureStatus), func(t *testing.T) {
					recorderErr := errors.New("usage store unavailable")
					var failed atomic.Bool
					ctx, report := usageFixtureWithRecorder(t, func(observation store.UsageObservation) error {
						if observation.Status == failureStatus && failed.CompareAndSwap(false, true) {
							return recorderErr
						}
						return nil
					})
					var primaryCalls, fallbackCalls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasPrefix(r.URL.Path, "/fallback/") {
							fallbackCalls.Add(1)
						} else {
							primaryCalls.Add(1)
						}
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprint(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"message","type":"message","role":"assistant","model":"served-model","usage":{"input_tokens":100,"output_tokens":0}}}`+"\n\n")
							_, _ = fmt.Fprint(w, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`+"\n\n")
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprint(w, `{"id":"message","type":"message","role":"assistant","model":"served-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20}}`)
					}))
					t.Cleanup(server.Close)
					provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL + "/primary/"})
					require.NoError(t, err)
					if strings.Contains(strategy, "retry") {
						provider = llm.NewRetryProvider(provider)
					}
					if strings.Contains(strategy, "fallback") {
						fallback, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL + "/fallback/"})
						require.NoError(t, err)
						provider = llm.NewFallbackProvider(provider, []llm.FallbackEntry{{Provider: fallback}})
					}
					if streaming {
						var stream <-chan llm.StreamChunk
						stream, err = provider.Stream(ctx, usageRequest())
						if err == nil {
							for chunk := range stream {
								err = errors.Join(err, chunk.Error)
							}
						}
					} else {
						_, err = provider.Complete(ctx, usageRequest())
					}
					require.ErrorIs(t, err, recorderErr)
					require.False(t, llm.ShouldRetry(err))
					require.False(t, llm.ShouldFallback(err))
					require.Zero(t, fallbackCalls.Load())
					got := report()
					if failureStatus == store.UsageStatusStarted {
						require.Zero(t, primaryCalls.Load())
						require.Zero(t, got.Summary.Calls)
						return
					}
					require.EqualValues(t, 1, primaryCalls.Load())
					require.Equal(t, 1, got.Summary.Calls)
					require.NotEqual(t, "complete", got.Summary.Completeness)
					if !streaming {
						require.Equal(t, "unavailable", got.Summary.Completeness)
						require.Equal(t, 1, got.Summary.MissingMeasurements)
					}
				})
			}
		}
	}
}

func TestUsagePersistenceFailureStopsSDKRetry(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			writeErr := errors.New("usage store unavailable during SDK retry")
			var failed atomic.Bool
			ctx, report := usageFixtureWithRecorder(t, func(observation store.UsageObservation) error {
				if strings.HasSuffix(observation.ID, "/http-finish") && failed.CompareAndSwap(false, true) {
					return writeErr
				}
				return nil
			})
			var primaryCalls, fallbackCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/fallback/") {
					fallbackCalls.Add(1)
				} else {
					primaryCalls.Add(1)
				}
				w.Header().Set("retry-after-ms", "1")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"fixture failure"}}`)
			}))
			t.Cleanup(server.Close)
			primary, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL + "/primary/"})
			require.NoError(t, err)
			fallback, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL + "/fallback/"})
			require.NoError(t, err)
			provider := llm.NewFallbackProvider(llm.NewRetryProvider(primary), []llm.FallbackEntry{{Provider: fallback}})
			if streaming {
				var stream <-chan llm.StreamChunk
				stream, err = provider.Stream(ctx, usageRequest())
				if err == nil {
					for chunk := range stream {
						err = errors.Join(err, chunk.Error)
					}
				}
			} else {
				_, err = provider.Complete(ctx, usageRequest())
			}
			require.ErrorIs(t, err, writeErr)
			require.True(t, llm.IsUsagePersistenceError(err), "SDK conversion must preserve the accounting error")
			require.EqualValues(t, 1, primaryCalls.Load())
			require.Zero(t, fallbackCalls.Load())
			got := report()
			require.Equal(t, 1, got.Summary.Calls)
			require.Equal(t, 1, got.Summary.MissingMeasurements)
		})
	}
}
