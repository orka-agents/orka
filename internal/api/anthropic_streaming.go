/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/tools"
)

// handleStreamingMessages handles an Anthropic Messages API request with streaming and tool execution.
// It runs an agentic tool loop: stream LLM response → execute tools → stream results → repeat.
// The full Anthropic SSE envelope (message_start → ... → message_stop) spans all iterations.
func (h *AnthropicCompatHandler) handleStreamingMessages( //nolint:gocyclo
	c fiber.Ctx,
	provider llm.Provider,
	req *llm.CompletionRequest,
	model string,
	inputTokens int,
	toolCtx *tools.ToolContext,
) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	capturedProvider := provider
	capturedReq := req

	return c.SendStreamWriter(func(w *bufio.Writer) {
		msgID := "msg_" + uuid.New().String()

		streamCtx, streamCancel := context.WithTimeout(context.Background(), h.config.MaxDuration)
		defer streamCancel()

		// Emit message_start once for the entire tool loop
		if err := writeMessageStart(w, msgID, model, inputTokens); err != nil {
			anthropicLog.Error(err, "failed to write message_start")
			return
		}

		blockIndex := 0
		messages := make([]llm.Message, len(capturedReq.Messages))
		copy(messages, capturedReq.Messages)
		totalOutputTokens := 0
		repetitionTracker := make(map[string]int)
		exposedToolNames := completionToolNameSet(capturedReq.Tools)
		prematureEndRetries := 0

		for iteration := 0; iteration < h.config.MaxIterations; iteration++ {
			// Check context cancellation
			select {
			case <-streamCtx.Done():
				// Emit timeout message and close
				if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
					_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: "text_delta", Text: "Request timed out during tool execution."})
					_ = writeContentBlockStop(w, blockIndex)
				}
				_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalOutputTokens)
				_ = writeMessageStop(w)
				return
			default:
			}

			// Truncate conversation if needed
			if h.config.MaxSessionSize > 0 {
				tokenBudget := h.config.MaxSessionSize / 4
				messages = llm.TruncateMessages(messages, tokenBudget)
			}

			// Build request for this iteration
			compReq := &llm.CompletionRequest{
				Model:        model,
				Messages:     messages,
				SystemPrompt: capturedReq.SystemPrompt,
				Tools:        capturedReq.Tools,
				MaxTokens:    capturedReq.MaxTokens,
				Temperature:  capturedReq.Temperature,
			}

			// Try streaming from provider
			var toolCalls []llm.ToolCall
			var textContent string

			streamCh, err := capturedProvider.Stream(streamCtx, compReq)
			if err != nil {
				// Fallback to Complete
				resp, completeErr := capturedProvider.Complete(streamCtx, compReq)
				if completeErr != nil {
					anthropicLog.Error(completeErr, "completion failed in tool loop")
					if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
						_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: "text_delta", Text: "Error: " + completeErr.Error()})
						_ = writeContentBlockStop(w, blockIndex)
					}
					_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalOutputTokens)
					_ = writeMessageStop(w)
					return
				}

				totalOutputTokens += resp.OutputTokens

				// Emit text content
				if resp.Content != "" {
					textContent = resp.Content
					if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
						_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: "text_delta", Text: resp.Content})
						_ = writeContentBlockStop(w, blockIndex)
						blockIndex++
					}
				}

				// Capture tool calls for server-side execution (don't emit tool_use blocks
				// to the client — the client must not see them or it will try to execute locally)
				toolCalls = resp.ToolCalls
			} else {
				// Consume stream chunks
				inTextBlock := false
				streamError := false
				for chunk := range streamCh {
					if chunk.Error != nil {
						anthropicLog.Error(chunk.Error, "stream chunk error in tool loop")
						streamError = true
						break
					}

					// Handle text content
					if chunk.Content != "" {
						if !inTextBlock {
							if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
								Type: "text", Text: "",
							}); err != nil {
								break
							}
							inTextBlock = true
						}
						if err := writeContentBlockDelta(w, blockIndex, AnthropicDelta{
							Type: "text_delta", Text: chunk.Content,
						}); err != nil {
							break
						}
						textContent += chunk.Content
					}

					// Handle tool calls — capture for server-side execution but don't
					// emit tool_use blocks to the client stream
					if chunk.ToolCall != nil {
						if inTextBlock {
							_ = writeContentBlockStop(w, blockIndex)
							blockIndex++
							inTextBlock = false
						}

						toolCalls = append(toolCalls, *chunk.ToolCall)
					}

					if chunk.Done {
						if inTextBlock {
							_ = writeContentBlockStop(w, blockIndex)
							blockIndex++
							inTextBlock = false
						}
						totalOutputTokens += estimateTokens(textContent)
						break
					}
				}

				// Close text block if stream ended without Done
				if inTextBlock {
					_ = writeContentBlockStop(w, blockIndex)
					blockIndex++
				}

				// If stream errored with no content, fall back to Complete
				if streamError && textContent == "" && len(toolCalls) == 0 {
					resp, completeErr := capturedProvider.Complete(streamCtx, compReq)
					if completeErr != nil {
						anthropicLog.Error(completeErr, "fallback completion also failed")
						_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalOutputTokens)
						_ = writeMessageStop(w)
						return
					}
					totalOutputTokens += resp.OutputTokens
					if resp.Content != "" {
						textContent = resp.Content
						if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
							_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: "text_delta", Text: resp.Content})
							_ = writeContentBlockStop(w, blockIndex)
							blockIndex++
						}
					}
					toolCalls = resp.ToolCalls
				}
			}

			// No tool calls — potentially final response. Guard against premature
			// end-of-turn: if the streamed text lacks the GOAL_STATE sentinel and
			// we haven't yet exhausted the retry budget, inject a "continue with
			// next tool_use" reminder and re-loop. The client will see the
			// premature text already streamed plus the recovery, but the
			// workflow continues instead of validation/review/PR being skipped.
			if len(toolCalls) == 0 {
				if hasGoalStateSentinelPrefix(textContent) {
					stopReason := oaiStopReasonEndTurn
					_ = writeMessageDelta(w, stopReason, totalOutputTokens)
					_ = writeMessageStop(w)
					return
				}
				if prematureEndRetries >= h.config.MaxPrematureEndRetries {
					anthropicLog.Info("streaming: premature end of turn — retry budget exhausted, closing stream anyway",
						"iteration", iteration,
						"retries", prematureEndRetries,
					)
					stopReason := oaiStopReasonEndTurn
					_ = writeMessageDelta(w, stopReason, totalOutputTokens)
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
				if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
					_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: "text_delta", Text: "[⚠️  premature end — re-prompting coordinator]"})
					_ = writeContentBlockStop(w, blockIndex)
					blockIndex++
				}
				messages = append(messages, llm.Message{
					Role:    "assistant",
					Content: textContent,
				})
				messages = append(messages, llm.Message{
					Role:    "user",
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

			// Append assistant message with tool calls
			messages = append(messages, llm.Message{
				Role:      "assistant",
				Content:   textContent,
				ToolCalls: toolCalls,
			})

			// Execute tools and emit results
			var repetitionWarning string
			for _, tc := range toolCalls {
				// Check repetition
				argsHash := hashArgs(tc.Name, tc.Arguments)
				repetitionTracker[argsHash]++
				if repetitionTracker[argsHash] >= 3 {
					repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, repetitionTracker[argsHash])
					iteration += 5
				}

				result := executeExposedToolCall(streamCtx, tc, h.config.ToolTimeout, toolCtx, exposedToolNames)

				// Emit tool result as a text content block so the user sees what happened
				resultPreview := result
				if len(resultPreview) > 2000 {
					resultPreview = resultPreview[:2000] + "..."
				}
				if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
					Type: "text", Text: "",
				}); err == nil {
					_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{
						Type: "text_delta",
						Text: fmt.Sprintf("[Tool %s result]: %s", tc.Name, resultPreview),
					})
					_ = writeContentBlockStop(w, blockIndex)
					blockIndex++
				}

				messages = append(messages, llm.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    result,
				})
			}

			if repetitionWarning != "" {
				messages = append(messages, llm.Message{
					Role:    "user",
					Content: repetitionWarning,
				})
			}

			// Auto-poll: if all tool calls were wait_for_task/check_task_progress
			// and all returned "still running", skip the LLM call and re-execute
			// directly. This prevents the LLM from randomly ending the loop.
			if repetitionWarning == "" && isAllWaitingPolls(toolCalls, messages) {
				for {
					select {
					case <-streamCtx.Done():
						_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalOutputTokens)
						_ = writeMessageStop(w)
						return
					default:
					}

					// Emit progress
					if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
						_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{
							Type: "text_delta", Text: "[⏳ Auto-polling tasks...]",
						})
						_ = writeContentBlockStop(w, blockIndex)
						blockIndex++
					}

					// Re-execute the same wait/check calls
					allStillRunning := true
					// Remove the old tool results from messages (last len(toolCalls) messages)
					messages = messages[:len(messages)-len(toolCalls)]
					for _, tc := range toolCalls {
						result := executeExposedToolCall(streamCtx, tc, h.config.ToolTimeout, toolCtx, exposedToolNames)
						messages = append(messages, llm.Message{
							Role:       "tool",
							ToolCallID: tc.ID,
							Name:       tc.Name,
							Content:    result,
						})
						if !isTaskStillRunning(result) {
							allStillRunning = false
						}
					}

					if !allStillRunning {
						// Task finished — break out to let the LLM process the result
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

		// Reached iteration limit — emit final message and close
		_ = writeMessageDelta(w, oaiStopReasonEndTurn, totalOutputTokens)
		_ = writeMessageStop(w)
	})
}

// handleStreamingProxy is the transparent-proxy streaming path.
// It streams the LLM response directly to the client without executing tools server-side.
func (h *AnthropicCompatHandler) handleStreamingProxy(
	c fiber.Ctx,
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

	return c.SendStreamWriter(func(w *bufio.Writer) {
		msgID := "msg_" + uuid.New().String()

		streamCtx, streamCancel := context.WithTimeout(context.Background(), h.config.MaxDuration)
		defer streamCancel()

		if err := writeMessageStart(w, msgID, model, 0); err != nil {
			anthropicLog.Error(err, "failed to write message_start")
			return
		}

		streamCh, err := capturedProvider.Stream(streamCtx, capturedReq)
		if err != nil {
			// Fallback to non-streaming Complete
			h.handleStreamingFallback(w, streamCtx, capturedProvider, capturedReq)
			return
		}

		blockIndex := 0
		inTextBlock := false
		hasToolCalls := false

		for chunk := range streamCh {
			if chunk.Error != nil {
				anthropicLog.Error(chunk.Error, "stream chunk error")
				break
			}

			if chunk.Content != "" {
				if !inTextBlock {
					if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
						Type: "text", Text: "",
					}); err != nil {
						break
					}
					inTextBlock = true
				}
				if err := writeContentBlockDelta(w, blockIndex, AnthropicDelta{
					Type: "text_delta", Text: chunk.Content,
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

			if chunk.Done {
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

		// Use the correct stop reason — "tool_use" if tool calls were emitted
		stopReason := oaiStopReasonEndTurn
		if hasToolCalls {
			stopReason = oaiStopReasonToolUse
		}
		_ = writeMessageDelta(w, stopReason, 0)
		_ = writeMessageStop(w)
	})
}

// estimateTokens provides a rough token count estimate from text length.
func estimateTokens(text string) int {
	return len(text) / 4
}

// handleStreamingFallback uses provider.Complete() and emits the result as a complete SSE sequence.
func (h *AnthropicCompatHandler) handleStreamingFallback(
	w *bufio.Writer,
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
) {
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		anthropicLog.Error(err, "fallback complete failed")
		// Emit an error as text content and close
		_ = writeContentBlockStart(w, 0, AnthropicContentBlock{Type: "text", Text: ""})
		_ = writeContentBlockDelta(w, 0, AnthropicDelta{Type: "text_delta", Text: "Error: " + err.Error()})
		_ = writeContentBlockStop(w, 0)
		stopReason := oaiStopReasonEndTurn
		_ = writeMessageDelta(w, stopReason, 0)
		_ = writeMessageStop(w)
		return
	}

	blockIndex := 0

	// Emit text content
	if resp.Content != "" {
		if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{
			Type: "text",
			Text: "",
		}); err != nil {
			anthropicLog.Error(err, "fallback: failed to write content_block_start")
			return
		}
		if err := writeContentBlockDelta(w, blockIndex, AnthropicDelta{
			Type: "text_delta",
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

	// Emit tool calls
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

	stopReason := resp.StopReason
	if stopReason == "" {
		stopReason = oaiStopReasonEndTurn
	}
	if len(resp.ToolCalls) > 0 && stopReason == oaiStopReasonEndTurn {
		stopReason = "tool_use"
	}

	_ = writeMessageDelta(w, stopReason, resp.OutputTokens)
	_ = writeMessageStop(w)
}

// writeAnthropicSSE writes a named SSE event in Anthropic format.
func writeAnthropicSSE(w *bufio.Writer, eventType string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
	return w.Flush()
}

// writeMessageStart emits the message_start event.
func writeMessageStart(w *bufio.Writer, id, model string, inputTokens int) error {
	return writeAnthropicSSE(w, "message_start", AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:         id,
			Type:       "message",
			Role:       "assistant",
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

// writeMessageDelta emits the message_delta event with stop_reason and usage.
func writeMessageDelta(w *bufio.Writer, stopReason string, outputTokens int) error {
	return writeAnthropicSSE(w, "message_delta", AnthropicStreamEvent{
		Type: "message_delta",
		Delta: &AnthropicDelta{
			StopReason: &stopReason,
		},
		Usage: &AnthropicUsage{
			OutputTokens: outputTokens,
		},
	})
}

// writeMessageStop emits the message_stop event.
func writeMessageStop(w *bufio.Writer) error {
	return writeAnthropicSSE(w, "message_stop", AnthropicStreamEvent{
		Type: "message_stop",
	})
}

// writeContentBlockStart emits a content_block_start event.
func writeContentBlockStart(w *bufio.Writer, index int, block AnthropicContentBlock) error {
	return writeAnthropicSSE(w, "content_block_start", AnthropicStreamEvent{
		Type:         "content_block_start",
		Index:        index,
		ContentBlock: &block,
	})
}

// writeContentBlockDelta emits a content_block_delta event.
func writeContentBlockDelta(w *bufio.Writer, index int, delta AnthropicDelta) error {
	return writeAnthropicSSE(w, "content_block_delta", AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: index,
		Delta: &delta,
	})
}

// writeContentBlockStop emits a content_block_stop event.
func writeContentBlockStop(w *bufio.Writer, index int) error {
	return writeAnthropicSSE(w, "content_block_stop", AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: index,
	})
}

// isAllWaitingPolls returns true if every tool call in the last iteration was
// wait_for_task and every result indicates the task is still running.
// Only wait_for_task is allowed (it has a built-in 2s polling interval).
// check_task_progress is excluded because it returns instantly and would cause a tight loop.
func isAllWaitingPolls(toolCalls []llm.ToolCall, messages []llm.Message) bool {
	if len(toolCalls) == 0 {
		return false
	}
	for _, tc := range toolCalls {
		if tc.Name != "wait_for_task" {
			return false
		}
	}
	// Check the last N messages (tool results) for running phase
	for i := len(messages) - len(toolCalls); i < len(messages); i++ {
		if i < 0 || i >= len(messages) {
			return false
		}
		m := messages[i]
		if m.Role != "tool" {
			return false
		}
		if !isTaskStillRunning(m.Content) {
			return false
		}
	}
	return true
}

// isTaskStillRunning parses a tool result JSON and checks if the task is in a non-terminal phase.
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
	// Terminal phases — auto-poll should stop
	switch parsed.Data.Phase {
	case "Succeeded", "Failed", "Cancelled":
		return false
	default:
		// Running, Pending, Scheduled — still active
		return true
	}
}
