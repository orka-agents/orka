/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent regression fixtures retain literal wire values.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResponsesProductionUnfinishedFunctionCannotComplete(t *testing.T) {
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamSSE(w, []map[string]any{
			{"type": "response.output_text.delta", "output_index": 0, "item_id": "text-item", "content_index": 0, "delta": "I will read the file."},
			{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"id": "partial-function", "type": "function_call", "call_id": "call-pending", "name": "file_read", "status": "in_progress", "arguments": ""}},
			{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "partial-function", "delta": `{"path":`},
			{"type": "response.completed", "response": map[string]any{"id": "upstream", "status": "completed", "output": []any{}}},
		})
	})
	status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"read the file"}`, true)
	require.Equal(t, http.StatusOK, status)
	events := parseResponsesSSE(t, body)
	terminal := events[len(events)-1]
	require.Equal(t, "response.failed", terminal["type"], "an unfinished function must not be silently dropped")
}

func TestResponsesProductionIncompleteOutputCanBeReplayed(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var rounds atomic.Int32
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				response := responsesFixtureResponse("continued answer")
				if rounds.Add(1) == 1 {
					response = responsesFixtureResponse("partial answer")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					response["output"].([]any)[0].(map[string]any)["status"] = "incomplete"
				} else {
					history := request["input"].([]any)
					require.Len(t, history, 3)
					require.Equal(t, "assistant", history[1].(map[string]any)["role"])
					require.Equal(t, "partial answer", history[1].(map[string]any)["content"])
				}
				if request["stream"] == true {
					upstreamSSE(w, []map[string]any{{"type": "response." + response["status"].(string), "response": response}})
				} else {
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(response))
				}
			})
			url := listenResponsesApp(t, server.app)
			status, _, body := productionResponsesRequest(t, url, token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"answer in detail","max_output_tokens":3}`, stream), true)
			require.Equal(t, http.StatusOK, status)
			var response map[string]any
			if stream {
				events := parseResponsesSSE(t, body)
				require.Equal(t, "response.incomplete", events[len(events)-1]["type"])
				response = events[len(events)-1]["response"].(map[string]any)
			} else {
				require.NoError(t, json.Unmarshal(body, &response))
			}
			require.Equal(t, "incomplete", response["status"])
			input := []any{map[string]any{"role": "user", "content": "answer in detail"}}
			input = append(input, response["output"].([]any)...)
			input = append(input, map[string]any{"role": "user", "content": "continue"})
			request, err := json.Marshal(map[string]any{"model": "fixture/test-model", "store": false, "stream": stream, "input": input})
			require.NoError(t, err)
			status, _, body = productionResponsesRequest(t, url, token, string(request), true)
			require.Equal(t, http.StatusOK, status, "the endpoint must accept its own returned text as stateless history")
			require.Contains(t, string(body), "continued answer")
			require.EqualValues(t, 2, rounds.Load())
		})
	}
}

func TestResponsesHTTPAnthropicPreservesSystemRole(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			captured := make(chan map[string]any, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				captured <- request
				response := map[string]any{
					"id": "fixture-message", "type": "message", "role": "assistant", "model": "fixture-model",
					"content": []any{map[string]any{"type": "text", "text": "ok"}}, "stop_reason": "end_turn",
					"usage": map[string]any{"input_tokens": 4, "output_tokens": 1},
				}
				if request["stream"] == true {
					response["content"] = []any{}
					response["stop_reason"] = nil
					response["usage"].(map[string]any)["output_tokens"] = 0
					upstreamSSE(w, []map[string]any{
						{"type": "message_start", "message": response},
						{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
						{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}},
						{"type": "content_block_stop", "index": 0},
						{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 1}},
						{"type": "message_stop"},
					})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(response))
			}))
			defer upstream.Close()
			provider := &corev1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "default"}, Spec: corev1alpha1.ProviderSpec{
				Type: corev1alpha1.ProviderTypeAnthropic, DefaultModel: "fixture-model", BaseURL: upstream.URL,
				SecretRef: corev1alpha1.ProviderSecretRef{Name: "fixture", Key: "key"},
			}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "default"}, Data: map[string][]byte{"key": []byte("local-fixture-only")}}
			h, app := setupTestOpenAIHandler(provider, secret)
			app.Post(responsesPath, h.HandleResponses)
			status, body := requestResponses(t, app, fmt.Sprintf(`{"model":"fixture/fixture-model","store":false,"stream":%t,"instructions":"Keep it short.","input":[{"role":"system","content":"Answer only in JSON."},{"role":"user","content":"hello"},{"role":"developer","content":"Include a source."}]}`, stream), true)
			require.Equal(t, http.StatusOK, status, string(body))
			if stream {
				events := parseResponsesSSE(t, body)
				require.Equal(t, "response.completed", events[len(events)-1]["type"])
			}
			request := <-captured
			require.Equal(t, []any{
				map[string]any{"type": "text", "text": "Keep it short."},
				map[string]any{"type": "text", "text": "Answer only in JSON."},
				map[string]any{"type": "text", "text": "Include a source."},
			}, request["system"])
			require.Equal(t, []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}}}, request["messages"])
		})
	}
}

func TestResponsesProductionItemTypeMustRemainStable(t *testing.T) {
	for _, mode := range []string{"text-delta", "text-index-only", "message-id-only", "function-id-only", "completed-function"} {
		t.Run(mode, func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				function := map[string]any{"id": "shared-item", "type": "function_call", "call_id": "call-one", "name": "client_tool", "arguments": "{}", "status": "completed"}
				message := map[string]any{"id": "shared-item", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "text for one item"}}}
				response := responsesFixtureResponse("")
				response["output"] = []any{function}
				events := []map[string]any{{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "shared-item", "delta": "text for one item"}}
				switch mode {
				case "text-index-only":
					delete(events[0], "item_id")
				case "message-id-only":
					events = []map[string]any{{"type": "response.output_item.added", "item": message}}
				case "function-id-only":
					events = []map[string]any{
						{"type": "response.function_call_arguments.delta", "item_id": "shared-item", "delta": "{"},
						{"type": "response.output_item.added", "output_index": 0, "item": message},
					}
				case "completed-function":
					events = []map[string]any{{"type": "response.output_item.added", "output_index": 0, "item": function}}
					response["output"] = []any{message}
				}
				events = append(events, map[string]any{"type": "response.completed", "response": response})
				upstreamSSE(w, events)
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			requireResponsesIntegrityFailure(t, status, body, true)
			if mode != "completed-function" {
				require.NotContains(t, string(body), "call-one", "contradictory metadata must fail before exposing a function")
			}
		})
	}
}

func TestResponsesProductionFinalCoordinatorRecoversStreamingRequired(t *testing.T) {
	for _, limit := range []int{1, 2} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("limit=%d/stream=%t", limit, stream), func(t *testing.T) {
				path := filepath.Join(responsesToolTempDir(t), "read.txt")
				require.NoError(t, os.WriteFile(path, []byte("fixture content"), 0600))
				arguments, err := json.Marshal(map[string]string{"path": path})
				require.NoError(t, err)
				var recovered atomic.Int32
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					require.Equal(t, float64(0), request["temperature"])
					require.Equal(t, float64(37), request["max_output_tokens"])
					require.Contains(t, request["instructions"], "Be precise.")
					hasResult := false
					for _, raw := range request["input"].([]any) {
						if raw.(map[string]any)["type"] == "function_call_output" {
							hasResult = true
						}
					}
					if !hasResult {
						response := responsesFixtureResponse("")
						response["output"] = []any{map[string]any{"id": "read-item", "type": "function_call", "call_id": "read-call", "name": "file_read", "arguments": string(arguments), "status": "completed"}}
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
						return
					}
					if limit == 1 {
						require.Empty(t, request["tools"], "final recovery must remain tools-free")
					}
					if request["stream"] == true {
						recovered.Add(1)
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": responsesFixtureResponse(goalStateSentinel + "\nsummary is available")}})
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "streaming is required"}}))
				})
				server.openaiHandler.config.MaxIterations = limit
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"temperature":0,"max_output_tokens":37,"instructions":"Be precise.","input":"read then summarize"}`, stream), false)
				if stream {
					events := parseResponsesSSE(t, body)
					require.Equal(t, "response.completed", events[len(events)-1]["type"])
				} else {
					require.Equal(t, http.StatusOK, status, string(body))
				}
				require.NotContains(t, string(body), goalStateSentinel)
				require.Contains(t, string(body), "summary is available")
				require.EqualValues(t, 1, recovered.Load())
			})
		}
	}
}

func TestResponsesProductionProgressCannotMaskEmptyFinal(t *testing.T) {
	for _, final := range []string{"empty", "sentinel", "answer", "empty-budget"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", final, stream), func(t *testing.T) {
				path := filepath.Join(responsesToolTempDir(t), "read.txt")
				require.NoError(t, os.WriteFile(path, []byte("fixture content"), 0600))
				arguments, err := json.Marshal(map[string]string{"path": path})
				require.NoError(t, err)
				var calls atomic.Int32
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						upstreamResponse(w, "Reading the file.", llm.ToolCall{ID: "read-call", Name: "file_read", Arguments: arguments})
						return
					}
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Empty(t, request["tools"])
					response := responsesFixtureResponse("")
					switch final {
					case "sentinel":
						response = responsesFixtureResponse(goalStateSentinel + "\n")
					case "answer":
						response = responsesFixtureResponse(goalStateSentinel + "\nFinal answer.")
					case "empty-budget":
						response["status"] = "incomplete"
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					}
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(response))
				})
				server.openaiHandler.config.MaxIterations = 1
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read then summarize"}`, stream), false)
				require.EqualValues(t, 2, calls.Load())
				if stream {
					require.Contains(t, string(body), "Reading the file.", "the fixture must emit progress before the final completion")
				}
				if final == "empty" || final == "sentinel" {
					requireResponsesIntegrityFailure(t, status, body, stream)
					return
				}
				require.Equal(t, http.StatusOK, status)
				wantStatus := "completed"
				if final == "empty-budget" {
					wantStatus = "incomplete"
				}
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, body)
					require.Equal(t, "response."+wantStatus, events[len(events)-1]["type"])
					response = events[len(events)-1]["response"].(map[string]any)
				} else {
					require.NoError(t, json.Unmarshal(body, &response))
				}
				require.Equal(t, wantStatus, response["status"])
				if final == "answer" {
					require.Contains(t, string(body), "Final answer.")
				}
			})
		}
	}
}

func TestResponsesProductionFailedTerminalDoesNotFlush(t *testing.T) {
	for _, ending := range []string{"failed", "eof", "error", "incomplete", "budget-call", "failed-status", "completed", "budget-text"} {
		for _, snapshot := range []bool{false, true} {
			if snapshot && (ending == "eof" || ending == "error") {
				continue
			}
			t.Run(fmt.Sprintf("%s/snapshot=%t", ending, snapshot), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					queued := "call-one"
					if ending == "budget-text" {
						queued = "later text"
					}
					response, _ := orderedResponsesWire([]string{"partial", queued}, "client_tool", "{}")
					output := response["output"].([]any)
					events := []map[string]any{
						{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "partial"},
						{"type": "response.output_item.done", "output_index": 1, "item": output[1]},
					}
					terminal := "response.completed"
					switch ending {
					case "failed":
						terminal, response["status"] = "response.failed", "failed"
					case "failed-status":
						response["status"] = "failed"
					case "incomplete", "budget-call", "budget-text":
						terminal, response["status"] = "response.incomplete", "incomplete"
						reason := "max_output_tokens"
						if ending == "incomplete" {
							reason = "content_filter"
						}
						response["incomplete_details"] = map[string]any{"reason": reason}
					case "error":
						events = append(events, map[string]any{"type": "error", "message": "upstream failed"})
					}
					if !snapshot {
						response["output"] = []any{}
					}
					if ending != "eof" && ending != "error" {
						events = append(events, map[string]any{"type": terminal, "response": response})
					}
					upstreamSSE(w, events)
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				output := terminal["response"].(map[string]any)["output"].([]any)
				switch ending {
				case "completed":
					require.Equal(t, "response.completed", terminal["type"])
					require.Equal(t, []string{"partial", "call-one"}, responsesItemOrder(t, output))
				case "budget-text":
					require.Equal(t, "response.incomplete", terminal["type"])
					require.Equal(t, []string{"partial", "later text"}, responsesItemOrder(t, output))
				default:
					require.Equal(t, "response.failed", terminal["type"])
					require.Equal(t, []string{"partial"}, responsesItemOrder(t, output), "failed terminal events must not publish queued calls")
					require.Equal(t, "incomplete", output[0].(map[string]any)["status"])
				}
			})
		}
	}
}

func TestResponsesProductionTerminalValidatesQueuedFunctions(t *testing.T) {
	for _, change := range []string{"matching", "arguments", "name", "trailing-text"} {
		for _, callCount := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/calls=%d", change, callCount), func(t *testing.T) {
				order := []string{"partial", "call-one"}
				if callCount == 2 {
					order = append(order, "call-two")
				}
				if change == "trailing-text" {
					order = append(order, "original trailing text")
				}
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					response, _ := orderedResponsesWire(order, "client_tool", `{"value":"original"}`)
					output := response["output"].([]any)
					events := []map[string]any{{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "partial"}}
					for index := 1; index < len(output); index++ {
						events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": maps.Clone(output[index].(map[string]any))})
					}
					last := output[len(output)-1].(map[string]any)
					switch change {
					case "arguments":
						last["arguments"] = `{"value":"changed"}`
					case "name":
						last["name"] = "changed_tool"
					case "trailing-text":
						last["content"] = []any{map[string]any{"type": "output_text", "text": "changed trailing text"}}
					}
					upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				if change == "matching" {
					require.Equal(t, "response.completed", terminal["type"])
					require.Equal(t, order, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
					return
				}
				requireResponsesIntegrityFailure(t, status, body, true)
				for _, event := range events {
					if item, ok := event["item"].(map[string]any); ok {
						require.NotEqual(t, "function_call", item["type"], "validate every terminal snapshot before releasing any queued call")
					}
				}
			})
		}
	}
}

func TestResponsesProductionTerminalRequiresMatchingStatus(t *testing.T) {
	for _, eventStatus := range []string{"completed", "incomplete"} {
		for _, nestedStatus := range []string{"incomplete", "", "failed", "cancelled", "completed", "in_progress", "queued", "unknown", "stop", "length", "tool_calls"} {
			for _, snapshot := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/snapshot=%t", eventStatus, nestedStatus, snapshot), func(t *testing.T) {
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
						response, _ := orderedResponsesWire([]string{"partial", "queued text"}, "", "")
						events := []map[string]any{
							{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "partial"},
							{"type": "response.output_item.done", "output_index": 1, "item": response["output"].([]any)[1]},
						}
						response["status"] = nestedStatus
						if nestedStatus == "" {
							delete(response, "status")
						}
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						if !snapshot {
							response["output"] = []any{}
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response." + eventStatus, "response": response}))
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
					require.Equal(t, http.StatusOK, status)
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					output := terminal["response"].(map[string]any)["output"].([]any)
					if nestedStatus == eventStatus {
						require.Equal(t, "response."+eventStatus, terminal["type"])
						require.Equal(t, []string{"partial", "queued text"}, responsesItemOrder(t, output))
						return
					}
					require.Equal(t, "response.failed", terminal["type"])
					require.Equal(t, []string{"partial"}, responsesItemOrder(t, output), "a contradictory terminal status must not recover queued text")
				})
			}
		}
	}
}

func TestResponsesProductionTerminalPreservesMembership(t *testing.T) {
	for _, mode := range []string{"queued-text", "emitted-text", "queued-call", "emitted-call", "call-id-bridge", "id-only"} {
		hasCall := mode == "queued-call" || mode == "emitted-call" || mode == "call-id-bridge"
		for _, ending := range []string{"completed", "incomplete"} {
			if hasCall && ending == "incomplete" {
				continue // Token-budget truncation only supports text output.
			}
			for _, snapshot := range []string{"truncated", "full", "empty", "omitted"} {
				if mode == "id-only" && (snapshot == "empty" || snapshot == "omitted") {
					continue
				}
				t.Run(mode+"/"+ending+"/"+snapshot, func(t *testing.T) {
					order := []string{"first", "second"}
					if hasCall {
						order[1] = "call-one"
					}
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
						response, _ := orderedResponsesWire(order, "client_tool", "{}")
						output := response["output"].([]any)
						events := []map[string]any{{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "first"}}
						if mode == "emitted-text" || mode == "emitted-call" {
							events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": output[0]})
						}
						switch mode {
						case "id-only":
							events = append(events, map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "ordered-item-1", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
						case "call-id-bridge":
							added := maps.Clone(output[1].(map[string]any))
							delete(output[1].(map[string]any), "id")
							events = append(events,
								map[string]any{"type": "response.output_item.added", "item": added},
								map[string]any{"type": "response.output_item.done", "output_index": 1, "item": output[1]})
						default:
							events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 1, "item": output[1]})
						}
						response["status"] = ending
						if ending == "incomplete" {
							response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						}
						switch snapshot {
						case "truncated":
							response["output"] = output[:1]
						case "empty":
							response["output"] = []any{}
						case "omitted":
							delete(response, "output")
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response." + ending, "response": response}))
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
					require.Equal(t, http.StatusOK, status)
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					output := terminal["response"].(map[string]any)["output"].([]any)
					if snapshot == "truncated" {
						require.Equal(t, "response.failed", terminal["type"], "a nonempty terminal snapshot cannot omit an observed item")
						if mode != "emitted-text" && mode != "emitted-call" {
							require.Equal(t, []string{"first"}, responsesItemOrder(t, output), "the invalid terminal must not release queued output")
						}
						return
					}
					require.Equal(t, "response."+ending, terminal["type"])
					require.Equal(t, order, responsesItemOrder(t, output))
				})
			}
		}
	}
}

func TestResponsesProductionIncompleteStatusCannotComplete(t *testing.T) {
	for _, layout := range []string{"indexed", "unindexed", "queued"} {
		for _, ending := range []string{"completed", "incomplete"} {
			for _, snapshot := range []string{"explicit", "without-status", "empty", "omitted"} {
				t.Run(layout+"/"+ending+"/"+snapshot, func(t *testing.T) {
					order := []string{"partial"}
					if layout == "queued" {
						order = []string{"before", "partial"}
					}
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
						response, _ := orderedResponsesWire(order, "", "")
						last := response["output"].([]any)[len(order)-1].(map[string]any)
						last["status"] = "incomplete"
						done := map[string]any{"type": "response.output_item.done", "item": maps.Clone(last)}
						if layout != "unindexed" {
							done["output_index"] = len(order) - 1
						}
						events := []map[string]any{}
						if layout == "queued" {
							events = append(events, map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "before"})
						}
						events = append(events, done)
						response["status"] = ending
						if ending == "incomplete" {
							response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						}
						switch snapshot {
						case "without-status":
							delete(last, "status")
						case "empty":
							response["output"] = []any{}
						case "omitted":
							delete(response, "output")
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response." + ending, "response": response}))
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
					require.Equal(t, http.StatusOK, status)
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					output := terminal["response"].(map[string]any)["output"].([]any)
					if ending == "completed" {
						require.Equal(t, "response.failed", terminal["type"], "omitted terminal metadata cannot erase an explicitly incomplete message")
						if layout == "queued" {
							require.Equal(t, []string{"before"}, responsesItemOrder(t, output))
						}
						return
					}
					require.Equal(t, "response.incomplete", terminal["type"])
					require.Equal(t, order, responsesItemOrder(t, output))
					require.Equal(t, "incomplete", output[len(output)-1].(map[string]any)["status"])
				})
			}
		}
	}
}

func TestResponsesProductionIncompleteMessagesCannotReleaseCalls(t *testing.T) {
	for _, queuedFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(queuedFirst), func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				response, _ := orderedResponsesWire([]string{"partial", "call-one"}, "client_tool", "{}")
				output := response["output"].([]any)
				output[0].(map[string]any)["status"] = "incomplete"
				messageDone := map[string]any{"type": "response.output_item.done", "output_index": 0, "item": output[0]}
				callDone := map[string]any{"type": "response.output_item.done", "output_index": 1, "item": output[1]}
				events := []map[string]any{{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "partial"}}
				if queuedFirst {
					events = append(events, callDone, messageDone)
				} else {
					events = append(events, messageDone, callDone)
				}
				response["status"] = "incomplete"
				response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				upstreamSSE(w, append(events, map[string]any{"type": "response.incomplete", "response": response}))
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			requireResponsesIntegrityFailure(t, status, body, true)
			for _, event := range parseResponsesSSE(t, body) {
				if item, ok := event["item"].(map[string]any); ok {
					require.NotEqual(t, "function_call", item["type"], "explicitly incomplete output cannot release executable calls")
				}
			}
		})
	}
}
