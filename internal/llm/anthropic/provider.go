/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package anthropic

import (
	"context"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/sozercan/mercan/internal/llm"
)

func init() {
	llm.RegisterProvider("anthropic", func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
}

// Provider implements the llm.Provider interface for Anthropic
type Provider struct {
	client *anthropic.Client
	config llm.ProviderConfig
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
		opts = append(opts, option.WithBaseURL(config.BaseURL))
	}

	client := anthropic.NewClient(opts...)

	return &Provider{
		client: &client,
		config: config,
	}, nil
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "anthropic"
}

// Complete sends a completion request
func (p *Provider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	// Convert messages to Anthropic format
	messages := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				// Handle tool use response
				blocks := make([]anthropic.ContentBlockParamUnion, 0)
				if msg.Content != "" {
					blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
				}
				for _, tc := range msg.ToolCalls {
					var input map[string]interface{}
					json.Unmarshal(tc.Arguments, &input)
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
				}
				messages = append(messages, anthropic.NewAssistantMessage(blocks...))
			} else {
				messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
			}
		case "tool":
			// Tool result
			messages = append(messages, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false),
			))
		}
	}

	// Build request params
	maxTokens := int64(4096)
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
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

	// Add tools if present
	if len(req.Tools) > 0 {
		tools := make([]anthropic.ToolUnionParam, 0, len(req.Tools))
		for _, tool := range req.Tools {
			var schema map[string]interface{}
			json.Unmarshal(tool.Parameters, &schema)

			var required []string
			if reqField, ok := schema["required"].([]interface{}); ok {
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
			tools = append(tools, anthropic.ToolUnionParam{OfTool: &toolParam})
		}
		params.Tools = tools
	}

	// Make the request
	message, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, &llm.ProviderError{
			Provider: "anthropic",
			Message:  err.Error(),
		}
	}

	// Convert response
	resp := &llm.CompletionResponse{
		Model:        string(message.Model),
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

// Stream sends a streaming completion request
func (p *Provider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk)

	go func() {
		defer close(ch)

		// Convert messages to Anthropic format
		messages := make([]anthropic.MessageParam, 0, len(req.Messages))
		for _, msg := range req.Messages {
			switch msg.Role {
			case "user":
				messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
			case "assistant":
				messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(msg.Content)))
			}
		}

		maxTokens := int64(4096)
		if req.MaxTokens > 0 {
			maxTokens = int64(req.MaxTokens)
		}

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(req.Model),
			Messages:  messages,
			MaxTokens: maxTokens,
		}

		if req.SystemPrompt != "" {
			params.System = []anthropic.TextBlockParam{
				{Text: req.SystemPrompt},
			}
		}

		stream := p.client.Messages.NewStreaming(ctx, params)

		for stream.Next() {
			event := stream.Current()
			switch e := event.AsAny().(type) {
			case anthropic.ContentBlockDeltaEvent:
				if delta, ok := e.Delta.AsAny().(anthropic.TextDelta); ok {
					ch <- llm.StreamChunk{Content: delta.Text}
				}
			case anthropic.MessageStopEvent:
				ch <- llm.StreamChunk{Done: true}
			}
		}

		if err := stream.Err(); err != nil {
			ch <- llm.StreamChunk{Error: err, Done: true}
		}
	}()

	return ch, nil
}

// Ensure Provider implements llm.Provider
var _ llm.Provider = (*Provider)(nil)
