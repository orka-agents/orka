/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent protocol controls retain literal wire values.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestResponsesProductionSentinelMessageBoundaries(t *testing.T) {
	layouts := []struct{ input, want []string }{
		{[]string{" " + goalStateSentinel + "\nfirst", " second"}, []string{"first", " second"}},
		{[]string{goalStateSentinel[:10], goalStateSentinel[10:] + "\nfirst", " second"}, []string{"first", " second"}},
		{[]string{"first " + goalStateSentinel[:10], goalStateSentinel[10:] + "\nsecond"}, []string{"first ", "\nsecond"}},
		{[]string{goalStateSentinel + "\nfirst", goalStateSentinel + "\nsecond"}, []string{"first", "\nsecond"}},
		{[]string{"界" + goalStateSentinel[:10], goalStateSentinel[10:] + "\n雪"}, []string{"界", "\n雪"}},
	}
	for index, layout := range layouts {
		for _, mode := range []struct{ aggregate, disabled, stream bool }{
			{}, {stream: true}, {aggregate: true}, {aggregate: true, stream: true}, {disabled: true}, {disabled: true, stream: true},
		} {
			aggregate, disabled, stream := mode.aggregate, mode.disabled, mode.stream
			t.Run(fmt.Sprintf("%d/aggregate=%t/disabled=%t/stream=%t", index, aggregate, disabled, stream), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var req map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					require.Equal(t, false, req["store"])
					if aggregate && req["stream"] != true {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
						return
					}
					response, _ := orderedResponsesWire(layout.input, "", "")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					items := response["output"].([]any)
					items[len(items)-1].(map[string]any)["status"] = "incomplete"
					if req["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.incomplete", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), disabled)
				require.Equal(t, http.StatusOK, code)
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, body)
					require.Equal(t, "response.incomplete", events[len(events)-1]["type"])
					response = events[len(events)-1]["response"].(map[string]any)
				} else {
					require.NoError(t, json.Unmarshal(body, &response))
				}
				require.Equal(t, "incomplete", response["status"])
				items := response["output"].([]any)
				want := layout.want
				if disabled {
					want = layout.input
				}
				require.Equal(t, want, responsesItemOrder(t, items))
				for i, item := range items {
					wantStatus := "completed"
					if i == len(items)-1 {
						wantStatus = "incomplete"
					}
					require.Equal(t, wantStatus, item.(map[string]any)["status"])
				}
			})
		}
	}
}

func TestResponsesProductionEmptyMessageStatus(t *testing.T) {
	for _, status := range []string{"", "completed", "incomplete", "in_progress", "unknown"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", status, stream), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					response, _ := orderedResponsesWire([]string{""}, "", "")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					item := response["output"].([]any)[0].(map[string]any)
					if status == "" {
						delete(item, "status")
					} else {
						item["status"] = status
					}
					if stream {
						upstreamSSE(w, []map[string]any{{"type": "response.incomplete", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), true)
				if status == "in_progress" || status == "unknown" {
					requireResponsesIntegrityFailure(t, code, body, stream)
					return
				}
				require.Equal(t, http.StatusOK, code)
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					require.Equal(t, "response.incomplete", terminal["type"])
					response = terminal["response"].(map[string]any)
				} else {
					require.NoError(t, json.Unmarshal(body, &response))
				}
				require.Equal(t, "incomplete", response["status"])
				require.Empty(t, response["output"])
			})
		}
	}
}

func TestResponsesProductionChatFallbackOutcome(t *testing.T) {
	for _, reason := range []string{"length", "tool_calls", "empty"} {
		t.Run(reason, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				require.Equal(t, false, req["store"])
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/responses") {
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"error":{"message":"responses endpoint not supported"}}`)
					return
				}
				require.Equal(t, "/v1/chat/completions", r.URL.Path)
				if req["stream"] == true {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is not supported"}}`)
					return
				}
				finish := reason
				message := map[string]any{"role": "assistant", "content": ""}
				if reason == "empty" {
					finish = "length"
				} else {
					message["content"] = "before-call"
					message["tool_calls"] = []any{map[string]any{"id": "call-one", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}}}
				}
				response := map[string]any{"id": "chat-fixture", "object": "chat.completion", "created": 1700000000, "model": "test-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}}
				require.NoError(t, json.NewEncoder(w).Encode(response))
			}
			h, _ := setupResponsesHTTP(t, handler, false)
			issuer := newTestOIDCProvider(t)
			server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, OIDC: issuer.config()})
			token := issuer.issueToken(t, testOIDCTokenOptions{Namespace: "default"})
			code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			if reason == "length" {
				requireResponsesIntegrityFailure(t, code, body, true)
				require.NotContains(t, string(body), "call-one")
				return
			}
			require.Equal(t, http.StatusOK, code)
			events := parseResponsesSSE(t, body)
			if reason == "empty" {
				require.Equal(t, "response.incomplete", events[len(events)-1]["type"])
				require.Empty(t, events[len(events)-1]["response"].(map[string]any)["output"])
				return
			}
			require.Equal(t, "response.completed", events[len(events)-1]["type"])
			require.Contains(t, string(body), "call-one")
		})
	}
}

func TestResponsesProductionAddedFunctionReadiness(t *testing.T) {
	for _, status := range []string{"", "in_progress", "completed"} {
		for _, ending := range []string{"error", "unfinished", "terminal", "done", "prefix", "repeat-prefix", "snapshot-extension"} {
			if status == "completed" && (ending == "prefix" || ending == "repeat-prefix" || ending == "snapshot-extension") {
				continue
			}
			t.Run(status+"/"+ending, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					response, _ := orderedResponsesWire([]string{"call-one"}, "client_tool", `{"value":"full"}`)
					added := map[string]any{"id": "ordered-item-0", "type": "function_call", "call_id": "call-one", "name": "client_tool", "arguments": `{"value":"full"}`}
					if status != "" {
						added["status"] = status
					}
					if ending == "prefix" || ending == "repeat-prefix" || ending == "snapshot-extension" {
						added["arguments"] = `{"value":`
					}
					events := []map[string]any{{"type": "response.output_item.added", "output_index": 0, "item": added}}
					switch ending {
					case "error":
						events = append(events, map[string]any{"type": "error", "message": "provider interrupted"})
					case "unfinished":
						events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "ordered-item-0", "type": "function_call", "call_id": "call-one", "name": "client_tool", "arguments": `{"value":"full"}`, "status": "incomplete"}})
					default:
						if ending == "prefix" {
							events = append(events, map[string]any{"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "output_index": 0, "delta": `"full"}`})
						}
						if ending == "repeat-prefix" {
							events = append(events,
								map[string]any{"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "output_index": 0, "delta": `"full"`},
								map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added},
								map[string]any{"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "output_index": 0, "delta": `}`})
						}
						if ending == "snapshot-extension" {
							extended := map[string]any{}
							maps.Copy(extended, added)
							extended["arguments"] = `{"value":"full"}`
							events = append(events, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": extended})
						}
						if ending == "done" || ending == "prefix" || ending == "repeat-prefix" {
							events = append(events, map[string]any{"type": "response.function_call_arguments.done", "item_id": "ordered-item-0", "output_index": 0, "arguments": `{"value":"full"}`})
						}
						events = append(events, map[string]any{"type": "response.completed", "response": response})
					}
					upstreamSSE(w, events)
				})
				code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				if ending == "error" || ending == "unfinished" {
					requireResponsesIntegrityFailure(t, code, body, true)
					if status != "completed" {
						require.NotContains(t, string(body), "call-one")
					}
					return
				}
				require.Equal(t, http.StatusOK, code)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				require.Equal(t, "response.completed", terminal["type"])
				output := terminal["response"].(map[string]any)["output"].([]any)
				require.Equal(t, []string{"call-one"}, responsesItemOrder(t, output))
				require.JSONEq(t, `{"value":"full"}`, output[0].(map[string]any)["arguments"].(string))
			})
		}
	}
}

func TestResponsesProductionIndexlessMessageDone(t *testing.T) {
	for _, mode := range []string{"done-only", "matching", "changed-id", "changed-id-no-terminal", "added-then-indexed"} {
		t.Run(mode, func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				response, _ := orderedResponsesWire([]string{"same text"}, "", "")
				item := response["output"].([]any)[0].(map[string]any)
				events := []map[string]any{}
				if mode == "added-then-indexed" {
					events = append(events, map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": item["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
				}
				if mode != "done-only" {
					delta := map[string]any{"type": "response.output_text.delta", "item_id": item["id"], "content_index": 0, "delta": "same text"}
					if mode == "added-then-indexed" {
						delta["output_index"] = 0
					}
					events = append(events, delta)
				}
				doneItem := map[string]any{}
				maps.Copy(doneItem, item)
				if strings.HasPrefix(mode, "changed-id") {
					doneItem["id"] = "different-item"
				}
				if mode == "changed-id-no-terminal" {
					response["output"] = []any{}
				}
				events = append(events, map[string]any{"type": "response.output_item.done", "item": doneItem}, map[string]any{"type": "response.completed", "response": response})
				upstreamSSE(w, events)
			})
			code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			if strings.HasPrefix(mode, "changed-id") {
				requireResponsesIntegrityFailure(t, code, body, true)
				return
			}
			require.Equal(t, http.StatusOK, code)
			events := parseResponsesSSE(t, body)
			terminal := events[len(events)-1]
			require.Equal(t, "response.completed", terminal["type"])
			require.Equal(t, []string{"same text"}, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
		})
	}
}

func TestResponsesProductionCompletedTextBoundary(t *testing.T) {
	for _, kind := range []string{"no-output", "empty-item", "space", "newline"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", kind, stream), func(t *testing.T) {
				text := ""
				if kind == "space" {
					text = " "
				}
				if kind == "newline" {
					text = "\t\n"
				}
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					response, _ := orderedResponsesWire([]string{text}, "", "")
					if kind == "no-output" {
						response["output"] = []any{}
					}
					if stream {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), true)
				if text == "" {
					requireResponsesIntegrityFailure(t, code, body, stream)
					require.NotContains(t, string(body), "response.incomplete")
					return
				}
				require.Equal(t, http.StatusOK, code)
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					require.Equal(t, "response.completed", terminal["type"])
					response = terminal["response"].(map[string]any)
				} else {
					require.NoError(t, json.Unmarshal(body, &response))
				}
				require.Equal(t, "completed", response["status"])
				require.Equal(t, []string{text}, responsesItemOrder(t, response["output"].([]any)))
			})
		}
	}
}

func TestResponsesSentinelRetainsProviderSnapshot(t *testing.T) {
	original := &llm.CompletionResponse{Content: goalStateSentinel + "\nfirst second", StopReason: "length", OutputItems: []llm.AssistantOutputItem{{Content: goalStateSentinel + "\nfirst", Status: "completed"}, {Content: " second", Status: "incomplete"}}}
	beforeContent := original.Content
	beforeItems := append([]llm.AssistantOutputItem(nil), original.OutputItems...)
	result := stripResponsesGoalStateSentinel(original)
	require.Equal(t, beforeContent, original.Content)
	require.Equal(t, beforeItems, original.OutputItems)
	require.Equal(t, "first second", result.Content)
	require.Equal(t, []llm.AssistantOutputItem{{Content: "first", Status: "completed"}, {Content: " second", Status: "incomplete"}}, result.OutputItems)
}
