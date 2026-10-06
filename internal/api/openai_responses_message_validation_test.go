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

func TestResponsesProductionIncompleteMessageNeverCompletes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/disabled=%t", stream, disabled), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					response := responsesFixtureResponse("partial")
					response["output"].([]any)[0].(map[string]any)["status"] = "incomplete"
					if request["stream"] == true {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
				status, _, data := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, body, disabled)
				requireResponsesIntegrityFailure(t, status, data, stream)
			})
		}
	}
}

func TestResponsesProductionMessageRole(t *testing.T) {
	for _, role := range []struct {
		name        string
		value       any
		omit, valid bool
	}{
		{name: "assistant", value: "assistant", valid: true},
		{name: "omitted", omit: true, valid: true},
		{name: "user", value: "user"},
		{name: "system", value: "system"},
		{name: "empty", value: ""},
		{name: "null"},
	} {
		for _, stream := range []bool{false, true} {
			for _, disabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/disabled=%t", role.name, stream, disabled), func(t *testing.T) {
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						response := responsesFixtureResponse("Done. <!-- GOAL_STATE:SATISFIED -->")
						item := response["output"].([]any)[0].(map[string]any)
						if role.omit {
							delete(item, "role")
						} else {
							item["role"] = role.value
						}
						if request["stream"] == true {
							upstreamSSE(w, []map[string]any{{"type": "response.output_item.added", "output_index": 0, "item": item}, {"type": "response.completed", "response": response}})
						} else {
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(response))
						}
					})
					server.openaiHandler.config.MaxPrematureEndRetries = 0
					body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
					status, _, data := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, body, disabled)
					if !role.valid {
						requireResponsesIntegrityFailure(t, status, data, stream)
						return
					}
					require.Equal(t, http.StatusOK, status, string(data))
					if stream {
						events := parseResponsesSSE(t, data)
						require.Equal(t, "response.completed", events[len(events)-1]["type"])
					}
				})
			}
		}
	}
}
