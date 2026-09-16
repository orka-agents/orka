/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures retain literal statuses.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesProductionPerMessageStatus(t *testing.T) {
	for _, statuses := range [][2]string{{"completed", "incomplete"}, {"incomplete", "completed"}, {"completed", "completed"}, {"completed", ""}, {"", ""}, {"", "completed"}, {"", "incomplete"}, {"completed", "in_progress"}} {
		for _, mode := range []string{"json", "sse", "coordinator-json", "stream-required", "stream-unsupported"} {
			t.Run(fmt.Sprintf("%s/%s/%s", mode, statuses[0], statuses[1]), func(t *testing.T) {
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
				stream := mode == "sse" || mode == "stream-unsupported"
				disabled := mode != "coordinator-json" && mode != "stream-required"
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
