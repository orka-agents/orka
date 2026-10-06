/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestResponsesProductionAnthropicTerminalRefusal(t *testing.T) {
	for _, outcome := range []string{"refusal", "end_turn", "max_tokens"} {
		for _, disabled := range []bool{false, true} {
			for _, streamMode := range []string{"both", "required", "unsupported"} {
				t.Run(fmt.Sprintf("%s/disabled=%t/stream=%s", outcome, disabled, streamMode), func(t *testing.T) {
					text := "Done. <!-- GOAL_STATE:SATISFIED -->"
					if outcome == "refusal" {
						text = "private fixture refusal detail"
					}
					handler, _ := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						if (streamMode == "required" && request["stream"] != true) || (streamMode == "unsupported" && request["stream"] == true) {
							message := "streaming is required"
							if streamMode == "unsupported" {
								message = "streaming is not supported"
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}}))
							return
						}
						message := map[string]any{
							"id": "fixture-message", "type": "message", "role": "assistant", "model": "test-model",
							"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": outcome,
							"usage": map[string]any{"input_tokens": 4, "output_tokens": 1, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1},
						}
						if request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(message))
							return
						}
						message["content"], message["stop_reason"] = []any{}, nil
						message["usage"].(map[string]any)["output_tokens"] = 0
						upstreamSSE(w, []map[string]any{
							{"type": "message_start", "message": message},
							{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
							{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}},
							{"type": "content_block_stop", "index": 0},
							{"type": "message_delta", "delta": map[string]any{"stop_reason": outcome}, "usage": map[string]any{"output_tokens": 1}},
							{"type": "message_stop"},
						})
					})
					provider := &corev1alpha1.Provider{}
					require.NoError(t, handler.client.Get(t.Context(), client.ObjectKey{Namespace: defaultNamespace, Name: "fixture"}, provider))
					provider.Spec.Type = corev1alpha1.ProviderTypeAnthropic
					require.NoError(t, handler.client.Update(t.Context(), provider))
					issuer := newTestOIDCProvider(t)
					server := NewServer(handler.client, nil, ServerConfig{WatchNamespace: defaultNamespace, Chat: handler.config, OIDC: issuer.config()})
					token := issuer.issueToken(t, testOIDCTokenOptions{Namespace: defaultNamespace})
					status, _, body := productionResponsesRequest(t, listenResponsesApp(t, server.app), token,
						`{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, disabled)
					require.Equal(t, http.StatusOK, status, string(body))
					events := parseResponsesSSE(t, body)
					terminal := events[len(events)-1]
					if outcome == "refusal" {
						require.NotContains(t, string(body), text)
						require.Equal(t, "response.failed", terminal["type"])
						require.Equal(t, "unsupported_provider_outcome", events[len(events)-2]["code"])
						require.NotContains(t, string(body), "response.output_text.delta")
						return
					}
					want := "response.completed"
					if outcome == "max_tokens" {
						want = "response.incomplete"
					}
					require.Equal(t, want, terminal["type"], string(body))
					require.Contains(t, string(body), "Done.")
					usage := terminal["response"].(map[string]any)["usage"].(map[string]any)
					require.EqualValues(t, 7, usage["input_tokens"])
					require.EqualValues(t, 1, usage["output_tokens"])
				})
			}
		}
	}
}
