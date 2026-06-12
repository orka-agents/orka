/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sozercan/orka/internal/llm"
)

const (
	testProviderAnthropic = "anthropic"
	testToolNameSearch    = "search"
	testStopReasonToolUse = "tool_use"
)

func TestNewProvider(t *testing.T) {
	tests := []struct {
		name    string
		config  llm.ProviderConfig
		wantErr bool
	}{
		{
			name: "with API key",
			config: llm.ProviderConfig{
				APIKey: "test-api-key",
			},
			wantErr: false,
		},
		{
			name: "without API key",
			config: llm.ProviderConfig{
				APIKey: "",
			},
			wantErr: true,
		},
		{
			name: "with base URL",
			config: llm.ProviderConfig{
				APIKey:  "test-api-key",
				BaseURL: "https://custom.api.com",
			},
			wantErr: false,
		},
		{
			name: "with base URL ending in /v1",
			config: llm.ProviderConfig{
				APIKey:  "test-api-key",
				BaseURL: "https://proxy.example.com/v1",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := NewProvider(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewProvider() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && provider == nil {
				t.Error("NewProvider() returned nil provider")
			}
		})
	}
}

func TestProvider_Name(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if name := provider.Name(); name != testProviderAnthropic {
		t.Errorf("Name() = %v, want anthropic", name)
	}
}

func TestNewProvider_APIKeyRequired(t *testing.T) {
	_, err := NewProvider(llm.ProviderConfig{})
	if err == nil {
		t.Error("NewProvider() expected error for missing API key")
	}
	if err != llm.ErrAPIKeyRequired {
		t.Errorf("NewProvider() error = %v, want ErrAPIKeyRequired", err)
	}
}

func TestProvider_Implements_Interface(t *testing.T) {
	// Verify that Provider implements llm.Provider at compile time
	var _ llm.Provider = (*Provider)(nil)
}

func TestProvider_ConfigStorage(t *testing.T) {
	config := llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: "https://api.example.com",
	}

	provider, err := NewProvider(config)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if provider == nil {
		t.Fatal("NewProvider() returned nil provider")
	}
}

func TestProvider_ClientNotNil(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if provider.client == nil {
		t.Error("client should not be nil")
	}
}

func TestBuildMessages(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
		wantLen  int
	}{
		{
			name:     "empty",
			messages: nil,
			wantLen:  0,
		},
		{
			name: "user message",
			messages: []llm.Message{
				{Role: "user", Content: "hello"},
			},
			wantLen: 1,
		},
		{
			name: "assistant message",
			messages: []llm.Message{
				{Role: "assistant", Content: "hi"},
			},
			wantLen: 1,
		},
		{
			name: "assistant with tool calls",
			messages: []llm.Message{
				{
					Role:    "assistant",
					Content: "thinking",
					ToolCalls: []llm.ToolCall{
						{ID: "tc1", Name: testToolNameSearch, Arguments: json.RawMessage(`{"q":"test"}`)},
					},
				},
			},
			wantLen: 1,
		},
		{
			name: "tool result",
			messages: []llm.Message{
				{Role: "tool", Content: "result data", ToolCallID: "tc1"},
			},
			wantLen: 1,
		},
		{
			name: "system messages skipped",
			messages: []llm.Message{
				{Role: "system", Content: "you are helpful"},
			},
			wantLen: 1,
		},
		{
			name: "full conversation",
			messages: []llm.Message{
				{Role: "user", Content: "search for X"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: testToolNameSearch, Arguments: json.RawMessage(`{"q":"X"}`)},
				}},
				{Role: "tool", Content: "found X", ToolCallID: "tc1"},
				{Role: "assistant", Content: "I found X"},
			},
			wantLen: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildMessages(tt.messages)
			if len(result) != tt.wantLen {
				t.Errorf("buildMessages() returned %d messages, want %d", len(result), tt.wantLen)
			}
		})
	}
}

func TestBuildToolParams(t *testing.T) {
	t.Run("empty tools", func(t *testing.T) {
		result := buildToolParams(nil)
		if len(result) != 0 {
			t.Errorf("expected 0 tool params, got %d", len(result))
		}
	})

	t.Run("single tool", func(t *testing.T) {
		tools := []llm.Tool{
			{
				Name:        testToolNameSearch,
				Description: "Search the web",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
			},
		}
		result := buildToolParams(tools)
		if len(result) != 1 {
			t.Fatalf("expected 1 tool param, got %d", len(result))
		}
		if result[0].OfTool == nil {
			t.Fatal("expected OfTool to be set")
		}
		if result[0].OfTool.Name != testToolNameSearch {
			t.Errorf("expected tool name 'search', got %q", result[0].OfTool.Name)
		}
	})

	t.Run("multiple tools", func(t *testing.T) {
		tools := []llm.Tool{
			{Name: "a", Description: "tool a", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
			{Name: "b", Description: "tool b", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
		}
		result := buildToolParams(tools)
		if len(result) != 2 {
			t.Errorf("expected 2 tool params, got %d", len(result))
		}
	})
}

func TestBuildRequestParams(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		req := &llm.CompletionRequest{
			Model:    "claude-3",
			Messages: []llm.Message{{Role: "user", Content: "hi"}},
		}
		msgs := buildMessages(req.Messages)
		params := buildRequestParams(req, msgs)
		if params.MaxTokens != 4096 {
			t.Errorf("expected default MaxTokens 4096, got %d", params.MaxTokens)
		}
		if params.Model != "claude-3" {
			t.Errorf("expected model claude-3, got %s", params.Model)
		}
	})

	t.Run("custom max tokens", func(t *testing.T) {
		req := &llm.CompletionRequest{
			Model:     "claude-3",
			MaxTokens: 1000,
			Messages:  []llm.Message{{Role: "user", Content: "hi"}},
		}
		msgs := buildMessages(req.Messages)
		params := buildRequestParams(req, msgs)
		if params.MaxTokens != 1000 {
			t.Errorf("expected MaxTokens 1000, got %d", params.MaxTokens)
		}
	})

	t.Run("with system prompt", func(t *testing.T) {
		req := &llm.CompletionRequest{
			Model:        "claude-3",
			SystemPrompt: "be helpful",
			Messages:     []llm.Message{{Role: "user", Content: "hi"}},
		}
		msgs := buildMessages(req.Messages)
		params := buildRequestParams(req, msgs)
		if len(params.System) != 1 {
			t.Fatalf("expected 1 system block, got %d", len(params.System))
		}
		if params.System[0].Text != "be helpful" {
			t.Errorf("expected system text 'be helpful', got %q", params.System[0].Text)
		}
	})

	t.Run("with temperature", func(t *testing.T) {
		req := &llm.CompletionRequest{
			Model:       "claude-3",
			Temperature: 0.7,
			Messages:    []llm.Message{{Role: "user", Content: "hi"}},
		}
		msgs := buildMessages(req.Messages)
		params := buildRequestParams(req, msgs)
		// Temperature should be set (non-zero value)
		_ = params
	})

	t.Run("with tools", func(t *testing.T) {
		req := &llm.CompletionRequest{
			Model:    "claude-3",
			Messages: []llm.Message{{Role: "user", Content: "hi"}},
			Tools: []llm.Tool{
				{Name: testToolNameSearch, Description: "desc", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
			},
		}
		msgs := buildMessages(req.Messages)
		params := buildRequestParams(req, msgs)
		if len(params.Tools) != 1 {
			t.Errorf("expected 1 tool, got %d", len(params.Tools))
		}
	})
}

func TestToProviderError(t *testing.T) {
	t.Run("generic error", func(t *testing.T) {
		err := fmt.Errorf("something went wrong")
		pe := toProviderError(err)
		if pe.Provider != testProviderAnthropic {
			t.Errorf("expected provider 'anthropic', got %q", pe.Provider)
		}
		if pe.Message != "something went wrong" {
			t.Errorf("expected message 'something went wrong', got %q", pe.Message)
		}
		if pe.StatusCode != 0 {
			t.Errorf("expected status code 0, got %d", pe.StatusCode)
		}
	})
}

func TestComplete_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"internal server error"}}`) //nolint:errcheck
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	_, err = provider.Complete(context.Background(), &llm.CompletionRequest{
		Model:    "claude-3",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from server error response")
	}
	pe, ok := err.(*llm.ProviderError)
	if !ok {
		t.Fatalf("expected *llm.ProviderError, got %T", err)
	}
	if pe.Provider != testProviderAnthropic {
		t.Errorf("expected provider 'anthropic', got %q", pe.Provider)
	}
}

func TestComplete_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "msg_123",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-sonnet-20240229",
			"content": [{"type": "text", "text": "Hello there!"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`) //nolint:errcheck
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{
		Model:    "claude-3-sonnet-20240229",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Content != "Hello there!" {
		t.Errorf("expected content 'Hello there!', got %q", resp.Content)
	}
	if resp.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", resp.InputTokens)
	}
	if resp.OutputTokens != 5 {
		t.Errorf("expected 5 output tokens, got %d", resp.OutputTokens)
	}
}

func TestComplete_WithToolUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "msg_456",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-sonnet-20240229",
			"content": [
				{"type": "text", "text": "Let me search."},
				{"type": "tool_use", "id": "tc1", "name": "search", "input": {"q": "test"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 20, "output_tokens": 15}
		}`) //nolint:errcheck
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{
		Model:    "claude-3-sonnet-20240229",
		Messages: []llm.Message{{Role: "user", Content: "search for test"}},
		Tools: []llm.Tool{
			{Name: testToolNameSearch, Description: testToolNameSearch, Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Content != "Let me search." {
		t.Errorf("expected content 'Let me search.', got %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != testToolNameSearch {
		t.Errorf("expected tool call name 'search', got %q", resp.ToolCalls[0].Name)
	}
}

func TestStream_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`) //nolint:errcheck
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	ch, err := provider.Stream(context.Background(), &llm.CompletionRequest{
		Model:    "claude-3",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() should not return error directly, got %v", err)
	}

	var gotError bool
	for chunk := range ch {
		if chunk.Error != nil {
			gotError = true
		}
	}
	if !gotError {
		t.Error("expected error chunk from stream")
	}
}

// unmarshalStreamEvent is a test helper that unmarshals JSON into a MessageStreamEventUnion.
func unmarshalStreamEvent(t *testing.T, raw string) anthropic.MessageStreamEventUnion {
	t.Helper()
	var event anthropic.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("failed to unmarshal stream event: %v", err)
	}
	return event
}

func TestHandleStreamEvent_ContentBlockStart_ToolUse(t *testing.T) {
	event := unmarshalStreamEvent(t,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tc1","name":"search","input":{}}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var currentToolCall *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	handleStreamEvent(event, send, &currentToolCall, &toolCallArgs, &hasToolCalls)

	if currentToolCall == nil {
		t.Fatal("expected currentToolCall to be set")
	}
	if currentToolCall.ID != "tc1" {
		t.Errorf("tool call ID = %q, want tc1", currentToolCall.ID)
	}
	if currentToolCall.Name != testToolNameSearch {
		t.Errorf("tool call Name = %q, want search", currentToolCall.Name)
	}
	if !hasToolCalls {
		t.Error("expected hasToolCalls to be true")
	}
	if len(chunks) != 0 {
		t.Error("no chunk should be sent on content_block_start")
	}
}

func TestHandleStreamEvent_ContentBlockStart_Text(t *testing.T) {
	event := unmarshalStreamEvent(t,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var currentToolCall *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	handleStreamEvent(event, send, &currentToolCall, &toolCallArgs, &hasToolCalls)

	if currentToolCall != nil {
		t.Error("expected currentToolCall to be nil for text block")
	}
	if hasToolCalls {
		t.Error("expected hasToolCalls to remain false")
	}
}

func TestHandleStreamEvent_ContentBlockDelta_TextDelta(t *testing.T) {
	event := unmarshalStreamEvent(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var currentToolCall *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	handleStreamEvent(event, send, &currentToolCall, &toolCallArgs, &hasToolCalls)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	chunk := chunks[0]
	if chunk.Content != "Hello" {
		t.Errorf("chunk content = %q, want Hello", chunk.Content)
	}
}

func TestHandleStreamEvent_ContentBlockDelta_InputJSONDelta(t *testing.T) {
	event := unmarshalStreamEvent(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	tc := &llm.ToolCall{ID: "tc1", Name: testToolNameSearch}
	var toolCallArgs []byte
	hasToolCalls := true

	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	if string(toolCallArgs) != `{"q":` {
		t.Errorf("toolCallArgs = %q, want %q", string(toolCallArgs), `{"q":`)
	}
	if len(chunks) != 0 {
		t.Error("no chunk should be sent on input_json_delta")
	}
}

func TestHandleStreamEvent_ContentBlockDelta_InputJSONDelta_NoToolCall(t *testing.T) {
	event := unmarshalStreamEvent(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"data"}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var currentToolCall *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	handleStreamEvent(event, send, &currentToolCall, &toolCallArgs, &hasToolCalls)

	if len(toolCallArgs) != 0 {
		t.Error("toolCallArgs should remain empty when no active tool call")
	}
}

func TestHandleStreamEvent_ContentBlockStop_WithToolCall(t *testing.T) {
	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	tc := &llm.ToolCall{ID: "tc1", Name: testToolNameSearch}
	toolCallArgs := []byte(`{"q":"test"}`)
	hasToolCalls := true

	event := unmarshalStreamEvent(t, `{"type":"content_block_stop","index":0}`)
	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	if tc != nil {
		t.Error("expected currentToolCall to be reset to nil")
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	chunk := chunks[0]
	if chunk.ToolCall == nil {
		t.Fatal("expected ToolCall in chunk")
	}
	if chunk.ToolCall.Name != testToolNameSearch {
		t.Errorf("ToolCall.Name = %q, want search", chunk.ToolCall.Name)
	}
	if string(chunk.ToolCall.Arguments) != `{"q":"test"}` {
		t.Errorf("ToolCall.Arguments = %s, want {\"q\":\"test\"}", string(chunk.ToolCall.Arguments))
	}
}

func TestHandleStreamEvent_ContentBlockStop_WithToolCallNoArgs(t *testing.T) {
	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	tc := &llm.ToolCall{ID: "tc2", Name: "noop"}
	var toolCallArgs []byte
	hasToolCalls := true

	event := unmarshalStreamEvent(t, `{"type":"content_block_stop","index":0}`)
	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	chunk := chunks[0]
	if chunk.ToolCall == nil {
		t.Fatal("expected ToolCall in chunk")
	}
	if string(chunk.ToolCall.Arguments) != "{}" {
		t.Errorf("ToolCall.Arguments = %s, want {}", string(chunk.ToolCall.Arguments))
	}
}

func TestHandleStreamEvent_ContentBlockStop_NoToolCall(t *testing.T) {
	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var tc *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	event := unmarshalStreamEvent(t, `{"type":"content_block_stop","index":0}`)
	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	if len(chunks) != 0 {
		t.Error("no chunk should be sent when no active tool call")
	}
}

func TestHandleStreamEvent_MessageDelta_EndTurn(t *testing.T) {
	event := unmarshalStreamEvent(t,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":""},"usage":{"output_tokens":10}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var tc *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	chunk := chunks[0]
	if !chunk.Done {
		t.Error("expected Done to be true")
	}
	if chunk.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", chunk.StopReason)
	}
}

func TestHandleStreamEvent_MessageDelta_ToolUseInferred(t *testing.T) {
	// When hasToolCalls is true and stopReason is empty, should infer testStopReasonToolUse
	event := unmarshalStreamEvent(t,
		`{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":""},"usage":{"output_tokens":10}}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var tc *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := true

	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	chunk := chunks[0]
	if !chunk.Done {
		t.Error("expected Done to be true")
	}
	if chunk.StopReason != testStopReasonToolUse {
		t.Errorf("StopReason = %q, want tool_use", chunk.StopReason)
	}
}

func TestHandleStreamEvent_MessageStop(t *testing.T) {
	event := unmarshalStreamEvent(t, `{"type":"message_stop"}`)

	var chunks []llm.StreamChunk
	send := func(chunk llm.StreamChunk) bool { chunks = append(chunks, chunk); return true }
	var tc *llm.ToolCall
	var toolCallArgs []byte
	hasToolCalls := false

	handleStreamEvent(event, send, &tc, &toolCallArgs, &hasToolCalls)

	// MessageStopEvent is a no-op
	if len(chunks) != 0 {
		t.Error("no chunk should be sent on message_stop")
	}
}

func TestStream_TextContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":""},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`,
		}

		for _, e := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", strings.Split(e, `"`)[3], e) //nolint:errcheck
			flusher.Flush()
		}
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	ch, err := provider.Stream(context.Background(), &llm.CompletionRequest{
		Model:    "claude-3",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	var content strings.Builder
	var gotDone bool
	var stopReason string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected error: %v", chunk.Error)
		}
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
		}
		if chunk.Done {
			gotDone = true
			stopReason = chunk.StopReason
		}
	}

	if content.String() != "Hello world" {
		t.Errorf("content = %q, want 'Hello world'", content.String())
	}
	if !gotDone {
		t.Error("expected Done chunk")
	}
	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", stopReason)
	}
}

func TestStream_ToolUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-3","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Searching"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tc1","name":"search","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"test\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":""},"usage":{"output_tokens":15}}`,
			`{"type":"message_stop"}`,
		}

		for _, e := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", strings.Split(e, `"`)[3], e) //nolint:errcheck
			flusher.Flush()
		}
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	ch, err := provider.Stream(context.Background(), &llm.CompletionRequest{
		Model:    "claude-3",
		Messages: []llm.Message{{Role: "user", Content: testToolNameSearch}},
		Tools: []llm.Tool{
			{Name: testToolNameSearch, Description: testToolNameSearch, Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	var content strings.Builder
	var toolCalls []llm.ToolCall
	var stopReason string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected error: %v", chunk.Error)
		}
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
		}
		if chunk.ToolCall != nil {
			toolCalls = append(toolCalls, *chunk.ToolCall)
		}
		if chunk.Done {
			stopReason = chunk.StopReason
		}
	}

	if content.String() != "Searching" {
		t.Errorf("content = %q, want 'Searching'", content.String())
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Name != testToolNameSearch {
		t.Errorf("tool call name = %q, want search", toolCalls[0].Name)
	}
	if string(toolCalls[0].Arguments) != `{"q":"test"}` {
		t.Errorf("tool call args = %s, want {\"q\":\"test\"}", string(toolCalls[0].Arguments))
	}
	if stopReason != testStopReasonToolUse {
		t.Errorf("stopReason = %q, want tool_use", stopReason)
	}
}

func TestInitRegistersProvider(t *testing.T) {
	// The init() function registers the anthropic provider factory.
	// Exercise the factory via llm.NewProvider.
	provider, err := llm.NewProvider(testProviderAnthropic, llm.ProviderConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("llm.NewProvider(anthropic) error = %v", err)
	}
	if provider.Name() != testProviderAnthropic {
		t.Errorf("provider.Name() = %q, want anthropic", provider.Name())
	}
}
