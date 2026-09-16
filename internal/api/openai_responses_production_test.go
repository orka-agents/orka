/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Independent wire fixtures keep protocol fields visible.
package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// Only model decisions and Kubernetes object storage are substituted. NewServer
// installs production routes/middleware/auth, and the real OIDC verifier reads
// the ephemeral issuer's JWKS over HTTP. No handler is manually registered.
func setupProductionResponses(t *testing.T, upstream http.HandlerFunc) (*Server, string) {
	t.Helper()
	h, _ := setupResponsesHTTP(t, upstream)
	issuer := newTestOIDCProvider(t)
	server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, OIDC: issuer.config()})
	return server, issuer.issueToken(t, testOIDCTokenOptions{Namespace: "default"})
}

// Independent upstream wire data: never use the production response builder.
func responsesFixtureResponse(content string, calls ...llm.ToolCall) map[string]any {
	output := []any{}
	if content != "" {
		output = append(output, map[string]any{"id": "upstream-message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}, "logprobs": []any{}}}})
	}
	for i, call := range calls {
		output = append(output, map[string]any{"id": fmt.Sprintf("upstream-function-%d", i), "type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments), "status": "completed"})
	}
	return map[string]any{
		"id": "upstream-response", "object": "response", "created_at": 1700000000, "status": "completed", "model": "test-model", "output": output, "store": false,
		"error": nil, "incomplete_details": nil, "parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{}, "text": map[string]any{"format": map[string]any{"type": "text"}},
		"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}},
	}
}

func productionResponsesRequest(t *testing.T, url, token, body string, disabled bool) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+responsesPath, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if disabled {
		req.Header.Set("X-Orka-Tools", "disabled")
	}
	// Own the pool so concurrent fixtures do not leave speculative or idle
	// connections on the default transport when the test server shuts down.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Timeout: 15 * time.Second, Transport: transport}).Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, data
}

func TestResponsesProductionRouteAndAuthentication(t *testing.T) {
	var calls atomic.Int32
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/v1/responses", r.URL.Path)
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		// Chat Completions has its separate historical store default.
		if input["store"] != nil {
			require.Equal(t, false, input["store"])
		}
		upstreamResponse(w, "mounted production route")
	})
	url := listenResponsesApp(t, server.app)
	body := `{"model":"fixture/test-model","store":false,"input":"hello"}`
	for _, auth := range []string{"", "invalid-fixture-token"} {
		status, header, data := productionResponsesRequest(t, url, auth, body, true)
		require.Equal(t, 401, status, string(data))
		require.Contains(t, header.Get("Content-Type"), "application/json")
		require.NotContains(t, string(data), "<html")
	}
	require.Zero(t, calls.Load())
	status, header, data := productionResponsesRequest(t, url, token, body, true)
	require.Equal(t, 200, status, string(data))
	require.NotEmpty(t, header.Get("X-Request-ID"))
	require.Contains(t, string(data), "mounted production route")
	require.EqualValues(t, 1, calls.Load())
	status, _, data = productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":true,"input":"hello"}`, true)
	require.Equal(t, 400, status, string(data))
	require.EqualValues(t, 1, calls.Load())
	for _, path := range []string{"/openai/v1/models", "/openai/v1/chat/completions"} {
		method, input := http.MethodGet, ""
		if strings.HasSuffix(path, "completions") {
			method, input = http.MethodPost, `{"model":"fixture/test-model","messages":[{"role":"user","content":"chat"}]}`
		}
		req, err := http.NewRequestWithContext(t.Context(), method, url+path, strings.NewReader(input))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Orka-Tools", "disabled")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, string(data))
	}
}

func TestResponsesProductionInputAndConcurrentIsolation(t *testing.T) {
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, false, input["store"])
		require.NotContains(t, input, "previous_response_id")
		if strings.Contains(input["instructions"].(string), "client_disabled=true") {
			require.Empty(t, input["tools"], "coordinator tools leaked into a client-managed request")
		} else {
			require.NotEmpty(t, input["tools"], "coordinator lost its tools")
		}
		require.True(t, r.Header.Get("Authorization") == "Bearer local-fixture-only", "caller credential was forwarded instead of provider credential")
		items := input["input"].([]any)
		require.Len(t, items, 1, "another client's history leaked")
		upstreamResponse(w, "echo:"+items[0].(map[string]any)["content"].(string))
	})
	url := listenResponsesApp(t, server.app)
	inputs := make([]string, 0, 11)
	inputs = append(inputs, "", "你好 🌊 e\u0301\n\"quoted\"", strings.Repeat("large界", 25000))
	for i := range 8 {
		inputs = append(inputs, fmt.Sprintf("isolated-client-%d", i))
	}
	var group sync.WaitGroup
	ids := make(chan string, len(inputs)*2)
	for _, input := range inputs {
		for _, disabled := range []bool{false, true} {
			group.Go(func() {
				body, err := json.Marshal(map[string]any{"model": "fixture/test-model", "store": false, "input": input, "instructions": fmt.Sprintf("client_disabled=%t", disabled)})
				require.NoError(t, err)
				status, _, data := productionResponsesRequest(t, url, token, string(body), disabled)
				require.Equal(t, 200, status)
				var response map[string]any
				require.NoError(t, json.Unmarshal(data, &response))
				output := response["output"].([]any)
				require.Len(t, output, 1)
				content := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
				require.Equal(t, "echo:"+input, content)
				ids <- response["id"].(string)
			})
		}
	}
	group.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		require.False(t, seen[id], "response ID reused")
		seen[id] = true
	}
	require.Len(t, seen, len(inputs)*2)
}

func TestResponsesProductionMalformedUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"HTTP error", "invalid JSON", "truncated JSON", "truncated stream", "error after delta"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, kind), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch kind {
					case "HTTP error":
						w.WriteHeader(400)
						_, _ = io.WriteString(w, `{"error":{"message":"private diagnostic marker"}}`)
					case "invalid JSON":
						_, _ = io.WriteString(w, "private diagnostic marker")
					case "truncated JSON":
						_, _ = io.WriteString(w, `{"id":"partial"`)
					default:
						upstreamSSE(w, []map[string]any{{"type": "response.output_text.delta", "delta": "partial"}})
						if kind == "error after delta" {
							upstreamSSE(w, []map[string]any{{"type": "error", "message": "private diagnostic marker"}})
						}
					}
				})
				url := listenResponsesApp(t, server.app)
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
				status, _, data := productionResponsesRequest(t, url, token, body, true)
				require.NotContains(t, string(data), "private diagnostic marker")
				if stream {
					require.Equal(t, 200, status)
					events := parseResponsesSSE(t, data)
					require.Equal(t, "response.failed", events[len(events)-1]["type"])
					require.NotContains(t, string(data), "response.completed")
				} else {
					require.Equal(t, 502, status, string(data))
				}
			})
		}
	}
}

func TestResponsesProductionTimeoutAndCleanup(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			canceled := make(chan struct{})
			server, token := setupProductionResponses(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(canceled) })
			server.openaiHandler.config.MaxDuration = 300 * time.Millisecond
			url := listenResponsesApp(t, server.app)
			status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"wait"}`, disabled)
			require.Equal(t, 200, status)
			events := parseResponsesSSE(t, data)
			require.Equal(t, "response.failed", events[len(events)-1]["type"])
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream outlived request timeout")
			}
		})
	}
}

// The client keeps the TCP connection open but never reads the response. Small
// real socket buffers make backpressure deterministic without a fake writer.
func TestResponsesProductionBackpressureReleasesConnection(t *testing.T) {
	for _, target := range []string{responsesPath, responsesPath + "#suffix", "/OPENAI/V1/RESPONSES/", responsesPath + "?namespace=default", "http://localhost/openai/v1/responses?namespace=default#suffix"} {
		t.Run(target, func(t *testing.T) { checkResponsesBackpressure(t, target) })
	}
}

func checkResponsesBackpressure(t *testing.T, target string) {
	t.Helper()
	canceled := make(chan struct{})
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamSSE(w, []map[string]any{{"type": "response.output_text.delta", "delta": strings.Repeat("x", 1<<20)}})
		<-r.Context().Done()
		close(canceled)
	})
	server.openaiHandler.config.MaxDuration = 500 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tracked := &responsesObservedListener{Listener: listener, accepted: make(chan *responsesObservedConn, 1)}
	done := make(chan error, 1)
	go func() { done <- server.app.Listener(tracked, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { require.NoError(t, server.app.ShutdownWithTimeout(3*time.Second)); require.NoError(t, <-done) })
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))
	body := `{"model":"fixture/test-model","store":false,"stream":true,"input":"slow reader"}`
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nX-Orka-Tools: disabled\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", target, token, len(body), body)
	require.NoError(t, err)
	accepted := <-tracked.accepted
	t.Cleanup(func() { _ = accepted.Close() })
	require.Eventually(t, func() bool { return accepted.writing.Load() > 0 }, 2*time.Second, 10*time.Millisecond, "real socket never experienced backpressure")
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("slow client prevented upstream cancellation")
	}
	require.Eventually(t, func() bool { return accepted.closed.Load() }, 2*time.Second, 10*time.Millisecond, "request timeout left a blocked socket/stream writer alive")
}

type responsesObservedListener struct {
	net.Listener
	accepted chan *responsesObservedConn
}

func (l *responsesObservedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4096)
	}
	observed := &responsesObservedConn{Conn: conn}
	l.accepted <- observed
	return observed, nil
}

type responsesObservedConn struct {
	net.Conn
	writing atomic.Int32
	closed  atomic.Bool
}

func (c *responsesObservedConn) Write(p []byte) (int, error) {
	c.writing.Add(1)
	defer c.writing.Add(-1)
	return c.Conn.Write(p)
}
func (c *responsesObservedConn) Close() error { c.closed.Store(true); return c.Conn.Close() }

func TestResponsesProductionKeepAliveClearsWriteDeadline(t *testing.T) {
	var calls atomic.Int32
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 2 {
			time.Sleep(1300 * time.Millisecond)
		}
		upstreamResponse(w, "keep-alive result")
	})
	server.openaiHandler.config.MaxDuration = 100 * time.Millisecond
	url := listenResponsesApp(t, server.app)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	reader := bufio.NewReader(conn)
	for i, path := range []string{responsesPath, "/openai/v1/chat/completions"} {
		body := `{"model":"fixture/test-model","store":false,"input":"first"}`
		if i == 1 {
			server.openaiHandler.config.MaxDuration = 3 * time.Second
			body = `{"model":"fixture/test-model","messages":[{"role":"user","content":"second"}]}`
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Orka-Tools", "disabled")
		require.NoError(t, req.Write(conn))
		resp, err := http.ReadResponse(reader, req)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, string(data))
		require.Contains(t, string(data), "keep-alive result")
	}
	require.EqualValues(t, 2, calls.Load())
}

func TestResponsesProductionFragmentedMultipleToolContinuation(t *testing.T) {
	var rounds atomic.Int32
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, false, request["store"])
		if rounds.Add(1) == 2 {
			input := request["input"].([]any)
			results := map[string]string{}
			for _, raw := range input {
				item := raw.(map[string]any)
				if item["type"] == "function_call_output" {
					results[item["call_id"].(string)] = item["output"].(string)
				}
			}
			require.Equal(t, map[string]string{"call-first": "first result", "call-second": "second result"}, results)
			upstreamResponse(w, "continued both calls")
			return
		}
		call := func(id, name, args string) map[string]any {
			return map[string]any{"id": "item-" + id, "type": "function_call", "call_id": "call-" + id, "name": name, "arguments": args, "status": "completed"}
		}
		first, second := call("first", "client_first", `{"text":"你好"}`), call("second", "client_second", `{"n":2}`)
		events := []map[string]any{
			{"type": "response.output_item.added", "output_index": 0, "item": call("first", "client_first", "")},
			{"type": "response.output_item.added", "output_index": 1, "item": call("second", "client_second", "")},
			{"type": "response.function_call_arguments.delta", "item_id": "item-first", "output_index": 0, "delta": "{\"text\":\"你"},
			{"type": "response.function_call_arguments.delta", "item_id": "item-second", "output_index": 1, "delta": "{\"n\":"},
			{"type": "response.function_call_arguments.delta", "item_id": "item-first", "output_index": 0, "delta": "好\"}"},
			{"type": "response.function_call_arguments.delta", "item_id": "item-second", "output_index": 1, "delta": "2}"},
			// Completion order differs from start order. Arguments must stay with IDs.
			{"type": "response.function_call_arguments.done", "item_id": "item-second", "output_index": 1, "arguments": second["arguments"]},
			{"type": "response.output_item.done", "output_index": 1, "item": second},
			{"type": "response.function_call_arguments.done", "item_id": "item-first", "output_index": 0, "arguments": first["arguments"]},
			{"type": "response.output_item.done", "output_index": 0, "item": first},
			{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{first, second}}},
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			data, err := json.Marshal(event)
			require.NoError(t, err)
			wire := fmt.Sprintf("event: %s\ndata: %s\n\n", event["type"], data)
			for len(wire) > 0 {
				n := min(7, len(wire))
				_, err = io.WriteString(w, wire[:n])
				require.NoError(t, err)
				w.(http.Flusher).Flush()
				wire = wire[n:]
			}
		}
	})
	url := listenResponsesApp(t, server.app)
	status, _, data := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"tools","tools":[{"type":"function","name":"client_first"},{"type":"function","name":"client_second"}]}`, true)
	require.Equal(t, 200, status, string(data))
	events := parseResponsesSSE(t, data)
	terminal := events[len(events)-1]
	require.Equal(t, "response.completed", terminal["type"])
	added := map[string]int{}
	done := map[string]bool{}
	args := map[string]string{}
	calls := map[string]string{}
	terminals := 0
	for _, event := range events {
		switch event["type"] {
		case "response.output_item.added":
			item := event["item"].(map[string]any)
			id := item["id"].(string)
			require.NotContains(t, added, id)
			added[id] = int(event["output_index"].(float64))
			calls[id] = item["call_id"].(string)
		case "response.function_call_arguments.delta":
			id := event["item_id"].(string)
			require.Contains(t, added, id)
			require.False(t, done[id])
			args[id] += event["delta"].(string)
			require.EqualValues(t, added[id], event["output_index"])
		case "response.function_call_arguments.done":
			id := event["item_id"].(string)
			require.Equal(t, args[id], event["arguments"])
			require.False(t, done[id])
			done[id] = true
		case "response.completed":
			terminals++
		}
	}
	require.Len(t, added, 2)
	require.Len(t, done, 2)
	require.Equal(t, 1, terminals)
	for id, callID := range calls {
		expected := `{"n":2}`
		if callID == "call-first" {
			expected = `{"text":"你好"}`
		}
		require.Equal(t, expected, args[id])
	}
	output := terminal["response"].(map[string]any)["output"].([]any)
	require.Len(t, output, 2)
	history := make([]any, 0, 1+len(output)+2)
	history = append(history, map[string]any{"role": "user", "content": "tools"})
	history = append(history, output...)
	history = append(history, map[string]any{"type": "function_call_output", "call_id": "call-first", "output": "first result"}, map[string]any{"type": "function_call_output", "call_id": "call-second", "output": "second result"})
	body, err := json.Marshal(map[string]any{"model": "fixture/test-model", "store": false, "input": history})
	require.NoError(t, err)
	status, _, data = productionResponsesRequest(t, url, token, string(body), true)
	require.Equal(t, 200, status, string(data))
	require.Contains(t, string(data), "continued both calls")
	require.EqualValues(t, 2, rounds.Load())
}

func TestResponsesProductionUnsupportedContract(t *testing.T) {
	var calls atomic.Int32
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) })
	url := listenResponsesApp(t, server.app)
	for _, field := range []string{
		`"store":true`, `"store":null`, `"previous_response_id":"saved"`, `"previous_response_id":null`, `"conversation":null`,
		`"conversation":"saved"`, `"reasoning":{"effort":"high"}`, `"include":["reasoning.encrypted_content"]`, `"background":true`,
		`"tools":[{"type":"web_search"}]`, `"input":[{"type":"reasoning","id":"r"}]`, `"input":[{"type":"item_reference","id":"r"}]`,
		`"input":[{"type":"function_call_output","call_id":"missing","output":"result"}]`, `"parallel_tool_calls":false`, `"tool_choice":"required"`,
	} {
		t.Run(field, func(t *testing.T) {
			// Later duplicates deliberately exercise the JSON parser's actual last-value behavior.
			body := `{"model":"fixture/test-model","store":false,"input":"hello",` + field + `}`
			status, header, data := productionResponsesRequest(t, url, token, body, true)
			require.Equal(t, 400, status, string(data))
			require.Contains(t, header.Get("Content-Type"), "application/json")
			var response map[string]any
			require.NoError(t, json.Unmarshal(data, &response))
			detail := response["error"].(map[string]any)
			require.Equal(t, "invalid_request_error", detail["type"])
			require.NotEmpty(t, detail["param"])
		})
	}
	require.Zero(t, calls.Load(), "rejected state reached provider")
}

// Force reuse before the asynchronous stream consumes the parsed request. This
// directly invokes the production Fiber entry with a real fasthttp RequestCtx;
// separate TCP tests establish the listener seam. No handler or parser is stubbed.
func TestResponsesProductionRequestBufferLifetime(t *testing.T) {
	server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		encoded, err := json.Marshal(request)
		require.NoError(t, err)
		require.Contains(t, string(encoded), "original-input")
		require.Contains(t, string(encoded), "original-instructions")
		require.NotContains(t, string(encoded), "recycled")
		require.Equal(t, false, request["store"])
		if request["stream"] == true {
			upstreamSSE(w, []map[string]any{{"type": "response.output_text.delta", "delta": "owned input"}, {"type": "response.completed", "response": responsesFixtureResponse("owned input")}})
		} else {
			upstreamResponse(w, "owned input")
		}
	})
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			var requestCtx fasthttp.RequestCtx
			requestCtx.Init(&fasthttp.Request{}, nil, nil)
			body := `{"model":"fixture/test-model","store":false,"stream":true,"input":"original-input","instructions":"original-instructions"}`
			requestCtx.Request.Header.SetMethod(http.MethodPost)
			requestCtx.Request.SetRequestURI(responsesPath)
			requestCtx.Request.Header.SetContentType("application/json")
			requestCtx.Request.Header.Set("Authorization", "Bearer "+token)
			if disabled {
				requestCtx.Request.Header.Set("X-Orka-Tools", "disabled")
			}
			requestCtx.Request.SetBodyString(body)
			borrowed := requestCtx.Request.Body()
			server.app.Handler()(&requestCtx)
			require.Equal(t, 200, requestCtx.Response.StatusCode())
			require.True(t, requestCtx.Response.IsBodyStream())
			// Mutate the original backing array, not a replacement allocation.
			copy(borrowed, strings.ReplaceAll(strings.ReplaceAll(body, "original-input", "recycled-input"), "original-instructions", "recycled-instructions"))
			require.Contains(t, string(requestCtx.Request.Body()), "recycled-input")
			data, err := io.ReadAll(requestCtx.Response.BodyStream())
			require.NoError(t, err)
			require.NoError(t, requestCtx.Response.CloseBodyStream())
			events := parseResponsesSSE(t, data)
			require.Equal(t, "response.completed", events[len(events)-1]["type"])
			require.Contains(t, string(data), "owned input")
			requestCtx.Request.Reset()
			requestCtx.Response.Reset()
		})
	}
}

func TestResponsesProductionJSONCoordinatorRounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			path := filepath.Join(responsesToolTempDir(t), "value.txt")
			require.NoError(t, os.WriteFile(path, []byte("boundary value"), 0600))
			args, err := json.Marshal(map[string]string{"path": path})
			require.NoError(t, err)
			var rounds atomic.Int32
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, false, request["store"])
				require.Equal(t, float64(64), request["max_output_tokens"])
				require.Equal(t, float64(0), request["temperature"])
				format := request["text"].(map[string]any)["format"].(map[string]any)
				require.Equal(t, "json_schema", format["type"])
				require.Equal(t, true, format["strict"])
				require.Equal(t, "answer", format["name"])
				require.Equal(t, false, format["schema"].(map[string]any)["additionalProperties"])
				if rounds.Add(1) == 1 {
					upstreamResponse(w, "reading before JSON", llm.ToolCall{ID: "json-file-call", Name: "file_read", Arguments: args})
					return
				}
				input := request["input"].([]any)
				last := input[len(input)-1].(map[string]any)
				require.Equal(t, "function_call_output", last["type"])
				require.Equal(t, "json-file-call", last["call_id"])
				require.Contains(t, last["output"], "boundary value")
				upstreamResponse(w, `{"answer":"boundary value"}`)
			})
			url := listenResponsesApp(t, server.app)
			body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"temperature":0,"max_output_tokens":64,"input":"read and answer","text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}}`, stream)
			status, _, data := productionResponsesRequest(t, url, token, body, false)
			require.Equal(t, 200, status, string(data))
			require.EqualValues(t, 2, rounds.Load())
			require.NotContains(t, string(data), "reading before JSON")
			var response map[string]any
			if stream {
				events := parseResponsesSSE(t, data)
				require.Equal(t, "response.completed", events[len(events)-1]["type"])
				response = events[len(events)-1]["response"].(map[string]any)
			} else {
				require.NoError(t, json.Unmarshal(data, &response))
			}
			output := response["output"].([]any)
			require.Len(t, output, 1)
			text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
			var answer map[string]string
			require.NoError(t, json.Unmarshal([]byte(text), &answer))
			require.Equal(t, map[string]string{"answer": "boundary value"}, answer)
		})
	}
}
