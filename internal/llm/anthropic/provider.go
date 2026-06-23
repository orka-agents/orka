/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/sozercan/orka/internal/llm"
)

func init() {
	llm.RegisterProvider("anthropic", func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
}

// Provider implements the llm.Provider interface for Anthropic
type Provider struct {
	client *anthropic.Client
}

// NewProvider creates a new Anthropic provider
func NewProvider(config llm.ProviderConfig) (*Provider, error) {
	if config.APIKey == "" {
		return nil, llm.ErrAPIKeyRequired
	}

	opts := []option.RequestOption{
		option.WithAPIKey(config.APIKey),
	}

	if config.BaseURL != "" {
		// The Anthropic SDK appends "v1/messages" to the base URL, so strip
		// a trailing "/v1" or "/v1/" to avoid a doubled path segment.
		base := strings.TrimRight(config.BaseURL, "/")
		base = strings.TrimSuffix(base, "/v1")
		opts = append(opts, option.WithBaseURL(base))
	}

	client := anthropic.NewClient(opts...)

	return &Provider{
		client: &client,
	}, nil
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "anthropic"
}

func (p *Provider) TelemetryProviderName() string {
	return "anthropic"
}

// Complete sends a completion request
func (p *Provider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	messages := buildMessages(req.Messages)
	params := buildRequestParams(req, messages)

	// Make the request
	message, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, toProviderError(err)
	}

	// Convert response
	resp := &llm.CompletionResponse{
		Provider:     p.TelemetryProviderName(),
		ID:           message.ID,
		Model:        message.Model,
		StopReason:   string(message.StopReason),
		InputTokens:  int(message.Usage.InputTokens),
		OutputTokens: int(message.Usage.OutputTokens),
	}

	// Extract content and tool calls
	for _, block := range message.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			resp.Content += b.Text
		case anthropic.ToolUseBlock:
			resp.ToolCalls = append(resp.ToolCalls, llm.ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Input,
			})
		}
	}

	return resp, nil
}

// buildMessages converts llm messages to Anthropic format.
func buildMessages(msgs []llm.Message) []anthropic.MessageParam {
	messages := make([]anthropic.MessageParam, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				blocks := []anthropic.ContentBlockParamUnion{}
				if msg.Content != "" {
					blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
				}
				for _, tc := range msg.ToolCalls {
					var input any
					_ = json.Unmarshal(tc.Arguments, &input)
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
				}
				messages = append(messages, anthropic.NewAssistantMessage(blocks...))
			} else {
				messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
			}
		case "tool":
			messages = append(messages, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false),
			))
		case "system":
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
		}
	}
	return messages
}

// buildToolParams converts llm tool definitions to Anthropic tool params.
func buildToolParams(tools []llm.Tool) []anthropic.ToolUnionParam {
	params := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		var schema map[string]any
		_ = json.Unmarshal(tool.Parameters, &schema)

		var required []string
		if reqField, ok := schema["required"].([]any); ok {
			for _, r := range reqField {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
		}

		toolParam := anthropic.ToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: schema["properties"],
				Required:   required,
			},
		}
		params = append(params, anthropic.ToolUnionParam{OfTool: &toolParam})
	}
	return params
}

// buildRequestParams creates Anthropic MessageNewParams from a completion request.
func buildRequestParams(req *llm.CompletionRequest, messages []anthropic.MessageParam) anthropic.MessageNewParams {
	maxTokens := int64(4096)
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     req.Model,
		Messages:  messages,
		MaxTokens: maxTokens,
	}

	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}

	if req.Temperature > 0 {
		params.Temperature = anthropic.Float(req.Temperature)
	}

	if len(req.Tools) > 0 {
		params.Tools = buildToolParams(req.Tools)
	}

	return params
}

// handleStreamEvent processes a single stream event and sends chunks via send.
// Returns false if the context was cancelled and the caller should stop.
func handleStreamEvent(
	event anthropic.MessageStreamEventUnion,
	send func(llm.StreamChunk) bool,
	currentToolCall **llm.ToolCall,
	toolCallArgs *[]byte,
	hasToolCalls *bool,
) bool {
	switch e := event.AsAny().(type) {
	case anthropic.ContentBlockStartEvent:
		cb := e.ContentBlock
		if cb.Type == "tool_use" {
			*currentToolCall = &llm.ToolCall{
				ID:   cb.ID,
				Name: cb.Name,
			}
			*toolCallArgs = nil
			*hasToolCalls = true
		}
	case anthropic.ContentBlockDeltaEvent:
		switch delta := e.Delta.AsAny().(type) {
		case anthropic.TextDelta:
			if !send(llm.StreamChunk{Content: delta.Text}) {
				return false
			}
		case anthropic.InputJSONDelta:
			if *currentToolCall != nil {
				*toolCallArgs = append(*toolCallArgs, []byte(delta.PartialJSON)...)
			}
		}
	case anthropic.ContentBlockStopEvent:
		if *currentToolCall != nil {
			if len(*toolCallArgs) > 0 {
				(*currentToolCall).Arguments = json.RawMessage(*toolCallArgs)
			} else {
				(*currentToolCall).Arguments = json.RawMessage("{}")
			}
			if !send(llm.StreamChunk{ToolCall: *currentToolCall}) {
				return false
			}
			*currentToolCall = nil
			*toolCallArgs = nil
		}
	case anthropic.MessageDeltaEvent:
		stopReason := string(e.Delta.StopReason)
		if *hasToolCalls && stopReason == "" {
			stopReason = "tool_use"
		}
		if !send(llm.StreamChunk{Done: true, StopReason: stopReason}) {
			return false
		}
	case anthropic.MessageStopEvent:
		// Final stop if not already sent via MessageDeltaEvent
	}
	return true
}

// Stream sends a streaming completion request
func (p *Provider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk)

	go func() {
		defer close(ch)

		send := func(chunk llm.StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		messages := buildMessages(req.Messages)
		params := buildRequestParams(req, messages)
		stream := p.client.Messages.NewStreaming(ctx, params)

		var currentToolCall *llm.ToolCall
		var toolCallArgs []byte
		hasToolCalls := false

		for stream.Next() {
			if !handleStreamEvent(stream.Current(), send, &currentToolCall, &toolCallArgs, &hasToolCalls) {
				return
			}
		}

		if err := stream.Err(); err != nil {
			send(llm.StreamChunk{Error: toProviderError(err), Done: true})
		}
	}()

	return ch, nil
}

// toProviderError wraps an error as a ProviderError, extracting the HTTP status
// code from the Anthropic SDK error type when available.
func toProviderError(err error) *llm.ProviderError {
	pe := &llm.ProviderError{Provider: "anthropic", Message: err.Error()}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		pe.StatusCode = apiErr.StatusCode
	}
	return pe
}

// Ensure Provider implements llm.Provider
var _ llm.Provider = (*Provider)(nil)
