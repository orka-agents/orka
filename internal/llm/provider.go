/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"encoding/json"
)

// Provider is the interface for LLM providers
type Provider interface {
	// Complete sends a completion request and returns the response
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

	// Stream sends a streaming completion request
	Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)

	// Name returns the provider name
	Name() string
}

// CompletionRequest represents a completion request
type CompletionRequest struct {
	Model         string    `json:"model"`
	Messages      []Message `json:"messages"`
	SystemPrompt  string    `json:"system_prompt,omitempty"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	Temperature   float64   `json:"temperature,omitempty"`
	Tools         []Tool    `json:"tools,omitempty"`
	StopSequences []string  `json:"stop_sequences,omitempty"`
}

// CompletionResponse represents a completion response
type CompletionResponse struct {
	Content      string     `json:"content"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	StopReason   string     `json:"stop_reason"`
	InputTokens  int        `json:"input_tokens"`
	OutputTokens int        `json:"output_tokens"`
	Model        string     `json:"model"`
}

// Message represents a chat message
type Message struct {
	Role       string     `json:"role"` // user, assistant, system, tool
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"` // For tool results
}

// Tool represents a tool definition
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ToolCall represents a tool call from the model
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// StreamChunk represents a chunk of a streaming response
type StreamChunk struct {
	Content  string    `json:"content,omitempty"`
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	Done     bool      `json:"done"`
	Error    error     `json:"error,omitempty"`
}

// ProviderConfig holds configuration for creating a provider
type ProviderConfig struct {
	APIKey     string
	BaseURL    string // Optional override for API URL
	MaxRetries int
	Timeout    int // Timeout in seconds
}

// ProviderFactory is a function that creates a provider
type ProviderFactory func(config ProviderConfig) (Provider, error)

// providerRegistry holds registered provider factories
var providerRegistry = map[string]ProviderFactory{}

// RegisterProvider registers a provider factory
func RegisterProvider(name string, factory ProviderFactory) {
	providerRegistry[name] = factory
}

// NewProvider creates a new provider based on the provider name
func NewProvider(name string, config ProviderConfig) (Provider, error) {
	factory, ok := providerRegistry[name]
	if !ok {
		return nil, ErrUnknownProvider
	}
	return factory(config)
}

// Error types
type ProviderError struct {
	Provider string
	Message  string
	Code     string
}

func (e *ProviderError) Error() string {
	return e.Message
}

var (
	ErrUnknownProvider = &ProviderError{Message: "unknown provider"}
	ErrAPIKeyRequired  = &ProviderError{Message: "API key is required"}
	ErrRateLimited     = &ProviderError{Message: "rate limited"}
	ErrContextTooLong  = &ProviderError{Message: "context too long"}
)
