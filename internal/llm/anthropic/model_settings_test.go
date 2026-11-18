/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestProviderModelSettingsRequestBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tt := range []struct {
			name            string
			temperature     float64
			temperatureSet  bool
			wantTemperature bool
			maxTokens       int
			wantMaxTokens   float64
		}{
			{name: "unset", wantMaxTokens: 4096},
			{name: "explicit_zero", temperatureSet: true, wantTemperature: true, maxTokens: 256, wantMaxTokens: 256},
			{name: "temperature_only", temperature: 0.2, temperatureSet: true, wantTemperature: true, wantMaxTokens: 4096},
			{name: "tokens_only", maxTokens: 256, wantMaxTokens: 256},
			{name: "legacy_positive", temperature: 0.7, wantTemperature: true, maxTokens: 8192, wantMaxTokens: 8192},
			{name: "legacy_negative", temperature: -1, wantMaxTokens: 4096},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tt.name), func(t *testing.T) {
				bodies := make(chan map[string]any, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/messages" {
						t.Errorf("path = %q, want /v1/messages", r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode request: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					bodies <- body
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
					_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
					_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}))
				defer server.Close()

				provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
				if err != nil {
					t.Fatal(err)
				}
				req := &llm.CompletionRequest{
					Model: "claude-3", Messages: []llm.Message{{Role: "user", Content: "hi"}},
					Temperature: tt.temperature, TemperatureSet: tt.temperatureSet, MaxTokens: tt.maxTokens,
				}
				if stream {
					chunks, err := provider.Stream(context.Background(), req)
					if err != nil {
						t.Fatalf("Stream(): %v", err)
					}
					for chunk := range chunks {
						if chunk.Error != nil {
							t.Errorf("stream chunk: %v", chunk.Error)
						}
					}
				} else if _, err := provider.Complete(context.Background(), req); err != nil {
					t.Fatalf("Complete(): %v", err)
				}

				body := <-bodies
				temperature, present := body["temperature"]
				if present != tt.wantTemperature || (present && temperature != tt.temperature) {
					t.Errorf("temperature = %v (present=%t), want %v (present=%t)", temperature, present, tt.temperature, tt.wantTemperature)
				}
				if tokens := body["max_tokens"]; tokens != tt.wantMaxTokens {
					t.Errorf("max_tokens = %v, want %v", tokens, tt.wantMaxTokens)
				}
			})
		}
	}
}
