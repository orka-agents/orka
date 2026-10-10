/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures retain literal protocol fields.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesProductionFunctionArgumentSnapshotPrefix(t *testing.T) {
	const arguments = `{"value":"full"}`
	for _, snapshot := range []string{"arguments", "item", "terminal"} {
		for _, fragment := range []string{"", `{"value":`, arguments, `{"value":"other"}`, arguments + " "} {
			t.Run(snapshot+"/"+fragment, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					response, original := orderedResponsesWire([]string{"call-one"}, "client_tool", arguments)
					events := []map[string]any{original[0], {"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "output_index": 0, "delta": fragment[:len(fragment)/2]}, {"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "output_index": 0, "delta": fragment[len(fragment)/2:]}}
					switch snapshot {
					case "arguments":
						events = append(events, map[string]any{"type": "response.function_call_arguments.done", "item_id": "ordered-item-0", "output_index": 0, "arguments": arguments})
					case "item":
						events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": response["output"].([]any)[0]})
					}
					upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				if fragment == `{"value":"other"}` || fragment == arguments+" " {
					requireResponsesIntegrityFailure(t, status, body, true)
					return
				}
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				require.Equal(t, "response.completed", terminal["type"])
				output := terminal["response"].(map[string]any)["output"].([]any)
				require.Equal(t, []string{"call-one"}, responsesItemOrder(t, output))
				require.Equal(t, arguments, output[0].(map[string]any)["arguments"])
			})
		}
	}
}

// A failed terminal must not follow executable output from an unfinished call,
// including when an unsupported stream falls back to non-streaming completion.
func TestResponsesProductionTerminalFunctionStatus(t *testing.T) {
	for _, status := range []string{"", "completed", "incomplete", "in_progress"} {
		for _, mode := range []string{"json", "sse", "stream-unsupported"} {
			t.Run(mode+"/"+status, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					w.Header().Set("Content-Type", "application/json")
					if request["stream"] == true && mode == "stream-unsupported" {
						w.WriteHeader(http.StatusBadRequest)
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "streaming is not supported"}}))
						return
					}
					response, _ := orderedResponsesWire([]string{"call-one"}, "client_tool", "{}")
					item := response["output"].([]any)[0].(map[string]any)
					if status == "" {
						delete(item, "status")
					} else {
						item["status"] = status
					}
					if request["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
					} else {
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				stream := mode != "json"
				request := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
				code, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, request, true)
				if status == "incomplete" || status == "in_progress" {
					requireResponsesIntegrityFailure(t, code, body, stream)
					require.NotContains(t, string(body), "call-one", "unfinished calls must not be exposed before the terminal error")
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
				require.Equal(t, "completed", response["output"].([]any)[0].(map[string]any)["status"])
			})
		}
	}
}

func TestResponsesProductionPartialFunctionStateMerge(t *testing.T) {
	const full = `{"value":"full"}`
	const prefix = `{"value":`
	const conflict = `{"value":"other"}`
	for _, fragments := range [][2]string{{full, full}, {prefix, full}, {full, prefix}, {conflict, full}, {full, conflict}} {
		for _, bridge := range []string{"added", "done-omitted", "index-done"} {
			if bridge == "index-done" && fragments[1] == prefix {
				continue // A completed argument record must contain a full JSON object.
			}
			t.Run(fmt.Sprintf("%s/%q/%q", bridge, fragments[0], fragments[1]), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					response, _ := orderedResponsesWire([]string{"call-one"}, "client_tool", full)
					item := map[string]any{"id": "ordered-item-0", "type": "function_call", "call_id": "call-one", "name": "client_tool", "status": "in_progress"}
					eventType := "response.output_item.added"
					if bridge == "done-omitted" {
						eventType = "response.output_item.done"
						item["status"] = "completed"
					}
					indexed := map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": fragments[1]}
					if bridge == "index-done" {
						indexed = map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "arguments": fragments[1]}
					}
					upstreamSSE(w, []map[string]any{
						{"type": "response.function_call_arguments.delta", "item_id": "ordered-item-0", "delta": fragments[0]},
						indexed,
						{"type": eventType, "output_index": 0, "item": item},
						{"type": "response.completed", "response": response},
					})
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				if fragments[0] == conflict || fragments[1] == conflict {
					requireResponsesIntegrityFailure(t, status, body, true)
					require.NotContains(t, string(body), "call-one", "conflicting states must fail before exposing the call")
					return
				}
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				require.Equal(t, "response.completed", terminal["type"])
				output := terminal["response"].(map[string]any)["output"].([]any)
				require.Equal(t, []string{"call-one"}, responsesItemOrder(t, output))
				require.Equal(t, full, output[0].(map[string]any)["arguments"])
			})
		}
	}
}

func TestResponsesProductionOmittedTerminalMessageStatus(t *testing.T) {
	for _, order := range [][]string{{"first"}, {"first", "second"}, {"first", ""}} {
		for _, mode := range []string{"json", "sse", "coordinator-json", "stream-required", "stream-unsupported"} {
			t.Run(fmt.Sprintf("%s/%q", mode, order), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					if (mode == "stream-required" && request["stream"] != true) || (mode == "stream-unsupported" && request["stream"] == true) {
						message := "streaming is required"
						if mode == "stream-unsupported" {
							message = "streaming is not supported"
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}}))
						return
					}
					response, _ := orderedResponsesWire(order, "", "")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					for _, raw := range response["output"].([]any) {
						delete(raw.(map[string]any), "status")
					}
					if request["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.incomplete", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				stream := mode == "sse" || mode == "stream-unsupported"
				disabled := mode != "coordinator-json" && mode != "stream-required"
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), disabled)
				require.Equal(t, http.StatusOK, status)
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
				var expected []string
				for _, text := range order {
					if text != "" {
						expected = append(expected, text)
					}
				}
				output := response["output"].([]any)
				require.Equal(t, expected, responsesItemOrder(t, output))
				for i, raw := range output {
					want := "completed"
					if i == len(order)-1 {
						want = "incomplete"
					}
					require.Equal(t, want, raw.(map[string]any)["status"])
				}
			})
		}
	}
}
