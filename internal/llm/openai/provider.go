/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/sozercan/mercan/internal/llm"
)

// apiMode tracks which API surface to use.
type apiMode int32

const (
	apiModeUnknown         apiMode = iota
	apiModeResponses               // OpenAI Responses API
	apiModeChatCompletions         // OpenAI Chat Completions API
)

const eventTypeFunctionCall = "function_call"

func init() {
	llm.RegisterProvider("openai", func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
	llm.RegisterProvider("azure-openai", func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
}

// Provider implements the llm.Provider interface for OpenAI.
// It auto-detects whether the endpoint supports the Responses API and falls
// back to Chat Completions if not.
type Provider struct {
	client openai.Client
	config llm.ProviderConfig
	mode   atomic.Int32 // apiMode
}

// NewProvider creates a new OpenAI provider
func NewProvider(config llm.ProviderConfig) (*Provider, error) {
	if config.APIKey == "" {
		return nil, llm.ErrAPIKeyRequired
	}

	var opts []option.RequestOption
	if config.ProviderType == "azure-openai" {
		apiVersion := config.AzureAPIVersion
		if apiVersion == "" {
			apiVersion = "2025-03-01-preview"
		}
		endpoint := strings.TrimRight(config.BaseURL, "/")
		opts = append(opts, azure.WithEndpoint(endpoint, apiVersion), azure.WithAPIKey(config.APIKey))
	} else {
		opts = append(opts, option.WithAPIKey(config.APIKey))
		if config.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(config.BaseURL))
		}
	}

	client := openai.NewClient(opts...)

	return &Provider{
		client: client,
		config: config,
	}, nil
}

// Name returns the provider name
func (p *Provider) Name() string {
	return "openai"
}

// isUnsupportedAPIError returns true when the error indicates the endpoint
// does not support the Responses API (HTTP 404, 405, or known error codes).
func isUnsupportedAPIError(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 404, 405:
			return true
		}
		if apiErr.Code == "unsupported_api" || apiErr.Code == "invalid_url" {
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "Not Found") ||
		strings.Contains(msg, "invalid_url")
}

// -------------------------------------------------------------------------
// Responses API helpers
// -------------------------------------------------------------------------

func convertInputItems(messages []llm.Message) responses.ResponseInputParam {
	items := make(responses.ResponseInputParam, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
				},
			})
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					items = append(items, responses.ResponseInputItemUnionParam{
						OfFunctionCall: &responses.ResponseFunctionToolCallParam{
							CallID:    tc.ID,
							Name:      tc.Name,
							Arguments: string(tc.Arguments),
						},
					})
				}
			}
			if msg.Content != "" {
				items = append(items, responses.ResponseInputItemUnionParam{
					OfMessage: &responses.EasyInputMessageParam{
						Role:    responses.EasyInputMessageRoleAssistant,
						Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
					},
				})
			}
		case "tool":
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: msg.ToolCallID,
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
						OfString: openai.String(msg.Content),
					},
				},
			})
		case "system":
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRoleDeveloper,
					Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
				},
			})
		}
	}
	return items
}

func convertResponsesTools(tools []llm.Tool) []responses.ToolUnionParam {
	rTools := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		var params map[string]any
		_ = json.Unmarshal(tool.Parameters, &params)

		rTools = append(rTools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  params,
				Strict:      openai.Bool(false),
			},
		})
	}
	return rTools
}

func (p *Provider) completeResponses(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	params := responses.ResponseNewParams{
		Model: req.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: convertInputItems(req.Messages),
		},
	}
	if req.SystemPrompt != "" {
		params.Instructions = openai.String(req.SystemPrompt)
	}
	if req.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Temperature > 0 {
		params.Temperature = openai.Float(req.Temperature)
	}
	if len(req.Tools) > 0 {
		params.Tools = convertResponsesTools(req.Tools)
	}
	if req.ResponseFormat != nil {
		params.Text = convertResponsesTextFormat(req.ResponseFormat)
	}

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return nil, toProviderError(err)
	}

	result := &llm.CompletionResponse{
		Content:      resp.OutputText(),
		StopReason:   string(resp.Status),
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
		Model:        resp.Model,
	}
	for _, item := range resp.Output {
		if item.Type == eventTypeFunctionCall {
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: json.RawMessage(item.Arguments),
			})
		}
	}
	if len(result.ToolCalls) > 0 {
		result.StopReason = "tool_calls"
	} else if result.StopReason == "completed" {
		result.StopReason = "stop"
	}
	return result, nil
}

func (p *Provider) streamResponses(ctx context.Context, req *llm.CompletionRequest) <-chan llm.StreamChunk {
	ch := make(chan llm.StreamChunk)
	go func() {
		defer close(ch)

		params := responses.ResponseNewParams{
			Model: req.Model,
			Input: responses.ResponseNewParamsInputUnion{
				OfInputItemList: convertInputItems(req.Messages),
			},
		}
		if req.SystemPrompt != "" {
			params.Instructions = openai.String(req.SystemPrompt)
		}
		if req.MaxTokens > 0 {
			params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
		}
		if len(req.Tools) > 0 {
			params.Tools = convertResponsesTools(req.Tools)
		}
		if req.ResponseFormat != nil {
			params.Text = convertResponsesTextFormat(req.ResponseFormat)
		}

		stream := p.client.Responses.NewStreaming(ctx, params)

		type funcCallState struct {
			callID string
			name   string
			args   strings.Builder
		}
		active := make(map[string]*funcCallState)

		for stream.Next() {
			evt := stream.Current()
			switch evt.Type {
			case "response.output_text.delta":
				if evt.Delta != "" {
					ch <- llm.StreamChunk{Content: evt.Delta}
				}
			case "response.function_call_arguments.delta":
				fc := active[evt.ItemID]
				if fc == nil {
					fc = &funcCallState{}
					active[evt.ItemID] = fc
				}
				fc.args.WriteString(evt.Delta)
			case "response.function_call_arguments.done":
				fc := active[evt.ItemID]
				args, name, callID := evt.Arguments, evt.Name, ""
				if fc != nil {
					callID = fc.callID
					if args == "" {
						args = fc.args.String()
					}
					if name == "" {
						name = fc.name
					}
					delete(active, evt.ItemID)
				}
				if callID == "" {
					callID = evt.ItemID
				}
				ch <- llm.StreamChunk{
					ToolCall: &llm.ToolCall{ID: callID, Name: name, Arguments: json.RawMessage(args)},
				}
			case "response.output_item.added":
				if evt.Item.Type == eventTypeFunctionCall {
					active[evt.Item.ID] = &funcCallState{callID: evt.Item.CallID, name: evt.Item.Name}
				}
			case "response.completed":
				stopReason := "stop"
				for _, item := range evt.Response.Output {
					if item.Type == eventTypeFunctionCall {
						stopReason = "tool_calls"
						break
					}
				}
				ch <- llm.StreamChunk{Done: true, StopReason: stopReason}
				return
			case "response.failed", "response.incomplete":
				ch <- llm.StreamChunk{Done: true, StopReason: evt.Type}
				return
			case "error":
				ch <- llm.StreamChunk{Error: &llm.ProviderError{Provider: "openai", Message: evt.Message}, Done: true}
				return
			}
		}
		if err := stream.Err(); err != nil {
			ch <- llm.StreamChunk{Error: toProviderError(err), Done: true}
			return
		}
		ch <- llm.StreamChunk{Done: true}
	}()
	return ch
}

// -------------------------------------------------------------------------
// Chat Completions API helpers (fallback)
// -------------------------------------------------------------------------

func convertMessages(messages []llm.Message, systemPrompt string) []openai.ChatCompletionMessageParamUnion {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if systemPrompt != "" {
		msgs = append(msgs, openai.SystemMessage(systemPrompt))
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			msgs = append(msgs, openai.SystemMessage(msg.Content))
		case "user":
			msgs = append(msgs, openai.UserMessage(msg.Content))
		case "assistant":
			m := openai.AssistantMessage(msg.Content)
			if len(msg.ToolCalls) > 0 {
				tcs := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					tcs = append(tcs, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: string(tc.Arguments),
							},
						},
					})
				}
				m.OfAssistant.ToolCalls = tcs
			}
			msgs = append(msgs, m)
		case "tool":
			msgs = append(msgs, openai.ToolMessage(msg.Content, msg.ToolCallID))
		}
	}
	return msgs
}

func convertChatTools(tools []llm.Tool) []openai.ChatCompletionToolUnionParam {
	cTools := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		var params map[string]any
		_ = json.Unmarshal(tool.Parameters, &params)
		cTools = append(cTools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        tool.Name,
			Description: openai.String(tool.Description),
			Parameters:  params,
			Strict:      openai.Bool(false),
		}))
	}
	return cTools
}

// convertResponsesTextFormat maps an llm.ResponseFormat to the Responses API text config.
func convertResponsesTextFormat(rf *llm.ResponseFormat) responses.ResponseTextConfigParam {
	var cfg responses.ResponseTextConfigParam
	switch rf.Type {
	case "json_object":
		cfg.Format = responses.ResponseFormatTextConfigUnionParam{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	case "json_schema":
		if rf.JSONSchema != nil {
			param := &responses.ResponseFormatTextJSONSchemaConfigParam{
				Name:   rf.JSONSchema.Name,
				Schema: rf.JSONSchema.Schema,
			}
			if rf.JSONSchema.Strict != nil {
				param.Strict = openai.Bool(*rf.JSONSchema.Strict)
			}
			if rf.JSONSchema.Description != "" {
				param.Description = openai.String(rf.JSONSchema.Description)
			}
			cfg.Format = responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: param,
			}
		}
	}
	return cfg
}

// convertChatResponseFormat maps an llm.ResponseFormat to the Chat Completions API response_format.
func convertChatResponseFormat(rf *llm.ResponseFormat) openai.ChatCompletionNewParamsResponseFormatUnion {
	switch rf.Type {
	case "json_object":
		return openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		}
	case "json_schema":
		if rf.JSONSchema != nil {
			schemaParam := shared.ResponseFormatJSONSchemaJSONSchemaParam{
				Name:   rf.JSONSchema.Name,
				Schema: rf.JSONSchema.Schema,
			}
			if rf.JSONSchema.Strict != nil {
				schemaParam.Strict = openai.Bool(*rf.JSONSchema.Strict)
			}
			if rf.JSONSchema.Description != "" {
				schemaParam.Description = openai.String(rf.JSONSchema.Description)
			}
			return openai.ChatCompletionNewParamsResponseFormatUnion{
				OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
					JSONSchema: schemaParam,
				},
			}
		}
	}
	return openai.ChatCompletionNewParamsResponseFormatUnion{}
}

func (p *Provider) completeChatCompletions(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	params := openai.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: convertMessages(req.Messages, req.SystemPrompt),
	}
	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.Temperature > 0 {
		params.Temperature = openai.Float(req.Temperature)
	}
	if len(req.Tools) > 0 {
		params.Tools = convertChatTools(req.Tools)
	}
	if req.ResponseFormat != nil {
		params.ResponseFormat = convertChatResponseFormat(req.ResponseFormat)
	}

	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, toProviderError(err)
	}

	result := &llm.CompletionResponse{Model: resp.Model}
	result.InputTokens = int(resp.Usage.PromptTokens)
	result.OutputTokens = int(resp.Usage.CompletionTokens)
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		result.Content = choice.Message.Content
		result.StopReason = choice.FinishReason
		for _, tc := range choice.Message.ToolCalls {
			if tc.Type == "function" {
				result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: json.RawMessage(tc.Function.Arguments),
				})
			}
		}
	}
	return result, nil
}

func (p *Provider) streamChatCompletions(ctx context.Context, req *llm.CompletionRequest) <-chan llm.StreamChunk {
	ch := make(chan llm.StreamChunk)
	go func() {
		defer close(ch)

		params := openai.ChatCompletionNewParams{
			Model:    req.Model,
			Messages: convertMessages(req.Messages, req.SystemPrompt),
		}
		if req.MaxTokens > 0 {
			params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
		}
		if len(req.Tools) > 0 {
			params.Tools = convertChatTools(req.Tools)
		}
		if req.ResponseFormat != nil {
			params.ResponseFormat = convertChatResponseFormat(req.ResponseFormat)
		}

		stream := p.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}

		for stream.Next() {
			chunk := stream.Current()

			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta
				if delta.Content != "" {
					ch <- llm.StreamChunk{Content: delta.Content}
				}
			}

			if acc.AddChunk(chunk) {
				if tc, ok := acc.JustFinishedToolCall(); ok {
					ch <- llm.StreamChunk{
						ToolCall: &llm.ToolCall{
							ID:        tc.ID,
							Name:      tc.Name,
							Arguments: json.RawMessage(tc.Arguments),
						},
					}
				}
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
				ch <- llm.StreamChunk{Done: true, StopReason: chunk.Choices[0].FinishReason}
				return
			}
		}

		if err := stream.Err(); err != nil {
			ch <- llm.StreamChunk{Error: toProviderError(err), Done: true}
			return
		}
		ch <- llm.StreamChunk{Done: true}
	}()
	return ch
}

// toProviderError wraps an error as a ProviderError, extracting the HTTP status
// code from the OpenAI SDK error type when available.
func toProviderError(err error) *llm.ProviderError {
	pe := &llm.ProviderError{Provider: "openai", Message: err.Error()}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		pe.StatusCode = apiErr.StatusCode
	}
	return pe
}

// -------------------------------------------------------------------------
// Public interface — auto-detect API surface
// -------------------------------------------------------------------------

// Complete sends a completion request, auto-detecting Responses vs Chat Completions.
func (p *Provider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	mode := apiMode(p.mode.Load())

	if mode == apiModeChatCompletions {
		return p.completeChatCompletions(ctx, req)
	}

	if mode == apiModeResponses {
		resp, err := p.completeResponses(ctx, req)
		if err != nil {
			return nil, toProviderError(err)
		}
		return resp, nil
	}

	// Unknown — probe with responses.create
	resp, err := p.completeResponses(ctx, req)
	if err == nil {
		p.mode.Store(int32(apiModeResponses))
		return resp, nil
	}
	if isUnsupportedAPIError(err) {
		p.mode.Store(int32(apiModeChatCompletions))
		return p.completeChatCompletions(ctx, req)
	}
	return nil, toProviderError(err)
}

// Stream sends a streaming completion request, auto-detecting Responses vs Chat Completions.
func (p *Provider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	mode := apiMode(p.mode.Load())

	if mode == apiModeChatCompletions {
		return p.streamChatCompletions(ctx, req), nil
	}
	if mode == apiModeResponses {
		return p.streamResponses(ctx, req), nil
	}

	// Unknown — probe with a lightweight non-streaming responses.create
	probeReq := &llm.CompletionRequest{
		Model:     req.Model,
		Messages:  []llm.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 1,
	}
	_, err := p.completeResponses(ctx, probeReq)
	if err == nil {
		p.mode.Store(int32(apiModeResponses))
		return p.streamResponses(ctx, req), nil
	}
	if isUnsupportedAPIError(err) {
		p.mode.Store(int32(apiModeChatCompletions))
		return p.streamChatCompletions(ctx, req), nil
	}
	return nil, toProviderError(err)
}

// Ensure Provider implements llm.Provider
var _ llm.Provider = (*Provider)(nil)
