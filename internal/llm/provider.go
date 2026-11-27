/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type Provider interface {
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

	Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)

	Name() string
}

type CompletionRequest struct {
	Model              string          `json:"model"`
	Messages           []Message       `json:"messages"`
	SystemPrompt       string          `json:"system_prompt,omitempty"`
	SystemPromptStable int             `json:"-"`
	MaxTokens          int             `json:"max_tokens,omitempty"`
	Temperature        float64         `json:"temperature,omitempty"`
	TemperatureSet     bool            `json:"-"`
	Tools              []Tool          `json:"tools,omitempty"`
	StopSequences      []string        `json:"stop_sequences,omitempty"`
	ResponseFormat     *ResponseFormat `json:"response_format,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	ResponsesInput     bool            `json:"-"`
}

func (r *CompletionRequest) HasTemperature() bool {
	return r.TemperatureSet || r.Temperature > 0
}

type ResponseFormat struct {
	Type       string            `json:"type"`
	JSONSchema *JSONSchemaFormat `json:"json_schema,omitempty"`
}

type JSONSchemaFormat struct {
	Name        string         `json:"name"`
	Schema      map[string]any `json:"schema,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
	Description string         `json:"description,omitempty"`
}

type AssistantOutputItem struct {
	Content  string
	ToolCall *ToolCall
	Status   string
}

type CompletionResponse struct {
	OutputItems           []AssistantOutputItem `json:"-"`
	Content               string                `json:"content"`
	ToolCalls             []ToolCall            `json:"tool_calls,omitempty"`
	StopReason            string                `json:"stop_reason"`
	InputTokens           int                   `json:"input_tokens"`
	OutputTokens          int                   `json:"output_tokens"`
	CachedInputTokens     *int64                `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens *int64                `json:"cache_write_input_tokens,omitempty"`
	InputExcludesCache    bool                  `json:"-"`
	UsageReported         bool                  `json:"usage_reported,omitempty"`
	Model                 string                `json:"model"`
	Provider              string                `json:"provider,omitempty"`
	ID                    string                `json:"id,omitempty"`
}

type CompletionOutcome string

const (
	CompletionOutcomeCompleted  CompletionOutcome = "completed"
	CompletionOutcomeToolCalls  CompletionOutcome = "tool_calls"
	CompletionOutcomeIncomplete CompletionOutcome = "incomplete"
	CompletionOutcomeRefused    CompletionOutcome = "refused"
	CompletionOutcomeFailed     CompletionOutcome = "failed"
	CompletionOutcomeUnknown    CompletionOutcome = "unknown"
)

func NormalizeCompletionOutcome(resp *CompletionResponse) CompletionOutcome {
	if resp == nil {
		return CompletionOutcomeUnknown
	}

	outcome := completionOutcomeForStopReason(resp.StopReason)
	switch outcome {
	case CompletionOutcomeCompleted:
		if len(resp.ToolCalls) > 0 {
			return CompletionOutcomeToolCalls
		}
	case CompletionOutcomeToolCalls:
		if len(resp.ToolCalls) == 0 {
			return CompletionOutcomeUnknown
		}
	}
	return outcome
}

func completionOutcomeForStopReason(reason string) CompletionOutcome {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "stop", "end_turn", "stop_sequence", "completed", "response.completed":
		return CompletionOutcomeCompleted
	case "tool_use", "tool_calls", "function_call":
		return CompletionOutcomeToolCalls
	case "length", "max_tokens", "incomplete", "response.incomplete", "pause_turn":
		return CompletionOutcomeIncomplete
	case "content_filter", "refusal", "refused":
		return CompletionOutcomeRefused
	case "failed", "response.failed", "cancelled", "canceled", "response.cancelled", "response.canceled":
		return CompletionOutcomeFailed
	default:
		return CompletionOutcomeUnknown
	}
}

type Message struct {
	OutputItems []AssistantOutputItem `json:"-"`
	Role        string                `json:"role"`
	Content     string                `json:"content,omitempty"`
	ToolCalls   []ToolCall            `json:"tool_calls,omitempty"`
	ToolCallID  string                `json:"tool_call_id,omitempty"`
	Name        string                `json:"name,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type StreamChunk struct {
	OutputIndex           *int64    `json:"-"`
	OutputItemDone        bool      `json:"-"`
	OutputItemStatus      string    `json:"-"`
	Content               string    `json:"content,omitempty"`
	ToolCall              *ToolCall `json:"tool_call,omitempty"`
	Done                  bool      `json:"done"`
	StopReason            string    `json:"stop_reason,omitempty"`
	Provider              string    `json:"provider,omitempty"`
	Model                 string    `json:"model,omitempty"`
	InputTokens           int       `json:"input_tokens,omitempty"`
	OutputTokens          int       `json:"output_tokens,omitempty"`
	CachedInputTokens     *int64    `json:"cached_input_tokens,omitempty"`
	CacheWriteInputTokens *int64    `json:"cache_write_input_tokens,omitempty"`
	InputExcludesCache    bool      `json:"-"`
	UsageReported         bool      `json:"usage_reported,omitempty"`
	Error                 error     `json:"error,omitempty"`
}

func (c *StreamChunk) PromptTokens() int {
	return promptTokens(c.InputTokens, c.CachedInputTokens, c.CacheWriteInputTokens, c.InputExcludesCache)
}

func (r *CompletionResponse) PromptTokens() int {
	return promptTokens(r.InputTokens, r.CachedInputTokens, r.CacheWriteInputTokens, r.InputExcludesCache)
}

func promptTokens(input int, cached, cacheWrite *int64, excludesCache bool) int {
	if excludesCache {
		for _, n := range []*int64{cached, cacheWrite} {
			if n != nil {
				input += int(*n)
			}
		}
	}
	return input
}

type ProviderConfig struct {
	APIKey              string
	BearerToken         BearerTokenSource
	Relay               *ControllerRelay
	BaseURL             string
	ProviderType        string
	AzureAPIVersion     string
	AzureDeploymentName string
	HTTPTransport       http.RoundTripper
}

type ProviderFactory func(config ProviderConfig) (Provider, error)

var providerRegistry = map[string]ProviderFactory{}

func RegisterProvider(name string, factory ProviderFactory) {
	providerRegistry[name] = factory
}

func NewProvider(name string, config ProviderConfig) (Provider, error) {
	factory, ok := providerRegistry[name]
	if !ok {
		return nil, ErrUnknownProvider
	}
	provider, err := factory(config)
	if err != nil {
		return nil, err
	}
	return &usageProvider{Provider: provider}, nil
}

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

func ShouldRetry(err error) bool {
	if IsUsagePersistenceError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if pe, ok := errors.AsType[*ProviderError](err); ok {
		return pe.IsRetryable()
	}
	return true
}

func ShouldFallback(err error) bool {
	if IsUsagePersistenceError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if pe, ok := errors.AsType[*ProviderError](err); ok {
		return pe.IsProviderDown()
	}
	return true
}

func IsContextTooLongErr(err error) bool {
	if IsUsagePersistenceError(err) {
		return false
	}
	if pe, ok := errors.AsType[*ProviderError](err); ok {
		return pe.IsContextTooLong()
	}
	return false
}

var (
	ErrUnknownProvider = &ProviderError{Message: "unknown provider"}
	ErrAPIKeyRequired  = &ProviderError{Message: "API key is required"}
)
