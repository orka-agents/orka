/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sozercan/orka/internal/llm"
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

	if name := provider.Name(); name != "openai" {
		t.Errorf("Name() = %v, want openai", name)
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
		APIKey:     "test-key",
		BaseURL:    "https://api.example.com",
		MaxRetries: 3,
		Timeout:    30,
	}

	provider, err := NewProvider(config)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	if provider.config.APIKey != config.APIKey {
		t.Errorf("config.APIKey = %v, want %v", provider.config.APIKey, config.APIKey)
	}
	if provider.config.BaseURL != config.BaseURL {
		t.Errorf("config.BaseURL = %v, want %v", provider.config.BaseURL, config.BaseURL)
	}
}

func TestProvider_ClientNotNil(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{APIKey: "test"})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	// Verify the provider was created (client is a value type, always initialized)
	if provider == nil {
		t.Error("provider should not be nil")
	}
}

func TestIsUnsupportedAPIError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"404 in message", fmt.Errorf("got 404 Not Found"), true},
		{"Not Found in message", fmt.Errorf("Not Found"), true},
		{"invalid_url in message", fmt.Errorf("invalid_url"), true},
		{"unsupported_api_for_model in message", fmt.Errorf("unsupported_api_for_model"), true},
		{"normal error", fmt.Errorf("connection refused"), false},
		{"rate limited", fmt.Errorf("rate limited 429"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnsupportedAPIError(tt.err); got != tt.want {
				t.Errorf("isUnsupportedAPIError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConvertInputItems(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
		wantLen  int
	}{
		{"empty", nil, 0},
		{"user message", []llm.Message{{Role: "user", Content: "hi"}}, 1},
		{"system message", []llm.Message{{Role: "system", Content: "be helpful"}}, 1},
		{
			"assistant with tool calls",
			[]llm.Message{{
				Role:    "assistant",
				Content: "thinking",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
				},
			}},
			2, // function_call + assistant content message
		},
		{
			"assistant with tool calls no content",
			[]llm.Message{{
				Role: "assistant",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
				},
			}},
			1, // just function_call
		},
		{
			"tool result",
			[]llm.Message{{Role: "tool", Content: "result", ToolCallID: "tc1"}},
			1,
		},
		{
			"full conversation",
			[]llm.Message{
				{Role: "user", Content: "search"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "tc1", Name: "fn", Arguments: json.RawMessage(`{}`)}}},
				{Role: "tool", Content: "data", ToolCallID: "tc1"},
				{Role: "assistant", Content: "done"},
			},
			4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertInputItems(tt.messages)
			if len(result) != tt.wantLen {
				t.Errorf("convertInputItems() returned %d items, want %d", len(result), tt.wantLen)
			}
		})
	}
}

func TestConvertResponsesTools(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		result := convertResponsesTools(nil)
		if len(result) != 0 {
			t.Errorf("expected 0 tools, got %d", len(result))
		}
	})

	t.Run("single tool", func(t *testing.T) {
		tools := []llm.Tool{
			{
				Name:        "search",
				Description: "Search the web",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			},
		}
		result := convertResponsesTools(tools)
		if len(result) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(result))
		}
		if result[0].OfFunction == nil {
			t.Fatal("expected OfFunction to be set")
		}
		if result[0].OfFunction.Name != "search" {
			t.Errorf("expected name 'search', got %q", result[0].OfFunction.Name)
		}
	})

	t.Run("multiple tools", func(t *testing.T) {
		tools := []llm.Tool{
			{Name: "a", Description: "tool a", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "b", Description: "tool b", Parameters: json.RawMessage(`{"type":"object"}`)},
		}
		result := convertResponsesTools(tools)
		if len(result) != 2 {
			t.Errorf("expected 2 tools, got %d", len(result))
		}
	})
}

func TestConvertMessages(t *testing.T) {
	tests := []struct {
		name         string
		messages     []llm.Message
		systemPrompt string
		wantLen      int
	}{
		{"empty no system", nil, "", 0},
		{"empty with system", nil, "be helpful", 1},
		{
			"user message",
			[]llm.Message{{Role: "user", Content: "hi"}},
			"",
			1,
		},
		{
			"system in messages",
			[]llm.Message{{Role: "system", Content: "sys"}},
			"",
			1,
		},
		{
			"assistant with tool calls",
			[]llm.Message{{
				Role: "assistant",
				ToolCalls: []llm.ToolCall{
					{ID: "tc1", Name: "search", Arguments: json.RawMessage(`{"q":"test"}`)},
				},
			}},
			"",
			1,
		},
		{
			"tool result",
			[]llm.Message{{Role: "tool", Content: "data", ToolCallID: "tc1"}},
			"",
			1,
		},
		{
			"full with system prompt",
			[]llm.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
			"system",
			3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertMessages(tt.messages, tt.systemPrompt)
			if len(result) != tt.wantLen {
				t.Errorf("convertMessages() returned %d messages, want %d", len(result), tt.wantLen)
			}
		})
	}
}

func TestConvertChatTools(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		result := convertChatTools(nil)
		if len(result) != 0 {
			t.Errorf("expected 0 tools, got %d", len(result))
		}
	})

	t.Run("single tool", func(t *testing.T) {
		tools := []llm.Tool{
			{
				Name:        "search",
				Description: "Search",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			},
		}
		result := convertChatTools(tools)
		if len(result) != 1 {
			t.Errorf("expected 1 tool, got %d", len(result))
		}
	})
}

func TestToProviderError(t *testing.T) {
	t.Run("generic error", func(t *testing.T) {
		err := fmt.Errorf("something wrong")
		pe := toProviderError(err)
		if pe.Provider != "openai" {
			t.Errorf("expected provider 'openai', got %q", pe.Provider)
		}
		if pe.Message != "something wrong" {
			t.Errorf("expected message 'something wrong', got %q", pe.Message)
		}
		if pe.StatusCode != 0 {
			t.Errorf("expected status code 0, got %d", pe.StatusCode)
		}
	})
}

func TestNewProvider_AzureOpenAI(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:       "test-key",
		BaseURL:      "https://myresource.openai.azure.com",
		ProviderType: "azure-openai",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if provider == nil {
		t.Error("provider should not be nil")
	}
}

func TestNewProvider_AzureOpenAI_CustomAPIVersion(t *testing.T) {
	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:          "test-key",
		BaseURL:         "https://myresource.openai.azure.com",
		ProviderType:    "azure-openai",
		AzureAPIVersion: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if provider == nil {
		t.Error("provider should not be nil")
	}
}

func TestComplete_ChatCompletions_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return 404 for Responses API probe, then handle Chat Completions
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"Not Found","type":"invalid_request_error","code":"invalid_url"}}`)
			return
		}
		fmt.Fprint(w, `{
			"id": "chatcmpl-123",
			"object": "chat.completion",
			"model": "gpt-4",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Hello!"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`)
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	// Force chat completions mode
	provider.mode.Store(int32(apiModeChatCompletions))

	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{
		Model:    "gpt-4",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("expected content 'Hello!', got %q", resp.Content)
	}
	if resp.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", resp.InputTokens)
	}
}

func TestComplete_ChatCompletions_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"server error","type":"server_error"}}`)
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	// Force chat completions mode
	provider.mode.Store(int32(apiModeChatCompletions))

	_, err = provider.Complete(context.Background(), &llm.CompletionRequest{
		Model:    "gpt-4",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestComplete_ChatCompletions_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "chatcmpl-456",
			"object": "chat.completion",
			"model": "gpt-4",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "search", "arguments": "{\"q\":\"test\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30}
		}`)
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	provider.mode.Store(int32(apiModeChatCompletions))

	resp, err := provider.Complete(context.Background(), &llm.CompletionRequest{
		Model:    "gpt-4",
		Messages: []llm.Message{{Role: "user", Content: "search for test"}},
		Tools: []llm.Tool{
			{Name: "search", Description: "search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "search" {
		t.Errorf("expected tool call name 'search', got %q", resp.ToolCalls[0].Name)
	}
}

func TestComplete_AutoDetect_FallbackToChatCompletions(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"Not Found","type":"invalid_request_error","code":"invalid_url"}}`)
			return
		}
		fmt.Fprint(w, `{
			"id": "chatcmpl-789",
			"object": "chat.completion",
			"model": "gpt-4",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Fallback works!"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
		}`)
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
		Model:    "gpt-4",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Content != "Fallback works!" {
		t.Errorf("expected content 'Fallback works!', got %q", resp.Content)
	}
	// Should have tried responses first, then chat completions
	if apiMode(provider.mode.Load()) != apiModeChatCompletions {
		t.Error("expected mode to be set to apiModeChatCompletions after fallback")
	}
}

func TestStream_ChatCompletions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}

	provider.mode.Store(int32(apiModeChatCompletions))

	ch, err := provider.Stream(context.Background(), &llm.CompletionRequest{
		Model:    "gpt-4",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	var content string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected error: %v", chunk.Error)
		}
		content += chunk.Content
	}
	if content != "Hi there" {
		t.Errorf("expected 'Hi there', got %q", content)
	}
}
