/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep independent wire fixtures literal.
package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func snapshotTextEvent(kind, text string, part int, indexed bool) map[string]any {
	event := map[string]any{"type": kind, "item_id": "ordered-item-0", "content_index": part}
	if indexed {
		event["output_index"] = 0
	}
	switch kind {
	case "response.output_text.delta":
		event["delta"] = text
	case "response.output_text.done":
		event["text"] = text
	default:
		event["part"] = map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
	}
	return event
}

func TestResponsesProductionPartSnapshots(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, kind := range []string{"response.output_text.done", "response.content_part.done"} {
			for _, mode := range []string{"done-only", "prefix", "duplicates", "two-parts", "reverse-parts", "implicit-part", "conflict", "empty-conflict", "late-delta", "late-snapshot", "same-aggregate-conflict", "content-gap"} {
				t.Run(fmt.Sprintf("indexed=%t/%s/%s", indexed, kind, mode), func(t *testing.T) {
					want := "answer"
					if strings.Contains(mode, "parts") || mode == "same-aggregate-conflict" {
						want = "abc"
					}
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
						response, _ := orderedResponsesWire([]string{want}, "", "")
						item := response["output"].([]any)[0].(map[string]any)
						event := func(k, value string, part int) map[string]any { return snapshotTextEvent(k, value, part, indexed) }
						events := []map[string]any{}
						switch mode {
						case "done-only":
							events = append(events, event(kind, want, 0))
							delete(response, "output") // A compatible terminal carries only outcome/usage.
						case "prefix", "implicit-part":
							events = append(events, event("response.output_text.delta", "ans", 0), event(kind, want, 0))
							if mode == "implicit-part" {
								for _, e := range events {
									delete(e, "content_index")
								}
							}
						case "duplicates":
							events = append(events, event(kind, want, 0), event(kind, want, 0), event("response.output_text.done", want, 0), event("response.content_part.done", want, 0))
						case "two-parts", "reverse-parts", "same-aggregate-conflict":
							// Build parts independently of whether the event carries text or a part.
							item["content"] = []any{snapshotTextEvent("response.content_part.done", "a", 0, indexed)["part"], snapshotTextEvent("response.content_part.done", "bc", 1, indexed)["part"]}
							events = append(events, event(kind, "a", 0), event(kind, "bc", 1))
							if mode == "reverse-parts" {
								events[0], events[1] = events[1], events[0]
							}
							if mode == "same-aggregate-conflict" {
								item["content"] = []any{snapshotTextEvent("response.content_part.done", "ab", 0, indexed)["part"], snapshotTextEvent("response.content_part.done", "c", 1, indexed)["part"]}
							}
						case "conflict", "empty-conflict":
							final := "wrong"
							if mode == "empty-conflict" {
								final = ""
							}
							events = append(events, event("response.output_text.delta", want, 0), event(kind, final, 0))
						case "content-gap":
							events = append(events, event(kind, "a", 0), event(kind, "bc", 2))
							delete(response, "output")
						case "late-delta", "late-snapshot":
							events = append(events, event("response.output_text.delta", "ans", 0), event(kind, "ans", 0))
							if mode == "late-delta" {
								events = append(events, event("response.output_text.delta", "wer", 0))
							} else {
								events = append(events, event(kind, want, 0))
							}
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
					if strings.Contains(mode, "conflict") || strings.HasPrefix(mode, "late-") || mode == "content-gap" {
						requireResponsesIntegrityFailure(t, status, body, true)
						return
					}
					require.Equal(t, http.StatusOK, status)
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					require.Equal(t, "response.completed", terminal["type"])
					require.Equal(t, []string{want}, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
					var emitted strings.Builder
					for _, event := range events {
						if event["type"] == "response.output_text.delta" {
							emitted.WriteString(event["delta"].(string))
						}
					}
					require.Equal(t, want, emitted.String(), "part snapshots must not duplicate or reorder text")
				})
			}
		}
	}
}

func TestResponsesProductionDuplicateItemStatus(t *testing.T) {
	for _, layout := range []string{"indexed", "unindexed", "queued"} {
		for _, mode := range []string{"identical", "omitted", "terminal-omitted", "conflicting", "invalid", "terminal-conflicting", "terminal-invalid"} {
			t.Run(layout+"/"+mode, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					order := []string{"answer"}
					if layout == "queued" {
						order = []string{"before", "answer"}
					}
					response, _ := orderedResponsesWire(order, "", "")
					last := len(order) - 1
					item := response["output"].([]any)[last].(map[string]any)
					done := map[string]any{"type": "response.output_item.done", "output_index": last, "item": maps.Clone(item)}
					duplicate := map[string]any{"type": "response.output_item.done", "output_index": last, "item": maps.Clone(item)}
					copy := duplicate["item"].(map[string]any)
					if mode == "omitted" {
						delete(copy, "status")
					}
					if mode == "conflicting" {
						copy["status"] = "incomplete"
					}
					if mode == "invalid" {
						copy["status"] = "in_progress"
					}
					events := []map[string]any{}
					if layout == "unindexed" {
						delete(done, "output_index")
						delete(duplicate, "output_index")
						events = append(events, snapshotTextEvent("response.output_text.delta", "answer", 0, false))
					}
					events = append(events, done)
					if !strings.HasPrefix(mode, "terminal-") {
						events = append(events, duplicate)
					}
					if layout == "queued" {
						events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": response["output"].([]any)[0]})
					}
					terminalType := "response.completed"
					if mode == "terminal-omitted" {
						delete(item, "status")
						response["status"] = "incomplete"
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						terminalType = "response.incomplete"
					}
					if mode == "terminal-conflicting" {
						item["status"] = "incomplete"
						response["status"] = "incomplete"
						response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						terminalType = "response.incomplete"
					}
					if mode == "terminal-invalid" {
						item["status"] = "in_progress"
					}
					upstreamSSE(w, append(events, map[string]any{"type": terminalType, "response": response}))
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				if strings.HasSuffix(mode, "conflicting") || strings.HasSuffix(mode, "invalid") {
					requireResponsesIntegrityFailure(t, status, body, true)
					return
				}
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				want := "response.completed"
				if mode == "terminal-omitted" {
					want = "response.incomplete"
				}
				require.Equal(t, want, terminal["type"])
				for _, item := range terminal["response"].(map[string]any)["output"].([]any) {
					require.Equal(t, "completed", item.(map[string]any)["status"])
				}
			})
		}
	}
}

func TestResponsesProductionIncompleteRefusal(t *testing.T) {
	for _, mode := range []string{"json", "sse", "coordinator-json", "stream-required", "stream-unsupported", "pending-refusal"} {
		for _, overall := range []string{"completed", "incomplete"} {
			t.Run(mode+"/"+overall, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var input map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
					require.Equal(t, false, input["store"])
					if (mode == "stream-required" && input["stream"] != true) || (mode == "stream-unsupported" && input["stream"] == true) {
						message := "streaming is required"
						if mode == "stream-unsupported" {
							message = "streaming is not supported"
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message}}))
						return
					}
					response := responsesFixtureResponse("private text with refusal")
					response["status"] = overall
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					item := response["output"].([]any)[0].(map[string]any)
					item["status"] = "incomplete"
					item["content"] = append(item["content"].([]any), map[string]any{"type": "refusal", "refusal": "private refusal detail"})
					if input["stream"] == true {
						events := []map[string]any{}
						if mode == "pending-refusal" {
							e := snapshotTextEvent("response.output_text.done", "pending text", 0, true)
							e["output_index"] = 1
							e["item_id"] = "pending-item"
							events = append(events, e)
						}
						upstreamSSE(w, append(events, map[string]any{"type": "response." + overall, "response": response}))
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				stream := mode == "sse" || mode == "stream-unsupported" || mode == "pending-refusal"
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream), mode != "coordinator-json" && mode != "stream-required")
				require.NotContains(t, string(body), "private refusal detail")
				if stream {
					require.NotContains(t, string(body), "private text with refusal")
					require.NotContains(t, string(body), "pending text")
					requireResponsesIntegrityFailure(t, status, body, true)
					events := parseResponsesSSE(t, body)
					require.Equal(t, "unsupported_provider_outcome", events[len(events)-2]["code"])
					require.Equal(t, "server_error", events[len(events)-1]["response"].(map[string]any)["error"].(map[string]any)["code"])
				} else {
					require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
				}
				require.Contains(t, string(body), "refusals are not supported")
			})
		}
	}
}

func TestResponsesProductionCachedStreamNotFound(t *testing.T) {
	var completions, streams atomic.Int32
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, false, input["store"])
		require.EqualValues(t, 37, input["max_output_tokens"])
		if input["stream"] == true {
			streams.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"message":"route unavailable"}}`)
			return
		}
		completions.Add(1)
		upstreamResponse(w, "fallback answer")
	})
	url := listenResponsesApp(t, server.app)
	status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"max_output_tokens":37,"input":"probe"}`, true)
	require.Equal(t, http.StatusOK, status, string(body))
	status, _, body = productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"max_output_tokens":37,"stream":true,"input":"hello"}`, true)
	require.Equal(t, http.StatusOK, status)
	events := parseResponsesSSE(t, body)
	require.Equal(t, "response.completed", events[len(events)-1]["type"])
	require.Equal(t, []string{"fallback answer"}, responsesItemOrder(t, events[len(events)-1]["response"].(map[string]any)["output"].([]any)))
	require.EqualValues(t, 1, streams.Load())
	require.EqualValues(t, 2, completions.Load(), "cached probe followed by exactly one completion retry")
}
