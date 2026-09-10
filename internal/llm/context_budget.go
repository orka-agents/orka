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

// MessageTokenBudget subtracts every non-message input and output allowance.
// Framing is reserved for the original messages and for any increase from
// replacing a dropped message with an assistant reference note.
func MessageTokenBudget(req *CompletionRequest, window int) int {
	overhead := EstimateRequestTokens(req)
	noteFraming := messageFramingTokens(Message{Role: contextRoleAssistant})
	minimumFraming := noteFraming
	for _, message := range req.Messages {
		overhead -= estimateMessageTokens(message)
		minimumFraming = min(minimumFraming, messageFramingTokens(message))
	}
	overhead += noteFraming - minimumFraming
	return window - ResponseTokenReserve(req) - overhead
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
