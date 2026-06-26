/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/tracing/genai"
)

// apiMode tracks which API surface to use.
type apiMode int32

const (
	apiModeUnknown         apiMode = iota
	apiModeResponses               // OpenAI Responses API
	apiModeChatCompletions         // OpenAI Chat Completions API
)

const (
	eventTypeFunctionCall   = "function_call"
	providerTypeOpenAI      = "openai"
	providerTypeAzureOpenAI = "azure-openai"
)

func init() {
	llm.RegisterProvider(providerTypeOpenAI, func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
	llm.RegisterProvider(providerTypeAzureOpenAI, func(config llm.ProviderConfig) (llm.Provider, error) {
		return NewProvider(config)
	})
}

// Provider implements the llm.Provider interface for OpenAI.
// It auto-detects whether the endpoint supports the Responses API and falls
// back to Chat Completions if not.
type Provider struct {
	client                              openai.Client
	baseURL                             string
	providerType                        string
	mode                                atomic.Int32 // apiMode
	allowBareResponsesForbiddenFallback bool
}

// NewProvider creates a new OpenAI provider
func NewProvider(config llm.ProviderConfig) (*Provider, error) {
	if config.APIKey == "" {
		return nil, llm.ErrAPIKeyRequired
	}

	var opts []option.RequestOption
	if config.ProviderType == providerTypeAzureOpenAI {
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

	providerType := config.ProviderType
	if providerType == "" {
		providerType = providerTypeOpenAI
	}

	return &Provider{
		client:                              client,
		baseURL:                             config.BaseURL,
		providerType:                        providerType,
		allowBareResponsesForbiddenFallback: isCustomOpenAIBaseURL(config.ProviderType, config.BaseURL),
	}, nil
}

// Name returns the provider name. It remains "openai" for Azure because
// callers historically used this as the implementation family. Use
// TelemetryProviderName for the concrete GenAI provider identity.
func (p *Provider) Name() string {
	return providerTypeOpenAI
}

func (p *Provider) TelemetryProviderName() string {
	if p.providerType == "" {
		return genai.ProviderOpenAI
	}
	return genai.NormalizeProviderName(p.providerType)
}

// isUnsupportedAPIError returns true when the error indicates the endpoint
// does not support the Responses API. Some OpenAI-compatible gateways report
// unsupported API surfaces as 403 instead of 404/405, but a plain 403 can also
// mean auth or model entitlement failure.
func isUnsupportedAPIError(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return true
		case 403:
			if isUnsupportedAPIMessage(apiErr.Code) || isUnsupportedAPIMessage(apiErr.Message) {
				return true
			}
		}
		if isUnsupportedAPIMessage(apiErr.Code) {
			return true
		}
		if apiErr.StatusCode != 0 {
			return false
		}
	}

	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.StatusCode {
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return true
		case 403:
			if isUnsupportedAPIMessage(providerErr.Message) {
				return true
			}
		}
		if isUnsupportedAPIMessage(providerErr.Message) {
			return true
		}
		if providerErr.StatusCode != 0 {
			return false
		}
	}

	msg := err.Error()
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "Not Found") ||
		isUnsupportedAPIMessage(msg)
}

func isCustomOpenAIBaseURL(providerType, baseURL string) bool {
	if providerType != "" && providerType != providerTypeOpenAI {
		return false
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return false
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return host != "" && host != "api.openai.com" && !strings.HasSuffix(host, ".api.openai.com")
}

func isBareResponsesForbiddenError(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode != 403 {
			return false
		}
		if isUnsupportedAPIMessage(apiErr.Code) || isUnsupportedAPIMessage(apiErr.Message) {
			return false
		}
		return isBareResponsesForbiddenMessage(apiErr.Error())
	}

	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.StatusCode != 403 {
			return false
		}
		if isUnsupportedAPIMessage(providerErr.Message) {
			return false
		}
		return isBareResponsesForbiddenMessage(providerErr.Message)
	}

	return isBareResponsesForbiddenMessage(err.Error())
}

func isBareResponsesForbiddenMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "forbidden") {
		return false
	}
	if !strings.Contains(msg, "/responses") {
		return false
	}
	return !strings.Contains(msg, `"message"`) &&
		!strings.Contains(msg, "permission") &&
		!strings.Contains(msg, "authorization") &&
		!strings.Contains(msg, "authentication") &&
		!strings.Contains(msg, "entitlement")
}

func (p *Provider) shouldFallbackToChatCompletions(err error) bool {
	if isUnsupportedAPIError(err) {
		return true
	}
	return p.allowBareResponsesForbiddenFallback && isBareResponsesForbiddenError(err)
}

func isUnsupportedAPIMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	return hasUnsupportedAPICode(msg)
}

func hasUnsupportedAPICode(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(msg, "unsupported_api") ||
		strings.Contains(msg, "invalid_url") ||
		strings.Contains(msg, "unsupported_api_for_model") ||
		strings.Contains(msg, "does not support /responses") ||
		strings.Contains(msg, "does not support responses") ||
		strings.Contains(msg, "responses api is not supported") ||
		strings.Contains(msg, "unsupported responses") ||
		strings.Contains(msg, "unsupported api surface")
}

func isForbiddenAPIError(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		return true
	}
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusForbidden {
		return true
	}
	return strings.Contains(err.Error(), "403 Forbidden")
}

func isCopilotResponsesHost(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "copilot") && strings.Contains(value, "responses")
}

func (p *Provider) isCopilotResponsesForbiddenError(err error) bool {
	if !isForbiddenAPIError(err) {
		return false
	}
	if isCopilotResponsesHost(p.baseURL + "/responses") {
		return true
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.Request != nil && apiErr.Request.URL != nil {
		requestURL := apiErr.Request.URL.String()
		if isCopilotResponsesHost(requestURL) {
			return true
		}
	}

	return false
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
	params := buildResponsesParams(req)

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return nil, toProviderError(err)
	}

	result := &llm.CompletionResponse{
		Provider:     p.TelemetryProviderName(),
		ID:           resp.ID,
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
				Arguments: json.RawMessage(responseOutputArguments(item.Arguments)),
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

type streamSender func(llm.StreamChunk) bool

func newStreamSender(ctx context.Context, ch chan<- llm.StreamChunk) streamSender {
	return func(chunk llm.StreamChunk) bool {
		select {
		case ch <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func buildResponsesParams(req *llm.CompletionRequest) responses.ResponseNewParams {
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

	return params
}

type responseStream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
}

type responseFuncCallState struct {
	itemID         string
	outputIndex    int64
	hasOutputIndex bool
	callID         string
	name           string
	arguments      string
	argumentsDone  bool
	args           strings.Builder
	emitted        bool
}

type responseFuncCallTracker struct {
	byItemID      map[string]*responseFuncCallState
	byOutputIndex map[int64]*responseFuncCallState
	byCallID      map[string]*responseFuncCallState
}

func newResponseFuncCallTracker() *responseFuncCallTracker {
	return &responseFuncCallTracker{
		byItemID:      make(map[string]*responseFuncCallState),
		byOutputIndex: make(map[int64]*responseFuncCallState),
		byCallID:      make(map[string]*responseFuncCallState),
	}
}

func (t *responseFuncCallTracker) register(fc *responseFuncCallState) {
	if fc == nil {
		return
	}
	if fc.itemID != "" {
		t.byItemID[fc.itemID] = fc
	}
	if fc.hasOutputIndex {
		t.byOutputIndex[fc.outputIndex] = fc
	}
	if fc.callID != "" {
		t.byCallID[fc.callID] = fc
	}
}

func (t *responseFuncCallTracker) merge(dst, src *responseFuncCallState) {
	if dst == nil || src == nil || dst == src {
		return
	}
	if dst.itemID == "" {
		dst.itemID = src.itemID
	}
	if !dst.hasOutputIndex && src.hasOutputIndex {
		dst.outputIndex = src.outputIndex
		dst.hasOutputIndex = true
	}
	if dst.callID == "" {
		dst.callID = src.callID
	}
	if dst.name == "" {
		dst.name = src.name
	}
	if dst.arguments == "" {
		dst.arguments = src.arguments
	}
	if !dst.argumentsDone {
		dst.argumentsDone = src.argumentsDone
	}
	if dst.args.Len() == 0 && src.args.Len() > 0 {
		dst.args.WriteString(src.args.String())
	}
	dst.emitted = dst.emitted || src.emitted
}

func (t *responseFuncCallTracker) get(itemID string, outputIndex int64, hasOutputIndex bool, callID string) *responseFuncCallState {
	var fc *responseFuncCallState
	if itemID != "" {
		fc = t.byItemID[itemID]
	}
	if fc == nil && callID != "" {
		fc = t.byCallID[callID]
	}
	if fc == nil && hasOutputIndex {
		fc = t.byOutputIndex[outputIndex]
	}
	if fc == nil {
		fc = &responseFuncCallState{}
	}

	if itemID != "" {
		if other := t.byItemID[itemID]; other != nil && other != fc {
			t.merge(fc, other)
		}
		fc.itemID = itemID
	}
	if hasOutputIndex {
		if other := t.byOutputIndex[outputIndex]; other != nil && other != fc {
			t.merge(fc, other)
		}
		fc.outputIndex = outputIndex
		fc.hasOutputIndex = true
	}
	if callID != "" {
		if other := t.byCallID[callID]; other != nil && other != fc {
			t.merge(fc, other)
		}
		fc.callID = callID
	}
	t.register(fc)
	return fc
}

func responseOutputArguments(arguments responses.ResponseOutputItemUnionArguments) string {
	if arguments.JSON.OfString.Valid() || arguments.OfString != "" {
		return arguments.OfString
	}
	if arguments.OfResponseToolSearchCallArguments != nil {
		data, err := json.Marshal(arguments.OfResponseToolSearchCallArguments)
		if err == nil {
			return string(data)
		}
	}
	return ""
}

func (t *responseFuncCallTracker) mergeItem(fc *responseFuncCallState, item responses.ResponseOutputItemUnion, argumentsDone bool) {
	if fc == nil {
		return
	}
	if item.ID != "" {
		fc.itemID = item.ID
	}
	if item.CallID != "" {
		fc.callID = item.CallID
	}
	if item.Name != "" {
		fc.name = item.Name
	}
	if arguments := responseOutputArguments(item.Arguments); arguments != "" {
		fc.arguments = arguments
		fc.argumentsDone = true
	} else if argumentsDone {
		if fc.arguments == "" {
			fc.arguments = fc.args.String()
		}
		fc.argumentsDone = true
	}
	t.register(fc)
}

func (t *responseFuncCallTracker) emit(fc *responseFuncCallState, send streamSender) bool {
	if fc == nil || fc.emitted || fc.name == "" || !fc.argumentsDone {
		return true
	}
	args := fc.arguments
	if args == "" {
		args = fc.args.String()
	}
	callID := fc.callID
	if callID == "" {
		callID = fc.itemID
	}
	if !send(llm.StreamChunk{
		ToolCall: &llm.ToolCall{ID: callID, Name: fc.name, Arguments: json.RawMessage(args)},
	}) {
		return false
	}
	fc.emitted = true
	return true
}

func streamResponsesEvents(stream responseStream, send streamSender) {
	tracker := newResponseFuncCallTracker()
	for stream.Next() {
		if !handleResponsesStreamEvent(stream.Current(), tracker, send) {
			return
		}
	}
	if err := stream.Err(); err != nil {
		send(llm.StreamChunk{Error: toProviderError(err), Done: true})
		return
	}
	send(llm.StreamChunk{Done: true})
}

func handleResponsesStreamEvent(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender) bool {
	switch evt.Type {
	case "response.output_text.delta":
		return handleResponseTextDelta(evt, send)
	case "response.function_call_arguments.delta":
		return handleResponseFunctionCallArgumentsDelta(evt, tracker, send)
	case "response.function_call_arguments.done":
		return handleResponseFunctionCallArgumentsDone(evt, tracker, send)
	case "response.output_item.added":
		return handleResponseOutputItem(evt, tracker, send, false)
	case "response.output_item.done":
		return handleResponseOutputItem(evt, tracker, send, true)
	case "response.completed":
		return handleResponseCompleted(evt, tracker, send)
	case "response.failed", "response.incomplete":
		send(llm.StreamChunk{Done: true, StopReason: evt.Type})
		return false
	case "error":
		send(llm.StreamChunk{Error: &llm.ProviderError{Provider: "openai", Message: evt.Message}, Done: true})
		return false
	default:
		return true
	}
}

func handleResponseTextDelta(evt responses.ResponseStreamEventUnion, send streamSender) bool {
	if evt.Delta == "" {
		return true
	}
	return send(llm.StreamChunk{Content: evt.Delta})
}

func handleResponseFunctionCallArgumentsDelta(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender) bool {
	fc := tracker.get(evt.ItemID, evt.OutputIndex, evt.JSON.OutputIndex.Valid(), "")
	if evt.Name != "" {
		fc.name = evt.Name
	}
	fc.args.WriteString(evt.Delta)
	return tracker.emit(fc, send)
}

func handleResponseFunctionCallArgumentsDone(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender) bool {
	fc := tracker.get(evt.ItemID, evt.OutputIndex, evt.JSON.OutputIndex.Valid(), "")
	if evt.Name != "" {
		fc.name = evt.Name
	}
	if evt.Arguments != "" {
		fc.arguments = evt.Arguments
	} else {
		fc.arguments = fc.args.String()
	}
	fc.argumentsDone = true
	return tracker.emit(fc, send)
}

func handleResponseOutputItem(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender, argumentsDone bool) bool {
	if evt.Item.Type != eventTypeFunctionCall {
		return true
	}
	fc := tracker.get(evt.Item.ID, evt.OutputIndex, evt.JSON.OutputIndex.Valid(), evt.Item.CallID)
	tracker.mergeItem(fc, evt.Item, argumentsDone)
	return tracker.emit(fc, send)
}

func handleResponseCompleted(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender) bool {
	stopReason := "stop"
	for i, item := range evt.Response.Output {
		if item.Type != eventTypeFunctionCall {
			continue
		}
		stopReason = "tool_calls"
		fc := tracker.get(item.ID, int64(i), true, item.CallID)
		tracker.mergeItem(fc, item, true)
		if !tracker.emit(fc, send) {
			return false
		}
	}
	send(llm.StreamChunk{Done: true, StopReason: stopReason})
	return false
}

func (p *Provider) streamResponses(ctx context.Context, req *llm.CompletionRequest) <-chan llm.StreamChunk {
	ch := make(chan llm.StreamChunk)
	go func() {
		defer close(ch)

		send := newStreamSender(ctx, ch)
		streamResponsesEvents(p.client.Responses.NewStreaming(ctx, buildResponsesParams(req)), send)
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

	result := &llm.CompletionResponse{Model: resp.Model, Provider: p.TelemetryProviderName(), ID: resp.ID}
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

		send := func(chunk llm.StreamChunk) bool {
			select {
			case ch <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

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
		if req.Temperature > 0 {
			params.Temperature = openai.Float(req.Temperature)
		}

		stream := p.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}

		for stream.Next() {
			chunk := stream.Current()

			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta
				if delta.Content != "" {
					if !send(llm.StreamChunk{Content: delta.Content}) {
						return
					}
				}
			}

			if acc.AddChunk(chunk) {
				if tc, ok := acc.JustFinishedToolCall(); ok {
					if !send(llm.StreamChunk{
						ToolCall: &llm.ToolCall{
							ID:        tc.ID,
							Name:      tc.Name,
							Arguments: json.RawMessage(tc.Arguments),
						},
					}) {
						return
					}
				}
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
				send(llm.StreamChunk{Done: true, StopReason: chunk.Choices[0].FinishReason})
				return
			}
		}

		if err := stream.Err(); err != nil {
			send(llm.StreamChunk{Error: toProviderError(err), Done: true})
			return
		}
		send(llm.StreamChunk{Done: true})
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
		if err == nil {
			return resp, nil
		}
		if isUnsupportedAPIError(err) {
			p.mode.Store(int32(apiModeChatCompletions))
			return p.completeChatCompletions(ctx, req)
		}
		if p.isCopilotResponsesForbiddenError(err) {
			return p.completeChatCompletions(ctx, req)
		}
		return nil, err
	}

	// Unknown — probe with responses.create
	resp, err := p.completeResponses(ctx, req)
	if err == nil {
		p.mode.Store(int32(apiModeResponses))
		return resp, nil
	}
	if p.shouldFallbackToChatCompletions(err) {
		p.mode.Store(int32(apiModeChatCompletions))
		return p.completeChatCompletions(ctx, req)
	}
	if p.isCopilotResponsesForbiddenError(err) {
		p.mode.Store(int32(apiModeChatCompletions))
		return p.completeChatCompletions(ctx, req)
	}
	return nil, err
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
	if p.shouldFallbackToChatCompletions(err) {
		p.mode.Store(int32(apiModeChatCompletions))
		return p.streamChatCompletions(ctx, req), nil
	}
	if p.isCopilotResponsesForbiddenError(err) {
		p.mode.Store(int32(apiModeChatCompletions))
		return p.streamChatCompletions(ctx, req), nil
	}
	return nil, toProviderError(err)
}

// Ensure Provider implements llm.Provider
var _ llm.Provider = (*Provider)(nil)
