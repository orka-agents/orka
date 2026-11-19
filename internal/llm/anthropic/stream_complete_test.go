/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/llm/anthropic/anthropictest"
)

func TestAcceptsSamplingParams(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-5-5":                      false,
		"claude-opus-5":                        false,
		"claude-sonnet-5-5":                    false,
		"claude-sonnet-5":                      false,
		"claude-haiku-5-5":                     false,
		"claude-opus-4-8":                      false,
		"claude-opus-4-7":                      false,
		"claude-fable-5-2":                     false,
		"claude-mythos-5-2":                    false,
		"claude-mythos-preview":                false,
		"claude-opus-5.5":                      false, // Copilot spelling
		"CLAUDE-OPUS-4.7":                      false,
		"anthropic.claude-opus-5-5":            false, // Bedrock
		"us.anthropic.claude-sonnet-5":         false,
		"claude-opus-4-6":                      true,
		"claude-sonnet-4-6":                    true,
		"claude-haiku-4-5":                     true,
		"claude-haiku-4.5":                     true,
		"claude-opus-4-5@20251101":             true, // Vertex snapshot
		"claude-sonnet-4-20250514":             true, // date, not a minor version
		"claude-opus-4-1-20250805":             true,
		"claude-3-7-sonnet-20250219":           true,
		"claude-3-sonnet-20240229":             true,
		"claude-3":                             true,
		"claude-":                              true,
		"claude-opus":                          true,
		"my-foundry-deployment":                true,
		"kimi-k2":                              true,
		"":                                     true,
		"claude-instant-1":                     true,
		"claude-sonnet-x":                      true,
		"openrouter/anthropic/claude-opus-4-6": true,
	} {
		if got := acceptsSamplingParams(model); got != want {
			t.Errorf("acceptsSamplingParams(%q) = %t, want %t", model, got, want)
		}
	}
}

func TestBuildRequestParamsOmitsTemperatureForModelsThatRejectIt(t *testing.T) {
	for model, want := range map[string]bool{"claude-opus-5-5": false, "claude-sonnet-4-6": true} {
		params := buildRequestParams(&llm.CompletionRequest{
			Model: model, Temperature: 0.7, TemperatureSet: true,
			Messages: []llm.Message{{Role: "user", Content: "hi"}},
		})
		body, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if _, got := fields["temperature"]; got != want {
			t.Errorf("%s: temperature sent = %t, want %t: %s", model, got, want, body)
		}
	}
}

func TestBuildMessagesGroupsParallelToolResults(t *testing.T) {
	messages := buildMessages([]llm.Message{
		{Role: "user", Content: "look up a and b"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "a", Name: testToolNameSearch, Arguments: json.RawMessage(`{"q":"a"}`)},
			{ID: "b", Name: testToolNameSearch, Arguments: json.RawMessage(`{"q":"b"}`)},
		}},
		{Role: "tool", Content: "A", ToolCallID: "a"},
		{Role: "tool", Content: "B", ToolCallID: "b"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "c", Name: testToolNameSearch, Arguments: json.RawMessage(`{"q":"c"}`)},
		}},
		{Role: "tool", Content: "C", ToolCallID: "c"},
		{Role: "user", Content: "thanks"},
	})
	if len(messages) != 6 {
		t.Fatalf("got %d messages, want 6", len(messages))
	}
	for i, want := range [][]string{nil, nil, {"a", "b"}, nil, {"c"}, nil} {
		if want == nil {
			continue
		}
		got := make([]string, 0, len(messages[i].Content))
		for _, block := range messages[i].Content {
			if block.OfToolResult == nil {
				t.Fatalf("message %d holds a non-tool_result block", i)
			}
			got = append(got, block.OfToolResult.ToolUseID)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("message %d tool results = %v, want %v", i, got, want)
		}
	}
	if text := messages[5].Content[0].OfText; text == nil || text.Text != "thanks" {
		t.Errorf("the user turn after the tool results was merged into them: %+v", messages[5])
	}
}

func newStreamTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// The SDK refuses a non-streaming request above about 21,333 max_tokens
// before sending it.
func TestCompleteAcceptsLargeMaxTokens(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true || body["max_tokens"] != float64(64000) {
			t.Errorf("stream = %v, max_tokens = %v; want true, 64000", body["stream"], body["max_tokens"])
		}
		_ = anthropictest.WriteMessage(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"done"},{"type":"tool_use","id":"t1","name":"search","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`)
	})
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{
		Model: "claude-opus-5-5", MaxTokens: 64000,
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Content != "done" || resp.StopReason != "tool_use" || resp.Model != "claude-opus-5-5" || resp.ID != "msg_1" {
		t.Errorf("response = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
	if !resp.UsageReported || resp.InputTokens != 12 || resp.OutputTokens != 7 {
		t.Errorf("usage = reported %t, in %d, out %d", resp.UsageReported, resp.InputTokens, resp.OutputTokens)
	}
	if resp.CachedInputTokens == nil || *resp.CachedInputTokens != 3 || resp.CacheWriteInputTokens == nil || *resp.CacheWriteInputTokens != 2 {
		t.Errorf("cache usage = %v, %v", resp.CachedInputTokens, resp.CacheWriteInputTokens)
	}
}

func TestCompleteReportsMissingUsage(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = anthropictest.WriteMessage(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	})
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.UsageReported || resp.CachedInputTokens != nil || resp.CacheWriteInputTokens != nil {
		t.Errorf("usage reported = %t, cache = %v, %v; want unreported", resp.UsageReported, resp.CachedInputTokens, resp.CacheWriteInputTokens)
	}
}

func TestCompleteRejectsTruncatedStream(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
	})
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatalf("Complete() = %+v, want an error for a stream that ended mid-message", resp)
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		t.Fatalf("error type = %T, want *llm.ProviderError", err)
	}
	// A dropped connection is transient, and no output escaped Complete.
	if pe.StatusCode != http.StatusBadGateway || !llm.ShouldRetry(err) {
		t.Errorf("status code = %d, retry = %t; want 502, true", pe.StatusCode, llm.ShouldRetry(err))
	}
	// The attempt was billed for its input, so its usage survives the error.
	if resp == nil || resp.InputTokens != 1 || resp.Content != "" {
		t.Errorf("response = %+v, want only the reported input usage", resp)
	}
}

func TestCompleteReturnsStreamErrorEvent(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	})
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete() succeeded, want the stream's error event")
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		t.Fatalf("error type = %T, want *llm.ProviderError", err)
	}
	if resp == nil || resp.InputTokens != 1 || resp.Content != "" {
		t.Errorf("response = %+v, want only the reported input usage", resp)
	}
	// Overloaded before the stream starts is an HTTP 529, which is retried.
	if pe.StatusCode != 529 || !llm.ShouldRetry(err) {
		t.Errorf("status code = %d, retry = %t; want 529, true", pe.StatusCode, llm.ShouldRetry(err))
	}
}

func TestCompleteKeepsHTTPStatusCode(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`)
	})
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		t.Fatalf("error = %v (%T), want *llm.ProviderError", err, err)
	}
	if resp != nil {
		t.Errorf("response = %+v, want nil when the request failed before streaming", resp)
	}
	if pe.StatusCode != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", pe.StatusCode)
	}
}

func TestCompleteRetriesBrokenStream(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Promise more body than is sent, so the client reads an unexpected EOF.
		w.Header().Set("Content-Length", "100000")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
	})
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		t.Fatalf("error = %v (%T), want *llm.ProviderError", err, err)
	}
	if pe.StatusCode != http.StatusBadGateway || !llm.ShouldRetry(err) {
		t.Errorf("error = %q, status code = %d, retry = %t; want 502, true", pe.Message, pe.StatusCode, llm.ShouldRetry(err))
	}
	if resp == nil || resp.InputTokens != 1 || resp.Content != "" {
		t.Errorf("response = %+v, want only the reported input usage", resp)
	}
}

func TestCompleteDoesNotRetryCancelledStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	})
	_, err := provider.Complete(ctx, &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Complete() succeeded after its context was cancelled")
	}
	if pe, ok := errors.AsType[*llm.ProviderError](err); ok && pe.StatusCode == http.StatusBadGateway {
		t.Errorf("a cancelled request reported %d; cancellation must not look like a gateway failure", pe.StatusCode)
	}
}

const messageStartEvent = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"

func shortenStreamIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := streamIdleTimeout
	streamIdleTimeout = d
	t.Cleanup(func() { streamIdleTimeout = previous })
}

func TestCompleteAbandonsStalledStream(t *testing.T) {
	shortenStreamIdleTimeout(t, 200*time.Millisecond)
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, messageStartEvent)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stall without closing the connection
	})
	start := time.Now()
	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Complete() took %s on a stalled stream", elapsed)
	}
	pe, ok := errors.AsType[*llm.ProviderError](err)
	if !ok {
		t.Fatalf("error = %v (%T), want *llm.ProviderError", err, err)
	}
	if pe.StatusCode != http.StatusBadGateway || !llm.ShouldRetry(err) {
		t.Errorf("error = %q, status code = %d, retry = %t; want 502, true", pe.Message, pe.StatusCode, llm.ShouldRetry(err))
	}
	if resp == nil || resp.InputTokens != 1 || resp.Content != "" {
		t.Errorf("response = %+v, want only the reported input usage", resp)
	}
}

// Pings carry no event the SDK returns, but they show the stream is alive.
func TestCompleteKeepsStreamAliveOnPings(t *testing.T) {
	shortenStreamIdleTimeout(t, 300*time.Millisecond)
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, messageStartEvent)
		w.(http.Flusher).Flush()
		for range 8 {
			time.Sleep(100 * time.Millisecond)
			_, _ = fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			w.(http.Flusher).Flush()
		}
		_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	if _, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Complete() error = %v; pings for 800ms must keep a 300ms idle bound open", err)
	}
}

func TestCompleteRetriesStreamTimeoutEvent(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, messageStartEvent)
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"timeout_error\",\"message\":\"timed out\"}}\n\n")
	})
	_, err := provider.Complete(context.Background(), &llm.CompletionRequest{Model: "m", Messages: []llm.Message{{Role: "user", Content: "hi"}}})
	if err == nil || !llm.ShouldRetry(err) {
		t.Errorf("error = %v, retry = %t; want a retried error", err, llm.ShouldRetry(err))
	}
}

// A large max_tokens used to fail before sending and fall back to
// completeViaStream, which keeps text and tool calls in order for Responses.
func TestCompleteKeepsOutputOrderForResponses(t *testing.T) {
	provider := newStreamTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = anthropictest.WriteMessage(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[
			{"type":"text","text":"first "},{"type":"thinking","thinking":"","signature":"s"},{"type":"text","text":"look"},
			{"type":"tool_use","id":"a","name":"search","input":{"q":"a"}},
			{"type":"text","text":"then"},
			{"type":"tool_use","id":"b","name":"search","input":{"q":"b"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	for _, responses := range []bool{true, false} {
		resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{
			Model: "m", MaxTokens: 64000, ResponsesInput: responses,
			Messages: []llm.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Content != "first lookthen" || len(resp.ToolCalls) != 2 {
			t.Errorf("responses=%t: content = %q, %d tool calls", responses, resp.Content, len(resp.ToolCalls))
		}
		if !responses {
			if resp.OutputItems != nil {
				t.Errorf("OutputItems = %+v, want nil outside Responses", resp.OutputItems)
			}
			continue
		}
		var got []string
		for _, item := range resp.OutputItems {
			if item.ToolCall != nil {
				got = append(got, "call:"+item.ToolCall.ID)
			} else {
				got = append(got, "text:"+item.Content)
			}
		}
		if want := "[text:first look call:a text:then call:b]"; fmt.Sprint(got) != want {
			t.Errorf("OutputItems = %v, want %s", got, want)
		}
	}
}
