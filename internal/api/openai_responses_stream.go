/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep SSE JSON field names and event names literal to match the wire contract.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
)

// A single stream writer owns all event sequence numbers and output snapshots.
// Provider work runs separately so heartbeats detect disconnects even while a
// provider or coordinator tool is blocked. Write failure cancels that work.
type responsesStreamWriter struct {
	writer    *bufio.Writer
	response  *ResponsesResponse
	sequence  int
	textIndex int
	calls     map[string]bool
}

func (s *responsesStreamWriter) event(kind string, fields map[string]any) error {
	fields["type"] = kind
	fields["sequence_number"] = s.sequence
	s.sequence++
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.writer, "event: %s\ndata: %s\n\n", kind, data); err != nil {
		return err
	}
	return s.writer.Flush()
}

func (s *responsesStreamWriter) text(delta string) error {
	if delta == "" {
		return nil
	}
	if s.textIndex < 0 {
		s.textIndex = len(s.response.Output)
		item := newResponsesMessage()
		s.response.Output = append(s.response.Output, item)
		added := map[string]any{"id": item.ID, "type": item.Type, "status": item.Status, "role": item.Role, "content": []any{}}
		if err := s.event("response.output_item.added", map[string]any{"output_index": s.textIndex, "item": added}); err != nil {
			return err
		}
		if err := s.event("response.content_part.added", map[string]any{"item_id": item.ID, "output_index": s.textIndex, "content_index": 0, "part": item.Content[0]}); err != nil {
			return err
		}
	}
	item := &s.response.Output[s.textIndex]
	item.Content[0].Text += delta
	return s.event("response.output_text.delta", map[string]any{"item_id": item.ID, "output_index": s.textIndex, "content_index": 0, "delta": delta, "logprobs": []any{}})
}

func (s *responsesStreamWriter) finishText(status string) error {
	if s.textIndex < 0 {
		return nil
	}
	item := &s.response.Output[s.textIndex]
	item.Status = status
	fields := map[string]any{"item_id": item.ID, "output_index": s.textIndex, "content_index": 0, oaiContentTypeText: item.Content[0].Text, "logprobs": []any{}}
	if err := s.event("response.output_text.done", fields); err != nil {
		return err
	}
	if err := s.event("response.content_part.done", map[string]any{"item_id": item.ID, "output_index": s.textIndex, "content_index": 0, "part": item.Content[0]}); err != nil {
		return err
	}
	if err := s.event("response.output_item.done", map[string]any{"output_index": s.textIndex, "item": item}); err != nil {
		return err
	}
	s.textIndex = -1
	return nil
}

func (s *responsesStreamWriter) function(call llm.ToolCall) error {
	item, err := newResponsesFunction(call)
	if err != nil {
		return err
	}
	if s.calls[call.ID] {
		return fmt.Errorf("duplicate function call ID")
	}
	s.calls[call.ID] = true
	if err := s.finishText(completionStatusCompleted); err != nil {
		return err
	}
	index := len(s.response.Output)
	s.response.Output = append(s.response.Output, item)
	added := item
	empty := ""
	added.Arguments = &empty
	added.Status = responsesStatusInProgress
	if err := s.event("response.output_item.added", map[string]any{"output_index": index, "item": added}); err != nil {
		return err
	}
	if err := s.event("response.function_call_arguments.delta", map[string]any{"item_id": item.ID, "output_index": index, "delta": *item.Arguments}); err != nil {
		return err
	}
	if err := s.event("response.function_call_arguments.done", map[string]any{"item_id": item.ID, "output_index": index, "arguments": *item.Arguments, "name": item.Name}); err != nil {
		return err
	}
	return s.event("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

func (s *responsesStreamWriter) fail() {
	_ = s.finishText("incomplete")
	detail := &responsesError{Code: responsesServerError, Message: "provider failed to produce a valid Responses completion"}
	s.response.Status = "failed"
	s.response.Error = detail
	_ = s.event("error", map[string]any{"code": responsesServerError, responsesMessage: detail.Message, "param": nil})
	_ = s.event("response.failed", map[string]any{responsesObject: s.response})
}

func (h *OpenAICompatHandler) streamResponses(c fiber.Ctx, ctx context.Context, provider llm.Provider, req *llm.CompletionRequest, response *ResponsesResponse, coordinator bool, toolCtx *tools.ToolContext) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")
	base := detachedSpanContext(ctx)
	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, cancel := context.WithTimeout(base, h.config.MaxDuration)
		defer cancel()
		writer := &responsesStreamWriter{writer: w, response: response, textIndex: -1, calls: map[string]bool{}}
		if err := writer.event("response.created", map[string]any{responsesObject: response}); err != nil {
			return
		}
		if err := writer.event("response.in_progress", map[string]any{responsesObject: response}); err != nil {
			return
		}
		chunks := make(chan llm.StreamChunk)
		go h.produceResponsesChunks(streamCtx, provider, req, coordinator, toolCtx, chunks)
		heartbeat := time.NewTicker(time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-streamCtx.Done():
				writer.fail()
				return
			case <-heartbeat.C:
				if _, err := w.WriteString(": keep-alive\n\n"); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			case chunk, ok := <-chunks:
				if !ok || chunk.Error != nil {
					writer.fail()
					return
				}
				if err := writer.text(chunk.Content); err != nil {
					return
				}
				if chunk.ToolCall != nil {
					if err := writer.function(*chunk.ToolCall); err != nil {
						writer.fail()
						return
					}
				}
				if !chunk.Done {
					continue
				}
				completion := &llm.CompletionResponse{StopReason: chunk.StopReason, InputTokens: chunk.InputTokens, OutputTokens: chunk.OutputTokens}
				for _, item := range response.Output {
					if item.Type == finishReasonFunctionCall {
						completion.ToolCalls = append(completion.ToolCalls, llm.ToolCall{ID: item.CallID})
					}
				}
				if len(response.Output) == 0 || response.setOutcome(completion) != nil {
					writer.fail()
					return
				}
				if err := writer.finishText(response.Status); err != nil {
					return
				}
				_ = writer.event("response."+response.Status, map[string]any{responsesObject: response})
				return
			}
		}
	})
}

func (h *OpenAICompatHandler) produceResponsesChunks(ctx context.Context, provider llm.Provider, req *llm.CompletionRequest, coordinator bool, toolCtx *tools.ToolContext, chunks chan<- llm.StreamChunk) {
	defer close(chunks)
	send := func(chunk llm.StreamChunk) bool {
		select {
		case chunks <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if coordinator {
		structured := req.ResponseFormat != nil && req.ResponseFormat.Type != oaiContentTypeText
		observer := &toolLoopObserver{
			OnAssistantContent: func(content string) {
				if !structured {
					send(llm.StreamChunk{Content: stripGoalStateSentinel(content)})
				}
			},
			OnFinalContent: func(content string) { send(llm.StreamChunk{Content: stripGoalStateSentinel(content)}) },
			OnToolResult: func(call llm.ToolCall, result string) {
				if !structured {
					send(llm.StreamChunk{Content: formatToolProgress(call, result)})
				}
			},
		}
		completion, err := runToolLoopWithObserver(ctx, provider, req, req.Model, h.responsesLoopConfig(req), toolCtx, observer, toolLoopOptions{requireFinalCompletion: true})
		if err != nil || ctx.Err() != nil {
			send(llm.StreamChunk{Error: fmt.Errorf("coordinator completion failed")})
			return
		}
		if completion == nil {
			return
		}
		send(llm.StreamChunk{Done: true, StopReason: completion.StopReason, InputTokens: completion.InputTokens, OutputTokens: completion.OutputTokens})
		return
	}
	upstream, err := provider.Stream(ctx, req)
	if err != nil {
		// Preserve providers that only support non-streaming completions.
		completion, completeErr := provider.Complete(ctx, req)
		if completeErr != nil || completion == nil {
			send(llm.StreamChunk{Error: fmt.Errorf("completion failed")})
			return
		}
		if !send(llm.StreamChunk{Content: completion.Content}) {
			return
		}
		for _, call := range completion.ToolCalls {
			if !send(llm.StreamChunk{ToolCall: &call}) {
				return
			}
		}
		send(llm.StreamChunk{Done: true, StopReason: completion.StopReason, InputTokens: completion.InputTokens, OutputTokens: completion.OutputTokens})
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-upstream:
			if !ok {
				return
			}
			if !send(chunk) || chunk.Done || chunk.Error != nil {
				return
			}
		}
	}
}

func (h *OpenAICompatHandler) responsesLoopConfig(req *llm.CompletionRequest) ChatConfig {
	config := h.config
	if req.ResponseFormat != nil && req.ResponseFormat.Type != oaiContentTypeText {
		config.MaxPrematureEndRetries = 0
	}
	return config
}
