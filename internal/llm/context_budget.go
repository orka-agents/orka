package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrContextLimit means that input plus reserved output exceeds the selected
// model's configured allowance. It does not authorize discarding messages.
var ErrContextLimit = errors.New("model context allowance exceeded")

const contextRoleAssistant = "assistant"

// ContextLimitError carries the allowance of the concrete selected model,
// including when a fallback has a smaller window than the primary.
type ContextLimitError struct {
	Model        string
	Window       int
	InputTokens  int
	OutputTokens int
}

func (e *ContextLimitError) Error() string {
	return fmt.Sprintf("%v: estimated input %d plus reserved output %d exceeds %d tokens for model %q", ErrContextLimit, e.InputTokens, e.OutputTokens, e.Window, e.Model)
}

func (e *ContextLimitError) Unwrap() error { return ErrContextLimit }

// EstimateRequestTokens includes instructions, all message roles and tool
// arguments, tool definitions, structured output, and protocol overhead. This
// is an estimate, not a provider tokenizer; provider context errors still need
// bounded recovery. The fixed reserve covers provider-specific framing.
func EstimateRequestTokens(req *CompletionRequest) int {
	if req == nil {
		return 0
	}
	total := 256 + estimateTokens(req.SystemPrompt)
	for _, message := range req.Messages {
		total += estimateMessageTokens(message) + messageFramingTokens(message)
	}
	if len(req.Tools) > 0 {
		data, _ := json.Marshal(req.Tools)
		total += estimateTokens(string(data))
	}
	if req.ResponseFormat != nil {
		data, _ := json.Marshal(req.ResponseFormat)
		total += estimateTokens(string(data))
	}
	for _, stop := range req.StopSequences {
		total += estimateTokens(stop)
	}
	return total
}

func messageFramingTokens(message Message) int {
	tokens := 8 + estimateTokens(message.Role+message.Name+message.ToolCallID)
	for _, call := range message.ToolCalls {
		tokens += estimateTokens(call.ID)
	}
	return tokens
}

// ResponseTokenReserve matches the default output cap used by the AI worker.
func ResponseTokenReserve(req *CompletionRequest) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	return 4096
}

// MessageTokenBudget reserves non-message inputs and output. The remaining
// allowance covers both content and framing of the messages actually retained.
func MessageTokenBudget(req *CompletionRequest, window int) int {
	fixed := *req
	fixed.Messages = nil
	return window - ResponseTokenReserve(req) - EstimateRequestTokens(&fixed)
}

// FitRequestMessagesKeeping fits a complete request's messages, including their
// framing, while reserving instructions, tools, and output outside Messages.
// It preserves the same required messages and atomic exchanges as
// FitMessagesKeeping without charging for messages that are discarded.
// Extra required indexes pin complete exchanges, such as unread tool results.
func FitRequestMessagesKeeping(req *CompletionRequest, window, currentRequestIndex int, requiredMessageIndexes ...int) ([]Message, error) {
	return fitMessagesKeeping(req.Messages, MessageTokenBudget(req, window), currentRequestIndex, true, requiredMessageIndexes...)
}

// CheckContextWindow performs admission without changing message content.
func CheckContextWindow(req *CompletionRequest) error {
	if req == nil || req.ContextWindow <= 0 {
		return nil
	}
	input, output := EstimateRequestTokens(req), ResponseTokenReserve(req)
	if input+output > req.ContextWindow {
		return &ContextLimitError{Model: req.Model, Window: req.ContextWindow, InputTokens: input, OutputTokens: output}
	}
	return nil
}
