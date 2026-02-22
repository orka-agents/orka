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
)

// handleStreamingMessages handles an Anthropic Messages API request with streaming and tool execution.
// It runs an agentic tool loop: stream LLM response → execute tools → stream results → repeat.
// The full Anthropic SSE envelope (message_start → ... → message_stop) spans all iterations.
func (h *AnthropicCompatHandler) handleStreamingMessages(
	c fiber.Ctx,
	provider llm.Provider,
	req *llm.CompletionRequest,
	model string,
	inputTokens int,
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

		for iteration := 0; iteration < h.config.MaxIterations; iteration++ {
			// Check context cancellation
			select {
			case <-streamCtx.Done():
				// Emit timeout message and close
				if err := writeContentBlockStart(w, blockIndex, AnthropicContentBlock{Type: "text", Text: ""}); err == nil {
					_ = writeContentBlockDelta(w, blockIndex, AnthropicDelta{Type: "text_delta", Text: "Request timed out during tool execution."})
					_ = writeContentBlockStop(w, blockIndex)
				}
				_ = writeMessageDelta(w, "end_turn", totalOutputTokens)
				_ = writeMessageStop(w)
				return
			default:
			}

			// Progress check every 5 iterations
			if iteration > 0 && iteration%5 == 0 {
				messages = append(messages, llm.Message{
					Role:    "user",
					Content: "[System: Progress check — summarize what you've done so far and what remains.]",
				})
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
					_ = writeMessageDelta(w, "end_turn", totalOutputTokens)
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

				// Emit tool calls
				for _, tc := range resp.ToolCalls {
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
				toolCalls = resp.ToolCalls
			} else {
				// Consume stream chunks
				inTextBlock := false
				for chunk := range streamCh {
					if chunk.Error != nil {
						anthropicLog.Error(chunk.Error, "stream chunk error in tool loop")
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

					// Handle tool calls
					if chunk.ToolCall != nil {
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
						toolCalls = append(toolCalls, *tc)
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
			}

			// No tool calls → final response, close the stream
			if len(toolCalls) == 0 {
				stopReason := "end_turn"
				_ = writeMessageDelta(w, stopReason, totalOutputTokens)
				_ = writeMessageStop(w)
				return
			}

			anthropicLog.Info("streaming tool loop iteration",
				"iteration", iteration,
				"tool_calls", len(toolCalls),
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

				result := executeToolCall(streamCtx, tc, h.config.ToolTimeout)

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
		}

		// Reached iteration limit — emit final message and close
		_ = writeMessageDelta(w, "end_turn", totalOutputTokens)
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
	msgID, model string,
) {
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		anthropicLog.Error(err, "fallback complete failed")
		// Emit an error as text content and close
		_ = writeContentBlockStart(w, 0, AnthropicContentBlock{Type: "text", Text: ""})
		_ = writeContentBlockDelta(w, 0, AnthropicDelta{Type: "text_delta", Text: "Error: " + err.Error()})
		_ = writeContentBlockStop(w, 0)
		stopReason := "end_turn"
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
		stopReason = "end_turn"
	}
	if len(resp.ToolCalls) > 0 && stopReason == "end_turn" {
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
