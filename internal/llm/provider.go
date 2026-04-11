/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	SystemPrompt   string          `json:"system_prompt,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	Tools          []Tool          `json:"tools,omitempty"`
	StopSequences  []string        `json:"stop_sequences,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat specifies the output format the model must produce.
type ResponseFormat struct {
	Type       string            `json:"type"` // "text", "json_object", "json_schema"
	JSONSchema *JSONSchemaFormat `json:"json_schema,omitempty"`
}

// JSONSchemaFormat holds the schema details for json_schema response format.
type JSONSchemaFormat struct {
	Name        string         `json:"name"`
	Schema      map[string]any `json:"schema,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
	Description string         `json:"description,omitempty"`
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
	Content    string    `json:"content,omitempty"`
	ToolCall   *ToolCall `json:"tool_call,omitempty"`
	Done       bool      `json:"done"`
	StopReason string    `json:"stop_reason,omitempty"`
	Error      error     `json:"error,omitempty"`
}

// ProviderConfig holds configuration for creating a provider
type ProviderConfig struct {
	APIKey          string
	BaseURL         string // Optional override for API URL
	ProviderType    string // e.g. "openai", "azure-openai"
	AzureAPIVersion string // Azure OpenAI API version (e.g. "2024-02-15-preview")
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
	Provider   string
	Message    string
	StatusCode int
}

func (e *ProviderError) Error() string {
	return e.Message
}

func (e *ProviderError) IsRetryable() bool {
	switch e.StatusCode {
	case 429, 500, 502, 503, 529:
		return true
	default:
		return false
	}
}

func (e *ProviderError) IsProviderDown() bool {
	switch e.StatusCode {
	case 401, 403:
		return true
	default:
		return false
	}
}

func (e *ProviderError) IsContextTooLong() bool {
	if e.StatusCode != 400 {
		return false
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "context") || strings.Contains(msg, "token") || strings.Contains(msg, "too long") || strings.Contains(msg, "maximum")
}

// ShouldRetry reports whether the operation that produced err should be retried.
func ShouldRetry(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.IsRetryable()
	}
	return true // network errors, unknown errors → retry
}

// ShouldFallback reports whether a different provider should be tried.
func ShouldFallback(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.IsProviderDown()
	}
	return true // non-ProviderError (network) → try another provider
}

// IsContextTooLongErr reports whether err indicates the context/token limit was exceeded.
func IsContextTooLongErr(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.IsContextTooLong()
	}
	return false
}

var (
	ErrUnknownProvider = &ProviderError{Message: "unknown provider"}
	ErrAPIKeyRequired  = &ProviderError{Message: "API key is required"}
)
