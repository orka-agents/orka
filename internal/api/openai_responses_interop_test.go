/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
)

// This opt-in test uses an installed, pinned Agent Framework dependency. The
// client, Orka Fiber handler, provider SDK and upstream fixture communicate over
// real TCP. Neither the framework nor its tool/history management is mocked.
func TestAgentFrameworkResponsesInterop(t *testing.T) {
	python := os.Getenv("ORKA_RESPONSES_INTEROP_PYTHON")
	if python == "" {
		t.Skip("install scripts/fixtures/agent-framework-responses/requirements.txt and set ORKA_RESPONSES_INTEROP_PYTHON")
	}
	clientScript, err := filepath.Abs("../../scripts/fixtures/agent-framework-responses/client.py")
	require.NoError(t, err)
	for _, mode := range []string{"coordinator", "client"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				file := filepath.Join(responsesToolTempDir(t), "fixture.txt")
				require.NoError(t, os.WriteFile(file, []byte("server fixture value"), 0600))
				var calls atomic.Int32
				_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					require.NotContains(t, request, "previous_response_id")
					require.NotContains(t, request, "conversation")
					round := calls.Add(1)
					response := newResponsesResponse(&ResponsesRequest{}, "test-model")
					completion := &llm.CompletionResponse{StopReason: "completed", InputTokens: 10, OutputTokens: 4}
					switch round {
					case 1:
						name, args := "client_echo", `{"text":"fixture value"}`
						if mode == "coordinator" {
							name = "file_read"
							encoded, _ := json.Marshal(map[string]string{"path": file})
							args = string(encoded)
						}
						found := false
						for _, tool := range request["tools"].([]any) {
							if tool.(map[string]any)["name"] == name {
								found = true
							}
							if mode == "coordinator" {
								require.NotEqual(t, "client_echo", tool.(map[string]any)["name"])
							}
						}
						require.True(t, found, "required tool not advertised")
						completion.ToolCalls = []llm.ToolCall{{ID: "framework-call", Name: name, Arguments: json.RawMessage(args)}}
					case 2:
						input := request["input"].([]any)
						found := false
						for _, raw := range input {
							item := raw.(map[string]any)
							if item["type"] == "function_call_output" {
								found = true
								require.Equal(t, "framework-call", item["call_id"])
								if mode == "coordinator" {
									require.Contains(t, item["output"], "server fixture value")
								} else {
									require.Contains(t, item["output"], "client result: fixture value")
								}
							}
						}
						require.True(t, found, "tool result missing")
						completion.Content = "fixture final"
					case 3:
						encoded, _ := json.Marshal(request["input"])
						require.Contains(t, string(encoded), "fixture final", "follow-up lost prior assistant output")
						require.Contains(t, string(encoded), "Follow up using our earlier result.")
						completion.Content = "follow-up verified"
					default:
						t.Errorf("unexpected upstream call %d", round)
						w.WriteHeader(400)
						return
					}
					if mode == "coordinator" && completion.Content != "" {
						completion.Content = goalStateSentinel + "\n" + completion.Content
					}
					require.NoError(t, response.setCompletion(completion))
					if request["stream"] != true {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
						return
					}
					// Emit real upstream Responses SSE, including call item identity and chunks.
					events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": "upstream-response", "status": "in_progress", "output": []any{}}}}
					for index, item := range response.Output {
						if item.Type == "message" {
							events = append(events, map[string]any{"type": "response.output_text.delta", "delta": item.Content[0].Text})
							continue
						}
						added := item
						empty := ""
						added.Arguments = &empty
						added.Status = "in_progress"
						events = append(events, map[string]any{"type": "response.output_item.added", "output_index": index, "item": added}, map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": item.ID, "arguments": *item.Arguments})
					}
					events = append(events, map[string]any{"type": "response.completed", "response": response})
					upstreamSSE(w, events)
				})
				url := listenResponsesApp(t, app)
				args := []string{clientScript, "--base-url", url + "/openai/v1", "--mode", mode}
				if stream {
					args = append(args, "--stream")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, python, args...)
				// Do not forward ambient cloud/provider credentials to this deterministic test.
				command.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONUNBUFFERED=1", "NO_PROXY=127.0.0.1,localhost"}
				output, err := command.CombinedOutput()
				require.NoError(t, err, string(output))
				require.Contains(t, string(output), "PASS mode="+mode)
				require.EqualValues(t, 3, calls.Load())
				t.Log(strings.TrimSpace(string(output)))
			})
		}
	}
}
