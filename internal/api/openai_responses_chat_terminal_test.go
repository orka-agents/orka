/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the mounted, authenticated Responses route with the real OpenAI
// provider, including its unsupported-Responses probe and Chat SSE fallback.
func TestResponsesProductionChatTerminalToolCalls(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, outcome := range []string{"length", "missing_finish", "transport_error", "refusal", "malformed_arguments", "tool_calls", "stop"} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, outcome), func(t *testing.T) {
				var chatRequests atomic.Int32
				h, _ := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/responses" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					require.Equal(t, "/v1/chat/completions", r.URL.Path)
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, true, request["stream"])
					require.Equal(t, false, request["store"])
					chatRequests.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if outcome == "transport_error" {
						// Force a real unexpected EOF after the call deltas.
						w.Header().Set("Content-Length", "1000000")
					}
					emit := func(delta map[string]any, finish string) {
						chunk := map[string]any{"id": "chat-terminal", "object": "chat.completion.chunk", "model": "test-model", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
						data, err := json.Marshal(chunk)
						require.NoError(t, err)
						_, err = fmt.Fprintf(w, "data: %s\n\n", data)
						require.NoError(t, err)
					}
					emit(map[string]any{"content": "Before tools."}, "")
					arguments := `{}`
					if outcome == "malformed_arguments" {
						arguments = `{"unfinished":`
					}
					if legacy {
						emit(map[string]any{"function_call": map[string]any{"name": "client_tool", "arguments": arguments}}, "")
					} else {
						for i, args := range []string{`{}`, arguments} {
							emit(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": fmt.Sprintf("call-%d", i), "type": "function", "function": map[string]any{"name": "client_tool", "arguments": args}}}}, "")
						}
					}
					// The SDK marks the final modern call finished here, before
					// the upstream has supplied any terminal outcome.
					emit(map[string]any{}, "")
					finish := "tool_calls"
					if legacy {
						finish = "function_call"
					}
					switch outcome {
					case "length", "stop":
						finish = outcome
					case "missing_finish":
						finish = ""
					case "transport_error":
						return
					case "refusal":
						emit(map[string]any{"refusal": "private refusal detail"}, "")
					}
					emit(map[string]any{}, finish)
					_, err := fmt.Fprint(w, "data: {\"id\":\"chat-terminal\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\ndata: [DONE]\n\n")
					require.NoError(t, err)
				}, false)
				issuer := newTestOIDCProvider(t)
				server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, OIDC: issuer.config()})
				token := issuer.issueToken(t, testOIDCTokenOptions{Namespace: "default"})
				status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello","tools":[{"type":"function","name":"client_tool","parameters":{"type":"object","properties":{}}}]}`, true)
				require.Equal(t, http.StatusOK, status, string(body))
				require.EqualValues(t, 1, chatRequests.Load())
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				require.NotContains(t, string(body), "private refusal detail")
				if outcome != "tool_calls" && outcome != "stop" {
					require.Equal(t, "response.failed", terminal["type"], string(body))
					require.NotContains(t, string(body), "response.completed")
					require.NotContains(t, string(body), "response.incomplete")
					require.NotContains(t, string(body), `"type":"function_call"`, "no executable function item may precede a failed terminal outcome")
					require.NotContains(t, string(body), "response.function_call_arguments")
					if outcome == "refusal" {
						require.Contains(t, string(body), "refusals are not supported")
					}
					return
				}
				require.Equal(t, "response.completed", terminal["type"], string(body))
				response := terminal["response"].(map[string]any)
				wantOrder := []string{"Before tools.", "call-0", "call-1"}
				if legacy {
					wantOrder = []string{"Before tools.", "chat-terminal_function_call"}
				}
				require.Equal(t, wantOrder, responsesItemOrder(t, response["output"].([]any)))
				usage := response["usage"].(map[string]any)
				require.EqualValues(t, 7, usage["input_tokens"])
				require.EqualValues(t, 3, usage["output_tokens"])
				require.EqualValues(t, 2, usage["input_tokens_details"].(map[string]any)["cached_tokens"])
			})
		}
	}
}
