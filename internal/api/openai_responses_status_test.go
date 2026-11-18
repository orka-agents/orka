/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures retain literal statuses.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesProductionPerMessageStatus(t *testing.T) {
	for _, statuses := range [][2]string{{"completed", "incomplete"}, {"incomplete", "completed"}, {"completed", "completed"}, {"completed", ""}, {"", ""}, {"", "completed"}, {"", "incomplete"}, {"completed", "in_progress"}} {
		for _, mode := range []string{"json", "sse", "coordinator-json", "coordinator-sse", "stream-required", "stream-required-sse", "stream-unsupported"} {
			t.Run(fmt.Sprintf("%s/%s/%s", mode, statuses[0], statuses[1]), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					if ((mode == "stream-required" || mode == "stream-required-sse") && request["stream"] != true) || (mode == "stream-unsupported" && request["stream"] == true) {
						message := "streaming is required"
						if mode == "stream-unsupported" {
							message = "streaming is not supported"
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}}))
						return
					}
					response, _ := orderedResponsesWire([]string{"first", "second"}, "", "")
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					for i, item := range response["output"].([]any) {
						if statuses[i] == "" {
							delete(item.(map[string]any), "status")
						} else {
							item.(map[string]any)["status"] = statuses[i]
						}
					}
					if request["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.incomplete", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				stream := mode == "sse" || mode == "coordinator-sse" || mode == "stream-required-sse" || mode == "stream-unsupported"
				disabled := mode == "json" || mode == "sse" || mode == "stream-unsupported"
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
				status, _, data := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, body, disabled)
				if statuses[1] == "in_progress" {
					requireResponsesIntegrityFailure(t, status, data, stream)
					return
				}
				want := statuses
				if want[0] == "" {
					want[0] = "completed"
				}
				if want[1] == "" {
					want[1] = "incomplete"
				}
				require.Equal(t, http.StatusOK, status)
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, data)
					terminal := events[len(events)-1]
					require.Equal(t, "response.incomplete", terminal["type"])
					response = terminal["response"].(map[string]any)
					for _, event := range events {
						if event["type"] == "response.output_item.done" {
							index := int(event["output_index"].(float64))
							require.Equal(t, want[index], event["item"].(map[string]any)["status"])
						}
					}
				} else {
					require.NoError(t, json.Unmarshal(data, &response))
				}
				require.Equal(t, "incomplete", response["status"])
				items := response["output"].([]any)
				require.Equal(t, []string{"first", "second"}, responsesItemOrder(t, items))
				for i, item := range items {
					require.Equal(t, want[i], item.(map[string]any)["status"])
				}
			})
		}
	}
}

func TestResponsesProductionAddedItemStatuses(t *testing.T) {
	for _, layout := range []string{"indexed", "unindexed", "late-index", "anonymous", "multi-message"} {
		for _, itemStatus := range []string{"completed", "incomplete"} {
			for _, ending := range []string{"completed", "incomplete"} {
				for _, snapshot := range []string{"explicit", "without-status", "empty", "omitted"} {
					t.Run(layout+"/"+itemStatus+"/"+ending+"/"+snapshot, func(t *testing.T) {
						order := []string{"initial text"}
						if layout == "multi-message" {
							order = append(order, "later text")
						}
						server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
							response, _ := orderedResponsesWire(order, "", "")
							output := response["output"].([]any)
							first := output[0].(map[string]any)
							first["status"] = itemStatus
							added := maps.Clone(first)
							added["content"] = []any{map[string]any{"type": "output_text", "text": "initial"}}
							event := map[string]any{"type": "response.output_item.added", "item": added}
							delta := snapshotTextEvent("response.output_text.delta", " text", 0, layout != "unindexed" && layout != "anonymous")
							if layout == "late-index" || layout == "anonymous" {
								added["content"] = []any{}
								delta["delta"] = "initial text"
								if layout == "anonymous" {
									delete(delta, "item_id")
								}
							} else if layout != "unindexed" {
								event["output_index"] = 0
							}
							events := []map[string]any{event, delta}
							if layout == "multi-message" {
								events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 1, "item": output[1]})
							}
							response["status"] = ending
							if ending == "incomplete" {
								response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
							}
							switch snapshot {
							case "without-status":
								delete(first, "status")
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
						if itemStatus == "incomplete" && ending == "completed" {
							require.Equal(t, "response.failed", terminal["type"])
							return
						}
						require.Equal(t, "response."+ending, terminal["type"])
						output := terminal["response"].(map[string]any)["output"].([]any)
						require.Equal(t, order, responsesItemOrder(t, output), "observing an item status must not close its text parts early")
						require.Equal(t, itemStatus, output[0].(map[string]any)["status"])
						if layout == "multi-message" {
							require.Equal(t, "completed", output[1].(map[string]any)["status"])
						}
					})
				}
			}
		}
	}
}

func TestResponsesProductionEmptyPredecessorRecovery(t *testing.T) {
	for _, order := range [][]string{{"", "answer"}, {"", "", "answer"}, {"before", "", "answer"}, {"", "call-one"}} {
		for _, snapshot := range []string{"explicit", "empty", "omitted"} {
			t.Run(fmt.Sprintf("%v/%s", order, snapshot), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					response, _ := orderedResponsesWire(order, "client_tool", "{}")
					output := response["output"].([]any)
					var events []map[string]any
					for index, raw := range output {
						item := raw.(map[string]any)
						if order[index] == "" {
							item["content"] = []any{}
						}
						kind := "response.output_item.added"
						if index == len(output)-1 {
							kind = "response.output_item.done"
						}
						events = append(events, map[string]any{"type": kind, "output_index": index, "item": item})
					}
					switch snapshot {
					case "empty":
						response["output"] = []any{}
					case "omitted":
						delete(response, "output")
					}
					upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello","tools":[{"type":"function","name":"client_tool"}]}`, true)
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				require.Equal(t, "response.completed", terminal["type"])
				var want []string
				for _, text := range order {
					if text != "" {
						want = append(want, text)
					}
				}
				output := terminal["response"].(map[string]any)["output"].([]any)
				require.Equal(t, want, responsesItemOrder(t, output))
				for _, item := range output {
					require.Equal(t, "completed", item.(map[string]any)["status"])
				}
			})
		}
	}
}

func TestResponsesProductionAnonymousTextIdentity(t *testing.T) {
	for _, mode := range []string{"metadata-after-text", "multiple-before", "multiple-after", "different-text-id"} {
		t.Run(mode, func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				response, _ := orderedResponsesWire([]string{"answer"}, "", "")
				item := maps.Clone(response["output"].([]any)[0].(map[string]any))
				item["content"] = []any{}
				other := maps.Clone(item)
				other["id"] = "another-message"
				added := map[string]any{"type": "response.output_item.added", "item": item}
				another := map[string]any{"type": "response.output_item.added", "item": other}
				delta := snapshotTextEvent("response.output_text.delta", "answer", 0, false)
				delete(delta, "item_id")
				events := []map[string]any{delta, added}
				switch mode {
				case "multiple-before":
					events = []map[string]any{added, another, delta}
				case "multiple-after":
					events = append(events, another)
				case "different-text-id":
					delta["item_id"] = "another-message"
					events = []map[string]any{added, delta}
				}
				response["status"] = "incomplete"
				response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				delete(response, "output")
				upstreamSSE(w, append(events, map[string]any{"type": "response.incomplete", "response": response}))
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			if mode != "metadata-after-text" {
				requireResponsesIntegrityFailure(t, status, body, true)
				return
			}
			require.Equal(t, http.StatusOK, status)
			events := parseResponsesSSE(t, body)
			terminal := events[len(events)-1]
			require.Equal(t, "response.incomplete", terminal["type"])
			output := terminal["response"].(map[string]any)["output"].([]any)
			require.Equal(t, []string{"answer"}, responsesItemOrder(t, output))
			require.Equal(t, "completed", output[0].(map[string]any)["status"])
		})
	}
}

func TestResponsesProductionAddedIncompleteCannotReleaseCalls(t *testing.T) {
	for _, mode := range []string{"message-first", "call-queued-first", "function"} {
		t.Run(mode, func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				order := []string{"partial", "call-one"}
				if mode == "function" {
					order = []string{"call-one"}
				}
				response, _ := orderedResponsesWire(order, "client_tool", "{}")
				output := response["output"].([]any)
				added := maps.Clone(output[0].(map[string]any))
				added["status"] = "incomplete"
				start := map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added}
				call := map[string]any{"type": "response.output_item.done", "output_index": len(order) - 1, "item": output[len(order)-1]}
				events := []map[string]any{start, call}
				if mode == "call-queued-first" {
					events = []map[string]any{call, start}
				}
				response["output"] = []any{}
				upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			requireResponsesIntegrityFailure(t, status, body, true)
			for _, event := range parseResponsesSSE(t, body) {
				if item, ok := event["item"].(map[string]any); ok {
					require.NotEqual(t, "function_call", item["type"], "initial incomplete status must prevent executable output")
				}
			}
		})
	}
}
