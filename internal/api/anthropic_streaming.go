/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	anthropicTextDelta = "text_delta"
)

func (h *AnthropicCompatHandler) handleStreamingMessages( //nolint:gocyclo
	c fiber.Ctx,
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
	model string,
	toolCtx *tools.ToolContext,
) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	capturedProvider := provider
	capturedReq := req
	streamBaseCtx := detachedSpanContext(ctx)

	return c.SendStreamWriter(func(w *bufio.Writer) {
		msgID := "msg_" + uuid.New().String()

		streamCtx, streamCancel := context.WithTimeout(streamBaseCtx, h.config.MaxDuration)
		defer streamCancel()

		if err := writeMessageStart(w, msgID, model, 0); err != nil {
			anthropicLog.Error(err, "failed to write message_start")
			return
		}

		blockIndex := 0
		messages := make([]llm.Message, len(capturedReq.Messages))
		copy(messages, capturedReq.Messages)
		totalUsage := anthropicStreamingUsage{}
		repetitionTracker := make(map[string]int)
		exposedToolNames := completionToolNameSet(capturedReq.Tools)
		prematureEndRetries := 0

		for iteration := 0; iteration < h.config.MaxIterations; iteration++ {
			select {
			case <-streamCtx.Done():
				if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: oaiContentTypeText, Text: ""}); err == nil {
					_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: anthropicTextDelta, Text: "Request timed out during tool execution."})
					_ = writeContentBlockStop(w, blockIndex)
				}
				_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalUsage)
				_ = writeMessageStop(w)
				return
			default:
			}

			sent := messages
			if h.config.MaxSessionSize > 0 {
				sent = llm.TruncateMessages(messages, h.config.MaxSessionSize/4)
			}

			compReq := &llm.CompletionRequest{
				Model:        model,
				Messages:     sent,
				SystemPrompt: capturedReq.SystemPrompt,
				Tools:        capturedReq.Tools,
				MaxTokens:    capturedReq.MaxTokens,
				Temperature:  capturedReq.Temperature,
			}

			resp, streamErr := completeViaStream(streamCtx, capturedProvider, compReq)
			usedStream := streamErr == nil
			if streamErr != nil {
				if !errors.Is(streamErr, errStreamUnavailable) {
					anthropicLog.Error(streamErr, "invalid provider stream in tool loop")
					_ = writeAnthropicStreamError(w, "provider_error")
					return
				}
				var completeErr error
				resp, completeErr = capturedProvider.Complete(streamCtx, compReq)
				if completeErr != nil {
					anthropicLog.Error(completeErr, "completion failed in tool loop")
					_ = writeAnthropicStreamError(w, "provider_error")
					return
				}
				if err := validateToolLoopCompletion(resp); err != nil {
					anthropicLog.Error(err, "invalid completion in tool loop")
					reason := ""
					if resp != nil {
						reason = resp.StopReason
					}
					_ = writeAnthropicStreamError(w, reason)
					return
				}
			}

			textContent := resp.Content
			toolCalls := resp.ToolCalls
			usageResponse := *resp
			if resp.OutputTokens == 0 && usedStream && !resp.UsageReported {
				usageResponse.OutputTokens = estimateTokens(textContent)
			}
			if err := totalUsage.addResponse(&usageResponse); err != nil {
				anthropicLog.Error(err, "invalid provider usage in tool loop")
				_ = writeAnthropicStreamError(w, anthropicUsageOutOfRange)
				return
			}

			if len(toolCalls) == 0 {
				if isTokenBudgetTruncatedText(resp) {
					writeAnthropicTextProgress(w, &blockIndex, stripGoalStateSentinel(textContent))
					_ = writeMessageDelta(w, oaiParamMaxTokens, totalUsage)
					_ = writeMessageStop(w)
					return
				}
				if hasGoalStateSentinelPrefix(textContent) {
					writeAnthropicTextProgress(w, &blockIndex, stripGoalStateSentinel(textContent))
					stopReason := oaiStopReasonEndTurn
					_ = writeMessageDelta(w, stopReason, totalUsage)
					_ = writeMessageStop(w)
					return
				}
				if prematureEndRetries >= h.config.MaxPrematureEndRetries {
					anthropicLog.Info("streaming: premature end of turn — retry budget exhausted, closing stream anyway",
						"iteration", iteration,
						"retries", prematureEndRetries,
					)
					writeAnthropicTextProgress(w, &blockIndex, stripGoalStateSentinel(textContent))
					stopReason := oaiStopReasonEndTurn
					_ = writeMessageDelta(w, stopReason, totalUsage)
					_ = writeMessageStop(w)
					return
				}
				prematureEndRetries++
				anthropicLog.Info("streaming: premature end of turn — injecting continue message and re-looping",
					"iteration", iteration,
					"retries", prematureEndRetries,
					"content_prefix", truncateForLog(textContent, 120),
				)
				continueMsg := fmt.Sprintf(
					"[System: You emitted text but did not include the literal %q sentinel that marks GOAL STATE A or GOAL STATE B. The workflow is not done. Per the TURN-ENDING INVARIANT, your next response MUST contain a tool_use (not text). Look at the POSTCONDITION TABLE and call the correct next tool. Do NOT emit any text until you are ready to write your final report that begins with %q on its own line.]",
					goalStateSentinel, goalStateSentinel,
				)
				writeAnthropicTextProgress(w, &blockIndex, "[Continuing workflow...]\n\n")
				messages = append(messages, llm.Message{
					Role:    chatRoleAssistant,
					Content: textContent,
				})
				messages = append(messages, llm.Message{
					Role:    chatRoleUser,
					Content: continueMsg,
				})
				continue
			}

			toolNames := make([]string, len(toolCalls))
			for i, tc := range toolCalls {
				toolNames[i] = tc.Name
			}
			anthropicLog.Info("streaming tool loop iteration",
				"iteration", iteration,
				"tool_calls", len(toolCalls),
				"tools", toolNames,
			)
			writeAnthropicTextProgress(w, &blockIndex, stripGoalStateSentinel(textContent))

			messages = append(messages, llm.Message{
				Role:      chatRoleAssistant,
				Content:   textContent,
				ToolCalls: toolCalls,
			})

			var repetitionWarning string
			for _, tc := range toolCalls {
				argsHash := hashArgs(tc.Name, tc.Arguments)
				repetitionTracker[argsHash]++
				if repetitionTracker[argsHash] >= 3 {
					repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, repetitionTracker[argsHash])
					iteration += 5
				}

				result := executeExposedToolCall(streamCtx, tc, h.config.ToolTimeout, toolCtx, exposedToolNames)

				writeAnthropicTextProgress(w, &blockIndex, formatToolProgress(tc, result))

				messages = append(messages, llm.Message{
					Role:       chatRoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    result,
				})
			}

			if repetitionWarning != "" {
				messages = append(messages, llm.Message{
					Role:    chatRoleUser,
					Content: repetitionWarning,
				})
			}

			if repetitionWarning == "" && isAllWaitingPolls(toolCalls, messages) {
				for {
					select {
					case <-streamCtx.Done():
						_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalUsage)
						_ = writeMessageStop(w)
						return
					default:
					}

					if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: oaiContentTypeText, Text: ""}); err == nil {
						_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{
							Type: anthropicTextDelta, Text: "[⏳ Auto-polling tasks...]",
						})
						_ = writeContentBlockStop(w, blockIndex)
						blockIndex++
					}

					allStillRunning := true
					messages = messages[:len(messages)-len(toolCalls)]
					for _, tc := range toolCalls {
						result := executeExposedToolCall(streamCtx, tc, h.config.ToolTimeout, toolCtx, exposedToolNames)
						messages = append(messages, llm.Message{
							Role:       chatRoleTool,
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Content:    result,
						})
						if !isTaskStillRunning(result) {
							allStillRunning = false
						}
					}

					if !allStillRunning {
						anthropicLog.Info("auto-poll: task state changed, resuming LLM loop")
						break
					}

					iteration++
					if iteration >= h.config.MaxIterations {
						break
					}
				}
			}
		}

		_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalUsage)
		_ = writeMessageStop(w)
	})
}

func (h *AnthropicCompatHandler) handleStreamingProxy(
	c fiber.Ctx,
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
	model string,
) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	capturedProvider := provider
	capturedReq := req
	streamBaseCtx := detachedSpanContext(ctx)

	return c.SendStreamWriter(func(w *bufio.Writer) {
		msgID := "msg_" + uuid.New().String()

		streamCtx, streamCancel := context.WithTimeout(streamBaseCtx, h.config.MaxDuration)
		defer streamCancel()

		if err := writeMessageStart(w, msgID, model, 0); err != nil {
			anthropicLog.Error(err, "failed to write message_start")
			return
		}

		streamCh, err := capturedProvider.Stream(streamCtx, capturedReq)
		if err != nil {
			if llm.IsUsagePersistenceError(err) {
				anthropicLog.Error(err, "stream usage persistence failed")
				_ = writeAnthropicStreamError(w, "provider_error")
				return
			}
			h.handleStreamingFallback(w, streamCtx, capturedProvider, capturedReq)
			return
		}

		blockIndex := 0
		inTextBlock := false
		hasToolCalls := false
		usageResponse := &llm.CompletionResponse{}
		terminalStopReason := ""

		for chunk := range streamCh {
			if chunk.Error != nil {
				anthropicLog.Error(chunk.Error, "stream chunk error")
				_ = writeAnthropicStreamError(w, "provider_error")
				return
			}

			if chunk.Content != "" {
				if !inTextBlock {
					if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
						Type: oaiContentTypeText, Text: "",
					}); err != nil {
						break
					}
					inTextBlock = true
				}
				if err := writeContentBlockDelta(w, blockIndex, AnthropicDelta{
					Type: anthropicTextDelta, Text: chunk.Content,
				}); err != nil {
					break
				}
			}

			if chunk.ToolCall != nil {
				hasToolCalls = true
				if inTextBlock {
					_ = writeContentBlockStop(w, blockIndex)
					blockIndex++
					inTextBlock = false
				}

				tc := chunk.ToolCall
				if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: json.RawMessage(""),
				}); err == nil {
					_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{
						Type: "input_json_delta", PartialJSON: string(tc.Arguments),
					})
					_ = writeContentBlockStop(w, blockIndex)
					blockIndex++
				}
			}

			retainStreamUsage(usageResponse, chunk)

			if chunk.Done {
				terminalStopReason = chunk.StopReason
				if inTextBlock {
					_ = writeContentBlockStop(w, blockIndex)
					inTextBlock = false
				}
				break
			}
		}

		if inTextBlock {
			_ = writeContentBlockStop(w, blockIndex)
		}

		stopReason, ok := mapAnthropicStreamStopReason(terminalStopReason, hasToolCalls)
		if !ok {
			_ = writeAnthropicStreamError(w, terminalStopReason)
			return
		}
		usage := anthropicStreamingUsage{}
		if err := usage.addResponse(usageResponse); err != nil {
			anthropicLog.Error(err, "invalid provider stream usage")
			_ = writeAnthropicStreamError(w, anthropicUsageOutOfRange)
			return
		}
		_ = writeMessageDelta(w, stopReason, usage)
		_ = writeMessageStop(w)
	})
}

func estimateTokens(text string) int {
	return len(text) / 4
}

func mapAnthropicStreamStopReason(reason string, hasToolCalls bool) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case finishReasonStop, oaiStopReasonEndTurn, completionStatusCompleted, "response.completed":
		if hasToolCalls {
			return oaiStopReasonToolUse, true
		}
		return oaiStopReasonEndTurn, true
	case finishReasonStopSequence:
		if hasToolCalls {
			return oaiStopReasonToolUse, true
		}
		return finishReasonStopSequence, true
	case finishReasonToolCalls, oaiStopReasonToolUse, finishReasonFunctionCall:
		if !hasToolCalls {
			return "", false
		}
		return oaiStopReasonToolUse, true
	case oaiParamMaxTokens, oaiStopReasonLength:
		return oaiParamMaxTokens, true
	case "pause_turn":
		return "pause_turn", true
	case "refusal":
		return "refusal", true
	case anthropicStopReasonModelContextWindowExceeded:
		return anthropicStopReasonModelContextWindowExceeded, true
	default:
		return "", false
	}
}

func (h *AnthropicCompatHandler) handleStreamingFallback(
	w *bufio.Writer,
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
) {
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		anthropicLog.Error(err, "fallback complete failed")
		_ = writeAnthropicStreamError(w, "provider_error")
		return
	}
	stopReason, ok := mapAnthropicCompletionStopReason(resp)
	if !ok {
		reason := ""
		if resp != nil {
			reason = resp.StopReason
		}
		_ = writeAnthropicStreamError(w, reason)
		return
	}

	blockIndex := 0

	if resp.Content != "" {
		if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
			Type: oaiContentTypeText,
			Text: "",
		}); err != nil {
			anthropicLog.Error(err, "fallback: failed to write content_block_start")
			return
		}
		if err := writeContentBlockDelta(w, blockIndex, AnthropicDelta{
			Type: anthropicTextDelta,
			Text: resp.Content,
		}); err != nil {
			anthropicLog.Error(err, "fallback: failed to write content_block_delta")
			return
		}
		if err := writeContentBlockStop(w, blockIndex); err != nil {
			anthropicLog.Error(err, "fallback: failed to write content_block_stop")
			return
		}
		blockIndex++
	}

	for _, tc := range resp.ToolCalls {
		if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: json.RawMessage(""),
		}); err != nil {
			anthropicLog.Error(err, "fallback: failed to write tool content_block_start")
			return
		}
		if err := writeContentBlockDelta(w, blockIndex, AnthropicDelta{
			Type:        "input_json_delta",
			PartialJSON: string(tc.Arguments),
		}); err != nil {
			anthropicLog.Error(err, "fallback: failed to write tool content_block_delta")
			return
		}
		if err := writeContentBlockStop(w, blockIndex); err != nil {
			anthropicLog.Error(err, "fallback: failed to write tool content_block_stop")
			return
		}
		blockIndex++
	}

	usage := anthropicStreamingUsage{}
	if err := usage.addResponse(resp); err != nil {
		anthropicLog.Error(err, "invalid fallback usage")
		_ = writeAnthropicStreamError(w, anthropicUsageOutOfRange)
		return
	}
	_ = writeMessageDelta(w, stopReason, usage)
	_ = writeMessageStop(w)
}

func writeAnthropicSSE(w *bufio.Writer, eventType string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
	return w.Flush()
}

func writeAnthropicStreamError(w *bufio.Writer, reason string) error {
	message := "provider stream ended without a successful terminal outcome"
	if strings.TrimSpace(reason) != "" {
		message = fmt.Sprintf("provider stream ended with non-success outcome %q", reason)
	}
	return writeAnthropicSSE(w, apiFieldError, AnthropicError{
		Type: apiFieldError,
		Error: AnthropicErrorDetail{
			Type:    "api_error",
			Message: message,
		},
	})
}

func writeAnthropicTextProgress(w *bufio.Writer, blockIndex *int, text string) {
	if text == "" {
		return
	}
	if err := writeContentBlockStart(w, *blockIndex, AnthropicContentBlock{Type: oaiContentTypeText, Text: ""}); err != nil {
		anthropicLog.Error(err, "failed to write progress content_block_start")
		return
	}
	_ = writeContentBlockDelta(w, *blockIndex, AnthropicDelta{Type: anthropicTextDelta, Text: text})
	_ = writeContentBlockStop(w, *blockIndex)
	(*blockIndex)++
}

func writeMessageStart(w *bufio.Writer, id, model string, inputTokens int) error {
	return writeAnthropicSSE(w, "message_start", AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:         id,
			Type:       apiFieldMessage,
			Role:       chatRoleAssistant,
			Content:    []AnthropicContentBlock{},
			Model:      model,
			StopReason: nil,
			Usage: AnthropicUsage{
				InputTokens:  inputTokens,
				OutputTokens: 0,
			},
		},
	})
}

type anthropicStreamingUsage struct {
	InputTokens           *int64 `json:"input_tokens,omitempty"`
	OutputTokens          int64  `json:"output_tokens"`
	CachedInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	hasResponse           bool
	deductedCacheRead     bool
	deductedCacheWrite    bool
}

const anthropicUsageOutOfRange = "usage_out_of_range"

func (usage *anthropicStreamingUsage) addResponse(resp *llm.CompletionResponse) error {
	if resp.InputTokens < 0 || int64(resp.InputTokens) > store.MaxUsageTokenCount ||
		resp.OutputTokens < 0 || int64(resp.OutputTokens) > store.MaxUsageTokenCount-usage.OutputTokens {
		return errors.New(anthropicUsageOutOfRange)
	}
	for _, count := range []*int64{resp.CachedInputTokens, resp.CacheWriteInputTokens} {
		if count != nil && (*count < 0 || *count > store.MaxUsageTokenCount) {
			return errors.New(anthropicUsageOutOfRange)
		}
	}
	var input *int64
	if resp.UsageReported || resp.InputTokens > 0 {
		count := int64(resp.InputTokens)
		if !resp.InputExcludesCache {
			if resp.CachedInputTokens != nil {
				count -= *resp.CachedInputTokens
				usage.deductedCacheRead = usage.deductedCacheRead || *resp.CachedInputTokens > 0
			}
			if resp.CacheWriteInputTokens != nil {
				count -= *resp.CacheWriteInputTokens
				usage.deductedCacheWrite = usage.deductedCacheWrite || *resp.CacheWriteInputTokens > 0
			}
		}
		if count < 0 {
			return errors.New(anthropicUsageOutOfRange)
		}
		input = &count
	}
	if usage.hasResponse {
		var err error
		usage.InputTokens, err = addReportedStreamTokens(usage.InputTokens, input)
		if err != nil {
			return err
		}
		usage.CachedInputTokens, err = addReportedStreamTokens(usage.CachedInputTokens, resp.CachedInputTokens)
		if err != nil {
			return err
		}
		usage.CacheWriteInputTokens, err = addReportedStreamTokens(usage.CacheWriteInputTokens, resp.CacheWriteInputTokens)
		if err != nil {
			return err
		}
	} else {
		usage.InputTokens = input
		usage.CachedInputTokens = resp.CachedInputTokens
		usage.CacheWriteInputTokens = resp.CacheWriteInputTokens
	}
	if usage.deductedCacheRead && usage.CachedInputTokens == nil || usage.deductedCacheWrite && usage.CacheWriteInputTokens == nil {
		usage.InputTokens = nil
	}
	usage.OutputTokens += int64(resp.OutputTokens)
	usage.hasResponse = true
	return nil
}

func addReportedStreamTokens(total, count *int64) (*int64, error) {
	if total == nil || count == nil {
		return nil, nil
	}
	if *count > store.MaxUsageTokenCount-*total {
		return nil, errors.New(anthropicUsageOutOfRange)
	}
	value := *total + *count
	return &value, nil
}

func retainStreamUsage(resp *llm.CompletionResponse, chunk llm.StreamChunk) {
	if chunk.UsageReported || chunk.InputTokens > 0 {
		resp.InputTokens = chunk.InputTokens
	}
	if chunk.UsageReported || chunk.OutputTokens > 0 {
		resp.OutputTokens = chunk.OutputTokens
	}
	if chunk.CachedInputTokens != nil {
		resp.CachedInputTokens = chunk.CachedInputTokens
	}
	if chunk.CacheWriteInputTokens != nil {
		resp.CacheWriteInputTokens = chunk.CacheWriteInputTokens
	}
	if chunk.UsageReported || chunk.InputTokens > 0 || chunk.CachedInputTokens != nil || chunk.CacheWriteInputTokens != nil {
		resp.InputExcludesCache = chunk.InputExcludesCache
	}
	resp.UsageReported = resp.UsageReported || chunk.UsageReported
}

func writeMessageDelta(w *bufio.Writer, stopReason string, usage anthropicStreamingUsage) error {
	return writeAnthropicSSE(w, "message_delta", struct {
		Type  string                  `json:"type"`
		Delta *AnthropicDelta         `json:"delta"`
		Usage anthropicStreamingUsage `json:"usage"`
	}{
		Type: "message_delta",
		Delta: &AnthropicDelta{
			StopReason: &stopReason,
		},
		Usage: usage,
	})
}

func writeMessageStop(w *bufio.Writer) error {
	return writeAnthropicSSE(w, "message_stop", AnthropicStreamEvent{
		Type: "message_stop",
	})
}

func writeContentBlockStart(w *bufio.Writer, index int, block AnthropicContentBlock) error {
	return writeAnthropicSSE(w, "content_block_start", AnthropicStreamEvent{
		Type:         "content_block_start",
		Index:        index,
		ContentBlock: &block,
	})
}

func writeContentBlockDelta(w *bufio.Writer, index int, delta AnthropicDelta) error {
	return writeAnthropicSSE(w, "content_block_delta", AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: &delta,
	})
}

func writeContentBlockStop(w *bufio.Writer, index int) error {
	return writeAnthropicSSE(w, "content_block_stop", AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: index,
	})
}

func isAllWaitingPolls(toolCalls []llm.ToolCall, messages []llm.Message) bool {
	if len(toolCalls) == 0 {
		return false
	}
	for _, tc := range toolCalls {
		if tc.Name != "wait_for_task" {
			return false
		}
	}
	for i := len(messages) - len(toolCalls); i < len(messages); i++ {
		if i < 0 || i >= len(messages) {
			return false
		}
		m := messages[i]
		if m.Role != chatRoleTool {
			return false
		}
		if !isTaskStillRunning(m.Content) {
			return false
		}
	}
	return true
}

func isTaskStillRunning(result string) bool {
	var parsed struct {
		Success bool `json:"success"`
		Data    struct {
			Phase string `json:"phase"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		return false
	}
	if !parsed.Success {
		return false
	}
	return !corev1alpha1.TaskPhase(parsed.Data.Phase).IsTerminal()
}
