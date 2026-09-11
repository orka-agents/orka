package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
)

type contextToolLoopProvider struct {
	mockAnthropicProvider
}

func (p *contextToolLoopProvider) Stream(ctx context.Context, req *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	response, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	chunks := make(chan llm.StreamChunk, len(response.ToolCalls)+2)
	chunks <- llm.StreamChunk{Content: response.Content}
	for _, call := range response.ToolCalls {
		chunks <- llm.StreamChunk{ToolCall: &call}
	}
	chunks <- llm.StreamChunk{Done: true, StopReason: response.StopReason}
	close(chunks)
	return chunks, nil
}

func TestAnthropicToolLoopsKeepOriginalRequestAfterControlPrompts(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, control := range []string{"continuation", "repetition"} {
			t.Run(fmt.Sprintf("%s/streaming=%t", control, streaming), func(t *testing.T) {
				current := "Keep the original request exactly: " + strings.Repeat("request detail ", 100)
				progress := "Progress: " + strings.Repeat("intermediate detail ", 95)
				mock := &contextToolLoopProvider{}
				if control == "continuation" {
					for range 2 {
						mock.responses = append(mock.responses, &llm.CompletionResponse{
							Content: progress, StopReason: oaiStopReasonEndTurn,
						})
					}
				} else {
					for i := range 3 {
						mock.responses = append(mock.responses, &llm.CompletionResponse{
							Content: progress, StopReason: oaiStopReasonToolUse,
							ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("call-%d", i),
								Name: "unavailable_context_probe", Arguments: json.RawMessage(`{}`)}},
						})
					}
				}
				mock.responses = append(mock.responses, &llm.CompletionResponse{
					Content: goalStateSentinel + "\nFinished.", StopReason: oaiStopReasonEndTurn,
				})
				req := &llm.CompletionRequest{Model: "fixture", SystemPrompt: "Follow the caller's instructions.",
					Messages: []llm.Message{
						{Role: "system", Content: "Keep this instruction."},
						{Role: "user", Content: "An earlier request."},
						{Role: "assistant", Content: strings.Repeat("old prior record ", 300)},
						{Role: "user", Content: current},
					},
				}
				config := ChatConfig{MaxSessionSize: 2800, MaxIterations: 20,
					MaxPrematureEndRetries: 3, MaxDuration: time.Minute, ToolTimeout: time.Second}
				if streaming {
					handler, app := setupTestAnthropicHandler()
					handler.config = config
					app.Post("/test", func(c fiber.Ctx) error {
						return handler.handleStreamingMessages(c, context.Background(), mock, req, req.Model, nil)
					})
					response, err := app.Test(httptest.NewRequest(http.MethodPost, "/test", nil))
					require.NoError(t, err)
					defer func() { _ = response.Body.Close() }()
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.Contains(t, string(body), "Finished.")
					require.Contains(t, string(body), "event: message_stop")
					require.NotContains(t, string(body), "event: error")
				} else {
					response, err := runNonStreamingToolLoop(context.Background(), mock, req, req.Model, config, nil)
					require.NoError(t, err)
					require.Equal(t, goalStateSentinel+"\nFinished.", response.Content)
				}
				require.Len(t, mock.requests, len(mock.responses))
				for i, sent := range mock.requests {
					count := 0
					for _, message := range sent.Messages {
						if message.Role == "user" && message.Content == current {
							count++
						}
					}
					require.Equal(t, 1, count, "provider request %d must retain the original user request exactly once", i)
					require.Equal(t, req.SystemPrompt, sent.SystemPrompt)
				}
				require.Empty(t, req.Messages[3].ID, "internal request identity must not mutate caller input")
			})
		}
	}
}

func TestAnthropicToolLoopContextOverflowKeepsOriginalRequest(t *testing.T) {
	current := "Keep the original request exactly: " + strings.Repeat("request detail ", 100)
	mock := &mockAnthropicProvider{
		responses: []*llm.CompletionResponse{
			{Content: "Progress: " + strings.Repeat("intermediate detail ", 95), StopReason: oaiStopReasonEndTurn},
			nil,
			{Content: goalStateSentinel + "\nFinished.", StopReason: oaiStopReasonEndTurn},
		},
		errors: []error{nil, &llm.ProviderError{StatusCode: 400, Message: "context length exceeded"}},
	}
	req := &llm.CompletionRequest{Model: "fixture", Messages: []llm.Message{{Role: "user", Content: current}}}
	_, err := runNonStreamingToolLoop(context.Background(), mock, req, req.Model,
		ChatConfig{MaxIterations: 20, MaxPrematureEndRetries: 3}, nil)
	require.NoError(t, err)
	require.Len(t, mock.requests, 3)
	require.Less(t, len(mock.requests[2].Messages), len(mock.requests[1].Messages))
	var found bool
	for _, message := range mock.requests[2].Messages {
		found = found || message.Role == "user" && message.Content == current
	}
	require.True(t, found, "context overflow retry must retain the original user request")
}
