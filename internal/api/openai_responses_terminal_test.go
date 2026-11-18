/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep independent wire fixtures literal.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/stretchr/testify/require"
)

// A nil-returning provider is deliberately substituted at its interface. The
// route, middleware, authentication and response serialization remain real.
func TestResponsesProductionNilCompletion(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			const providerType = "responses-nil-completion-fixture"
			provider := &oaiMockProvider{}
			llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
			h, _ := setupTestOpenAIHandler(providerCRD("fixture", "default", providerType, "test-model")...)
			issuer := newTestOIDCProvider(t)
			server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, OIDC: issuer.config()})
			token := issuer.issueToken(t, testOIDCTokenOptions{Namespace: "default"})
			url := listenResponsesApp(t, server.app)
			status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"input":"hello"}`, disabled)
			require.Equal(t, http.StatusBadGateway, status, string(body))
			require.Contains(t, string(body), "provider failed to produce a valid Responses completion")
			require.Len(t, provider.requests, 1)
		})
	}
}

func TestResponsesProductionTerminalMessages(t *testing.T) {
	for _, mode := range []string{"terminal-only", "terminal-mixed", "terminal-adjacent", "partial-delta", "item-done-only", "contradictory-delta", "contradictory-done", "incomplete-only", "incomplete-prefix", "incomplete-item-done"} {
		t.Run(mode, func(t *testing.T) {
			order := []string{"before", "call-one", "after"}
			if mode == "terminal-only" {
				order = []string{"before"}
			}
			if mode == "terminal-adjacent" {
				order = []string{"before", "after"}
			}
			if strings.HasPrefix(mode, "incomplete-") {
				order = []string{"partial"}
			}
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
				response, completeEvents := orderedResponsesWire(order, "client_tool", `{}`)
				terminal := map[string]any{"type": "response.completed", "response": response}
				if strings.HasPrefix(mode, "incomplete-") {
					response["status"] = "incomplete"
					response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
					response["output"].([]any)[0].(map[string]any)["status"] = "incomplete"
					terminal["type"] = "response.incomplete"
				}
				events := []map[string]any{}
				switch mode {
				case "partial-delta", "contradictory-delta", "incomplete-prefix":
					delta := "be"
					if mode == "incomplete-prefix" {
						delta = "pa"
					}
					if mode == "contradictory-delta" {
						delta = "wrong"
					}
					events = append(events, map[string]any{"type": "response.output_text.delta", "item_id": "ordered-item-0", "output_index": 0, "content_index": 0, "delta": delta})
				case "item-done-only", "incomplete-item-done":
					for _, event := range completeEvents {
						if event["type"] == "response.output_item.done" {
							events = append(events, event)
						}
					}
				case "contradictory-done":
					events = completeEvents[:len(completeEvents)-1]
					// Copy through JSON so terminal mutation cannot change earlier events.
					encoded, err := json.Marshal(response)
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal(encoded, &response))
					response["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "different"
					terminal["response"] = response
				}
				upstreamSSE(w, append(events, terminal))
			})
			url := listenResponsesApp(t, server.app)
			status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
			require.Equal(t, http.StatusOK, status, string(body))
			events := parseResponsesSSE(t, body)
			terminal := events[len(events)-1]
			if strings.HasPrefix(mode, "contradictory-") {
				require.Equal(t, "response.failed", terminal["type"])
				return
			}
			expected := "response.completed"
			if strings.HasPrefix(mode, "incomplete-") {
				expected = "response.incomplete"
			}
			require.Equal(t, expected, terminal["type"])
			if expected == "response.incomplete" {
				response := terminal["response"].(map[string]any)
				require.Equal(t, "incomplete", response["output"].([]any)[0].(map[string]any)["status"])
				require.EqualValues(t, 3, response["usage"].(map[string]any)["output_tokens"])
			}
			require.Equal(t, order, responsesItemOrder(t, terminal["response"].(map[string]any)["output"].([]any)))
			var text strings.Builder
			for _, event := range events {
				if event["type"] == "response.output_text.delta" {
					text.WriteString(event["delta"].(string))
				}
			}
			wantText := ""
			for _, item := range order {
				if !strings.HasPrefix(item, "call-") {
					wantText += item
				}
			}
			require.Equal(t, wantText, text.String(), "terminal snapshots must not duplicate streamed text")
		})
	}
}

func TestResponsesProductionUnsupportedRefusal(t *testing.T) {
	payloads := []map[string]any{}
	for _, stream := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			for _, streamRequired := range []bool{false, true} {
				if disabled && streamRequired {
					continue
				}
				t.Run(fmt.Sprintf("stream=%t/disabled=%t/streamRequired=%t", stream, disabled, streamRequired), func(t *testing.T) {
					server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
						var request map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						if streamRequired && request["stream"] != true {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
							return
						}
						response := responsesFixtureResponse("unused")
						item := response["output"].([]any)[0].(map[string]any)
						item["content"] = []any{map[string]any{"type": "refusal", "refusal": "private fixture refusal detail"}}
						if request["stream"] == true {
							upstreamSSE(w, []map[string]any{
								{"type": "response.refusal.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": "private fixture refusal detail"},
								{"type": "response.output_item.done", "output_index": 0, "item": item},
								{"type": "response.completed", "response": response},
							})
						} else {
							w.Header().Set("Content-Type", "application/json")
							require.NoError(t, json.NewEncoder(w).Encode(response))
						}
					})
					url := listenResponsesApp(t, server.app)
					request := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
					status, _, body := productionResponsesRequest(t, url, token, request, disabled)
					require.NotContains(t, string(body), "private fixture refusal detail")
					if stream {
						require.Equal(t, http.StatusOK, status)
						events := parseResponsesSSE(t, body)
						terminal := events[len(events)-1]
						require.Equal(t, "response.failed", terminal["type"])
						detail := terminal["response"].(map[string]any)["error"].(map[string]any)
						require.Equal(t, "server_error", detail["code"])
						require.Contains(t, detail["message"], "refusals are not supported")
						diagnostic := events[len(events)-2]
						require.Equal(t, "error", diagnostic["type"])
						require.Equal(t, "unsupported_provider_outcome", diagnostic["code"])
						require.Contains(t, diagnostic["message"], "refusals are not supported")
						require.NotContains(t, string(body), "response.completed")
						payloads = append(payloads, events...)
					} else {
						require.Equal(t, http.StatusUnprocessableEntity, status, string(body))
						var result map[string]any
						require.NoError(t, json.Unmarshal(body, &result))
						detail := result["error"].(map[string]any)
						require.Equal(t, "unsupported_provider_outcome", detail["code"])
						require.Contains(t, detail["message"], "refusals are not supported")
					}
				})
			}
		}
	}
	validateResponsesTerminalSchemas(t, payloads)
}

func validateResponsesTerminalSchemas(t *testing.T, payloads []map[string]any) {
	t.Helper()
	if python := os.Getenv("ORKA_RESPONSES_INTEROP_PYTHON"); python != "" {
		data, err := json.Marshal(payloads)
		require.NoError(t, err)
		cmd := exec.CommandContext(t.Context(), python, "../../scripts/fixtures/agent-framework-responses/validate.py")
		cmd.Stdin = bytes.NewReader(data)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		t.Log(string(output))
	}
}

func TestResponsesProductionTerminalCoordinator(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			server, token := setupProductionResponses(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				if request["stream"] != true {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprint(w, `{"error":{"message":"streaming is required"}}`)
					return
				}
				response, _ := orderedResponsesWire([]string{goalStateSentinel + "\nfinished"}, "", "")
				upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": response}})
			})
			url := listenResponsesApp(t, server.app)
			request := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
			status, _, body := productionResponsesRequest(t, url, token, request, false)
			require.Equal(t, http.StatusOK, status, string(body))
			require.NotContains(t, string(body), goalStateSentinel)
			require.Contains(t, string(body), "finished")
			require.EqualValues(t, 2, calls.Load(), "one non-stream attempt and one actual stream continuation")
		})
	}
}

func TestResponsesProductionRefusalNonStreamingProviderFallback(t *testing.T) {
	const providerType = "responses-refusal-fallback-fixture"
	provider := &oaiMockProvider{streamErr: fmt.Errorf("streaming unsupported"), resp: &llm.CompletionResponse{StopReason: "refusal"}}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
	h, _ := setupTestOpenAIHandler(providerCRD("fixture", "default", providerType, "test-model")...)
	issuer := newTestOIDCProvider(t)
	server := NewServer(h.client, nil, ServerConfig{WatchNamespace: "default", Chat: h.config, OIDC: issuer.config()})
	token := issuer.issueToken(t, testOIDCTokenOptions{Namespace: "default"})
	url := listenResponsesApp(t, server.app)
	status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
	require.Equal(t, http.StatusOK, status)
	events := parseResponsesSSE(t, body)
	require.Equal(t, "response.failed", events[len(events)-1]["type"])
	require.Equal(t, "unsupported_provider_outcome", events[len(events)-2]["code"])
	require.Len(t, provider.requests, 1)
	validateResponsesTerminalSchemas(t, events)
}

func TestResponsesProductionClosedTextItem(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		for _, mode := range []string{"unchanged", "open-prefix", "terminal-suffix", "incomplete-suffix", "done-suffix", "late-delta"} {
			t.Run(fmt.Sprintf("indexed=%t/%s", indexed, mode), func(t *testing.T) {
				server, token := setupProductionResponses(t, func(w http.ResponseWriter, _ *http.Request) {
					original, _ := orderedResponsesWire([]string{"before"}, "", "")
					item := original["output"].([]any)[0].(map[string]any)
					finalText := "before"
					if mode != "unchanged" {
						finalText += "after"
					}
					final, _ := orderedResponsesWire([]string{finalText}, "", "")
					delta := map[string]any{"type": "response.output_text.delta", "item_id": item["id"], "content_index": 0, "delta": "before"}
					done := map[string]any{"type": "response.output_item.done", "item": item}
					if indexed {
						delta["output_index"], done["output_index"] = 0, 0
					}
					events := []map[string]any{delta}
					if mode != "open-prefix" {
						events = append(events, done)
					}
					if mode == "done-suffix" || mode == "late-delta" {
						late := map[string]any{"type": "response.output_item.done", "item": final["output"].([]any)[0]}
						if mode == "late-delta" {
							late = map[string]any{"type": "response.output_text.delta", "item_id": item["id"], "content_index": 0, "delta": "after"}
						}
						if indexed {
							late["output_index"] = 0
						}
						events = append(events, late)
					}
					terminalType := "response.completed"
					if mode == "incomplete-suffix" {
						terminalType, final["status"] = "response.incomplete", "incomplete"
						final["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
						final["output"].([]any)[0].(map[string]any)["status"] = "incomplete"
					}
					upstreamSSE(w, append(events, map[string]any{"type": terminalType, "response": final}))
				})
				url := listenResponsesApp(t, server.app)
				status, _, body := productionResponsesRequest(t, url, token, `{"model":"fixture/test-model","store":false,"stream":true,"input":"hello"}`, true)
				require.Equal(t, http.StatusOK, status, string(body))
				events := parseResponsesSSE(t, body)
				terminal := events[len(events)-1]
				output := terminal["response"].(map[string]any)["output"].([]any)
				if mode == "unchanged" || mode == "open-prefix" {
					require.Equal(t, "response.completed", terminal["type"])
					want := "before"
					if mode == "open-prefix" {
						want += "after"
					}
					require.Equal(t, []string{want}, responsesItemOrder(t, output))
					return
				}
				require.Equal(t, "response.failed", terminal["type"], "completed text cannot change or reopen")
				require.Equal(t, []string{"before"}, responsesItemOrder(t, output))
			})
		}
	}
}
