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

// providerName is the name the provider registers under and reports in errors.
const providerName = "anthropic"

func init() {
	llm.RegisterProvider(providerName, func(config llm.ProviderConfig) (llm.Provider, error) {
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
	return providerName
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

	streamCtx, guard := newIdleGuard(ctx)
	defer guard.stop()
	stream := p.client.Messages.NewStreaming(streamCtx, params, guard.middleware())
	defer func() { _ = stream.Close() }()

	acc := completionAccumulator{provider: p.TelemetryProviderName()}
	for stream.Next() {
		if err := acc.add(stream.Current()); err != nil {
			return acc.failed(toProviderError(err))
		}
	}
	if idled(streamCtx) && ctx.Err() == nil {
		// A stalled connection, like a broken one, has shown the caller
		// nothing, so the retry layer may try again.
		return acc.failed(&llm.ProviderError{
			Provider:   providerName,
			Message:    "anthropic stream sent nothing for " + streamIdleTimeout.String(),
			StatusCode: http.StatusBadGateway,
		})
	}
	if err := stream.Err(); err != nil {
		pe := toProviderError(err)
		if acc.started && pe.StatusCode == 0 && ctx.Err() == nil {
			// The connection broke after the stream started. None of the
			// output has reached the caller, so treat it like a stream that
			// ends early.
			pe.StatusCode = http.StatusBadGateway
		}
		return acc.failed(pe)
	}
	if !acc.completed {
		// A dropped connection can also end the stream without an error.
		// Returning the partial message would pass truncated output off as
		// complete. None of it has reached the caller, so report a gateway
		// failure, which the retry layer retries.
		return acc.failed(&llm.ProviderError{
			Provider:   providerName,
			Message:    "anthropic stream ended before the message finished",
			StatusCode: http.StatusBadGateway,
		})
	}

	resp := acc.usage()
	if req.ResponsesInput {
		// Responses keeps text and tool calls in the order the model wrote
		// them, as completeViaStream does for a streamed turn.
		resp.OutputItems = []llm.AssistantOutputItem{}
	}
	textItemOpen := false
	for _, block := range acc.message.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			resp.Content += b.Text
			if req.ResponsesInput && b.Text != "" {
				if !textItemOpen {
					resp.OutputItems = append(resp.OutputItems, llm.AssistantOutputItem{})
					textItemOpen = true
				}
				resp.OutputItems[len(resp.OutputItems)-1].Content += b.Text
			}
		case anthropic.ToolUseBlock:
			call := llm.ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Input,
			}
			resp.ToolCalls = append(resp.ToolCalls, call)
			if req.ResponsesInput {
				resp.OutputItems = append(resp.OutputItems, llm.AssistantOutputItem{ToolCall: &call})
				textItemOpen = false
			}
		}
	}
	return resp, nil
}

// completionAccumulator assembles a streamed message and the usage the
// stream reported along the way.
type completionAccumulator struct {
	provider                                          string
	message                                           anthropic.Message
	started, inputReported, outputReported, completed bool
	cacheRead, cacheWrite                             *int64
}

func (a *completionAccumulator) add(event anthropic.MessageStreamEventUnion) error {
	if err := a.message.Accumulate(event); err != nil {
		return err
	}
	switch e := event.AsAny().(type) {
	case anthropic.MessageStartEvent:
		u := e.Message.Usage
		a.started = true
		a.inputReported = u.JSON.InputTokens.Valid()
		a.outputReported = u.JSON.OutputTokens.Valid()
		a.cacheRead = llm.ReportedTokenCount(u.CacheReadInputTokens, u.JSON.CacheReadInputTokens.Valid())
		a.cacheWrite = llm.ReportedTokenCount(u.CacheCreationInputTokens, u.JSON.CacheCreationInputTokens.Valid())
	case anthropic.MessageDeltaEvent:
		u := e.Usage
		a.inputReported = a.inputReported || u.JSON.InputTokens.Valid()
		a.outputReported = a.outputReported || u.JSON.OutputTokens.Valid()
		if u.JSON.CacheReadInputTokens.Valid() {
			a.cacheRead = llm.ReportedTokenCount(u.CacheReadInputTokens, true)
		}
		if u.JSON.CacheCreationInputTokens.Valid() {
			a.cacheWrite = llm.ReportedTokenCount(u.CacheCreationInputTokens, true)
		}
		a.completed = a.completed || e.Delta.StopReason != ""
	case anthropic.MessageStopEvent:
		a.completed = true
	}
	return nil
}

// usage returns a response that carries the message's identity and usage
// but none of its content.
func (a *completionAccumulator) usage() *llm.CompletionResponse {
	return &llm.CompletionResponse{
		Provider:              a.provider,
		ID:                    a.message.ID,
		Model:                 a.message.Model,
		StopReason:            string(a.message.StopReason),
		InputTokens:           int(a.message.Usage.InputTokens),
		OutputTokens:          int(a.message.Usage.OutputTokens),
		InputExcludesCache:    true,
		UsageReported:         a.inputReported && a.outputReported,
		CachedInputTokens:     a.cacheRead,
		CacheWriteInputTokens: a.cacheWrite,
	}
}

// failed returns err, with the usage the stream reported before it failed so
// usage accounting still counts the tokens the attempt was billed for. It
// leaves out the partial content, which callers must not use.
func (a *completionAccumulator) failed(err error) (*llm.CompletionResponse, error) {
	if !a.started {
		return nil, err
	}
	return a.usage(), err
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
	// Not 504: llm.ShouldRetry skips 504s, which the SDK has already retried
	// when they arrive before a stream. One inside a stream it has not.
	shared.ErrorTypeTimeoutError:    http.StatusBadGateway,
	shared.ErrorTypeOverloadedError: 529,
}

// toProviderError wraps an error as a ProviderError, extracting the HTTP status
// code from the Anthropic SDK error type when available.
func toProviderError(err error) *llm.ProviderError {
	pe := &llm.ProviderError{Provider: providerName, Message: err.Error()}
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
