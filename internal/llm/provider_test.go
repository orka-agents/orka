/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"testing"
)

// mockProvider is a mock implementation of Provider for testing
type mockProvider struct {
	name string
}

func (p *mockProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	return &CompletionResponse{Content: "mock response"}, nil
}

func (p *mockProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 1)
	ch <- StreamChunk{Content: "mock", Done: true}
	close(ch)
	return ch, nil
}

func (p *mockProvider) Name() string {
	return p.name
}

func TestRegisterProvider(t *testing.T) {
	// Clear the registry first
	originalRegistry := providerRegistry
	providerRegistry = make(map[string]ProviderFactory)
	defer func() { providerRegistry = originalRegistry }()

	factory := func(config ProviderConfig) (Provider, error) {
		return &mockProvider{name: "test"}, nil
	}

	RegisterProvider("test-provider", factory)

	if _, ok := providerRegistry["test-provider"]; !ok {
		t.Error("RegisterProvider did not register the provider")
	}
}

func TestNewProvider(t *testing.T) {
	// Clear the registry first
	originalRegistry := providerRegistry
	providerRegistry = make(map[string]ProviderFactory)
	defer func() { providerRegistry = originalRegistry }()

	// Register a test provider
	RegisterProvider("mock", func(config ProviderConfig) (Provider, error) {
		return &mockProvider{name: "mock"}, nil
	})

	tests := []struct {
		name       string
		provider   string
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:     "known provider",
			provider: "mock",
			wantErr:  false,
		},
		{
			name:     "unknown provider",
			provider: "unknown",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := ProviderConfig{APIKey: "test-key"}
			provider, err := NewProvider(tt.provider, config)
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

func TestProviderError_Error(t *testing.T) {
	tests := []struct {
		name    string
		err     *ProviderError
		wantMsg string
	}{
		{
			name:    "simple message",
			err:     &ProviderError{Message: "test error"},
			wantMsg: "test error",
		},
		{
			name:    "with provider and code",
			err:     &ProviderError{Provider: "openai", Message: "rate limited", Code: "429"},
			wantMsg: "rate limited",
		},
		{
			name:    "unknown provider error",
			err:     ErrUnknownProvider,
			wantMsg: "unknown provider",
		},
		{
			name:    "api key required error",
			err:     ErrAPIKeyRequired,
			wantMsg: "API key is required",
		},
		{
			name:    "rate limited error",
			err:     ErrRateLimited,
			wantMsg: "rate limited",
		},
		{
			name:    "context too long error",
			err:     ErrContextTooLong,
			wantMsg: "context too long",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("ProviderError.Error() = %v, want %v", got, tt.wantMsg)
			}
		})
	}
}

func TestProviderConfig(t *testing.T) {
	config := ProviderConfig{
		APIKey:     "test-api-key",
		BaseURL:    "https://api.example.com",
		MaxRetries: 3,
		Timeout:    30,
	}

	if config.APIKey != "test-api-key" {
		t.Errorf("APIKey = %v, want %v", config.APIKey, "test-api-key")
	}
	if config.BaseURL != "https://api.example.com" {
		t.Errorf("BaseURL = %v, want %v", config.BaseURL, "https://api.example.com")
	}
	if config.MaxRetries != 3 {
		t.Errorf("MaxRetries = %v, want %v", config.MaxRetries, 3)
	}
	if config.Timeout != 30 {
		t.Errorf("Timeout = %v, want %v", config.Timeout, 30)
	}
}

func TestCompletionRequest(t *testing.T) {
	req := &CompletionRequest{
		Model: "gpt-4",
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
		SystemPrompt:  "You are helpful",
		MaxTokens:     100,
		Temperature:   0.7,
		StopSequences: []string{"END"},
	}

	if req.Model != "gpt-4" {
		t.Errorf("Model = %v, want %v", req.Model, "gpt-4")
	}
	if len(req.Messages) != 1 {
		t.Errorf("Messages length = %v, want 1", len(req.Messages))
	}
	if req.SystemPrompt != "You are helpful" {
		t.Errorf("SystemPrompt = %v, want %v", req.SystemPrompt, "You are helpful")
	}
}

func TestCompletionResponse(t *testing.T) {
	resp := &CompletionResponse{
		Content:      "Hello!",
		StopReason:   "end_turn",
		InputTokens:  10,
		OutputTokens: 5,
		Model:        "gpt-4",
	}

	if resp.Content != "Hello!" {
		t.Errorf("Content = %v, want %v", resp.Content, "Hello!")
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %v, want %v", resp.StopReason, "end_turn")
	}
	if resp.InputTokens != 10 {
		t.Errorf("InputTokens = %v, want 10", resp.InputTokens)
	}
	if resp.OutputTokens != 5 {
		t.Errorf("OutputTokens = %v, want 5", resp.OutputTokens)
	}
}

func TestMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
	}{
		{
			name: "user message",
			msg:  Message{Role: "user", Content: "Hello"},
		},
		{
			name: "assistant message",
			msg:  Message{Role: "assistant", Content: "Hi there!"},
		},
		{
			name: "tool result message",
			msg:  Message{Role: "tool", Content: "result", ToolCallID: "call_123", Name: "web_search"},
		},
		{
			name: "assistant with tool calls",
			msg: Message{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "search", Arguments: []byte(`{"query": "test"}`)},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation - ensure messages can be created
			if tt.msg.Role == "" {
				t.Error("Message role should not be empty")
			}
		})
	}
}

func TestTool(t *testing.T) {
	tool := Tool{
		Name:        "web_search",
		Description: "Search the web",
		Parameters:  []byte(`{"type": "object", "properties": {"query": {"type": "string"}}}`),
	}

	if tool.Name != "web_search" {
		t.Errorf("Name = %v, want %v", tool.Name, "web_search")
	}
	if tool.Description != "Search the web" {
		t.Errorf("Description = %v, want %v", tool.Description, "Search the web")
	}
	if len(tool.Parameters) == 0 {
		t.Error("Parameters should not be empty")
	}
}

func TestToolCall(t *testing.T) {
	tc := ToolCall{
		ID:        "call_123",
		Name:      "web_search",
		Arguments: []byte(`{"query": "test"}`),
	}

	if tc.ID != "call_123" {
		t.Errorf("ID = %v, want %v", tc.ID, "call_123")
	}
	if tc.Name != "web_search" {
		t.Errorf("Name = %v, want %v", tc.Name, "web_search")
	}
}

func TestStreamChunk(t *testing.T) {
	tests := []struct {
		name  string
		chunk StreamChunk
	}{
		{
			name:  "content chunk",
			chunk: StreamChunk{Content: "Hello"},
		},
		{
			name:  "done chunk",
			chunk: StreamChunk{Done: true},
		},
		{
			name:  "tool call chunk",
			chunk: StreamChunk{ToolCall: &ToolCall{ID: "1", Name: "test"}},
		},
		{
			name:  "error chunk",
			chunk: StreamChunk{Error: ErrRateLimited},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation
			_ = tt.chunk
		})
	}
}
