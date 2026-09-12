/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/llm/anthropic"
)

func TestProviderModelSettingsRequestBody(t *testing.T) {
	for _, mode := range []struct {
		name       string
		mode       apiMode
		path       string
		tokenField string
	}{
		{name: "responses", mode: apiModeResponses, path: "/responses", tokenField: "max_output_tokens"},
		{name: "chat", mode: apiModeChatCompletions, path: "/chat/completions", tokenField: "max_completion_tokens"},
	} {
		for _, stream := range []bool{false, true} {
			for _, tt := range []struct {
				name            string
				temperature     float64
				temperatureSet  bool
				wantTemperature bool
				maxTokens       int
			}{
				{name: "unset"},
				{name: "explicit_zero", temperatureSet: true, wantTemperature: true, maxTokens: 256},
				{name: "temperature_only", temperature: 0.2, temperatureSet: true, wantTemperature: true},
				{name: "tokens_only", maxTokens: 256},
				{name: "legacy_positive", temperature: 0.7, wantTemperature: true, maxTokens: 8192},
				{name: "legacy_negative", temperature: -1},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", mode.name, stream, tt.name), func(t *testing.T) {
					bodies := make(chan map[string]any, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != mode.path {
							t.Errorf("path = %q, want %q", r.URL.Path, mode.path)
						}
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("decode request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						bodies <- body
						writeModelSettingsResponse(w, mode.mode, stream)
					}))
					defer server.Close()

					provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
					if err != nil {
						t.Fatal(err)
					}
					provider.mode.Store(int32(mode.mode))
					req := &llm.CompletionRequest{
						Model: "gpt-4", Messages: []llm.Message{{Role: "user", Content: "hi"}},
						Temperature: tt.temperature, TemperatureSet: tt.temperatureSet, MaxTokens: tt.maxTokens,
					}
					completeModelSettingsRequest(t, provider, req, stream)

					body := <-bodies
					temperature, present := body["temperature"]
					if present != tt.wantTemperature || (present && temperature != tt.temperature) {
						t.Errorf("temperature = %v (present=%t), want %v (present=%t)", temperature, present, tt.temperature, tt.wantTemperature)
					}
					tokens, present := body[mode.tokenField]
					if present != (tt.maxTokens > 0) || (present && tokens != float64(tt.maxTokens)) {
						t.Errorf("%s = %v (present=%t), want %d", mode.tokenField, tokens, present, tt.maxTokens)
					}
				})
			}
		}
	}
}

func TestFallbackPreservesModelSettingsRequestBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			bodies := make(chan map[string]any, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				bodies <- body
				if r.URL.Path == "/v1/messages" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"provider unavailable"}}`)
					return
				}
				if r.URL.Path != "/chat/completions" {
					t.Errorf("unexpected fallback path %q", r.URL.Path)
				}
				writeModelSettingsResponse(w, apiModeChatCompletions, stream)
			}))
			defer server.Close()
			primary, err := anthropic.NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			fallback, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			fallback.mode.Store(int32(apiModeChatCompletions))
			provider := llm.NewFallbackProvider(primary, []llm.FallbackEntry{{Provider: fallback, Model: "gpt-4"}})
			req := &llm.CompletionRequest{
				Model: "claude-3", Messages: []llm.Message{{Role: "user", Content: "hi"}},
				Temperature: 0, TemperatureSet: true, MaxTokens: 256,
			}
			completeModelSettingsRequest(t, provider, req, stream)
			for _, want := range []struct{ model, tokenField string }{
				{model: "claude-3", tokenField: "max_tokens"},
				{model: "gpt-4", tokenField: "max_completion_tokens"},
			} {
				body := <-bodies
				if body["model"] != want.model || body[want.tokenField] != float64(256) || body["temperature"] != float64(0) {
					t.Errorf("model settings = model:%v tokens:%v temperature:%v, want %s/256/0", body["model"], body[want.tokenField], body["temperature"], want.model)
				}
			}
			if req.Model != "claude-3" {
				t.Errorf("fallback mutated original model: %q", req.Model)
			}
		})
	}
}

func completeModelSettingsRequest(t *testing.T, provider llm.Provider, req *llm.CompletionRequest, stream bool) {
	t.Helper()
	if !stream {
		if _, err := provider.Complete(context.Background(), req); err != nil {
			t.Fatalf("Complete(): %v", err)
		}
		return
	}
	chunks, err := provider.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream(): %v", err)
	}
	for chunk := range chunks {
		if chunk.Error != nil {
			t.Errorf("stream chunk: %v", chunk.Error)
		}
	}
}

func writeModelSettingsResponse(w http.ResponseWriter, mode apiMode, stream bool) {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		if mode == apiModeResponses {
			_, _ = fmt.Fprint(w, `{"id":"resp_1","status":"completed","model":"gpt-4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		} else {
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if mode == apiModeResponses {
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\",\"item_id\":\"item_1\",\"output_index\":0,\"content_index\":0}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-4\",\"output\":[{\"type\":\"message\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	} else {
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
