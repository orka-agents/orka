/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent recovery fixtures retain literal wire values.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesProductionTrailingEmptyRecovery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, snapshot := range []string{"explicit", "empty", "omitted"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, snapshot), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					if request["stream"] != true {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
						return
					}
					response, _ := orderedResponsesWire([]string{"answer", ""}, "", "")
					output := response["output"].([]any)
					first := maps.Clone(output[0].(map[string]any))
					first["status"] = "in_progress"
					last := output[1].(map[string]any)
					last["status"] = "incomplete"
					last["content"] = []any{}
					events := []map[string]any{
						{"type": "response.output_item.added", "output_index": 0, "item": first},
						{"type": "response.output_item.added", "output_index": 1, "item": last},
					}
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					switch snapshot {
					case "empty":
						response["output"] = []any{}
					case "omitted":
						delete(response, "output")
					}
					upstreamSSE(w, append(events, map[string]any{"type": "response.incomplete", "response": response}))
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), true)
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
				output := response["output"].([]any)
				require.Equal(t, []string{"answer"}, responsesItemOrder(t, output))
				require.Equal(t, "completed", output[0].(map[string]any)["status"])
			})
		}
	}
}

func TestResponsesProductionClientJSONRecoversStreamingRequired(t *testing.T) {
	for _, outcome := range []string{"text", "tool", "empty-budget", "partial-budget", "failed", "refusal", "unterminated", "other-error"} {
		t.Run(outcome, func(t *testing.T) {
			var calls atomic.Int32
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, false, request["store"])
				require.Equal(t, float64(0), request["temperature"])
				require.Equal(t, float64(37), request["max_output_tokens"])
				require.Equal(t, "Be precise.", request["instructions"])
				require.Equal(t, "json_object", request["text"].(map[string]any)["format"].(map[string]any)["type"])
				tools := request["tools"].([]any)
				require.Len(t, tools, 1)
				require.Equal(t, "client_tool", tools[0].(map[string]any)["name"])
				if request["stream"] != true {
					message := "streaming is required"
					if outcome == "other-error" {
						message = "unsupported model"
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}}))
					return
				}
				response := responsesFixtureResponse(`{"ok":true}`)
				switch outcome {
				case "tool":
					response, _ = orderedResponsesWire([]string{"call-one"}, "client_tool", `{}`)
				case "empty-budget", "partial-budget":
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					if outcome == "empty-budget" {
						response["output"] = []any{}
					} else {
						response["output"].([]any)[0].(map[string]any)["status"] = "incomplete"
					}
				case "failed":
					response["status"] = "failed"
				case "refusal":
					response["output"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "refusal", "refusal": "cannot comply"}}
				case "unterminated":
					upstreamSSE(w, []map[string]any{{"type": "response.output_text.delta", "output_index": 0, "delta": "partial"}})
					return
				}
				upstreamSSE(w, []map[string]any{{"type": "response." + response["status"].(string), "response": response}})
			})
			status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"input":"hello","temperature":0,"max_output_tokens":37,"instructions":"Be precise.","text":{"format":{"type":"json_object"}},"tools":[{"type":"function","name":"client_tool","parameters":{"type":"object","properties":{}}}]}`, true)
			wantCalls := 2
			if outcome == "other-error" {
				wantCalls = 1
			}
			require.EqualValues(t, wantCalls, calls.Load())
			if outcome == "refusal" {
				require.Equal(t, http.StatusUnprocessableEntity, status)
				return
			}
			if outcome == "failed" || outcome == "unterminated" || outcome == "other-error" {
				requireResponsesIntegrityFailure(t, status, body, false)
				return
			}
			require.Equal(t, http.StatusOK, status, string(body))
			var response map[string]any
			require.NoError(t, json.Unmarshal(body, &response))
			wantStatus := "completed"
			if outcome == "empty-budget" || outcome == "partial-budget" {
				wantStatus = "incomplete"
				require.Equal(t, "max_output_tokens", response["incomplete_details"].(map[string]any)["reason"])
			}
			require.Equal(t, wantStatus, response["status"])
			require.EqualValues(t, 10, response["usage"].(map[string]any)["total_tokens"])
			output := response["output"].([]any)
			switch outcome {
			case "empty-budget":
				require.Empty(t, output)
			case "tool":
				require.Equal(t, []string{"call-one"}, responsesItemOrder(t, output))
				require.Equal(t, "{}", output[0].(map[string]any)["arguments"])
			default:
				require.Equal(t, []string{`{"ok":true}`}, responsesItemOrder(t, output))
				require.Equal(t, wantStatus, output[0].(map[string]any)["status"])
			}
		})
	}
}

func TestResponsesProductionFinalFunctionArgumentsPresence(t *testing.T) {
	for _, source := range []string{"arguments-done", "repeated-arguments-done", "item-done", "completed-added", "completed-added-only"} {
		for _, snapshot := range []string{"empty", "null", "omitted", "matching"} {
			for _, mode := range []string{"json", "sse", "coordinator"} {
				t.Run(source+"/"+snapshot+"/"+mode, func(t *testing.T) {
					path := filepath.Join(responsesToolTempDir(t), "result.txt")
					arguments, err := json.Marshal(map[string]string{"path": path, "content": "written"})
					require.NoError(t, err)
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						for _, raw := range request["input"].([]any) {
							if raw.(map[string]any)["type"] == "function_call_output" {
								upstreamResponse(w, goalStateSentinel+"\nfinished writing")
								return
							}
						}
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						response, _ := orderedResponsesWire([]string{"call-one"}, "file_write", string(arguments))
						item := response["output"].([]any)[0].(map[string]any)
						delete(item, "arguments")
						done := map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": item["id"]}
						target := item
						argumentEvent := source == "arguments-done" || source == "repeated-arguments-done"
						if argumentEvent {
							target = done
						}
						switch snapshot {
						case "empty":
							target["arguments"] = ""
						case "null":
							target["arguments"] = nil
						case "matching":
							target["arguments"] = string(arguments)
						}
						events := []map[string]any{{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": item["id"], "delta": string(arguments)}}
						if source == "repeated-arguments-done" {
							events[0]["delta"] = string(arguments[:len(arguments)/2])
							events = append(events, map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": item["id"], "arguments": string(arguments)})
						}
						if argumentEvent {
							events = append(events, done)
						}
						kind := "response.output_item.done"
						if source == "completed-added" || source == "completed-added-only" {
							kind = "response.output_item.added"
						}
						events = append(events, map[string]any{"type": kind, "output_index": 0, "item": item})
						// A later done event may omit arguments without retracting an explicit final snapshot.
						if source == "completed-added" {
							omitted := maps.Clone(item)
							delete(omitted, "arguments")
							events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": omitted})
						}
						response["output"] = []any{}
						upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
					})
					stream := mode == "sse"
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"write the file","tools":[{"type":"function","name":"file_write"}]}`, stream), mode != "coordinator")
					if snapshot == "empty" || snapshot == "null" {
						require.NoFileExists(t, path, "contradictory final arguments must not reach execution")
						requireResponsesIntegrityFailure(t, status, body, stream)
						require.NotContains(t, string(body), "call-one", "contradictory final arguments must not expose an executable call")
						return
					}
					require.Equal(t, http.StatusOK, status, string(body))
					if mode == "coordinator" {
						data, err := os.ReadFile(path)
						require.NoError(t, err)
						require.Equal(t, "written", string(data))
						require.Contains(t, string(body), "finished writing")
					} else {
						require.NoFileExists(t, path, "client-managed functions must remain client-managed during recovery")
						var response map[string]any
						if stream {
							events := parseResponsesSSE(t, body)
							terminal := events[len(events)-1]
							require.Equal(t, "response.completed", terminal["type"])
							response = terminal["response"].(map[string]any)
						} else {
							require.NoError(t, json.Unmarshal(body, &response))
						}
						output := response["output"].([]any)
						require.Equal(t, []string{"call-one"}, responsesItemOrder(t, output))
						require.JSONEq(t, string(arguments), output[0].(map[string]any)["arguments"].(string))
					}
				})
			}
		}
	}
}

func TestResponsesProductionRecoveredFunctionRequiresObject(t *testing.T) {
	for _, arguments := range []string{"[]", "null", "42", `"text"`, "", "{", "valid"} {
		for _, source := range []string{"added", "done"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%q/stream=%t", source, arguments, stream), func(t *testing.T) {
					path := filepath.Join(responsesToolTempDir(t), "read.txt")
					require.NoError(t, os.WriteFile(path, []byte("fixture content"), 0600))
					var processed atomic.Bool
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						for _, raw := range request["input"].([]any) {
							if item := raw.(map[string]any); item["type"] == "function_call_output" {
								processed.Store(true)
								if arguments == "valid" {
									require.Contains(t, item["output"], "fixture content")
								}
								upstreamResponse(w, goalStateSentinel+"\nfinished reading")
								return
							}
						}
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						args := arguments
						if arguments == "valid" {
							data, err := json.Marshal(map[string]string{"path": path})
							require.NoError(t, err)
							args = string(data)
						}
						response, _ := orderedResponsesWire([]string{"call-one"}, "file_read", args)
						item := response["output"].([]any)[0]
						response["output"] = []any{}
						upstreamSSE(w, []map[string]any{
							{"type": "response.output_item." + source, "output_index": 0, "item": item},
							{"type": "response.completed", "response": response},
						})
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read the file"}`, stream), false)
					if arguments != "valid" {
						require.False(t, processed.Load(), "malformed arguments must fail before coordinator tool processing")
						requireResponsesIntegrityFailure(t, status, body, stream)
						return
					}
					require.True(t, processed.Load())
					require.Equal(t, http.StatusOK, status)
					require.Contains(t, string(body), "finished reading")
				})
			}
		}
	}
}

func TestResponsesProductionChatFunctionIntegrity(t *testing.T) {
	for _, arguments := range []string{"[]", "null", "42", "missing-id", "missing-name", "duplicate-id", "valid"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/stream=%t", arguments, stream), func(t *testing.T) {
				path := filepath.Join(responsesToolTempDir(t), "read.txt")
				require.NoError(t, os.WriteFile(path, []byte("fixture content"), 0600))
				var processed atomic.Bool
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/v1/responses" {
						w.WriteHeader(http.StatusNotFound)
						_, _ = fmt.Fprint(w, `{"error":{"message":"responses endpoint not supported"}}`)
						return
					}
					require.Equal(t, "/v1/chat/completions", r.URL.Path)
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					for _, raw := range request["messages"].([]any) {
						if item := raw.(map[string]any); item["role"] == "tool" {
							processed.Store(true)
							if arguments == "valid" {
								require.Contains(t, item["content"], "fixture content")
							}
						}
					}
					message := map[string]any{"role": "assistant", "content": goalStateSentinel + "\nfinished reading"}
					finish := "stop"
					if !processed.Load() {
						args := arguments
						if arguments == "valid" || arguments == "missing-id" || arguments == "missing-name" || arguments == "duplicate-id" {
							data, err := json.Marshal(map[string]string{"path": path})
							require.NoError(t, err)
							args = string(data)
						}
						message["content"] = ""
						function := map[string]any{"name": "file_read", "arguments": args}
						call := map[string]any{"id": "call-one", "type": "function", "function": function}
						message["tool_calls"] = []any{call}
						switch arguments {
						case "missing-id":
							delete(call, "id")
						case "missing-name":
							delete(function, "name")
						case "duplicate-id":
							message["tool_calls"] = []any{call, call}
						}
						finish = "tool_calls"
					}
					response := map[string]any{"id": "chat-fixture", "object": "chat.completion", "created": 1700000000, "model": "test-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}}
					require.NoError(t, json.NewEncoder(w).Encode(response))
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read the file"}`, stream), false)
				if arguments != "valid" {
					require.False(t, processed.Load(), "malformed fallback calls must fail before coordinator tool processing")
					requireResponsesIntegrityFailure(t, status, body, stream)
					return
				}
				require.True(t, processed.Load())
				require.Equal(t, http.StatusOK, status)
				require.Contains(t, string(body), "finished reading")
			})
		}
	}
}

func invalidResponsesMetadataFixture(source, field, value string) bool {
	invalid := (value == "empty" || value == "null") && field != "id" && field != "status"
	if value == "empty" && (field == "delta" || (field == "arguments" && (source == "initial-added" || source == "in-progress-added"))) {
		return false // An empty argument prefix does not clear accumulated arguments.
	}
	return invalid
}

func TestResponsesProductionFunctionMetadataPresence(t *testing.T) {
	for _, tc := range []struct{ source, field string }{
		{"item-done", "name"}, {"item-done", "call_id"}, {"item-done", "id"}, {"item-done", "status"}, {"item-done", "output_index"},
		{"completed-added", "name"}, {"completed-added", "call_id"}, {"completed-added", "id"}, {"completed-added", "output_index"},
		{"in-progress-added", "name"}, {"in-progress-added", "call_id"}, {"in-progress-added", "id"}, {"in-progress-added", "output_index"},
		{"initial-added", "name"}, {"initial-added", "call_id"},
		{"in-progress-added", "arguments"}, {"initial-added", "arguments"},
		{"arguments-delta", "name"}, {"arguments-delta", "item_id"}, {"arguments-delta", "output_index"},
		{"arguments-delta", "delta"},
		{"arguments-done", "name"}, {"arguments-done", "item_id"}, {"arguments-done", "output_index"},
		{"terminal", "id"}, {"terminal", "status"},
		{"terminal", "name"}, {"terminal", "call_id"}, {"terminal", "arguments"},
	} {
		for _, value := range []string{"empty", "null", "omitted", "matching"} {
			for _, mode := range []string{"json", "sse", "coordinator"} {
				t.Run(tc.source+"/"+tc.field+"/"+value+"/"+mode, func(t *testing.T) {
					path := filepath.Join(responsesToolTempDir(t), "result.txt")
					arguments, err := json.Marshal(map[string]string{"path": path, "content": "written"})
					require.NoError(t, err)
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						for _, raw := range request["input"].([]any) {
							if raw.(map[string]any)["type"] == "function_call_output" {
								upstreamResponse(w, goalStateSentinel+"\nfinished writing")
								return
							}
						}
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						response, _ := orderedResponsesWire([]string{"call-one"}, "file_write", string(arguments))
						item := response["output"].([]any)[0].(map[string]any)
						added := maps.Clone(item)
						added["status"] = "in_progress"
						events := []map[string]any{{"type": "response.output_item.added", "output_index": 0, "item": added}}
						final := map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}
						target := item
						switch tc.source {
						case "completed-added":
							final["type"] = "response.output_item.added"
						case "in-progress-added", "initial-added":
							final["type"] = "response.output_item.added"
							target = maps.Clone(item)
							target["status"], target["arguments"] = "in_progress", ""
							final["item"] = target
							if tc.source == "initial-added" {
								events = nil
							}
						case "arguments-delta":
							final = map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": item["id"], "name": item["name"], "delta": ""}
							target = final
						case "arguments-done":
							final = map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": item["id"], "name": item["name"], "arguments": string(arguments)}
							target = final
						}
						if tc.field == "output_index" {
							target = final
						}
						switch value {
						case "empty":
							target[tc.field] = ""
						case "null":
							target[tc.field] = nil
						case "omitted":
							delete(target, tc.field)
						case "matching":
							if tc.field == "arguments" {
								target[tc.field] = string(arguments)
							}
						}
						if tc.source != "terminal" {
							events = append(events, final)
							if final["type"] != "response.output_item.done" {
								last := maps.Clone(item)
								if tc.source == "in-progress-added" || tc.source == "arguments-delta" {
									delete(last, "name")
									delete(last, "call_id")
									if tc.field == "arguments" {
										delete(last, "arguments")
									}
								}
								events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": last})
							}
							response["output"] = []any{}
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
					})
					stream := mode == "sse"
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"write the file","tools":[{"type":"function","name":"file_write"}]}`, stream), mode != "coordinator")
					// Item IDs and statuses retain their optional metadata normalization.
					if invalidResponsesMetadataFixture(tc.source, tc.field, value) {
						require.NoFileExists(t, path, "explicitly invalid metadata must fail before execution")
						requireResponsesIntegrityFailure(t, status, body, stream)
						require.NotContains(t, string(body), "call-one", "invalid metadata must not expose an executable call")
						return
					}
					require.Equal(t, http.StatusOK, status, string(body))
					if mode == "coordinator" {
						data, err := os.ReadFile(path)
						require.NoError(t, err)
						require.Equal(t, "written", string(data))
						require.Contains(t, string(body), "finished writing")
					} else {
						require.NoFileExists(t, path)
						require.Contains(t, string(body), "call-one")
						if stream {
							events := parseResponsesSSE(t, body)
							require.Equal(t, "response.completed", events[len(events)-1]["type"])
						}
					}
				})
			}
		}
	}
}

func TestResponsesProductionTerminalFunctionRecovery(t *testing.T) {
	for _, field := range []string{"name", "call_id", "arguments", "all"} {
		for _, prior := range []bool{false, true} {
			for _, mode := range []string{"json", "sse", "coordinator"} {
				t.Run(fmt.Sprintf("%s/prior=%t/%s", field, prior, mode), func(t *testing.T) {
					path := filepath.Join(responsesToolTempDir(t), "result.txt")
					arguments, err := json.Marshal(map[string]string{"path": path, "content": "written"})
					require.NoError(t, err)
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						for _, raw := range request["input"].([]any) {
							if raw.(map[string]any)["type"] == "function_call_output" {
								upstreamResponse(w, goalStateSentinel+"\nfinished writing")
								return
							}
						}
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						response, _ := orderedResponsesWire([]string{"call-one"}, "file_write", string(arguments))
						item := response["output"].([]any)[0].(map[string]any)
						var events []map[string]any
						if prior {
							events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": maps.Clone(item)})
						}
						for _, key := range []string{"name", "call_id", "arguments"} {
							if field == key || field == "all" {
								delete(item, key)
							}
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
					})
					stream := mode == "sse"
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"write the file","tools":[{"type":"function","name":"file_write"}]}`, stream), mode != "coordinator")
					if !prior {
						require.NoFileExists(t, path)
						requireResponsesIntegrityFailure(t, status, body, stream)
						require.NotContains(t, string(body), "call-one")
						return
					}
					require.Equal(t, http.StatusOK, status, string(body))
					if mode == "coordinator" {
						data, err := os.ReadFile(path)
						require.NoError(t, err)
						require.Equal(t, "written", string(data))
						require.Contains(t, string(body), "finished writing")
						return
					}
					require.NoFileExists(t, path)
					var response map[string]any
					if stream {
						events := parseResponsesSSE(t, body)
						require.Equal(t, "response.completed", events[len(events)-1]["type"])
						response = events[len(events)-1]["response"].(map[string]any)
					} else {
						require.NoError(t, json.Unmarshal(body, &response))
					}
					output := response["output"].([]any)
					require.Equal(t, []string{"call-one"}, responsesItemOrder(t, output))
					require.Equal(t, "file_write", output[0].(map[string]any)["name"])
					require.JSONEq(t, string(arguments), output[0].(map[string]any)["arguments"].(string))
				})
			}
		}
	}
}

func TestResponsesProductionOmittedMessageStatusRecovery(t *testing.T) {
	for _, layout := range []string{"indexed", "unindexed", "two-messages"} {
		for _, snapshot := range []string{"explicit", "without-status", "empty"} {
			for _, mode := range []string{"client-json", "client-sse", "coordinator-json", "coordinator-sse"} {
				t.Run(layout+"/"+snapshot+"/"+mode, func(t *testing.T) {
					want := []string{"partial"}
					if layout == "two-messages" {
						want = []string{"before", "partial"}
					}
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						response, _ := orderedResponsesWire(want, "", "")
						output := response["output"].([]any)
						var events []map[string]any
						for i, raw := range output {
							item := raw.(map[string]any)
							done := maps.Clone(item)
							delete(done, "status")
							event := map[string]any{"type": "response.output_item.done", "output_index": i, "item": done}
							if layout == "unindexed" {
								delete(event, "output_index")
							}
							events = append(events, event)
						}
						last := output[len(output)-1].(map[string]any)
						last["status"] = "incomplete"
						switch snapshot {
						case "without-status":
							delete(last, "status")
						case "empty":
							response["output"] = []any{}
						}
						response["status"] = "incomplete"
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						upstreamSSE(w, append(events, map[string]any{"type": "response.incomplete", "response": response}))
					})
					stream := strings.HasSuffix(mode, "sse")
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), strings.HasPrefix(mode, "client"))
					require.Equal(t, http.StatusOK, status, string(body))
					var response map[string]any
					if stream {
						events := parseResponsesSSE(t, body)
						require.Equal(t, "response.incomplete", events[len(events)-1]["type"])
						response = events[len(events)-1]["response"].(map[string]any)
						var statuses []string
						for _, event := range events {
							if event["type"] == "response.output_item.done" {
								statuses = append(statuses, event["item"].(map[string]any)["status"].(string))
							}
						}
						require.Len(t, statuses, len(want))
						require.Equal(t, "incomplete", statuses[len(statuses)-1])
					} else {
						require.NoError(t, json.Unmarshal(body, &response))
					}
					require.Equal(t, "incomplete", response["status"])
					output := response["output"].([]any)
					require.Equal(t, want, responsesItemOrder(t, output))
					for i, raw := range output {
						status := "completed"
						if i == len(output)-1 {
							status = "incomplete"
						}
						require.Equal(t, status, raw.(map[string]any)["status"])
					}
				})
			}
		}
	}
}

func TestResponsesProductionLateTextIndexRecovery(t *testing.T) {
	for _, identity := range []string{"matching", "anonymous", "omitted", "conflicting", "wrong-index"} {
		for _, ending := range []string{"completed", "incomplete"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", identity, ending, stream), func(t *testing.T) {
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						response, _ := orderedResponsesWire([]string{"partial"}, "", "")
						response["status"] = ending
						item := response["output"].([]any)[0].(map[string]any)
						item["status"] = ending
						if ending == "incomplete" {
							response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						}
						delta := snapshotTextEvent("response.output_text.delta", "partial", 0, false)
						if identity == "anonymous" {
							delete(delta, "item_id")
						}
						final := maps.Clone(item)
						done := map[string]any{"type": "response.output_item.done", "output_index": 0, "item": final}
						switch identity {
						case "omitted":
							delete(final, "id")
						case "conflicting":
							final["id"] = "another-item"
						case "wrong-index":
							done["output_index"] = 1
						}
						upstreamSSE(w, []map[string]any{delta, done, {"type": "response." + ending, "response": response}})
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), true)
					if identity == "conflicting" || identity == "wrong-index" {
						requireResponsesIntegrityFailure(t, status, body, stream)
						return
					}
					require.Equal(t, http.StatusOK, status, string(body))
					var response map[string]any
					if stream {
						events := parseResponsesSSE(t, body)
						require.Equal(t, "response."+ending, events[len(events)-1]["type"])
						response = events[len(events)-1]["response"].(map[string]any)
					} else {
						require.NoError(t, json.Unmarshal(body, &response))
					}
					output := response["output"].([]any)
					require.Equal(t, []string{"partial"}, responsesItemOrder(t, output))
					require.Equal(t, ending, output[0].(map[string]any)["status"])
				})
			}
		}
	}
}
