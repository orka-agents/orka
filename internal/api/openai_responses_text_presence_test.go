/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// The official SDK parameter types require text to be a string. Missing/null
// text must not be mistaken for an explicitly supplied, valid empty string.
func TestResponsesRejectsMissingTextParts(t *testing.T) {
	for _, location := range []string{"user", "assistant", "tool_result"} {
		for _, value := range []string{"missing", "null", "empty"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", location, value, stream), func(t *testing.T) {
					typ := "input_text"
					if location == "assistant" {
						typ = "output_text"
					}
					part := map[string]any{"type": typ}
					switch value {
					case "null":
						part["text"] = nil
					case "empty":
						part["text"] = ""
					}
					var input []any
					if location == "tool_result" {
						input = []any{map[string]any{"type": "function_call", "call_id": "call-shape", "name": "client_tool", "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": "call-shape", "output": []any{part}}}
					} else {
						input = []any{map[string]any{"role": location, "content": []any{part}}}
					}
					request, err := json.Marshal(map[string]any{"model": "fixture/test-model", "store": false, "stream": stream, "input": input})
					require.NoError(t, err)
					var calls atomic.Int32
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						var body map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						response := responsesFixtureResponse("explicit empty text is valid")
						if body["stream"] == true {
							upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
						} else {
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(response))
						}
					})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, string(request), true)
					t.Logf("HTTP status=%d upstream_requests=%d", status, calls.Load())
					if value == "empty" {
						require.Equal(t, http.StatusOK, status)
						require.EqualValues(t, 1, calls.Load())
						require.Contains(t, string(body), "explicit empty text is valid")
						if stream {
							require.Contains(t, string(body), "response.completed")
						}
						return
					}
					require.Equal(t, http.StatusBadRequest, status)
					require.Zero(t, calls.Load())
					require.True(t, strings.Contains(string(body), "invalid_request_error"))
				})
			}
		}
	}
}
