/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/llm"
	openaiprovider "github.com/orka-agents/orka/internal/llm/openai"
	"github.com/orka-agents/orka/workers/common"
)

// Exercise providers constructed by run, not a second implementation of its
// environment-to-config mapping. Kubernetes startup is outside this regression.
func TestAzureDeploymentWorkerRouting(t *testing.T) {
	for _, api := range []string{"responses", "chat"} {
		t.Run(api, func(t *testing.T) {
			type observedRequest struct {
				uri  string
				body map[string]any
			}
			requests := make(chan observedRequest, 12)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode routing body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- observedRequest{r.URL.RequestURI(), body}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/responses") && api == "chat" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"error":{"message":"unsupported API","code":"unsupported_api"}}`)
					return
				}
				if body["stream"] == true {
					w.Header().Set("Content-Type", "text/event-stream")
					if api == "responses" {
						_, _ = fmt.Fprint(w, "event: response.completed\ndata: "+
							`{"type":"response.completed","response":{"id":"resp-fixture",`+
							`"status":"completed","model":"served-model","output":[]}}`+"\n\n")
					} else {
						_, _ = fmt.Fprint(w, `data: {"id":"chat-fixture","object":"chat.completion.chunk",`+
							`"model":"served-model","choices":[{"index":0,"delta":{"content":"hello"},`+
							`"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
					}
					return
				}
				if api == "responses" {
					_, _ = fmt.Fprint(w, `{"id":"resp-fixture","status":"completed","model":"served-model",`+
						`"output":[{"type":"message","role":"assistant",`+
						`"content":[{"type":"output_text","text":"hello"}]}]}`)
				} else {
					_, _ = fmt.Fprint(w, `{"id":"chat-fixture","object":"chat.completion","model":"served-model",`+
						`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},`+
						`"finish_reason":"stop"}]}`)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("ORKA_AI_PROVIDER", "azure-openai")
			t.Setenv("ORKA_AI_MODEL", "primary-logical")
			t.Setenv("ORKA_AI_PROMPT", "hello")
			t.Setenv("ORKA_AI_BASE_URL", server.URL)
			t.Setenv("ORKA_AI_AZURE_DEPLOYMENT_NAME", "primary-slot")
			t.Setenv("ORKA_AI_AZURE_API_VERSION", "2024-06-01")
			t.Setenv("OPENAI_API_KEY", "placeholder")
			t.Setenv("ORKA_AI_FALLBACK_COUNT", "1")
			t.Setenv("ORKA_AI_FALLBACK_0_PROVIDER", "azure-openai")
			t.Setenv("ORKA_AI_FALLBACK_0_MODEL", "fallback-logical")
			t.Setenv("ORKA_AI_FALLBACK_0_BASE_URL", server.URL)
			t.Setenv("ORKA_AI_FALLBACK_0_AZURE_DEPLOYMENT_NAME", "fallback-slot")
			t.Setenv("ORKA_AI_FALLBACK_0_AZURE_API_VERSION", "2024-10-21")
			t.Setenv("ORKA_AI_FALLBACK_0_API_KEY", "placeholder")
			t.Setenv("KUBERNETES_SERVICE_HOST", "")
			t.Setenv("KUBERNETES_SERVICE_PORT", "")

			var providers []llm.Provider
			llm.RegisterProvider("azure-openai", func(cfg llm.ProviderConfig) (llm.Provider, error) {
				provider, err := openaiprovider.NewProvider(cfg)
				providers = append(providers, provider)
				return provider, err
			})
			t.Cleanup(func() {
				llm.RegisterProvider("azure-openai", func(cfg llm.ProviderConfig) (llm.Provider, error) {
					return openaiprovider.NewProvider(cfg)
				})
			})
			err := run("")
			require.ErrorContains(t, err, "k8s client")
			require.Len(t, providers, 2)
			for i, fixture := range []struct{ logical, deployment, version string }{
				{"primary-logical", "primary-slot", "2024-06-01"},
				{"fallback-logical", "fallback-slot", "2024-10-21"},
			} {
				provider := providers[i]
				req := &llm.CompletionRequest{Model: fixture.logical, Messages: []llm.Message{{Role: "user", Content: "hello"}}}
				chunks, err := provider.Stream(t.Context(), req)
				require.NoError(t, err)
				for chunk := range chunks {
					require.NoError(t, chunk.Error)
				}
				// Completion goes through the worker's real agent loop.
				content, err := executeAgentLoopWithEvents(t.Context(), provider, req.Messages, "", fixture.logical,
					modelSettings{}, nil, nil, nil, common.NoopEventRecorder{})
				require.NoError(t, err)
				assert.Equal(t, "hello", content)
				assert.Equal(t, fixture.logical, req.Model)
				require.Len(t, requests, 3)
				for range 3 {
					got := <-requests
					wantURI := "/openai/responses?api-version=" + fixture.version
					wantBody := fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hello"}]}`, fixture.deployment)
					if strings.Contains(got.uri, "/chat/completions") {
						assert.Equal(t, "chat", api)
						wantURI = "/openai/deployments/" + fixture.deployment + "/chat/completions?api-version=" + fixture.version
						wantBody = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, fixture.deployment)
					}
					if got.body["max_output_tokens"] != nil {
						wantBody = fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hi"}],"max_output_tokens":1}`,
							fixture.deployment)
					} else if got.body["stream"] == true {
						wantBody = strings.TrimSuffix(wantBody, "}") + `,"stream":true}`
						if api == "chat" {
							wantBody = strings.TrimSuffix(wantBody, "}") + `,"stream_options":{"include_usage":true}}`
						}
					}
					assert.Equal(t, wantURI, got.uri)
					body, err := json.Marshal(got.body)
					require.NoError(t, err)
					assert.JSONEq(t, wantBody, string(body))
				}
			}
		})
	}
}
