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

package openai

import (
	"context"
	"encoding/json"
	"io"

	"github.com/sashabaranov/go-openai"

	"github.com/sozercan/mercan/internal/llm"
)

func init() {
	llm.RegisterProvider("openai", func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
}

// Provider implements the llm.Provider interface for OpenAI
type Provider struct {
	client *openai.Client
	config llm.ProviderConfig
}

// NewProvider creates a new OpenAI provider
func NewProvider(config llm.ProviderConfig) (*Provider, error) {
	if config.APIKey == "" {
		return nil, llm.ErrAPIKeyRequired
	}

	clientConfig := openai.DefaultConfig(config.APIKey)
	if config.BaseURL != "" {
		clientConfig.BaseURL = config.BaseURL
	}

	client := openai.NewClientWithConfig(clientConfig)

	return &Provider{
		client: client,
		config: config,
	}, nil
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "openai"
}

// Complete sends a completion request
func (p *Provider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	// Convert messages to OpenAI format
	messages := make([]openai.ChatCompletionMessage, 0, len(req.Messages)+1)

	// Add system prompt if present
	if req.SystemPrompt != "" {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleSystem,
			Content: req.SystemPrompt,
		})
	}

	for _, msg := range req.Messages {
		oaiMsg := openai.ChatCompletionMessage{
			Content: msg.Content,
		}

		switch msg.Role {
		case "user":
			oaiMsg.Role = openai.ChatMessageRoleUser
		case "assistant":
			oaiMsg.Role = openai.ChatMessageRoleAssistant
			// Add tool calls if present
			if len(msg.ToolCalls) > 0 {
				oaiMsg.ToolCalls = make([]openai.ToolCall, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					oaiMsg.ToolCalls = append(oaiMsg.ToolCalls, openai.ToolCall{
						ID:   tc.ID,
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      tc.Name,
							Arguments: string(tc.Arguments),
						},
					})
				}
			}
		case "tool":
			oaiMsg.Role = openai.ChatMessageRoleTool
			oaiMsg.ToolCallID = msg.ToolCallID
			oaiMsg.Name = msg.Name
		case "system":
			oaiMsg.Role = openai.ChatMessageRoleSystem
		}

		messages = append(messages, oaiMsg)
	}

	// Build request
	chatReq := openai.ChatCompletionRequest{
		Model:    req.Model,
		Messages: messages,
	}

	if req.MaxTokens > 0 {
		chatReq.MaxTokens = req.MaxTokens
	}

	if req.Temperature > 0 {
		chatReq.Temperature = float32(req.Temperature)
	}

	// Add tools if present
	if len(req.Tools) > 0 {
		tools := make([]openai.Tool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			var params map[string]interface{}
			json.Unmarshal(tool.Parameters, &params)

			tools = append(tools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  params,
				},
			})
		}
		chatReq.Tools = tools
	}

	// Make the request
	resp, err := p.client.CreateChatCompletion(ctx, chatReq)
	if err != nil {
		return nil, &llm.ProviderError{
			Provider: "openai",
			Message:  err.Error(),
		}
	}

	if len(resp.Choices) == 0 {
		return nil, &llm.ProviderError{
			Provider: "openai",
			Message:  "no choices in response",
		}
	}

	choice := resp.Choices[0]
	result := &llm.CompletionResponse{
		Content:      choice.Message.Content,
		StopReason:   string(choice.FinishReason),
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		Model:        resp.Model,
	}

	// Extract tool calls
	if len(choice.Message.ToolCalls) > 0 {
		result.ToolCalls = make([]llm.ToolCall, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(tc.Function.Arguments),
			})
		}
	}

	return result, nil
}

// Stream sends a streaming completion request
func (p *Provider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk)

	go func() {
		defer close(ch)

		// Convert messages to OpenAI format
		messages := make([]openai.ChatCompletionMessage, 0, len(req.Messages)+1)

		if req.SystemPrompt != "" {
			messages = append(messages, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: req.SystemPrompt,
			})
		}

		for _, msg := range req.Messages {
			oaiMsg := openai.ChatCompletionMessage{
				Content: msg.Content,
			}
			switch msg.Role {
			case "user":
				oaiMsg.Role = openai.ChatMessageRoleUser
			case "assistant":
				oaiMsg.Role = openai.ChatMessageRoleAssistant
			case "system":
				oaiMsg.Role = openai.ChatMessageRoleSystem
			}
			messages = append(messages, oaiMsg)
		}

		chatReq := openai.ChatCompletionRequest{
			Model:    req.Model,
			Messages: messages,
			Stream:   true,
		}

		if req.MaxTokens > 0 {
			chatReq.MaxTokens = req.MaxTokens
		}

		stream, err := p.client.CreateChatCompletionStream(ctx, chatReq)
		if err != nil {
			ch <- llm.StreamChunk{Error: err, Done: true}
			return
		}
		defer stream.Close()

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				ch <- llm.StreamChunk{Done: true}
				return
			}
			if err != nil {
				ch <- llm.StreamChunk{Error: err, Done: true}
				return
			}

			if len(resp.Choices) > 0 {
				ch <- llm.StreamChunk{
					Content: resp.Choices[0].Delta.Content,
				}
			}
		}
	}()

	return ch, nil
}

// Ensure Provider implements llm.Provider
var _ llm.Provider = (*Provider)(nil)
