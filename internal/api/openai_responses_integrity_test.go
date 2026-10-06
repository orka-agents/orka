/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep independent protocol fixtures literal.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func requireResponsesIntegrityFailure(t *testing.T, status int, body []byte, stream bool) {
	t.Helper()
	if !stream {
		require.Equal(t, http.StatusBadGateway, status)
		return
	}
	require.Equal(t, http.StatusOK, status)
	events := parseResponsesSSE(t, body)
	require.Equal(t, "response.failed", events[len(events)-1]["type"])
}

func TestResponsesProductionStructuredSentinelValue(t *testing.T) {
	for _, format := range []string{"json_object", "json_schema"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				value := "{\"value\":\"" + goalStateSentinel + "\"}"
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var input map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
					require.Equal(t, format, input["text"].(map[string]any)["format"].(map[string]any)["type"])
					upstreamResponse(w, value)
				})
				f := map[string]any{"type": format}
				if format == "json_schema" {
					f["name"], f["strict"] = "answer", true
					f["schema"] = map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}, "additionalProperties": false}
				}
				request, err := json.Marshal(map[string]any{"model": "fixture/test-model", "store": false, "stream": stream, "input": "hello", "text": map[string]any{"format": f}})
				require.NoError(t, err)
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, string(request), false)
				require.Equal(t, http.StatusOK, status)
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, body)
					require.Equal(t, "response.completed", events[len(events)-1]["type"])
					response = events[len(events)-1]["response"].(map[string]any)
				} else {
					require.NoError(t, json.Unmarshal(body, &response))
				}
				require.Equal(t, []string{value}, responsesItemOrder(t, response["output"].([]any)))
			})
		}
	}
}

func TestResponsesProductionFinalRoundRejectsCalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Empty(t, request["tools"])
				upstreamResponse(w, "summary", llm.ToolCall{ID: "internal-call", Name: "file_read", Arguments: json.RawMessage("{}")})
			})
			server.openaiHandler.config.MaxIterations = 0
			request := fmt.Sprintf("{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":%t,\"input\":\"hello\"}", stream)
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, request, false)
			requireResponsesIntegrityFailure(t, status, body, stream)
			require.EqualValues(t, 1, calls.Load())
			require.NotContains(t, string(body), "internal-call")
		})
	}
}

func TestResponsesProductionUnsupportedOutput(t *testing.T) {
	for _, kind := range []string{"reasoning", "web_search_call", "unknown_item", "unknown_content"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					response := responsesFixtureResponse("ordinary text")
					item := map[string]any{"id": "unsupported-item", "type": kind, "status": "completed", "summary": []any{}}
					if kind == "unknown_content" {
						item = response["output"].([]any)[0].(map[string]any)
						item["content"] = append(item["content"].([]any), map[string]any{"type": "unknown_content", "text": "hidden"})
					} else {
						response["output"] = append(response["output"].([]any), item)
					}
					if stream {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				request := fmt.Sprintf("{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":%t,\"input\":\"hello\"}", stream)
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, request, true)
				requireResponsesIntegrityFailure(t, status, body, stream)
			})
		}
	}
}

func TestResponsesProductionFunctionIntegrity(t *testing.T) {
	for _, mode := range []string{"duplicate-call-id", "missing-call-id", "late-delta", "late-done", "terminal-arguments", "terminal-name", "terminal-call-id", "index-gap", "open-index-gap"} {
		t.Run(mode, func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				order := []string{"call-one"}
				if mode == "duplicate-call-id" {
					order = append(order, "call-two")
				}
				response, events := orderedResponsesWire(order, "client_tool", "{}")
				switch mode {
				case "duplicate-call-id", "missing-call-id":
					for _, event := range events {
						if item, ok := event["item"].(map[string]any); ok {
							if mode == "missing-call-id" {
								delete(item, "call_id")
							} else {
								item["call_id"] = "call-one"
							}
						}
					}
					for _, raw := range response["output"].([]any) {
						item := raw.(map[string]any)
						if mode == "missing-call-id" {
							delete(item, "call_id")
						} else {
							item["call_id"] = "call-one"
						}
					}
				case "late-delta", "late-done":
					event := map[string]any{"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "output_index": 0, "delta": " "}
					if mode == "late-done" {
						event = map[string]any{"type": "response.function_call_arguments.done", "item_id": "ordered-item-0", "output_index": 0, "arguments": "{\"changed\":true}"}
					}
					events = append(events[:len(events)-1], event, events[len(events)-1])
				case "terminal-arguments", "terminal-name", "terminal-call-id":
					data, err := json.Marshal(response)
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal(data, &response))
					item := response["output"].([]any)[0].(map[string]any)
					field, value := "arguments", "{\"changed\":true}"
					if mode == "terminal-name" {
						field, value = "name", "other_tool"
					}
					if mode == "terminal-call-id" {
						field, value = "call_id", "other-call"
					}
					item[field] = value
					events[len(events)-1]["response"] = response
				case "index-gap", "open-index-gap":
					response = responsesFixtureResponse("")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					events = []map[string]any{}
					if mode == "open-index-gap" {
						events = append(events, map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "first", "content_index": 0, "delta": "first"})
					}
					events = append(events,
						map[string]any{"type": "response.output_text.delta", "output_index": 2, "item_id": "gap", "content_index": 0, "delta": "gap"},
						map[string]any{"type": "response.incomplete", "response": response})
				}
				upstreamSSE(w, events)
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, "{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":true,\"input\":\"hello\"}", true)
			requireResponsesIntegrityFailure(t, status, body, true)
		})
	}
}

func TestResponsesProductionStreamOpenFallback(t *testing.T) {
	for _, mode := range []string{"unsupported", "auth", "event-error", "partial-error", "invalid-output", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			var streams, completions atomic.Int32
			canceled := make(chan struct{})
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var input map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
				require.Equal(t, false, input["store"])
				if input["stream"] != true {
					completions.Add(1)
					upstreamResponse(w, "fallback answer")
					return
				}
				streams.Add(1)
				if mode == "canceled" {
					<-r.Context().Done()
					close(canceled)
					return
				}
				if mode == "unsupported" || mode == "auth" {
					code := http.StatusBadRequest
					if mode == "auth" {
						code = http.StatusUnauthorized
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(code)
					_, _ = fmt.Fprint(w, "{\"error\":{\"message\":\"streaming is not supported\"}}")
					return
				}
				events := []map[string]any{}
				if mode == "partial-error" {
					events = append(events, map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "partial", "delta": "partial"})
				}
				if mode == "invalid-output" {
					events = append(events, map[string]any{"type": "response.output_item.added", "output_index": -1, "item": map[string]any{"id": "invalid", "type": "message", "role": "assistant", "content": []any{}}})
				}
				upstreamSSE(w, append(events, map[string]any{"type": "error", "message": "streaming is not supported"}))
			})
			if mode == "canceled" {
				server.openaiHandler.config.MaxDuration = 100 * time.Millisecond
			}
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, "{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":true,\"input\":\"hello\"}", true)
			if mode == "unsupported" {
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				require.Equal(t, "response.completed", events[len(events)-1]["type"])
				require.EqualValues(t, 1, completions.Load())
				require.Contains(t, string(body), "fallback answer")
			} else {
				requireResponsesIntegrityFailure(t, status, body, true)
				require.Zero(t, completions.Load())
			}
			if mode == "canceled" {
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("upstream did not observe cancellation")
				}
			}
			require.EqualValues(t, 1, streams.Load())
		})
	}
}

func TestResponsesProductionDeadlineDuringTerminalWrite(t *testing.T) {
	const providerType = "responses-terminal-deadline-fixture"
	chunks := make(chan llm.StreamChunk, 1)
	chunks <- llm.StreamChunk{Content: strings.Repeat("x", 8<<20), Done: true, StopReason: "stop"}
	close(chunks)
	provider := &responsesDeadlineProvider{oaiMockProvider: &oaiMockProvider{streamCh: chunks}, observed: make(chan context.Context, 1)}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
	h, _ := setupTestOpenAIHandler(providerCRD("fixture", "default", providerType, "test-model")...)
	issuer := newTestOIDCProvider(t)
	server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, OIDC: issuer.config()})
	server.openaiHandler.config.MaxDuration = 10 * time.Millisecond
	// Race instrumentation makes this large response exceed the normal socket
	// grace period. Isolate the handler deadline; socket deadlines have their
	// own real-TCP backpressure and keep-alive tests.
	server.app.Server().HeaderReceived = func(header *fasthttp.RequestHeader) fasthttp.RequestConfig {
		config := server.requestConfig(header)
		config.WriteTimeout = 15 * time.Second
		return config
	}
	token := issuer.issueToken(t, testOIDCTokenOptions{Namespace: "default"})
	status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, "{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":true,\"input\":\"hello\"}", true)
	streamContext := <-provider.observed
	require.ErrorIs(t, streamContext.Err(), context.DeadlineExceeded)
	requireResponsesIntegrityFailure(t, status, body, true)
}

func TestResponsesProductionFragmentedFunctionIdentity(t *testing.T) {
	for _, mode := range []string{"late-item-call-id", "terminal-call-id", "terminal-name", "split-identity", "duplicate-notifications"} {
		t.Run(mode, func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				response, events := orderedResponsesWire([]string{"call-one", "after"}, "client_tool", "{}")
				switch mode {
				case "late-item-call-id", "terminal-call-id", "terminal-name":
					for _, event := range events {
						if item, ok := event["item"].(map[string]any); ok && item["type"] == "function_call" {
							copy := make(map[string]any, len(item))
							maps.Copy(copy, item)
							if mode != "late-item-call-id" || event["type"] == "response.output_item.added" {
								field := "call_id"
								if mode == "terminal-name" {
									field = "name"
								}
								delete(copy, field)
							}
							event["item"] = copy
						}
					}
				case "split-identity":
					response, _ = orderedResponsesWire([]string{"call-one"}, "client_tool", "{}")
					item := response["output"].([]any)[0]
					events = []map[string]any{
						{"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "delta": "{}"},
						{"type": "response.function_call_arguments.done", "item_id": "ordered-item-0", "arguments": "{}"},
						{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "call-one", "name": "client_tool", "arguments": "", "status": "in_progress"}},
						{"type": "response.output_item.done", "output_index": 0, "item": item},
						{"type": "response.completed", "response": response},
					}
				case "duplicate-notifications":
					terminal := events[len(events)-1]
					events = append(events[:len(events)-1],
						map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "ordered-item-0", "arguments": "{}"},
						map[string]any{"type": "response.output_item.done", "output_index": 0, "item": response["output"].([]any)[0]},
						terminal)
				}
				upstreamSSE(w, events)
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, "{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":true,\"input\":\"hello\"}", true)
			require.Equal(t, http.StatusOK, status)
			events := parseResponsesSSE(t, body)
			terminal := events[len(events)-1]
			require.Equal(t, "response.completed", terminal["type"])
			items := terminal["response"].(map[string]any)["output"].([]any)
			want := []string{"call-one", "after"}
			if mode == "split-identity" {
				want = []string{"call-one"}
			}
			require.Equal(t, want, responsesItemOrder(t, items))
			require.Equal(t, "{}", items[0].(map[string]any)["arguments"])
		})
	}
}

// Only the provider interface is substituted. One large terminal chunk crosses
// the deadline inside the actual TCP writer, without relying on select luck.
type responsesDeadlineProvider struct {
	*oaiMockProvider
	observed chan context.Context
}

func (p *responsesDeadlineProvider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	p.observed <- ctx
	return p.oaiMockProvider.Stream(ctx, req)
}

func TestResponsesProductionSplitStateConflict(t *testing.T) {
	for _, mode := range []string{"matching", "name-conflict", "arguments-conflict", "both-conflict"} {
		t.Run(mode, func(t *testing.T) {
			const nameA = "client_tool"
			const argsA = "{\"value\":\"terminal\"}"
			nameB, argsB := nameA, argsA
			if mode == "name-conflict" || mode == "both-conflict" {
				nameB = "other_tool"
			}
			if mode == "arguments-conflict" || mode == "both-conflict" {
				argsB = "{\"value\":\"early\"}"
			}
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				response, _ := orderedResponsesWire([]string{"call-one"}, nameA, argsA)
				item := response["output"].([]any)[0]
				upstreamSSE(w, []map[string]any{
					{"type": "response.function_call_arguments.done", "item_id": "ordered-item-0", "name": nameA, "arguments": argsA},
					{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "call-one", "name": nameB, "arguments": argsB, "status": "in_progress"}},
					{"type": "response.output_item.done", "output_index": 0, "item": item},
					{"type": "response.completed", "response": response},
				})
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token,
				"{\"model\":\"fixture/test-model\",\"store\":false,\"stream\":true,\"input\":\"hello\"}", true)
			require.Equal(t, http.StatusOK, status)
			events := parseResponsesSSE(t, body)
			terminal := events[len(events)-1]
			if mode == "matching" {
				require.Equal(t, "response.completed", terminal["type"])
				item := terminal["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
				require.Equal(t, nameA, item["name"])
				require.Equal(t, argsA, item["arguments"])
			} else {
				require.Equal(t, "response.failed", terminal["type"], "conflicting completed function states must not merge into successful output")
			}
		})
	}
}
