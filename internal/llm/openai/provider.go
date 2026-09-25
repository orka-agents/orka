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

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tracing/genai"
)

const (
	messageRoleUser = "user"
)

// apiMode tracks which API surface to use.
type apiMode int32

const (
	apiModeUnknown         apiMode = iota
	apiModeResponses               // OpenAI Responses API
	apiModeChatCompletions         // OpenAI Chat Completions API
)

const (
	eventTypeResponseOutputTextDelta            = "response.output_text.delta"
	eventTypeResponseContentPartAdded           = "response.content_part.added"
	eventTypeResponseContentPartDone            = "response.content_part.done"
	eventTypeResponseFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	eventTypeResponseFunctionCallArgumentsDone  = "response.function_call_arguments.done"
	responseFunctionArgumentsChanged            = "response function arguments changed after completion"
	responseContentTypeOutputText               = "output_text"
	eventTypeResponseOutputItemAdded            = "response.output_item.added"
	eventTypeResponseOutputItemDone             = "response.output_item.done"
	eventTypeResponseCompleted                  = "response.completed"
	eventTypeFunctionCall                       = "function_call"
	responseOutputTypeMessage                   = "message"
	providerTypeOpenAI                          = "openai"
	providerTypeAzureOpenAI                     = "azure-openai"
	eventTypeResponseIncomplete                 = "response.incomplete"
	incompleteReasonMaxOutputTokens             = "max_output_tokens"
	stopReasonCompleted                         = "completed"
	stopReasonFunctionCall                      = "function_call"
	stopReasonIncomplete                        = "incomplete"
	stopReasonLength                            = "length"
	stopReasonRefusal                           = "refusal"
	stopReasonStop                              = "stop"
	stopReasonToolCalls                         = "tool_calls"
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

	opts := []option.RequestOption{option.WithMiddleware(llm.UsageHTTPMiddleware)}
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
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
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

	if providerErr, ok := errors.AsType[*llm.ProviderError](err); ok {
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
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		if apiErr.StatusCode != 403 {
			return false
		}
		if isUnsupportedAPIMessage(apiErr.Code) || isUnsupportedAPIMessage(apiErr.Message) {
			return false
		}
		return isBareResponsesForbiddenMessage(apiErr.Error())
	}

	if providerErr, ok := errors.AsType[*llm.ProviderError](err); ok {
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
		case messageRoleUser:
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
				},
			})
		case "assistant":
			if msg.OutputItems != nil {
				for _, item := range msg.OutputItems {
					if item.ToolCall != nil {
						items = appendResponsesInputCall(items, *item.ToolCall)
					} else if item.Content != "" {
						items = appendResponsesInputText(items, item.Content)
					}
				}
				continue
			}
			for _, tc := range msg.ToolCalls {
				items = appendResponsesInputCall(items, tc)
			}
			if msg.Content != "" {
				items = appendResponsesInputText(items, msg.Content)
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

func appendResponsesInputCall(items responses.ResponseInputParam, call llm.ToolCall) responses.ResponseInputParam {
	return append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: &responses.ResponseFunctionToolCallParam{
		CallID: call.ID, Name: call.Name, Arguments: string(call.Arguments),
	}})
}

func appendResponsesInputText(items responses.ResponseInputParam, content string) responses.ResponseInputParam {
	return append(items, responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
		Role:    responses.EasyInputMessageRoleAssistant,
		Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(content)},
	}})
}

func convertResponsesTools(tools []llm.Tool) []responses.ToolUnionParam {
	rTools := make([]responses.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		var params map[string]any
		_ = json.Unmarshal(tool.Parameters, &params)

		strict := false
		if tool.Strict != nil {
			strict = *tool.Strict
		}
		rTools = append(rTools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  params,
				Strict:      openai.Bool(strict),
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

	if req.ResponsesInput {
		if err := validateResponsesOutput(resp.Output); err != nil {
			return nil, err
		}
	}

	result := &llm.CompletionResponse{
		Provider:              p.TelemetryProviderName(),
		ID:                    resp.ID,
		Content:               resp.OutputText(),
		StopReason:            string(resp.Status),
		InputTokens:           int(resp.Usage.InputTokens),
		OutputTokens:          int(resp.Usage.OutputTokens),
		UsageReported:         resp.Usage.JSON.InputTokens.Valid() && resp.Usage.JSON.OutputTokens.Valid(),
		CachedInputTokens:     llm.ReportedTokenCount(resp.Usage.InputTokensDetails.CachedTokens, resp.Usage.InputTokensDetails.JSON.CachedTokens.Valid()),
		CacheWriteInputTokens: llm.ReportedTokenCount(resp.Usage.InputTokensDetails.CacheWriteTokens, resp.Usage.InputTokensDetails.JSON.CacheWriteTokens.Valid()),
		Model:                 resp.Model,
	}
	if req.ResponsesInput {
		result.OutputItems = make([]llm.AssistantOutputItem, 0, len(resp.Output))
	}
	for _, item := range resp.Output {
		if item.Type == eventTypeFunctionCall {
			call := llm.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(responseOutputArguments(item.Arguments))}
			result.ToolCalls = append(result.ToolCalls, call)
			if req.ResponsesInput {
				result.OutputItems = append(result.OutputItems, llm.AssistantOutputItem{ToolCall: &call})
			}
		} else if req.ResponsesInput && item.Type == responseOutputTypeMessage {
			var content strings.Builder
			for _, part := range item.Content {
				if part.Type == responseContentTypeOutputText {
					content.WriteString(part.Text)
				}
			}
			result.OutputItems = append(result.OutputItems, llm.AssistantOutputItem{Content: content.String(), Status: item.Status})
		}
	}
	result.StopReason = normalizeResponsesStopReason(result.StopReason, resp.Output, false)
	result.StopReason = normalizeResponsesIncompleteStopReason(result.StopReason, resp.IncompleteDetails.Reason)
	if req.ResponsesInput && responsesHaveRefusal(resp.Output) {
		result.StopReason = stopReasonRefusal
	}
	return result, nil
}

// normalizeResponsesIncompleteStopReason maps output-budget exhaustion to the
// provider-neutral token-limit reason while preserving other incomplete states.
func normalizeResponsesIncompleteStopReason(stopReason, incompleteReason string) string {
	if incompleteReason == incompleteReasonMaxOutputTokens &&
		(stopReason == stopReasonIncomplete || stopReason == eventTypeResponseIncomplete) {
		return stopReasonLength
	}
	return stopReason
}

func normalizeResponsesStopReason(
	status string,
	output []responses.ResponseOutputItemUnion,
	blankIsIncomplete bool,
) string {
	status = strings.TrimSpace(status)
	if status != stopReasonCompleted {
		return status
	}

	hasToolCalls := false
	for _, item := range output {
		if item.Status != "" && item.Status != stopReasonCompleted {
			return item.Status
		}
		if item.Type == responseOutputTypeMessage {
			for _, content := range item.Content {
				if content.Type == stopReasonRefusal {
					return stopReasonRefusal
				}
			}
		}
		if item.Type == eventTypeFunctionCall {
			hasToolCalls = true
		}
	}
	if hasToolCalls {
		return stopReasonToolCalls
	}
	if blankIsIncomplete {
		// Streaming callers do not have the agent loop's tool-free final-answer retry state.
		return stopReasonIncomplete
	}
	if status == stopReasonCompleted {
		return stopReasonStop
	}
	return status
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
	if req.Store != nil {
		params.Store = openai.Bool(*req.Store)
	}
	if req.SystemPrompt != "" {
		params.Instructions = openai.String(req.SystemPrompt)
	}
	if req.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.HasTemperature() {
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
	itemDone       bool
	args           strings.Builder
	emitted        bool
}

type responseFuncCallTracker struct {
	ordered       bool
	outputOrder   responseOutputOrder
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
	dst.itemDone = dst.itemDone || src.itemDone
	if src.args.Len() > 0 && (dst.args.Len() == 0 || (t.ordered && src.args.Len() > dst.args.Len())) {
		// In ordered mode, getChecked verified compatible prefixes. Retain
		// the longest observed prefix, including when item metadata is late.
		dst.args.Reset()
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

func (t *responseFuncCallTracker) mergeItem(fc *responseFuncCallState, item responses.ResponseOutputItemUnion, itemDone bool) error {
	if fc == nil {
		return nil
	}
	arguments := responseOutputArguments(item.Arguments)
	hasArguments := arguments != "" || item.JSON.Arguments.Raw() != ""
	prefixOnly := t.ordered && !itemDone && item.Status != stopReasonCompleted
	if prefixOnly {
		partial := &responseFuncCallState{name: item.Name}
		partial.args.WriteString(arguments)
		if err := validateResponseFunctionMerge(fc, partial); err != nil {
			return err
		}
	} else if err := t.validateSnapshot(fc, item.Name, arguments, hasArguments); err != nil {
		return err
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
	if arguments != "" || (t.ordered && hasArguments && !prefixOnly) {
		if prefixOnly {
			if len(arguments) > fc.args.Len() {
				fc.args.Reset()
				fc.args.WriteString(arguments)
			}
		} else {
			fc.arguments = arguments
			fc.argumentsDone = true
		}
	} else if itemDone || (t.ordered && item.Status == stopReasonCompleted) {
		if fc.arguments == "" {
			fc.arguments = fc.args.String()
		}
		fc.argumentsDone = true
	}
	fc.itemDone = fc.itemDone || itemDone || item.Status == stopReasonCompleted
	t.register(fc)
	return nil
}

func (t *responseFuncCallTracker) emit(fc *responseFuncCallState, send streamSender) bool {
	// Finalized arguments do not establish that a call finished successfully.
	// Responses callers may execute as soon as we publish the completed item.
	if fc == nil || fc.emitted || fc.name == "" || !fc.argumentsDone || (t.ordered && (!fc.itemDone || !fc.hasOutputIndex || fc.callID == "")) {
		return true
	}
	args := fc.arguments
	if args == "" {
		args = fc.args.String()
	}
	if t.ordered {
		// Recovered calls must satisfy the same argument contract as calls
		// supplied in a complete response, before coordinator processing.
		var arguments map[string]any
		if json.Unmarshal([]byte(args), &arguments) != nil || arguments == nil {
			return failResponsesStream(send, errors.New("provider returned an invalid Responses function call"))
		}
	}
	callID := fc.callID
	if callID == "" {
		callID = fc.itemID
	}
	chunk := llm.StreamChunk{ToolCall: &llm.ToolCall{ID: callID, Name: fc.name, Arguments: json.RawMessage(args)}}
	if t.ordered {
		index := fc.outputIndex
		chunk.OutputIndex = &index
	}
	if !send(chunk) {
		return false
	}
	fc.emitted = true
	return true
}

func (t *responseFuncCallTracker) hasUnemittedFunctionCall(output []responses.ResponseOutputItemUnion) bool {
	seen := make(map[*responseFuncCallState]struct{})
	pending := func(fc *responseFuncCallState) bool {
		if fc == nil {
			return false
		}
		if _, ok := seen[fc]; ok {
			return false
		}
		seen[fc] = struct{}{}
		return !fc.emitted
	}
	for _, fc := range t.byItemID {
		if pending(fc) {
			return true
		}
	}
	for _, fc := range t.byCallID {
		if pending(fc) {
			return true
		}
	}
	for _, fc := range t.byOutputIndex {
		if pending(fc) {
			return true
		}
	}
	for i, item := range output {
		if item.Type != eventTypeFunctionCall {
			continue
		}
		fc := t.get(item.ID, int64(i), true, item.CallID)
		if !fc.emitted {
			return true
		}
	}
	return false
}

func streamResponsesEvents(stream responseStream, providerName string, send streamSender, ordered ...bool) {
	tracker := newResponseFuncCallTracker()
	tracker.ordered = len(ordered) > 0 && ordered[0]
	var queue *orderedResponseSender
	if tracker.ordered {
		queue = &orderedResponseSender{send: send, order: &tracker.outputOrder, pending: map[int64][]llm.StreamChunk{}}
		send = queue.chunk
	}
	for stream.Next() {
		evt := stream.Current()
		if queue != nil && (evt.Type == eventTypeResponseCompleted || evt.Type == eventTypeResponseIncomplete) {
			queue.terminal(func(send streamSender) {
				handleResponsesStreamEvent(evt, tracker, providerName, send)
			})
			return
		}
		if !handleResponsesStreamEvent(evt, tracker, providerName, send) {
			return
		}
	}
	if err := stream.Err(); err != nil {
		send(llm.StreamChunk{Error: toProviderError(err), Done: true})
		return
	}
	send(llm.StreamChunk{Done: true})
}

func handleResponsesStreamEvent(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, providerName string, send streamSender) bool {
	var outputIndex *int64
	if tracker.ordered {
		if responsesEventHasRefusal(evt) {
			send(llm.StreamChunk{Done: true, StopReason: stopReasonRefusal})
			return false
		}
		if err := tracker.prepareEvent(&evt); err != nil {
			return failResponsesStream(send, err)
		}
		var err error
		outputIndex, err = tracker.outputOrder.eventIndex(evt)
		if err != nil {
			send(llm.StreamChunk{Error: err, Done: true})
			return false
		}
		if isResponseTextEvent(evt) {
			return tracker.outputOrder.textEvent(evt, outputIndex, send)
		}
	}
	switch evt.Type {
	case eventTypeResponseOutputTextDelta:
		return handleResponseTextDelta(evt, send, outputIndex)
	case eventTypeResponseFunctionCallArgumentsDelta:
		return handleResponseFunctionCallArgumentsDelta(evt, tracker, send)
	case eventTypeResponseFunctionCallArgumentsDone:
		return handleResponseFunctionCallArgumentsDone(evt, tracker, send)
	case eventTypeResponseOutputItemAdded:
		return handleResponseOutputItem(evt, tracker, send, false, outputIndex)
	case eventTypeResponseOutputItemDone:
		return handleResponseOutputItem(evt, tracker, send, true, outputIndex)
	case eventTypeResponseCompleted:
		return handleResponseCompleted(evt, tracker, providerName, send)
	case "response.failed":
		stopReason := normalizeResponsesIncompleteStopReason(evt.Type, evt.Response.IncompleteDetails.Reason)
		send(responseTerminalChunk(evt, stopReason, providerName))
		return false
	case eventTypeResponseIncomplete:
		stopReason := normalizeResponsesIncompleteStopReason(evt.Type, evt.Response.IncompleteDetails.Reason)
		if tracker.hasUnemittedFunctionCall(evt.Response.Output) {
			stopReason = eventTypeResponseIncomplete
		}
		if tracker.ordered {
			// Only token-budget text outcomes are supported. Decide this
			// before message snapshots can release queued function calls.
			hasCalls := len(tracker.byItemID) > 0 || len(tracker.byCallID) > 0 || len(tracker.byOutputIndex) > 0
			if stopReason != stopReasonLength || hasCalls {
				stopReason = eventTypeResponseIncomplete
			} else if !tracker.outputOrder.completeMessages(evt, send) {
				return false
			}
		}
		send(responseTerminalChunk(evt, stopReason, providerName))
		return false
	case "error":
		send(llm.StreamChunk{Error: &llm.ProviderError{Provider: "openai", Message: evt.Message}, Done: true})
		return false
	default:
		return true
	}
}

func handleResponseTextDelta(evt responses.ResponseStreamEventUnion, send streamSender, outputIndex *int64) bool {
	if evt.Delta == "" {
		return true
	}
	chunk := llm.StreamChunk{Content: evt.Delta, OutputIndex: outputIndex}
	return send(chunk)
}

func handleResponseFunctionCallArgumentsDelta(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender) bool {
	fc, err := tracker.getChecked(evt.ItemID, evt.OutputIndex, evt.JSON.OutputIndex.Valid(), "")
	if err != nil {
		return failResponsesStream(send, err)
	}
	if err := tracker.validateSnapshot(fc, evt.Name, "", false); err != nil {
		return failResponsesStream(send, err)
	}
	if tracker.ordered && fc.argumentsDone && evt.Delta != "" {
		return failResponsesStream(send, errors.New(responseFunctionArgumentsChanged))
	}
	if evt.Name != "" {
		fc.name = evt.Name
	}
	fc.args.WriteString(evt.Delta)
	return tracker.emit(fc, send)
}

func handleResponseFunctionCallArgumentsDone(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender) bool {
	fc, err := tracker.getChecked(evt.ItemID, evt.OutputIndex, evt.JSON.OutputIndex.Valid(), "")
	if err != nil {
		return failResponsesStream(send, err)
	}
	arguments := evt.Arguments
	// An omitted final field may recover prior data; an explicit empty
	// snapshot must still agree with every observed argument fragment.
	if arguments == "" && (!tracker.ordered || evt.JSON.Arguments.Raw() == "") {
		if tracker.ordered && fc.argumentsDone {
			arguments = fc.arguments
		} else {
			arguments = fc.args.String()
		}
	}
	if err := tracker.validateSnapshot(fc, evt.Name, arguments, true); err != nil {
		return failResponsesStream(send, err)
	}
	if evt.Name != "" {
		fc.name = evt.Name
	}
	fc.arguments = arguments
	fc.argumentsDone = true
	return tracker.emit(fc, send)
}

func handleResponseOutputItem(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, send streamSender, itemDone bool, outputIndex *int64) bool {
	if itemDone && tracker.ordered && evt.Item.Type == responseOutputTypeMessage {
		if evt.Item.Status == "" {
			evt.Item.Status = tracker.outputOrder.messageStatus(outputIndex)
		}
		if err := tracker.outputOrder.observeStatus(evt.Item.ID, outputIndex, evt.Item.Status); err != nil {
			return failResponsesStream(send, err)
		}
	}
	if evt.Item.Type == eventTypeFunctionCall {
		fc, err := tracker.getChecked(evt.Item.ID, evt.OutputIndex, evt.JSON.OutputIndex.Valid(), evt.Item.CallID)
		if err != nil {
			return failResponsesStream(send, err)
		}
		if err := tracker.mergeItem(fc, evt.Item, itemDone); err != nil {
			return failResponsesStream(send, err)
		}
		if !tracker.emit(fc, send) {
			return false
		}
		// A compatible upstream may supply the call ID or name only in its
		// terminal snapshot. Keep this position open until the call is ready.
		if tracker.ordered && !fc.emitted {
			return true
		}
	}
	if tracker.ordered && evt.Item.Type == responseOutputTypeMessage && (itemDone || len(evt.Item.Content) > 0) {
		if !tracker.outputOrder.messageSnapshot(evt.Item, outputIndex, itemDone, send) {
			return false
		}
		// The content is final, but an omitted status remains unresolved
		// until the terminal outcome can supply it without rewriting SSE.
		if itemDone && evt.Item.Status == "" {
			return true
		}
	}
	if itemDone && (outputIndex != nil || (tracker.ordered && evt.Item.Type == responseOutputTypeMessage)) {
		return send(llm.StreamChunk{OutputIndex: outputIndex, OutputItemDone: true, OutputItemStatus: evt.Item.Status})
	}
	return true
}

func handleResponseCompleted(evt responses.ResponseStreamEventUnion, tracker *responseFuncCallTracker, providerName string, send streamSender) bool {
	stopReason := normalizeResponsesStopReason(
		string(evt.Response.Status),
		evt.Response.Output,
		!tracker.ordered && strings.TrimSpace(evt.Response.OutputText()) == "",
	)
	terminal := responseTerminalChunk(evt, stopReason, providerName)
	if tracker.ordered && !responsesTerminalAllowsOutput(stopReason) {
		send(terminal)
		return false
	}
	if tracker.ordered && !tracker.outputOrder.completeMessages(evt, send) {
		return false
	}
	if stopReason == stopReasonToolCalls {
		for i, item := range evt.Response.Output {
			if item.Type != eventTypeFunctionCall {
				continue
			}
			fc, err := tracker.getChecked(item.ID, int64(i), true, item.CallID)
			if err != nil {
				return failResponsesStream(send, err)
			}
			if err := tracker.mergeItem(fc, item, true); err != nil {
				return failResponsesStream(send, err)
			}
			if !tracker.emit(fc, send) {
				return false
			}
		}
	}
	if tracker.ordered && tracker.hasUnemittedFunctionCall(evt.Response.Output) {
		return failResponsesStream(send, errors.New("response completed with an unfinished function call"))
	}
	send(terminal)
	return false
}

func responseTerminalChunk(evt responses.ResponseStreamEventUnion, stopReason, providerName string) llm.StreamChunk {
	return llm.StreamChunk{
		Done:                  true,
		StopReason:            stopReason,
		InputTokens:           int(evt.Response.Usage.InputTokens),
		OutputTokens:          int(evt.Response.Usage.OutputTokens),
		UsageReported:         evt.Response.Usage.JSON.InputTokens.Valid() && evt.Response.Usage.JSON.OutputTokens.Valid(),
		CachedInputTokens:     llm.ReportedTokenCount(evt.Response.Usage.InputTokensDetails.CachedTokens, evt.Response.Usage.InputTokensDetails.JSON.CachedTokens.Valid()),
		CacheWriteInputTokens: llm.ReportedTokenCount(evt.Response.Usage.InputTokensDetails.CacheWriteTokens, evt.Response.Usage.InputTokensDetails.JSON.CacheWriteTokens.Valid()),
		Model:                 evt.Response.Model,
		Provider:              providerName,
	}
}

func (p *Provider) streamResponses(ctx context.Context, req *llm.CompletionRequest) <-chan llm.StreamChunk {
	ch := make(chan llm.StreamChunk)
	go func() {
		defer close(ch)

		send := newStreamSender(ctx, ch)
		streamResponsesEvents(p.client.Responses.NewStreaming(ctx, buildResponsesParams(req)), p.TelemetryProviderName(), send, req.ResponsesInput)
	}()
	return ch
}

// -------------------------------------------------------------------------
// Chat Completions API helpers (fallback)
// -------------------------------------------------------------------------

// groupAssistantTurns adapts ordered Responses history to Chat Completions,
// which represents assistant text and function calls together in one turn.
// Copy call slices so normalization never mutates the caller's history.
func groupAssistantTurns(messages []llm.Message) []llm.Message {
	grouped := make([]llm.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "assistant" && len(grouped) > 0 && grouped[len(grouped)-1].Role == "assistant" {
			last := &grouped[len(grouped)-1]
			last.Content += msg.Content
			last.ToolCalls = append(last.ToolCalls, msg.ToolCalls...)
			continue
		}
		msg.ToolCalls = append([]llm.ToolCall(nil), msg.ToolCalls...)
		grouped = append(grouped, msg)
	}
	return grouped
}

func convertChatRequestMessages(req *llm.CompletionRequest) []openai.ChatCompletionMessageParamUnion {
	messages := req.Messages
	if req.ResponsesInput {
		messages = groupAssistantTurns(messages)
	}
	return convertMessages(messages, req.SystemPrompt)
}

func convertMessages(messages []llm.Message, systemPrompt string) []openai.ChatCompletionMessageParamUnion {
	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if systemPrompt != "" {
		msgs = append(msgs, openai.SystemMessage(systemPrompt))
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			msgs = append(msgs, openai.SystemMessage(msg.Content))
		case messageRoleUser:
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
		strict := false
		if tool.Strict != nil {
			strict = *tool.Strict
		}
		cTools = append(cTools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        tool.Name,
			Description: openai.String(tool.Description),
			Parameters:  params,
			Strict:      openai.Bool(strict),
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
		Messages: convertChatRequestMessages(req),
	}
	if req.Store != nil {
		params.Store = openai.Bool(*req.Store)
	}
	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}
	if req.HasTemperature() {
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
	result.UsageReported = resp.Usage.JSON.PromptTokens.Valid() && resp.Usage.JSON.CompletionTokens.Valid()
	result.CachedInputTokens = llm.ReportedTokenCount(resp.Usage.PromptTokensDetails.CachedTokens, resp.Usage.PromptTokensDetails.JSON.CachedTokens.Valid())
	result.CacheWriteInputTokens = llm.ReportedTokenCount(resp.Usage.PromptTokensDetails.CacheWriteTokens, resp.Usage.PromptTokensDetails.JSON.CacheWriteTokens.Valid())
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		result.Content = choice.Message.Content
		result.StopReason = choice.FinishReason
		if strings.TrimSpace(choice.Message.Refusal) != "" {
			result.StopReason = stopReasonRefusal
		}
		for _, tc := range choice.Message.ToolCalls {
			if tc.Type == "function" {
				result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: json.RawMessage(tc.Function.Arguments),
				})
			}
		}
		legacyName, legacyArguments := legacyFunctionCallFromJSON(choice.Message.RawJSON())
		if len(result.ToolCalls) == 0 && strings.TrimSpace(legacyName) != "" {
			result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
				ID:        legacyFunctionCallID(resp.ID),
				Name:      legacyName,
				Arguments: legacyFunctionCallArguments(legacyArguments),
			})
		}
	}
	return result, nil
}

func (p *Provider) streamChatCompletions(ctx context.Context, req *llm.CompletionRequest) <-chan llm.StreamChunk {
	return p.streamChatCompletionsWithUsage(ctx, req, true)
}

func (p *Provider) streamChatCompletionsWithUsage(ctx context.Context, req *llm.CompletionRequest, includeUsage bool) <-chan llm.StreamChunk { //nolint:gocyclo // Handles modern and legacy streaming fields inline.
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
			Messages: convertChatRequestMessages(req),
		}
		if req.Store != nil {
			params.Store = openai.Bool(*req.Store)
		}
		if includeUsage {
			params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
				IncludeUsage: openai.Bool(true),
			}
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
		if req.HasTemperature() {
			params.Temperature = openai.Float(req.Temperature)
		}

		stream := p.client.Chat.Completions.NewStreaming(ctx, params)
		acc := openai.ChatCompletionAccumulator{}
		finishReason := ""
		hasContent := false
		hasRefusal := false
		hasToolCalls := false
		legacyFunctionCallSeen := false
		legacyFunctionCallIDValue := ""
		var legacyFunctionCallName strings.Builder
		var legacyFunctionCallArgs strings.Builder
		var inputTokens, outputTokens int
		var cachedInputTokens, cacheWriteInputTokens *int64
		var usageReported bool
		streamModel := req.Model

		for stream.Next() {
			chunk := stream.Current()

			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta
				if delta.Content != "" {
					hasContent = hasContent || strings.TrimSpace(delta.Content) != ""
					if !send(llm.StreamChunk{Content: delta.Content}) {
						return
					}
				}
				if strings.TrimSpace(delta.Refusal) != "" {
					hasRefusal = true
				}
				legacyName, legacyArguments := legacyFunctionCallFromJSON(delta.RawJSON())
				if legacyName != "" {
					legacyFunctionCallSeen = true
					legacyFunctionCallName.WriteString(legacyName)
				}
				if legacyArguments != "" {
					legacyFunctionCallSeen = true
					legacyFunctionCallArgs.WriteString(legacyArguments)
				}
			}

			if acc.AddChunk(chunk) {
				if tc, ok := acc.JustFinishedToolCall(); ok {
					hasToolCalls = true
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

			if strings.TrimSpace(chunk.Model) != "" {
				streamModel = chunk.Model
			}
			if strings.TrimSpace(chunk.ID) != "" {
				legacyFunctionCallIDValue = chunk.ID
			}
			if chunk.JSON.Usage.Valid() {
				inputTokens = int(chunk.Usage.PromptTokens)
				outputTokens = int(chunk.Usage.CompletionTokens)
				usageReported = chunk.Usage.JSON.PromptTokens.Valid() && chunk.Usage.JSON.CompletionTokens.Valid()
				cachedInputTokens = llm.ReportedTokenCount(chunk.Usage.PromptTokensDetails.CachedTokens, chunk.Usage.PromptTokensDetails.JSON.CachedTokens.Valid())
				cacheWriteInputTokens = llm.ReportedTokenCount(chunk.Usage.PromptTokensDetails.CacheWriteTokens, chunk.Usage.PromptTokensDetails.JSON.CacheWriteTokens.Valid())
				if !send(llm.StreamChunk{InputTokens: inputTokens, OutputTokens: outputTokens, CachedInputTokens: cachedInputTokens,
					CacheWriteInputTokens: cacheWriteInputTokens, UsageReported: usageReported, Model: streamModel, Provider: p.TelemetryProviderName()}) {
					return
				}
			}

			if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != "" {
				finishReason = chunk.Choices[0].FinishReason
			}
		}

		if err := stream.Err(); err != nil {
			providerErr := toProviderError(err)
			if includeUsage && isUnsupportedStreamOptionsError(providerErr) {
				for chunk := range p.streamChatCompletionsWithUsage(ctx, req, false) {
					if !send(chunk) {
						return
					}
				}
				return
			}
			send(llm.StreamChunk{Error: providerErr, Done: true})
			return
		}
		legacyName := legacyFunctionCallName.String()
		if legacyFunctionCallSeen && !hasToolCalls && strings.TrimSpace(legacyName) != "" {
			hasToolCalls = true
			if !send(llm.StreamChunk{ToolCall: &llm.ToolCall{
				ID:        legacyFunctionCallID(legacyFunctionCallIDValue),
				Name:      legacyName,
				Arguments: legacyFunctionCallArguments(legacyFunctionCallArgs.String()),
			}}) {
				return
			}
		}
		finishReason = normalizeChatStreamStopReason(finishReason, hasContent, hasRefusal, hasToolCalls)
		send(llm.StreamChunk{
			Done:                  true,
			StopReason:            finishReason,
			InputTokens:           inputTokens,
			OutputTokens:          outputTokens,
			CachedInputTokens:     cachedInputTokens,
			CacheWriteInputTokens: cacheWriteInputTokens,
			UsageReported:         usageReported,
			Model:                 streamModel,
			Provider:              p.TelemetryProviderName(),
		})
	}()
	return ch
}

func legacyFunctionCallID(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return "legacy_function_call"
	}
	return responseID + "_function_call"
}

func legacyFunctionCallFromJSON(raw string) (string, string) {
	var payload struct {
		FunctionCall struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function_call"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &payload) != nil {
		return "", ""
	}
	return payload.FunctionCall.Name, payload.FunctionCall.Arguments
}

func legacyFunctionCallArguments(arguments string) json.RawMessage {
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(arguments)
}

func normalizeChatStreamStopReason(finishReason string, hasContent, hasRefusal, hasToolCalls bool) string {
	if hasRefusal {
		return stopReasonRefusal
	}
	if finishReason == "" {
		return stopReasonIncomplete
	}
	if finishReason == stopReasonStop {
		if hasToolCalls {
			return stopReasonToolCalls
		}
		if !hasContent {
			return stopReasonIncomplete
		}
	}
	if finishReason == stopReasonToolCalls || finishReason == stopReasonFunctionCall {
		if !hasToolCalls {
			return stopReasonIncomplete
		}
		return stopReasonToolCalls
	}
	return finishReason
}

func isUnsupportedStreamOptionsError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "stream_options") && !strings.Contains(msg, "include_usage") {
		return false
	}
	return strings.Contains(msg, "unsupported") ||
		strings.Contains(msg, "unknown") ||
		strings.Contains(msg, "unrecognized") ||
		strings.Contains(msg, "invalid")
}

// toProviderError wraps an error as a ProviderError, extracting the HTTP status
// code from the OpenAI SDK error type when available.
func toProviderError(err error) *llm.ProviderError {
	pe := &llm.ProviderError{Provider: "openai", Message: err.Error()}
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
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
		Messages:  []llm.Message{{Role: messageRoleUser, Content: "hi"}},
		MaxTokens: 1,
		Store:     req.Store,
	}
	probe, err := p.completeResponses(ctx, probeReq)
	if err == nil {
		if err := llm.RecordIntermediateUsage(ctx, probe); err != nil {
			return nil, err
		}
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
