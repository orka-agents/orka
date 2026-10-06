/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const responsesPath = "/openai/v1/responses"

func setupResponsesHTTP(t *testing.T, upstream http.HandlerFunc, handleProbe ...bool) (*OpenAICompatHandler, *fiber.App) {
	t.Helper()
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var probe map[string]any
		require.NoError(t, json.Unmarshal(body, &probe))
		if probe["max_output_tokens"] == float64(1) && (len(handleProbe) == 0 || handleProbe[0]) {
			require.Equal(t, false, probe["store"], "API-mode probe must also be stateless")
			upstreamResponse(w, "probe")
			return
		}
		upstream(w, r)
	}))
	t.Cleanup(fixture.Close)
	provider := &corev1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "default"}, Spec: corev1alpha1.ProviderSpec{
		Type: corev1alpha1.ProviderTypeOpenAI, DefaultModel: "test-model", BaseURL: fixture.URL + "/v1", SecretRef: corev1alpha1.ProviderSecretRef{Name: "fixture", Key: "key"},
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "default"}, Data: map[string][]byte{"key": []byte("local-fixture-only")}}
	h, app := setupTestOpenAIHandler(provider, secret)
	h.config.MaxDuration = 10 * time.Second
	h.config.MaxIterations = 4
	h.config.MaxPrematureEndRetries = 0
	app.Post(responsesPath, h.HandleResponses)
	return h, app
}

func requestResponses(t *testing.T, app *fiber.App, body string, disabled bool) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if disabled {
		request.Header.Set("X-Orka-Tools", "disabled")
	}
	response, err := app.Test(request, fiber.TestConfig{Timeout: 15 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}

func upstreamResponse(w http.ResponseWriter, content string, calls ...llm.ToolCall) {
	response := responsesFixtureResponse(content, calls...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func TestResponsesHTTPTextAndJSON(t *testing.T) {
	for _, test := range []struct{ name, input, format, expected string }{
		{"string", `"hello"`, "", "hello"},
		{"text parts", `[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_text","text":" world"}]}]`, "", "hello world"},
		{"json object", `"JSON please"`, `,"text":{"format":{"type":"json_object"}}`, `{"answer":42}`},
		{"json schema", `"JSON please"`, `,"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}}}`, `{"answer":42}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, disabled := range []bool{false, true} {
				var captured map[string]any
				_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/v1/responses", r.URL.Path)
					require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
					upstreamResponse(w, test.expected)
				})
				status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"temperature":0,"max_output_tokens":32,"instructions":"be precise","input":`+test.input+test.format+`}`, disabled)
				require.Equal(t, 200, status, string(body))
				var response ResponsesResponse
				require.NoError(t, json.Unmarshal(body, &response))
				require.Equal(t, "response", response.Object)
				require.Equal(t, "completed", response.Status)
				require.False(t, response.Store)
				require.Equal(t, "test-model", response.Model)
				require.Len(t, response.Output, 1)
				require.Equal(t, test.expected, response.Output[0].Content[0].Text)
				require.Equal(t, 10, response.Usage.TotalTokens)
				require.Equal(t, false, captured["store"])
				require.Equal(t, float64(0), captured["temperature"])
				require.Equal(t, float64(32), captured["max_output_tokens"])
				require.Contains(t, captured["instructions"], "be precise")
				if test.format != "" {
					require.NotNil(t, captured["text"])
				}
				if disabled {
					require.Nil(t, captured["tools"])
				} else {
					require.NotEmpty(t, captured["tools"])
				}
			}
		})
	}
}

func TestResponsesHTTPRejectsUnsupportedStateAndMalformedInput(t *testing.T) {
	cases := make([]string, 0, 31)
	cases = append(cases,
		`{"model":"fixture/test-model","input":"hi"}`,
		`{"model":"fixture/test-model","input":"hi","store":true}`,
		`{"model":"fixture/test-model","input":"hi","store":null}`,
	)
	for _, field := range []string{
		`"previous_response_id":"resp_saved"`, `"previous_response_id":null`, `"conversation":"conv_saved"`, `"conversation":null`,
		`"background":true`, `"reasoning":{"effort":"high"}`, `"include":["reasoning.encrypted_content"]`, `"truncation":"auto"`, `"unknown":true`,
		`"tools":[{"type":"web_search"}]`, `"tools":[{"type":"function","name":"x","parameters":null}]`,
		`"tool_choice":"required"`, `"parallel_tool_calls":false`, `"text":{"format":{"type":"xml"}}`, `"text":{"format":{"type":"json_schema"}}`,
		`"max_output_tokens":0`, `"temperature":-1`,
	} {
		cases = append(cases, `{"model":"fixture/test-model","input":"hi","store":false,`+field+`}`)
	}
	for _, input := range []string{
		`null`, `42`, `[]`, `[null]`, `[{"role":"alien","content":"hi"}]`, `[{"role":"user","content":[{"type":"input_image","image_url":"test"}]}]`,
		`[{"type":"reasoning","id":"r"}]`, `[{"type":"item_reference","id":"saved"}]`,
		`[{"type":"function_call_output","call_id":"missing","output":"result"}]`,
		`[{"type":"function_call","call_id":"c","name":"tool","arguments":"{}"}]`,
		`[{"type":"function_call","call_id":"c","name":"tool","arguments":"bad"},{"type":"function_call_output","call_id":"c","output":"ok"}]`,
		`[{"type":"function_call","status":"incomplete","call_id":"c","name":"tool","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}]`,
		`[{"type":"function_call","status":"in_progress","call_id":"c","name":"tool","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}]`,
		`[{"type":"function_call","call_id":"c","name":"tool","arguments":"{}"},{"type":"function_call_output","status":"incomplete","call_id":"c","output":"ok"}]`,
		`[{"role":"assistant","status":"in_progress","content":"pending text"}]`,
		`[{"role":"assistant","status":"unknown","content":"pending text"}]`,
		`[{"role":"user","status":"incomplete","content":"pending text"}]`,
		`[{"role":"system","status":"incomplete","content":"pending text"}]`,
		`[{"role":"developer","status":"incomplete","content":"pending text"}]`,
	} {
		cases = append(cases, `{"model":"fixture/test-model","store":false,"input":`+input+`}`)
	}
	_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid request reached upstream")
		w.WriteHeader(500)
	})
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			status, data := requestResponses(t, app, body, true)
			require.Equal(t, 400, status, string(data))
			var result OAIError
			require.NoError(t, json.Unmarshal(data, &result))
			require.Equal(t, OAIErrorTypeInvalidRequest, result.Error.Type)
			require.NotNil(t, result.Error.Param)
		})
	}
}

func TestResponsesHTTPClientToolHistory(t *testing.T) {
	captured := make(chan map[string]any, 2)
	var n atomic.Int32
	_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		captured <- request
		if n.Add(1) == 1 {
			upstreamResponse(w, "", llm.ToolCall{ID: "call-stable", Name: "client_echo", Arguments: json.RawMessage(`{"text":"hi"}`)})
			return
		}
		upstreamResponse(w, "follow-up complete")
	})
	status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"input":"use client tool","tools":[{"type":"function","name":"client_echo","strict":true,"parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}}]}`, true)
	require.Equal(t, 200, status, string(body))
	var response ResponsesResponse
	require.NoError(t, json.Unmarshal(body, &response))
	require.Len(t, response.Output, 1)
	call := response.Output[0]
	require.Equal(t, "call-stable", call.CallID)
	require.NotEqual(t, call.CallID, call.ID)
	require.Equal(t, "function_call", call.Type)
	require.Equal(t, `{"text":"hi"}`, *call.Arguments)
	first := <-captured
	require.Equal(t, true, first["tools"].([]any)[0].(map[string]any)["strict"])
	item, _ := json.Marshal(call)
	status, body = requestResponses(t, app, `{"model":"fixture/test-model","store":false,"input":[{"role":"user","content":"use client tool"},`+string(item)+`,{"type":"function_call_output","call_id":"call-stable","output":"hi"},{"role":"user","content":"follow up"}]}`, true)
	require.Equal(t, 200, status, string(body))
	followup := <-captured
	items := followup["input"].([]any)
	require.Len(t, items, 4)
	require.Equal(t, "call-stable", items[1].(map[string]any)["call_id"])
	require.Equal(t, "call-stable", items[2].(map[string]any)["call_id"])
	require.Equal(t, "hi", items[2].(map[string]any)["output"])
	require.Equal(t, false, followup["store"])
}

func TestResponsesHTTPCoordinatorToolRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			path := filepath.Join(responsesToolTempDir(t), "read.txt")
			require.NoError(t, os.WriteFile(path, []byte("coordinator file result"), 0600))
			args, _ := json.Marshal(map[string]string{"path": path})
			captured := make(chan map[string]any, 2)
			var n atomic.Int32
			_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				captured <- request
				if n.Add(1) == 1 {
					upstreamResponse(w, "reading", llm.ToolCall{ID: "server-call", Name: "file_read", Arguments: args})
					return
				}
				upstreamResponse(w, goalStateSentinel+"\ncoordinator file result")
			})
			status, body := requestResponses(t, app, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read file","tools":[{"type":"function","name":"client_tool"}]}`, stream), false)
			require.Equal(t, 200, status, string(body))
			require.Contains(t, string(body), "coordinator file result")
			require.NotContains(t, string(body), goalStateSentinel)
			require.EqualValues(t, 2, n.Load())
			first, second := <-captured, <-captured
			for _, request := range []map[string]any{first, second} {
				require.Equal(t, false, request["store"])
				for _, tool := range request["tools"].([]any) {
					require.NotEqual(t, "client_tool", tool.(map[string]any)["name"])
				}
			}
			inputs := second["input"].([]any)
			require.Contains(t, inputs[len(inputs)-1].(map[string]any)["output"], "coordinator file result")
			require.Equal(t, "server-call", inputs[len(inputs)-1].(map[string]any)["call_id"])
			if stream {
				events := parseResponsesSSE(t, body)
				require.Equal(t, "response.completed", events[len(events)-1]["type"])
				for _, event := range events {
					if event["type"] == "response.output_item.added" {
						require.Equal(t, "message", event["item"].(map[string]any)["type"])
					}
				}
			}
		})
	}
}

func parseResponsesSSE(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	events := []map[string]any{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// The complete captured response bounds even its largest event line.
	scanner.Buffer(nil, len(data)+1)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = value
		}
		value, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(value), &event))
		require.Equal(t, eventType, event["type"])
		require.Equal(t, float64(len(events)), event["sequence_number"])
		events = append(events, event)
	}
	require.NoError(t, scanner.Err())
	require.GreaterOrEqual(t, len(events), 2)
	require.Equal(t, "response.created", events[0]["type"])
	require.Equal(t, "response.in_progress", events[1]["type"])
	return events
}

func upstreamSSE(w http.ResponseWriter, events []map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
	}
}

func TestResponsesHTTPStreamingTextAndToolIDs(t *testing.T) {
	_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, true, req["stream"])
		require.Equal(t, false, req["store"])
		message := map[string]any{"type": "message", "id": "upstream-text", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Hello world", "annotations": []any{}}}}
		upstreamSSE(w, []map[string]any{
			{"type": "response.output_text.delta", "output_index": 0, "item_id": "upstream-text", "delta": "Hello "},
			{"type": "response.output_text.delta", "output_index": 0, "item_id": "upstream-text", "delta": "world"},
			{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "upstream-item", "call_id": "stable-call", "name": "client_tool", "arguments": ""}},
			{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "upstream-item", "delta": "{\"value\":"},
			{"type": "response.function_call_arguments.done", "output_index": 1, "item_id": "upstream-item", "arguments": "{\"value\":42}"},
			{"type": "response.completed", "response": map[string]any{"status": "completed", "model": "test-model", "output": []any{message, map[string]any{"type": "function_call", "id": "upstream-item", "call_id": "stable-call", "name": "client_tool", "arguments": "{\"value\":42}", "status": "completed"}}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 2}}},
		})
	})
	status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hi","tools":[{"type":"function","name":"client_tool"}]}`, true)
	require.Equal(t, 200, status, string(body))
	events := parseResponsesSSE(t, body)
	var deltas string
	var itemID string
	for _, event := range events {
		switch event["type"] {
		case "response.output_text.delta":
			deltas += event["delta"].(string)
		case "response.output_item.added":
			item := event["item"].(map[string]any)
			if item["type"] == "function_call" {
				require.Equal(t, "stable-call", item["call_id"])
				itemID = item["id"].(string)
				require.Equal(t, "", item["arguments"])
			}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			require.Equal(t, itemID, event["item_id"])
			require.Equal(t, float64(1), event["output_index"])
		}
	}
	require.Equal(t, "Hello world", deltas)
	terminal := events[len(events)-1]
	require.Equal(t, "response.completed", terminal["type"])
	response := terminal["response"].(map[string]any)
	items := response["output"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, itemID, items[1].(map[string]any)["id"])
	require.Equal(t, "stable-call", items[1].(map[string]any)["call_id"])
	require.Equal(t, `{"value":42}`, items[1].(map[string]any)["arguments"])
	require.Equal(t, false, response["store"])
}

func TestResponsesHTTPStreamingFailures(t *testing.T) {
	for _, reason := range []string{"missing terminal", "failed", "incomplete", "provider error"} {
		t.Run(reason, func(t *testing.T) {
			_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
				events := []map[string]any{{"type": "response.output_text.delta", "delta": "partial"}}
				switch reason {
				case "failed":
					events = append(events, map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed"}})
				case "incomplete":
					events = append(events, map[string]any{"type": "response.incomplete", "response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}}})
				case "provider error":
					events = append(events, map[string]any{"type": "error", "message": "private upstream diagnostic"})
				}
				upstreamSSE(w, events)
			})
			status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hi"}`, true)
			require.Equal(t, 200, status, string(body))
			require.NotContains(t, string(body), "private upstream diagnostic")
			events := parseResponsesSSE(t, body)
			last := events[len(events)-1]
			if reason == "incomplete" {
				require.Equal(t, "response.incomplete", last["type"])
			} else {
				require.Equal(t, "response.failed", last["type"])
				require.NotContains(t, string(body), "response.completed")
			}
		})
	}
}

func listenResponsesApp(t *testing.T, app *fiber.App) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { require.NoError(t, app.ShutdownWithTimeout(3*time.Second)); require.NoError(t, <-done) })
	return "http://" + listener.Addr().String()
}

func TestResponsesHTTPDisconnectCancelsUpstream(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			_, app := setupResponsesHTTP(t, func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(canceled)
			})
			url := listenResponsesApp(t, app)
			request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url+responsesPath, strings.NewReader(`{"model":"fixture/test-model","store":false,"stream":true,"input":"wait"}`))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			if disabled {
				request.Header.Set("X-Orka-Tools", "disabled")
			}
			client := &http.Client{Timeout: 8 * time.Second}
			response, err := client.Do(request)
			require.NoError(t, err)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not start")
			}
			require.NoError(t, response.Body.Close())
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("disconnect did not cancel upstream")
			}
		})
	}
}

func responsesToolTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp", "orka-responses-tool-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(path)) })
	return path
}

func TestResponsesHTTPStoreFalseOnChatFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/disabled=%t", stream, disabled), func(t *testing.T) {
				var calls atomic.Int32
				_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"], "including API-mode probes and fallbacks")
					if strings.HasSuffix(r.URL.Path, "/responses") {
						w.WriteHeader(404)
						_, _ = io.WriteString(w, `{"error":{"message":"unsupported endpoint","code":"invalid_url"}}`)
						return
					}
					require.Equal(t, "/v1/chat/completions", r.URL.Path)
					calls.Add(1)
					if request["stream"] == true {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, `data: {"id":"chat-fixture","choices":[{"index":0,"delta":{"content":"fallback text"},"finish_reason":null}]}`+"\n\n"+`data: {"id":"chat-fixture","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"chat-fixture","choices":[{"index":0,"message":{"role":"assistant","content":"fallback text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
					}
				}, false)
				status, body := requestResponses(t, app, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hi"}`, stream), disabled)
				require.Equal(t, 200, status, string(body))
				require.Contains(t, string(body), "fallback text")
				require.EqualValues(t, 1, calls.Load())
			})
		}
	}
}

func TestResponsesHTTPTimeoutFailsStream(t *testing.T) {
	canceled := make(chan struct{})
	h, app := setupResponsesHTTP(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(canceled) })
	h.config.MaxDuration = 100 * time.Millisecond
	status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"stream":true,"input":"wait"}`, true)
	require.Equal(t, 200, status, string(body))
	events := parseResponsesSSE(t, body)
	require.Equal(t, "response.failed", events[len(events)-1]["type"])
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timeout did not cancel upstream")
	}
}

func TestResponsesHTTPFinalCoordinatorRoundPreservesSettings(t *testing.T) {
	var n atomic.Int32
	h, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, false, request["store"])
		require.Equal(t, float64(0), request["temperature"])
		require.Equal(t, "json_object", request["text"].(map[string]any)["format"].(map[string]any)["type"])
		if n.Add(1) == 1 {
			upstreamResponse(w, "", llm.ToolCall{ID: "unexposed", Name: "client_tool", Arguments: json.RawMessage(`{}`)})
			return
		}
		require.Nil(t, request["tools"], "iteration limit must remove tools")
		upstreamResponse(w, `{"answer":42}`)
	})
	h.config.MaxIterations = 1
	status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"temperature":0,"input":"hi","text":{"format":{"type":"json_object"}}}`, false)
	require.Equal(t, 200, status, string(body))
	require.EqualValues(t, 2, n.Load())
}

func TestResponsesHTTPContextTokenAuthorization(t *testing.T) {
	issuer := newTestOIDCProvider(t)
	tokenConfig := testContextTokenConfig(t, issuer, "")
	for _, test := range []struct {
		name, scope, model string
		disabled           bool
		status             int
	}{
		{"missing provider scope", ContextTokenScopeToolsUse, "fixture/test-model", true, 403},
		{"missing tool scope", ContextTokenScopeProvidersUse, "fixture/test-model", false, 403},
		{"client tools", ContextTokenScopeProvidersUse, "fixture/test-model", true, 200},
		{"coordinator tools", ContextTokenScopeProvidersUse + " " + ContextTokenScopeToolsUse, "fixture/test-model", false, 200},
		{"implicit provider", ContextTokenScopeProvidersUse, "test-model", true, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			h, _ := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				if !test.disabled {
					tools := request["tools"].([]any)
					require.Len(t, tools, 1)
					require.Equal(t, "file_read", tools[0].(map[string]any)["name"])
				}
				upstreamResponse(w, "authorized")
			})
			authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
			require.NoError(t, err)
			h.contextTokenAuthorization = authz
			server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, ContextTokens: tokenConfig, ContextTokenAuthorization: authz})
			app := server.app
			token := issueTestContextToken(t, issuer, nil, map[string]any{"scope": test.scope, "tctx": map[string]any{"allowedProviders": []string{"openai"}, "allowedTools": []string{"file_read"}}})
			request := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(fmt.Sprintf(`{"model":%q,"store":false,"input":"hi"}`, test.model)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(TransactionTokenHeaderName, token)
			if test.disabled {
				request.Header.Set("X-Orka-Tools", "disabled")
			}
			response, err := app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, test.status, response.StatusCode, string(body))
			if test.status != 200 {
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestResponsesHTTPInterleavedAssistantToolHistory(t *testing.T) {
	captured := make(chan map[string]any, 1)
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		captured <- request
		upstreamResponse(w, "continued")
	})
	// Client-owned history must stay intact even with a tiny coordinator budget.
	server.openaiHandler.config.MaxSessionSize = 64
	url := listenResponsesApp(t, server.app)
	status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"input":[
 {"role":"user","content":"use tools"},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"before ","annotations":[]}]},
 {"type":"function_call","call_id":"call-one","name":"first","arguments":"{}"},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"after","annotations":[]}]},
 {"type":"function_call","call_id":"call-two","name":"second","arguments":"{}"},
 {"type":"function_call_output","call_id":"call-two","output":"two"},
 {"type":"function_call_output","call_id":"call-one","output":"one"},
 {"role":"user","content":"continue"}]}`, true)
	require.Equal(t, 200, status, string(body))
	data, err := json.Marshal((<-captured)["input"])
	require.NoError(t, err)
	require.JSONEq(t, `[
 {"role":"user","content":"use tools"},
 {"role":"assistant","content":"before "},
 {"type":"function_call","call_id":"call-one","name":"first","arguments":"{}"},
 {"role":"assistant","content":"after"},
 {"type":"function_call","call_id":"call-two","name":"second","arguments":"{}"},
 {"type":"function_call_output","call_id":"call-two","output":"two"},
 {"type":"function_call_output","call_id":"call-one","output":"one"},
 {"role":"user","content":"continue"}]`, string(data), "upstream must receive the client-supplied item order")
}

func TestResponsesHTTPInvalidProviderOutcomes(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled", "unknown", "empty", "bad call"} {
		t.Run(outcome, func(t *testing.T) {
			_, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
				response := newResponsesResponse(&ResponsesRequest{}, "test-model")
				response.Status = outcome
				if outcome == "empty" {
					response.Status = "completed"
				}
				if outcome == "bad call" {
					response.Status = "completed"
					args := "not JSON"
					response.Output = []responsesOutputItem{{Type: "function_call", Name: "client_tool", CallID: "bad", Arguments: &args, Status: "completed"}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			})
			status, body := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"input":"hi"}`, true)
			require.Equal(t, 502, status, string(body))
			var response OAIError
			require.NoError(t, json.Unmarshal(body, &response))
			require.Equal(t, "server_error", response.Error.Type)
		})
	}
}

func TestResponsesHTTPFinalCoordinatorRoundFailure(t *testing.T) {
	for _, format := range []string{"text", "json_object", "json_schema"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				path := filepath.Join(responsesToolTempDir(t), "read.txt")
				require.NoError(t, os.WriteFile(path, []byte("read before failure"), 0600))
				args, _ := json.Marshal(map[string]string{"path": path})
				var calls atomic.Int32
				h, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					require.Equal(t, false, request["store"])
					if calls.Add(1) == 1 {
						upstreamResponse(w, "", llm.ToolCall{ID: "server-read", Name: "file_read", Arguments: args})
						return
					}
					require.Nil(t, request["tools"])
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"message":"final completion failed","type":"invalid_request_error"}}`)
				})
				h.config.MaxIterations = 1
				formatJSON := fmt.Sprintf(`{"type":%q}`, format)
				if format == "json_schema" {
					formatJSON = `{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"answer":{"type":"string"}}}}`
				}
				status, body := requestResponses(t, app, fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"read then summarize","text":{"format":%s}}`, stream, formatJSON), false)
				require.EqualValues(t, 2, calls.Load())
				if stream {
					require.Equal(t, 200, status, string(body))
					events := parseResponsesSSE(t, body)
					require.Equal(t, "response.failed", events[len(events)-1]["type"])
					require.NotContains(t, string(body), "response.completed")
				} else {
					require.Equal(t, 502, status, string(body))
					var response OAIError
					require.NoError(t, json.Unmarshal(body, &response))
					require.Equal(t, "server_error", response.Error.Type)
				}
			})
		}
	}
}

func TestResponsesFinalErrorOptionPreservesExistingCompatFallback(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			provider := &oaiMockProvider{err: fmt.Errorf("final provider failure")}
			response, err := runNonStreamingToolLoop(context.Background(), provider, &llm.CompletionRequest{Model: "fixture"}, "fixture", ChatConfig{MaxIterations: 0}, nil, toolLoopOptions{requireFinalCompletion: strict})
			if strict {
				require.Error(t, err)
				require.Nil(t, response)
			} else {
				require.NoError(t, err)
				require.Equal(t, "Reached iteration limit.", response.Content)
				require.Equal(t, "end_turn", response.StopReason)
			}
		})
	}
}
