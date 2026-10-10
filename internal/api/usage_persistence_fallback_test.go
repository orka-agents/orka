/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
)

func TestCompatibilityStreamingDoesNotFallbackAfterUsagePersistenceFailure(t *testing.T) {
	for _, endpoint := range []string{"openai", "anthropic", "anthropic tool loop"} {
		for _, stage := range []string{"start", "probe", "first usage chunk"} {
			t.Run(endpoint+"/"+stage, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					serveUsagePersistenceFixture(w, r, stage == "first usage chunk")
				}))
				defer server.Close()
				providerType := "openai"
				if stage == "first usage chunk" {
					providerType = "anthropic"
				}
				provider, err := llm.NewProvider(providerType, llm.ProviderConfig{APIKey: "fixture-key", BaseURL: server.URL})
				require.NoError(t, err)
				var failed atomic.Bool
				ctx := llm.WithUsageRecorder(context.Background(), func(_ context.Context, observation store.UsageObservation) error {
					matches := stage == "start" && observation.Status == store.UsageStatusStarted ||
						stage == "probe" && strings.HasSuffix(observation.ID, "/response") ||
						stage == "first usage chunk" && observation.Status == "running"
					if matches && failed.CompareAndSwap(false, true) {
						return errors.New("fixture usage write failed")
					}
					// A later write succeeds, so a misplaced fallback would reach
					// the provider and could falsely return a successful response.
					return nil
				})
				body := runUsagePersistenceStream(t, endpoint, ctx, provider)
				require.True(t, failed.Load(), "recorder failure was not exercised")
				wantCalls := int32(1)
				if stage == "start" {
					wantCalls = 0
				}
				require.Equal(t, wantCalls, calls.Load(), "recorder failure must not cause another provider call")
				require.Contains(t, string(body), `"error"`)
				require.NotContains(t, string(body), "[DONE]")
				require.NotContains(t, string(body), "event: message_stop")
				require.NotContains(t, string(body), "event: message_delta")
			})
		}
	}
}

func TestAPICompletionDoesNotRetryUsagePersistenceFailure(t *testing.T) {
	tests := []struct {
		name          string
		providerError string
		nativeChat    bool
	}{
		{name: "tool loop streaming required", providerError: "streaming is required for this operation"},
		{name: "tool loop context overflow", providerError: "context length exceeded"},
		{name: "native chat context overflow", providerError: "context length exceeded", nativeChat: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": tt.providerError},
				})
			}))
			defer server.Close()
			provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture-key", BaseURL: server.URL})
			require.NoError(t, err)
			writeErr := errors.New("fixture usage write failed")
			var failed atomic.Bool
			ctx := llm.WithUsageRecorder(context.Background(), func(_ context.Context, observation store.UsageObservation) error {
				if strings.HasSuffix(observation.ID, "/finish") && failed.CompareAndSwap(false, true) {
					return writeErr
				}
				return nil
			})
			req := &llm.CompletionRequest{Model: "fixture", MaxTokens: 64, Messages: []llm.Message{{Role: "user", Content: "hello"}}}
			if tt.nativeChat {
				handler := &ChatHandler{}
				_, _, err = handler.callLLMWithRetry(ctx, provider, req.Messages, "", req.Model, nil, req.MaxTokens, 0)
			} else {
				_, err = runNonStreamingToolLoop(ctx, provider, req, req.Model, DefaultChatConfig(), nil)
			}
			require.ErrorIs(t, err, writeErr)
			require.True(t, llm.IsUsagePersistenceError(err))
			require.True(t, failed.Load())
			require.Equal(t, int32(1), calls.Load(), "recorder failure must prevent retries and stream fallback")
		})
	}
}

func TestToolLoopIterationLimitReturnsUsagePersistenceFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		serveUsagePersistenceFixture(w, r, true)
	}))
	defer server.Close()
	provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{APIKey: "fixture-key", BaseURL: server.URL})
	require.NoError(t, err)
	writeErr := errors.New("fixture usage write failed")
	ctx := llm.WithUsageRecorder(context.Background(), func(_ context.Context, observation store.UsageObservation) error {
		if strings.HasSuffix(observation.ID, "/finish") {
			return writeErr
		}
		return nil
	})
	config := DefaultChatConfig()
	config.MaxIterations = 0
	req := &llm.CompletionRequest{Model: "fixture", MaxTokens: 64, Messages: []llm.Message{{Role: "user", Content: "hello"}}}
	response, err := runNonStreamingToolLoop(ctx, provider, req, req.Model, config, nil)
	require.ErrorIs(t, err, writeErr)
	require.True(t, llm.IsUsagePersistenceError(err))
	require.Nil(t, response, "failed accounting must not be replaced with a successful synthetic response")
	require.Equal(t, int32(1), calls.Load())
}

func runUsagePersistenceStream(t *testing.T, endpoint string, ctx context.Context, provider llm.Provider) []byte {
	t.Helper()
	req := &llm.CompletionRequest{Model: "fixture", MaxTokens: 64, Messages: []llm.Message{{Role: "user", Content: "hello"}}}
	app := fiber.New()
	app.Post("/test", func(c fiber.Ctx) error {
		if endpoint == "openai" {
			handler, _ := setupTestOpenAIHandler()
			return handler.handleStreamingCompletion(c, ctx, provider, req, "completion-fixture", req.Model, 1, nil)
		}
		handler, _ := setupTestAnthropicHandler()
		if endpoint == "anthropic tool loop" {
			return handler.handleStreamingMessages(c, ctx, provider, req, req.Model, nil)
		}
		return handler.handleStreamingProxy(c, ctx, provider, req, req.Model)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodPost, "/test", nil))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return body
}

func serveUsagePersistenceFixture(w http.ResponseWriter, r *http.Request, anthropic bool) {
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	if anthropic && request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if anthropic {
		_, _ = io.WriteString(w, `{"id":"fixture","type":"message","role":"assistant","model":"fixture","content":[{"type":"text","text":"<ORKA_GOAL_STATE_REACHED>\nOK"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`)
		return
	}
	_, _ = io.WriteString(w, `{"id":"fixture","object":"response","model":"fixture","status":"completed","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"<ORKA_GOAL_STATE_REACHED>\nOK","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`)
}
