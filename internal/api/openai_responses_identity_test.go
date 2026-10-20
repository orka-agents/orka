/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures retain explicit protocol fields.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesProductionFunctionArgumentIdentity(t *testing.T) {
	for _, eventType := range []string{"response.function_call_arguments.delta", "response.function_call_arguments.done"} {
		for _, identity := range []string{"item", "index", "missing"} {
			t.Run(eventType+"/"+identity, func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					_, events := orderedResponsesWire([]string{"call-one"}, "client_tool", `{"value":"full"}`)
					for _, event := range events {
						if event["type"] == eventType {
							if identity == "missing" {
								field := "delta"
								if eventType == "response.function_call_arguments.done" {
									field = "arguments"
								}
								event[field] = `{"value":"lost fragment"}`
							}
							if identity != "index" {
								delete(event, "output_index")
							}
							if identity != "item" {
								delete(event, "item_id")
							}
						}
					}
					upstreamSSE(w, events)
				})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				if identity == "missing" {
					requireResponsesIntegrityFailure(t, status, body, true)
					return
				}
				require.Equal(t, http.StatusOK, status)
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

// After an HTTP200 stream has delivered metadata, provider/event failures do
// not become retry-eligible HTTP capability failures, even if their payload
// mentions an unsupported stream and a 404 status. Exercise both SDK branches.
func TestResponsesProductionMetadataErrorDoesNotRetry(t *testing.T) {
	for _, mode := range []string{"event", "nested-error", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			var completions, streams atomic.Int32
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, false, request["store"])
				if request["stream"] != true {
					completions.Add(1)
					upstreamResponse(w, "non-streaming answer")
					return
				}
				streams.Add(1)
				events := []map[string]any{{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "pending-call", "type": "function_call", "call_id": "call-one", "name": "client_tool", "arguments": "", "status": "in_progress"}}}
				switch mode {
				case "event":
					events = append(events, map[string]any{"type": "error", "message": "404 streaming is not supported", "status_code": 404})
				case "nested-error":
					events = append(events, map[string]any{"type": "error", "error": map[string]any{"message": "404 streaming is not supported", "status_code": 404}})
				case "truncated":
					w.Header().Set("Content-Length", "4096")
				}
				upstreamSSE(w, events)
			})
			url := listenResponsesApp(t, server.app)
			status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"max_output_tokens":37,"input":"probe"}`, true)
			require.Equal(t, http.StatusOK, status, string(body))
			status, _, body = productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"max_output_tokens":37,"stream":true,"input":"hello"}`, true)
			requireResponsesIntegrityFailure(t, status, body, true)
			require.NotContains(t, string(body), "non-streaming answer")
			require.EqualValues(t, 1, streams.Load())
			require.EqualValues(t, 1, completions.Load(), fmt.Sprintf("%s: only the initial successful probe, no completion retry", mode))
		})
	}
}
