/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tracing/genai"
)

type streamUsageState struct {
	InputTokens           int
	OutputTokens          int
	InputReported         bool
	CachedInputTokens     *int64
	CacheWriteInputTokens *int64
	Model                 string
	Provider              string
}

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
		option.WithMiddleware(llm.UsageHTTPMiddleware),
		option.WithHTTPClient(llm.SharedHTTPClient()),
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
	return genai.ProviderAnthropic
}

// Complete sends a completion request and returns the whole response.
//
// It streams the request and assembles the message from the events. The SDK
// refuses a non-streaming request whose max_tokens could take longer than its
// ten-minute HTTP timeout (about 21,333 tokens), and proxies such as Vekil
// give a non-streaming request far less time than a streaming one.
func (p *Provider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	params := buildRequestParams(req)

	stream := p.client.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()

	var (
		message                                  anthropic.Message
		inputReported, outputReported, completed bool
		cacheRead, cacheWrite                    *int64
	)
	for stream.Next() {
		event := stream.Current()
		if err := message.Accumulate(event); err != nil {
			return nil, toProviderError(err)
		}
		switch e := event.AsAny().(type) {
		case anthropic.MessageStartEvent:
			u := e.Message.Usage
			inputReported = u.JSON.InputTokens.Valid()
			outputReported = u.JSON.OutputTokens.Valid()
			cacheRead = llm.ReportedTokenCount(u.CacheReadInputTokens, u.JSON.CacheReadInputTokens.Valid())
			cacheWrite = llm.ReportedTokenCount(u.CacheCreationInputTokens, u.JSON.CacheCreationInputTokens.Valid())
		case anthropic.MessageDeltaEvent:
			u := e.Usage
			inputReported = inputReported || u.JSON.InputTokens.Valid()
			outputReported = outputReported || u.JSON.OutputTokens.Valid()
			if u.JSON.CacheReadInputTokens.Valid() {
				cacheRead = llm.ReportedTokenCount(u.CacheReadInputTokens, true)
			}
			if u.JSON.CacheCreationInputTokens.Valid() {
				cacheWrite = llm.ReportedTokenCount(u.CacheCreationInputTokens, true)
			}
			completed = completed || e.Delta.StopReason != ""
		case anthropic.MessageStopEvent:
			completed = true
		}
	}
	if err := stream.Err(); err != nil {
		return nil, toProviderError(err)
	}
	if !completed {
		// A dropped connection ends the stream without an error. Returning
		// the partial message would pass truncated output off as complete.
		// None of it has reached the caller, so report a gateway failure,
		// which the retry layer retries.
		return nil, &llm.ProviderError{
			Provider:   "anthropic",
			Message:    "anthropic stream ended before the message finished",
			StatusCode: http.StatusBadGateway,
		}
	}

	// Convert response
	resp := &llm.CompletionResponse{
		Provider:              p.TelemetryProviderName(),
		ID:                    message.ID,
		Model:                 message.Model,
		StopReason:            string(message.StopReason),
		InputTokens:           int(message.Usage.InputTokens),
		OutputTokens:          int(message.Usage.OutputTokens),
		InputExcludesCache:    true,
		UsageReported:         inputReported && outputReported,
		CachedInputTokens:     cacheRead,
		CacheWriteInputTokens: cacheWrite,
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
	prevTool := false
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
			// Return the results of parallel tool calls in one user message.
			// Splitting them across messages teaches Claude to stop making
			// parallel calls.
			result := anthropic.NewToolResultBlock(msg.ToolCallID, msg.Content, false)
			if n := len(messages); n > 0 && prevTool {
				messages[n-1].Content = append(messages[n-1].Content, result)
			} else {
				messages = append(messages, anthropic.NewUserMessage(result))
			}
		case "system":
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(msg.Content)))
		}
		prevTool = msg.Role == "tool"
	}
	return messages
}

// buildToolParams converts llm tool definitions to Anthropic tool params.
func buildToolParams(tools []llm.Tool) []anthropic.ToolUnionParam {
	params := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, tool := range tools {
		var schema map[string]json.RawMessage
		_ = json.Unmarshal(tool.Parameters, &schema)
		// The SDK's typed fields omit root constraints such as $defs and
		// additionalProperties. Preserve every field and its raw numeric values.
		fields := make(map[string]any, len(schema))
		for name, value := range schema {
			fields[name] = value
		}

		toolParam := anthropic.ToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			InputSchema: anthropic.ToolInputSchemaParam{ExtraFields: fields},
		}
		if tool.Strict != nil {
			toolParam.Strict = anthropic.Bool(*tool.Strict)
		}
		params = append(params, anthropic.ToolUnionParam{OfTool: &toolParam})
	}
	return params
}

// buildRequestParams creates Anthropic MessageNewParams from a completion request.
func buildRequestParams(req *llm.CompletionRequest) anthropic.MessageNewParams {
	maxTokens := int64(4096)
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}

	params := anthropic.MessageNewParams{
		Model:     req.Model,
		MaxTokens: maxTokens,
	}

	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
		if stable := req.SystemPromptStable; stable > 0 && stable < len(req.SystemPrompt) {
			params.System = []anthropic.TextBlockParam{
				{Text: req.SystemPrompt[:stable]},
				{Text: req.SystemPrompt[stable:]},
			}
		}
	}
	messages := req.Messages
	if req.ResponsesInput {
		// Responses retains system/developer messages in history. Anthropic
		// accepts instructions only in its top-level system field.
		messages = make([]llm.Message, 0, len(req.Messages))
		for _, message := range req.Messages {
			if message.Role == "system" {
				if message.Content != "" {
					params.System = append(params.System, anthropic.TextBlockParam{Text: message.Content})
				}
				continue
			}
			messages = append(messages, message)
		}
	}
	params.Messages = buildMessages(messages)

	if req.HasTemperature() && acceptsSamplingParams(req.Model) {
		params.Temperature = anthropic.Float(req.Temperature)
	}

	if len(req.Tools) > 0 {
		params.Tools = buildToolParams(req.Tools)
	}

	markPromptCache(&params)
	return params
}

// markPromptCache asks Anthropic to cache the request's prefix. Anthropic
// caches nothing unless a block names a cache_control breakpoint, and a later
// request reuses everything up to an earlier breakpoint whose prefix is byte
// for byte the same. The breakpoint on the first system block, the stable part
// of a split system prompt, keeps the tools and fixed instructions cached when
// the rest of the system prompt changes. The breakpoint on the last system
// block keeps the whole system prompt cached when history is truncated. The
// breakpoint on the last message block moves forward every step, so each
// tool-loop step reuses the whole conversation before it. At most three of the
// four breakpoints a request may name.
func markPromptCache(params *anthropic.MessageNewParams) {
	if n := len(params.System); n > 0 {
		for _, i := range []int{0, n - 1} {
			if params.System[i].Text != "" {
				params.System[i].CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
		}
	}
	if n := len(params.Messages); n > 0 {
		blocks := params.Messages[n-1].Content
		for _, block := range slices.Backward(blocks) {
			if text := block.OfText; text != nil && text.Text == "" {
				continue // the API refuses cache_control on an empty text block
			}
			if cacheControl := block.GetCacheControl(); cacheControl != nil {
				*cacheControl = anthropic.NewCacheControlEphemeralParam()
				return
			}
		}
	}
}

// handleStreamEvent processes a single stream event and sends chunks via send.
// Returns false if the context was cancelled and the caller should stop.
func handleStreamEvent(
	event anthropic.MessageStreamEventUnion,
	send func(llm.StreamChunk) bool,
	currentToolCall **llm.ToolCall,
	toolCallArgs *[]byte,
	hasToolCalls *bool,
	usage *streamUsageState,
) bool {
	switch e := event.AsAny().(type) {
	case anthropic.MessageStartEvent:
		if usage != nil {
			usage.InputTokens = int(e.Message.Usage.InputTokens)
			usage.InputReported = e.Message.Usage.JSON.InputTokens.Valid()
			usage.OutputTokens = int(e.Message.Usage.OutputTokens)
			usage.CachedInputTokens = llm.ReportedTokenCount(e.Message.Usage.CacheReadInputTokens, e.Message.Usage.JSON.CacheReadInputTokens.Valid())
			usage.CacheWriteInputTokens = llm.ReportedTokenCount(e.Message.Usage.CacheCreationInputTokens, e.Message.Usage.JSON.CacheCreationInputTokens.Valid())
			usage.Model = e.Message.Model
			usage.Provider = genai.ProviderAnthropic
			if !send(llm.StreamChunk{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
				CachedInputTokens: usage.CachedInputTokens, CacheWriteInputTokens: usage.CacheWriteInputTokens,
				UsageReported: usage.InputReported && e.Message.Usage.JSON.OutputTokens.Valid(), InputExcludesCache: true,
				Model: usage.Model, Provider: usage.Provider}) {
				return false
			}
		}
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
		inputTokens := int(e.Usage.InputTokens)
		inputReported := e.Usage.JSON.InputTokens.Valid()
		cached := llm.ReportedTokenCount(e.Usage.CacheReadInputTokens, e.Usage.JSON.CacheReadInputTokens.Valid())
		cacheWrite := llm.ReportedTokenCount(e.Usage.CacheCreationInputTokens, e.Usage.JSON.CacheCreationInputTokens.Valid())
		model := ""
		provider := genai.ProviderAnthropic
		if usage != nil {
			inputReported = inputReported || usage.InputReported
			if cached == nil {
				cached = usage.CachedInputTokens
			}
			if cacheWrite == nil {
				cacheWrite = usage.CacheWriteInputTokens
			}
			if inputTokens == 0 {
				inputTokens = usage.InputTokens
			}
			model = usage.Model
			if usage.Provider != "" {
				provider = usage.Provider
			}
		}
		if !send(llm.StreamChunk{
			Done:                  true,
			StopReason:            stopReason,
			InputTokens:           inputTokens,
			OutputTokens:          int(e.Usage.OutputTokens),
			CachedInputTokens:     cached,
			CacheWriteInputTokens: cacheWrite,
			InputExcludesCache:    true,
			UsageReported:         inputReported && e.Usage.JSON.OutputTokens.Valid(),
			Model:                 model,
			Provider:              provider,
		}) {
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

		params := buildRequestParams(req)
		stream := p.client.Messages.NewStreaming(ctx, params)

		var currentToolCall *llm.ToolCall
		var toolCallArgs []byte
		hasToolCalls := false
		usage := streamUsageState{}

		for stream.Next() {
			if !handleStreamEvent(stream.Current(), send, &currentToolCall, &toolCallArgs, &hasToolCalls, &usage) {
				return
			}
		}

		if err := stream.Err(); err != nil {
			send(llm.StreamChunk{Error: toProviderError(err), Done: true})
		}
	}()

	return ch, nil
}

// streamErrorStatus maps the type of an error event that arrives inside a
// stream to the HTTP status the API returns for the same error before a
// stream starts, so retry and fallback treat both alike.
var streamErrorStatus = map[shared.ErrorType]int{
	shared.ErrorTypeInvalidRequestError: http.StatusBadRequest,
	shared.ErrorTypeAuthenticationError: http.StatusUnauthorized,
	shared.ErrorTypeBillingError:        http.StatusPaymentRequired,
	shared.ErrorTypePermissionError:     http.StatusForbidden,
	shared.ErrorTypeNotFoundError:       http.StatusNotFound,
	shared.ErrorTypeRateLimitError:      http.StatusTooManyRequests,
	shared.ErrorTypeAPIError:            http.StatusInternalServerError,
	shared.ErrorTypeTimeoutError:        http.StatusGatewayTimeout,
	shared.ErrorTypeOverloadedError:     529,
}

// toProviderError wraps an error as a ProviderError, extracting the HTTP status
// code from the Anthropic SDK error type when available.
func toProviderError(err error) *llm.ProviderError {
	pe := &llm.ProviderError{Provider: "anthropic", Message: err.Error()}
	if apiErr, ok := errors.AsType[*anthropic.Error](err); ok {
		pe.StatusCode = apiErr.StatusCode
		// An error event inside a stream carries the stream's 200 status.
		if status, known := streamErrorStatus[apiErr.Type()]; known && apiErr.StatusCode < http.StatusBadRequest {
			pe.StatusCode = status
		}
	}
	return pe
}

// Ensure Provider implements llm.Provider
var _ llm.Provider = (*Provider)(nil)
