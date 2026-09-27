/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesRejectsObjectProviderArguments(t *testing.T) {
	for _, source := range []string{"snapshot", "item-added", "item-done"} {
		for _, object := range []bool{true, false} {
			for _, coordinator := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/object=%t/coordinator=%t/stream=%t", source, object, coordinator, stream), func(t *testing.T) {
						path := filepath.Join(responsesToolTempDir(t), "result.txt")
						args := map[string]string{"path": path, "content": "written"}
						encoded, err := json.Marshal(args)
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
							if source != "snapshot" && request["stream"] != true {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(http.StatusBadRequest)
								_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
								return
							}
							response, _ := orderedResponsesWire([]string{"call-one"}, "file_write", string(encoded))
							item := response["output"].([]any)[0].(map[string]any)
							if object {
								item["arguments"] = args
							}
							if request["stream"] != true {
								w.Header().Set("Content-Type", "application/json")
								require.NoError(t, json.NewEncoder(w).Encode(response))
								return
							}
							events := []map[string]any{}
							if source != "snapshot" {
								kind := "response.output_item.added"
								if source == "item-done" {
									kind = "response.output_item.done"
								}
								events = append(events, map[string]any{"type": kind, "output_index": 0, "item": item})
								response["output"] = []any{}
							}
							upstreamSSE(w, append(events, map[string]any{"type": "response.completed", "response": response}))
						})
						request := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"write the file","tools":[{"type":"function","name":"file_write"}]}`, stream)
						status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, request, !coordinator)
						if object {
							require.NoFileExists(t, path, "object-valued provider arguments must not execute")
							requireResponsesIntegrityFailure(t, status, body, stream)
							require.NotContains(t, string(body), "call-one", "malformed arguments must not expose a function call")
							return
						}
						require.Equal(t, http.StatusOK, status)
						if coordinator {
							require.FileExists(t, path)
						} else {
							require.Contains(t, string(body), "call-one")
						}
					})
				}
			}
		}
	}
}
