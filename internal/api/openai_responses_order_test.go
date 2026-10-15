/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures keep protocol fields visible.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/llm"

	"github.com/stretchr/testify/require"
)

// Each string denotes one upstream output item, including adjacent messages.
// Streaming starts items in index order, then completes them in reverse order.
func orderedResponsesWire(order []string, name, arguments string) (map[string]any, []map[string]any) {
	response := responsesFixtureResponse("")
	output := make([]any, 0, len(order))
	events := []map[string]any{}
	for index, value := range order {
		item := map[string]any{"id": fmt.Sprintf("ordered-item-%d", index), "status": "completed"}
		added := map[string]any{"id": item["id"], "status": "in_progress"}
		if strings.HasPrefix(value, "call-") {
			item["type"], item["call_id"], item["name"], item["arguments"] = "function_call", value, name, arguments
			added["type"], added["call_id"], added["name"], added["arguments"] = "function_call", value, name, ""
		} else {
			item["type"], item["role"] = "message", "assistant"
			item["content"] = []any{map[string]any{"type": "output_text", "text": value, "annotations": []any{}, "logprobs": []any{}}}
			added["type"], added["role"], added["content"] = "message", "assistant", []any{}
		}
		output = append(output, item)
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": index, "item": added})
	}
	for index, o := range slices.Backward(order) {
		item := output[index].(map[string]any)
		if item["type"] == "function_call" {
			events = append(events,
				map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "item_id": item["id"], "delta": arguments},
				map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": item["id"], "arguments": arguments})
		} else {
			part := item["content"].([]any)[0]
			events = append(events,
				map[string]any{"type": "response.content_part.added", "output_index": index, "item_id": item["id"], "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}},
				map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": item["id"], "content_index": 0, "delta": o},
				map[string]any{"type": "response.output_text.done", "output_index": index, "item_id": item["id"], "content_index": 0, "text": o},
				map[string]any{"type": "response.content_part.done", "output_index": index, "item_id": item["id"], "content_index": 0, "part": part})
		}
		events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	response["output"] = output
	events = append(events, map[string]any{"type": "response.completed", "response": response})
	for i, event := range events {
		event["sequence_number"] = i
	}
	return response, events
}

func responsesItemOrder(t *testing.T, items []any) []string {
	t.Helper()
	var order []string
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "function_call" {
			order = append(order, item["call_id"].(string))
		} else if item["role"] == "assistant" {
			switch content := item["content"].(type) {
			case string:
				order = append(order, content)
			case []any:
				require.Len(t, content, 1)
				order = append(order, content[0].(map[string]any)["text"].(string))
			default:
				t.Fatalf("unexpected assistant content type %T", content)
			}
		}
	}
	return order
}

func TestResponsesProductionPreservesMixedOutputOrder(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, order := range [][]string{{"call-one", "after"}, {"before", "call-one", "after"}, {"before", "after"}, {"call-one", "call-two"}} {
			t.Run(fmt.Sprintf("stream=%t/%v", stream, order), func(t *testing.T) {
				captured := make(chan map[string]any, 2)
				var rounds atomic.Int32
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					captured <- request
					if rounds.Add(1) == 2 {
						upstreamResponse(w, "continued in order")
						return
					}
					response, events := orderedResponsesWire(order, "client_tool", `{}`)
					if request["stream"] == true {
						upstreamSSE(w, events)
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				url := listenResponsesApp(t, server.app)
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"tools","tools":[{"type":"function","name":"client_tool"}]}`, stream)
				status, _, data := productionResponsesRequest(t, url, token, body, true)
				require.Equal(t, 200, status, string(data))
				var response map[string]any
				if stream {
					events := parseResponsesSSE(t, data)
					terminal := events[len(events)-1]
					require.Equal(t, "response.completed", terminal["type"])
					response = terminal["response"].(map[string]any)
					var added []any
					for _, event := range events {
						if event["type"] == "response.output_item.added" {
							require.EqualValues(t, len(added), event["output_index"])
							added = append(added, event["item"])
						}
						if event["type"] == "response.output_item.done" {
							index := int(event["output_index"].(float64))
							require.Less(t, index, len(added))
							require.Equal(t, added[index].(map[string]any)["id"], event["item"].(map[string]any)["id"])
							added[index] = event["item"]
						}
					}
					require.Equal(t, response["output"], added, "SSE indices and final snapshot must describe the same items")
				} else {
					require.NoError(t, json.Unmarshal(data, &response))
				}
				output := response["output"].([]any)
				require.Equal(t, order, responsesItemOrder(t, output))
				history := append([]any{map[string]any{"role": "user", "content": "tools"}}, output...)
				for _, value := range order {
					if strings.HasPrefix(value, "call-") {
						history = append(history, map[string]any{"type": "function_call_output", "call_id": value, "output": "client result"})
					}
				}
				history = append(history, map[string]any{"role": "user", "content": "continue"})
				followup, err := json.Marshal(map[string]any{"model": "fixture/test-model", "store": false, "input": history})
				require.NoError(t, err)
				status, _, data = productionResponsesRequest(t, url, token, string(followup), true)
				require.Equal(t, 200, status, string(data))
				require.Contains(t, string(data), "continued in order")
				first, second := <-captured, <-captured
				require.Equal(t, false, first["store"])
				require.Equal(t, false, second["store"])
				require.Equal(t, order, responsesItemOrder(t, second["input"].([]any)))
				require.EqualValues(t, 2, rounds.Load())
			})
		}
	}
}

func TestResponsesProductionOrderedCoordinatorContinuation(t *testing.T) {
	for _, clientStream := range []bool{false, true} {
		for _, streamRequired := range []bool{false, true} {
			t.Run(fmt.Sprintf("clientStream=%t/streamRequired=%t", clientStream, streamRequired), func(t *testing.T) {
				dir, err := os.MkdirTemp("/tmp", "orka-responses-ordered-")
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
				path := filepath.Join(dir, "ordered-tool.txt")
				require.NoError(t, os.WriteFile(path, []byte("ordered file result"), 0600))
				arguments, err := json.Marshal(map[string]any{"path": path})
				require.NoError(t, err)
				order := []string{"before", "call-one", "after"}
				captured := make(chan map[string]any, 2)
				var rounds atomic.Int32
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					if streamRequired && request["stream"] != true {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
						return
					}
					captured <- request
					response, events := orderedResponsesWire(order, "file_read", string(arguments))
					if rounds.Add(1) == 2 {
						response, events = orderedResponsesWire([]string{goalStateSentinel + "\nfinished"}, "", "")
					}
					if streamRequired {
						upstreamSSE(w, events)
					} else {
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(response))
					}
				})
				url := listenResponsesApp(t, server.app)
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read the file"}`, clientStream)
				status, _, data := productionResponsesRequest(t, url, token, body, false)
				require.Equal(t, 200, status, string(data))
				require.Contains(t, string(data), "finished")
				require.NotContains(t, string(data), goalStateSentinel)
				first, second := <-captured, <-captured
				require.Equal(t, false, first["store"])
				require.Equal(t, false, second["store"])
				input := second["input"].([]any)
				require.Equal(t, order, responsesItemOrder(t, input))
				result := input[len(input)-1].(map[string]any)
				require.Equal(t, "function_call_output", result["type"])
				require.Equal(t, "call-one", result["call_id"])
				require.Contains(t, result["output"], "ordered file result")
				require.EqualValues(t, 2, rounds.Load())
			})
		}
	}
}

func TestResponsesProductionTerminalOnlyCallsKeepOrder(t *testing.T) {
	order := []string{"call-one", "middle", "call-two"}
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		response, _ := orderedResponsesWire(order, "client_tool", `{}`)
		upstreamSSE(w, []map[string]any{
			{"type": "response.output_text.delta", "output_index": 1, "item_id": "ordered-item-1", "content_index": 0, "delta": "middle", "sequence_number": 0},
			{"type": "response.completed", "response": response, "sequence_number": 1},
		})
	})
	url := listenResponsesApp(t, server.app)
	status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"tools"}`, true)
	require.Equal(t, 200, status, string(data))
	events := parseResponsesSSE(t, data)
	terminal := events[len(events)-1]
	require.Equal(t, "response.completed", terminal["type"])
	require.Equal(t, order, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
}

func TestResponsesProductionOrderedStreamRejectsLateOutput(t *testing.T) {
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		_, events := orderedResponsesWire([]string{"before"}, "", "")
		terminal := events[len(events)-1]
		events = append(events[:len(events)-1], map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": "ordered-item-0", "content_index": 0, "delta": "unexpected late text"}, terminal)
		upstreamSSE(w, events)
	})
	url := listenResponsesApp(t, server.app)
	status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
	require.Equal(t, 200, status, string(data))
	events := parseResponsesSSE(t, data)
	require.Equal(t, "response.failed", events[len(events)-1]["type"])
	require.NotContains(t, string(data), "unexpected late text")
}

// Only the unsupported Stream method is substituted here. This exercises the
// fallback completion through the real SSE writer and HTTP serialization.
func TestResponsesNonStreamingProviderOrderedFallback(t *testing.T) {
	call := llm.ToolCall{ID: "call-one", Name: "client_tool", Arguments: json.RawMessage(`{}`)}
	provider := &oaiMockProvider{streamErr: fmt.Errorf("streaming unsupported"), resp: &llm.CompletionResponse{
		Content: "beforeafter", ToolCalls: []llm.ToolCall{call}, StopReason: "tool_calls",
		OutputItems: []llm.AssistantOutputItem{{ToolCall: &call}, {Content: "before"}, {Content: "after"}},
	}}
	h := &OpenAICompatHandler{config: ChatConfig{MaxDuration: 5 * time.Second}}
	app := fiber.New()
	app.Post(responsesPath, func(c fiber.Ctx) error {
		request := &ResponsesRequest{Model: "test-model"}
		return h.streamResponses(c, c.Context(), provider, &llm.CompletionRequest{ResponsesInput: true}, newResponsesResponse(request, request.Model), false, nil)
	})
	status, data := requestResponses(t, app, `{"input":"hello"}`, true)
	require.Equal(t, 200, status, string(data))
	events := parseResponsesSSE(t, data)
	terminal := events[len(events)-1]
	require.Equal(t, "response.completed", terminal["type"])
	require.Equal(t, []string{"call-one", "before", "after"}, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
	require.Len(t, provider.requests, 1)
}

func TestResponsesProductionOrderedCoordinatorTruncation(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprint(overflow), func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "orka-responses-order-truncate-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
			path := filepath.Join(dir, "tool.txt")
			require.NoError(t, os.WriteFile(path, []byte("file result"), 0600))
			arguments, err := json.Marshal(map[string]any{"path": path})
			require.NoError(t, err)
			captured := make(chan map[string]any, 3)
			var rounds atomic.Int32
			order := []string{strings.Repeat("before ", 600), "call-one", "after"}
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				captured <- request
				round := rounds.Add(1)
				if round == 1 {
					response, _ := orderedResponsesWire(order, "file_read", string(arguments))
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(response))
					return
				}
				if overflow && round == 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`)
					return
				}
				upstreamResponse(w, goalStateSentinel+"\nfinished")
			})
			if !overflow {
				server.openaiHandler.config.MaxSessionSize = 512
			}
			url := listenResponsesApp(t, server.app)
			status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"input":"read the file"}`, false)
			require.Equal(t, 200, status, string(data))
			require.Contains(t, string(data), "finished")
			<-captured
			if overflow {
				full := <-captured
				require.Equal(t, order, responsesItemOrder(t, full["input"].([]any)))
			}
			truncated := <-captured
			input := truncated["input"].([]any)
			require.Empty(t, responsesItemOrder(t, input), "the oversized assistant turn must be dropped together")
			for _, raw := range input {
				require.NotEqual(t, "function_call_output", raw.(map[string]any)["type"], "no orphaned tool results")
			}
			wantRounds := 2
			if overflow {
				wantRounds = 3
			}
			require.EqualValues(t, wantRounds, rounds.Load())
		})
	}
}

func TestResponsesProductionOrderedIncompleteItem(t *testing.T) {
	for _, itemDone := range []bool{false, true} {
		t.Run(fmt.Sprint(itemDone), func(t *testing.T) {
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				response, _ := orderedResponsesWire([]string{"partial"}, "", "")
				response["status"] = "incomplete"
				response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
				item := response["output"].([]any)[0].(map[string]any)
				item["status"] = "incomplete"
				events := []map[string]any{{"type": "response.output_text.delta", "output_index": 0, "item_id": item["id"], "content_index": 0, "delta": "partial"}}
				if itemDone {
					events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				}
				events = append(events, map[string]any{"type": "response.incomplete", "response": response})
				upstreamSSE(w, events)
			})
			url := listenResponsesApp(t, server.app)
			status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"max_output_tokens":3,"input":"hello"}`, true)
			require.Equal(t, 200, status, string(data))
			events := parseResponsesSSE(t, data)
			terminal := events[len(events)-1]
			require.Equal(t, "response.incomplete", terminal["type"])
			response := terminal["response"].(map[string]any)
			output := response["output"].([]any)
			require.Equal(t, []string{"partial"}, responsesItemOrder(t, output))
			require.Equal(t, "incomplete", output[0].(map[string]any)["status"])
			require.EqualValues(t, 3, response["usage"].(map[string]any)["output_tokens"])
		})
	}
}

func TestResponsesNonStreamingProviderIncompleteFallback(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(fmt.Sprint(ordered), func(t *testing.T) {
			completion := &llm.CompletionResponse{Content: "partial", StopReason: "length", OutputTokens: 3}
			if ordered {
				completion.Content = "beforepartial"
				completion.OutputItems = []llm.AssistantOutputItem{{Content: "before"}, {Content: "partial"}}
			}
			provider := &oaiMockProvider{streamErr: fmt.Errorf("streaming unsupported"), resp: completion}
			h := &OpenAICompatHandler{config: ChatConfig{MaxDuration: 5 * time.Second}}
			app := fiber.New()
			app.Post(responsesPath, func(c fiber.Ctx) error {
				request := &ResponsesRequest{Model: "test-model"}
				return h.streamResponses(c, c.Context(), provider, &llm.CompletionRequest{ResponsesInput: true}, newResponsesResponse(request, request.Model), false, nil)
			})
			status, data := requestResponses(t, app, `{"input":"hello"}`, true)
			require.Equal(t, 200, status, string(data))
			events := parseResponsesSSE(t, data)
			terminal := events[len(events)-1]
			require.Equal(t, "response.incomplete", terminal["type"])
			response := terminal["response"].(map[string]any)
			output := response["output"].([]any)
			want := []string{"partial"}
			if ordered {
				want = []string{"before", "partial"}
			}
			require.Equal(t, want, responsesItemOrder(t, output))
			for i, item := range output {
				status := "completed"
				if i == len(output)-1 {
					status = "incomplete"
				}
				require.Equal(t, status, item.(map[string]any)["status"])
			}
			for _, event := range events {
				if event["type"] == "response.output_item.done" {
					index := int(event["output_index"].(float64))
					require.Equal(t, output[index], event["item"], "done event and terminal snapshot must agree")
				}
			}
			require.EqualValues(t, 3, response["usage"].(map[string]any)["output_tokens"])
		})
	}
}

func TestResponsesProductionStreamMetadataOrder(t *testing.T) {
	for _, mode := range []string{"known-item", "unknown-item", "contradictory-index", "unindexed-before-indexed", "unindexed-terminal-mixed", "legacy-text"} {
		t.Run(mode, func(t *testing.T) {
			order := []string{"call-one", "middle", "call-two"}
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				response, events := orderedResponsesWire(order, "client_tool", `{}`)
				for _, event := range events {
					if event["type"] == "response.output_text.delta" {
						delete(event, "output_index")
						if mode == "unknown-item" {
							event["item_id"] = "unknown"
						}
						if mode == "contradictory-index" {
							event["output_index"] = 0
						}
					}
				}
				switch mode {
				case "unindexed-before-indexed":
					events = append([]map[string]any{{"type": "response.output_text.delta", "delta": "unexpected"}}, events...)
				case "unindexed-terminal-mixed":
					events = []map[string]any{{"type": "response.output_text.delta", "delta": "middle"}, {"type": "response.completed", "response": response}}
				case "legacy-text":
					response, _ = orderedResponsesWire([]string{"middle"}, "", "")
					events = []map[string]any{{"type": "response.output_text.delta", "delta": "middle"}, {"type": "response.completed", "response": response}}
				}
				upstreamSSE(w, events)
			})
			url := listenResponsesApp(t, server.app)
			status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"tools"}`, true)
			require.Equal(t, 200, status, string(data))
			events := parseResponsesSSE(t, data)
			terminal := events[len(events)-1]
			if mode != "known-item" && mode != "legacy-text" {
				require.Equal(t, "response.failed", terminal["type"], "ambiguous metadata must not produce successful reordered output")
				return
			}
			require.Equal(t, "response.completed", terminal["type"])
			if mode == "legacy-text" {
				order = []string{"middle"}
			}
			require.Equal(t, order, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
		})
	}
}
