/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures keep protocol fields visible.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestResponsesProductionEmptyOutcomes(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		for _, stream := range []bool{false, true} {
			for _, outcome := range []string{"max_output_tokens", "completed", "content_filter", "truncated call"} {
				t.Run(fmt.Sprintf("disabled=%t/stream=%t/%s", disabled, stream, outcome), func(t *testing.T) {
					var calls atomic.Int32
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						require.Equal(t, false, request["store"])
						response := responsesFixtureResponse("")
						if outcome != "completed" {
							response["status"] = "incomplete"
							reason := outcome
							if outcome == "truncated call" {
								reason = "max_output_tokens"
								response["output"] = responsesFixtureResponse("", llm.ToolCall{ID: "unfinished", Name: "file_read", Arguments: json.RawMessage(`{}`)})["output"]
							}
							response["incomplete_details"] = map[string]any{"reason": reason}
						}
						if request["stream"] == true {
							upstreamSSE(w, []map[string]any{{"type": "response." + response["status"].(string), "response": response}})
						} else {
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(response))
						}
					})
					// Budget exhaustion is terminal even without a goal-state sentinel.
					server.openaiHandler.config.MaxPrematureEndRetries = 2
					url := listenResponsesApp(t, server.app)
					body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"max_output_tokens":3,"input":"hello"}`, stream)
					status, _, data := productionResponsesRequest(t, url, token, body, disabled)
					var response map[string]any
					if stream {
						require.Equal(t, 200, status, string(data))
						events := parseResponsesSSE(t, data)
						terminal := events[len(events)-1]
						if outcome != "max_output_tokens" {
							require.Equal(t, "response.failed", terminal["type"])
							return
						}
						require.Equal(t, "response.incomplete", terminal["type"])
						require.Len(t, events, 3, "no output items or error events should be fabricated")
						response = terminal["response"].(map[string]any)
					} else {
						if outcome != "max_output_tokens" {
							require.Equal(t, 502, status, string(data))
							return
						}
						require.Equal(t, 200, status, string(data))
						require.NoError(t, json.Unmarshal(data, &response))
					}
					require.Equal(t, "incomplete", response["status"])
					require.Equal(t, []any{}, response["output"])
					require.Equal(t, map[string]any{"reason": "max_output_tokens"}, response["incomplete_details"])
					require.Nil(t, response["error"])
					require.EqualValues(t, 3, response["usage"].(map[string]any)["output_tokens"])
					require.EqualValues(t, 1, calls.Load(), "budget exhaustion must not trigger a coordinator retry")
				})
			}
		}
	}
}

func TestResponsesProductionExplicitTextAndChatHistory(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			captured := make(chan map[string]any, 1)
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				if fallback && r.URL.Path == "/v1/responses" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				captured <- request
				if fallback {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"chat-fixture","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"continued"},"finish_reason":"stop"}]}`) //nolint:errcheck
				} else {
					upstreamResponse(w, "continued")
				}
			})
			url := listenResponsesApp(t, server.app)
			status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"text":{"format":{"type":"text"}},"input":[
    {"role":"user","content":"use tools"},
    {"role":"assistant","content":"before "},
    {"type":"function_call","call_id":"call-one","name":"first","arguments":"{}"},
    {"role":"assistant","content":"after"},
    {"type":"function_call","call_id":"call-two","name":"second","arguments":"{}"},
    {"type":"function_call_output","call_id":"call-two","output":"two"},
    {"type":"function_call_output","call_id":"call-one","output":"one"},
    {"role":"user","content":"continue"}]}`, true)
			require.Equal(t, 200, status, string(data))
			request := <-captured
			require.Equal(t, false, request["store"])
			require.NotContains(t, request, "text", "explicit text must use the valid provider default")
			require.NotContains(t, request, "response_format", "explicit text must not serialize an empty format union")
			if fallback {
				messages, err := json.Marshal(request["messages"])
				require.NoError(t, err)
				require.JSONEq(t, `[
     {"role":"user","content":"use tools"},
     {"role":"assistant","content":"before after","tool_calls":[
      {"type":"function","id":"call-one","function":{"name":"first","arguments":"{}"}},
      {"type":"function","id":"call-two","function":{"name":"second","arguments":"{}"}}]},
     {"role":"tool","tool_call_id":"call-two","content":"two"},
     {"role":"tool","tool_call_id":"call-one","content":"one"},
     {"role":"user","content":"continue"}]`, string(messages))
			}
		})
	}
}

func TestResponsesProductionEmptyBudgetCoordinatorPaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, path := range []string{"final round", "stream required"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, path), func(t *testing.T) {
				var calls atomic.Int32
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					if path == "stream required" && request["stream"] != true {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`) //nolint:errcheck
						return
					}
					response := responsesFixtureResponse("")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					if request["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.incomplete", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.Empty(t, request["tools"], "final round must have no tools")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				if path == "final round" {
					server.openaiHandler.config.MaxIterations = 0
				}
				server.openaiHandler.config.MaxPrematureEndRetries = 2
				url := listenResponsesApp(t, server.app)
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"max_output_tokens":3,"input":"hello"}`, stream)
				status, _, data := productionResponsesRequest(t, url, token, body, false)
				require.Equal(t, 200, status, string(data))
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, data)
					require.Len(t, events, 3)
					require.Equal(t, "response.incomplete", events[2]["type"])
					response = events[2]["response"].(map[string]any)
				} else {
					require.NoError(t, json.Unmarshal(data, &response))
				}
				require.Equal(t, "incomplete", response["status"])
				require.Equal(t, []any{}, response["output"])
				require.EqualValues(t, 3, response["usage"].(map[string]any)["output_tokens"])
				expectedCalls := 1
				if path == "stream required" {
					expectedCalls = 2
				}
				require.EqualValues(t, expectedCalls, calls.Load())
			})
		}
	}
}

func TestResponsesProductionCoordinatorHistoryTruncation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, trigger := range []string{"configured", "context overflow"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, trigger), func(t *testing.T) {
				var calls atomic.Int32
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					round := calls.Add(1)
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					items := request["input"].([]any)
					// Production coordinator normalization runs before either truncation path.
					// Client calls/results must never reach the coordinator or become orphans.
					for _, raw := range items {
						item := raw.(map[string]any)
						require.NotEqual(t, "function_call", item["type"])
						require.NotEqual(t, "function_call_output", item["type"])
					}
					encoded, err := json.Marshal(items)
					require.NoError(t, err)
					if trigger == "context overflow" && round == 1 {
						require.Contains(t, string(encoded), "before ")
						require.Contains(t, string(encoded), "after prior")
						require.NotContains(t, string(encoded), "truncated")
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`) //nolint:errcheck
						return
					}
					require.Contains(t, string(encoded), "truncated", "the real coordinator must exercise the selected truncation path")
					require.Equal(t, "continue", items[len(items)-1].(map[string]any)["content"])
					upstreamResponse(w, "continued safely")
				})
				if trigger == "configured" {
					server.openaiHandler.config.MaxSessionSize = 128
				}
				url := listenResponsesApp(t, server.app)
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":[
     {"role":"user","content":"start"},
     {"role":"assistant","content":"before "},
     {"type":"function_call","call_id":"call-one","name":"first","arguments":"{}"},
     {"role":"assistant","content":%q},
     {"type":"function_call","call_id":"call-two","name":"second","arguments":"{}"},
     {"type":"function_call_output","call_id":"call-two","output":"two"},
     {"type":"function_call_output","call_id":"call-one","output":"one"},
     {"role":"user","content":"continue"}]}`, stream, "after "+strings.Repeat("prior ", 200))
				status, _, data := productionResponsesRequest(t, url, token, body, false)
				require.Equal(t, 200, status, string(data))
				require.Contains(t, string(data), "continued safely")
				expectedCalls := 1
				if trigger == "context overflow" {
					expectedCalls = 2
				}
				require.EqualValues(t, expectedCalls, calls.Load())
			})
		}
	}
}
