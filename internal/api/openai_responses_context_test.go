/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
)

func TestResponsesStreamPreservesRequestContext(t *testing.T) {
	chunks := make(chan llm.StreamChunk, 1)
	chunks <- llm.StreamChunk{Content: "Done.", Done: true, StopReason: "end_turn", InputTokens: 7, OutputTokens: 3, UsageReported: true}
	close(chunks)
	provider := &responsesDeadlineProvider{oaiMockProvider: &oaiMockProvider{streamCh: chunks}, observed: make(chan context.Context, 1)}
	const providerType = "responses-context-fixture"
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
	handler, app := setupTestOpenAIHandler(providerCRD("fixture", defaultNamespace, providerType, "test-model")...)
	handler.config.MaxDuration = 10 * time.Second

	type contextKey struct{}
	value := new(int(42))
	span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, Remote: true})
	bag, err := baggage.Parse("request=responses-context")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), contextKey{}, value)
	ctx = trace.ContextWithSpanContext(ctx, span)
	ctx = baggage.ContextWithBaggage(ctx, bag)
	observations := make(chan store.UsageObservation, 4)
	ctx = llm.WithUsageRecorder(ctx, func(_ context.Context, observation store.UsageObservation) error {
		observations <- observation
		return nil
	})
	deadline := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	app.Post(responsesPath, func(c fiber.Ctx) error {
		c.SetContext(ctx)
		defer cancel() // Match handler cleanup before Fiber invokes the writer.
		return handler.HandleResponses(c)
	})
	status, data := requestResponses(t, app, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
	require.Equal(t, http.StatusOK, status, string(data))
	events := parseResponsesSSE(t, data)
	require.Equal(t, "response.completed", events[len(events)-1]["type"], string(data))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	select {
	case streamCtx := <-provider.observed:
		actual, ok := streamCtx.Deadline()
		require.True(t, ok)
		require.Equal(t, deadline, actual, "handler setup must not restart the request budget")
		require.Same(t, value, streamCtx.Value(contextKey{}))
		require.Equal(t, span.TraceID(), trace.SpanContextFromContext(streamCtx).TraceID())
		require.Equal(t, bag.String(), baggage.FromContext(streamCtx).String())
		select {
		case <-streamCtx.Done():
		case <-time.After(time.Second):
			t.Fatal("completed stream context was not canceled")
		}
	default:
		t.Fatal("provider was not called")
	}
	var completed bool
	for len(observations) > 0 {
		observation := <-observations
		if observation.Complete {
			require.Equal(t, store.UsageStatusCompleted, observation.Status)
			require.Equal(t, new(int64(7)), observation.InputTokens)
			require.Equal(t, new(int64(3)), observation.OutputTokens)
			completed = true
		}
	}
	require.True(t, completed, "detaching the callback must retain the request usage recorder")
}

func TestResponsesStreamDeadlineCancellation(t *testing.T) {
	for _, hasDeadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("request deadline=%t", hasDeadline), func(t *testing.T) {
			provider := &responsesDeadlineProvider{oaiMockProvider: &oaiMockProvider{streamCh: make(chan llm.StreamChunk)}, observed: make(chan context.Context, 1)}
			handler := &OpenAICompatHandler{config: ChatConfig{MaxDuration: 200 * time.Millisecond}}
			var deadline time.Time
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if hasDeadline {
				deadline = time.Now().Add(200 * time.Millisecond)
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(ctx, deadline)
				defer deadlineCancel()
				handler.config.MaxDuration = 3 * time.Second
			}
			app := fiber.New()
			app.Post(responsesPath, func(c fiber.Ctx) error {
				defer cancel()
				request := &ResponsesRequest{Model: "test-model"}
				return handler.streamResponses(c, ctx, provider, &llm.CompletionRequest{ResponsesInput: true}, newResponsesResponse(request, request.Model), false, nil)
			})
			started := time.Now()
			status, data := requestResponses(t, app, `{"input":"wait"}`, true)
			require.Equal(t, http.StatusOK, status, string(data))
			events := parseResponsesSSE(t, data)
			require.Equal(t, "response.failed", events[len(events)-1]["type"])
			select {
			case streamCtx := <-provider.observed:
				actual, ok := streamCtx.Deadline()
				require.True(t, ok)
				if hasDeadline {
					require.Equal(t, deadline, actual)
				} else {
					require.False(t, actual.Before(started.Add(handler.config.MaxDuration)), "deadline-free callers need a fresh timeout")
				}
				require.ErrorIs(t, streamCtx.Err(), context.DeadlineExceeded, "handler return must not cancel inference early")
			default:
				t.Fatal("provider was not called")
			}
		})
	}
}

func TestResponsesStreamExpiredDeadline(t *testing.T) {
	chunks := make(chan llm.StreamChunk, 1)
	chunks <- llm.StreamChunk{Content: "must not complete", Done: true, StopReason: "end_turn"}
	close(chunks)
	provider := &oaiMockProvider{streamCh: chunks}
	handler := &OpenAICompatHandler{config: ChatConfig{MaxDuration: time.Second}}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	app := fiber.New()
	app.Post(responsesPath, func(c fiber.Ctx) error {
		request := &ResponsesRequest{Model: "test-model"}
		return handler.streamResponses(c, ctx, provider, &llm.CompletionRequest{ResponsesInput: true}, newResponsesResponse(request, request.Model), false, nil)
	})
	status, data := requestResponses(t, app, `{"input":"hello"}`, true)
	require.Equal(t, http.StatusOK, status, string(data))
	events := parseResponsesSSE(t, data)
	require.Equal(t, "response.failed", events[len(events)-1]["type"])
	require.NotContains(t, string(data), "must not complete")
}
