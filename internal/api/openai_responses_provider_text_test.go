/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesRejectsMalformedProviderText(t *testing.T) {
	for _, value := range []string{"missing", "null", "number", "boolean", "empty"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", value, stream), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					response := responsesFixtureResponse("valid first part")
					message := response["output"].([]any)[0].(map[string]any)
					part := map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}}
					if value == "null" {
						part["text"] = nil
					}
					if value == "number" {
						part["text"] = 123
					}
					if value == "boolean" {
						part["text"] = false
					}
					if value == "empty" {
						part["text"] = ""
					}
					message["content"] = append(message["content"].([]any), part)
					if request["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"input":"hello","stream":%t}`, stream), true)
				want := "completed"
				if value != "empty" {
					want = "failed"
				}
				if !stream {
					t.Logf("HTTP=%d expected=%s", status, want)
					if want == "failed" {
						require.Equal(t, http.StatusBadGateway, status)
					} else {
						require.Equal(t, http.StatusOK, status)
					}
					return
				}
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				last := events[len(events)-1]
				t.Logf("terminal=%s expected=response.%s", last["type"], want)
				require.Equal(t, "response."+want, last["type"])
			})
		}
	}
}

func TestResponsesRejectsMalformedProviderContentEvents(t *testing.T) {
	for _, eventType := range []string{"response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done"} {
		for _, value := range []string{"missing", "null", "number", "boolean", "empty"} {
			t.Run(eventType+"/"+value, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					part := map[string]any{"type": "output_text"}
					switch value {
					case "null":
						part["text"] = nil
					case "number":
						part["text"] = 123
					case "boolean":
						part["text"] = false
					case "empty":
						part["text"] = ""
					}
					response := responsesFixtureResponse("valid first part")
					message := response["output"].([]any)[0].(map[string]any)
					message["content"] = append(message["content"].([]any), map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}})
					textEvent := map[string]any{"type": eventType, "output_index": 0, "content_index": 1, "item_id": message["id"]}
					switch eventType {
					case "response.output_text.delta", "response.output_text.done":
						field := "text"
						if eventType == "response.output_text.delta" {
							field = "delta"
						}
						if text, present := part["text"]; present {
							textEvent[field] = text
						}
					default:
						textEvent["part"] = part
					}
					upstreamSSE(w, []map[string]any{
						{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": message["id"], "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
						{"type": "response.content_part.done", "output_index": 0, "content_index": 0, "item_id": message["id"], "part": map[string]any{"type": "output_text", "text": "valid first part"}},
						textEvent,
						{"type": "response.completed", "response": response},
					})
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"input":"hello","stream":true}`, true)
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, body)
				want := "response.failed"
				if value == "empty" {
					want = "response.completed"
				}
				require.Equal(t, want, events[len(events)-1]["type"])
			})
		}
	}
}
