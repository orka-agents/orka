/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
)

// Removing deployment propagation or using it for logical-model authorization
// must break this test at the HTTP or resolver boundary, respectively.
func TestAzureDeploymentResolverRouting(t *testing.T) {
	for _, route := range []struct {
		name, providerType, deployment, wantModel, version, wantVersion string
	}{
		{"configured", "azure-openai", "azure-slot", "azure-slot", "2024-06-01", "2024-06-01"},
		{"empty deployment", "azure-openai", "", "logical-model", "2024-06-01", "2024-06-01"},
		{"runtime version default", "azure-openai", "azure-slot", "azure-slot", "", "2025-03-01-preview"},
		{"CRD version default", "azure-openai", "azure-slot", "azure-slot", "2024-02-15-preview", "2024-02-15-preview"},
		{"non Azure ignores deployment", "openai", "azure-slot", "logical-model", "2024-06-01", ""},
	} {
		for _, api := range []string{"responses", "chat"} {
			for _, selection := range []string{"explicit", "model string"} {
				t.Run(route.name+"/"+api+"/"+selection, func(t *testing.T) {
					requests := make(chan azureObservedRequest, 12)
					server := newAzureRoutingServer(t, api, requests)
					crd := makeProvider("azure", "default", corev1alpha1.ProviderType(route.providerType), "fixture", "default-model")
					crd.Spec.BaseURL = server.URL
					crd.Spec.Azure = &corev1alpha1.AzureConfig{DeploymentName: route.deployment, APIVersion: route.version}
					kube := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(crd, makeSecret("fixture", "default", "api-key", "placeholder")).Build()
					resolver := NewProviderResolver(kube, DefaultChatConfig())
					opts := ResolveOpts{Namespace: "default", RequireModel: true}
					if selection == "explicit" {
						opts.ProviderName, opts.Model = "azure", "logical-model"
					} else {
						opts.ModelStr = "azure/logical-model"
					}
					var authorizedModel string
					opts.AuthorizeProviderUse = func(_ ProviderResolutionInfo, model string) error {
						authorizedModel = model
						return nil
					}
					provider, model, err := resolver.Resolve(t.Context(), opts)
					require.NoError(t, err)
					assert.Equal(t, "logical-model", model)
					assert.Equal(t, "logical-model", authorizedModel)
					// A fresh provider exercises Stream's non-streaming detection probe;
					// subsequent calls exercise cached routing in both APIs.
					req := &llm.CompletionRequest{Model: model, Messages: []llm.Message{{Role: "user", Content: "hello"}}}
					before, err := json.Marshal(req)
					require.NoError(t, err)
					for range 2 {
						chunks, err := provider.Stream(t.Context(), req)
						require.NoError(t, err)
						for chunk := range chunks {
							require.NoError(t, chunk.Error)
						}
						_, err = provider.Complete(t.Context(), req)
						require.NoError(t, err)
					}
					after, err := json.Marshal(req)
					require.NoError(t, err)
					assert.JSONEq(t, string(before), string(after), "caller request must remain unchanged")
					assertAzureRoutingRequests(t, requests, route.providerType, route.wantModel, route.wantVersion, api, 5)
				})
			}
		}
	}
}

func TestAzureDeploymentChatFallbackRouting(t *testing.T) {
	for _, api := range []string{"responses", "chat"} {
		t.Run(api, func(t *testing.T) {
			requests := make(chan azureObservedRequest, 8)
			server := newAzureRoutingServer(t, api, requests)
			crd := makeProvider("azure-fallback", "default", corev1alpha1.ProviderTypeAzureOpenAI, "fixture", "default-model")
			crd.Spec.BaseURL = server.URL
			crd.Spec.Azure = &corev1alpha1.AzureConfig{DeploymentName: "fallback-slot", APIVersion: "2024-06-01"}
			agent := makeAgent("fallback-agent", nil, &corev1alpha1.ModelConfig{Fallbacks: []corev1alpha1.ModelFallback{{ProviderRef: crd.Name, Model: "fallback-logical"}}})
			kube := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(crd, agent, makeSecret("fixture", "default", "api-key", "placeholder")).Build()
			ch := newTestChatHandler(t, kube, newTestSessionStore(t), newTestResultStore(t), DefaultChatConfig())
			provider, err := ch.wrapWithRetryAndFallback(t.Context(), nil, &chatMockProvider{name: "unavailable", err: &llm.ProviderError{StatusCode: 401, Message: "unavailable"}}, ChatRequest{AgentRef: agent.Name}, "default")
			require.NoError(t, err)
			req := &llm.CompletionRequest{Model: "primary-logical", Messages: []llm.Message{{Role: "user", Content: "hello"}}}
			_, err = provider.Complete(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, "primary-logical", req.Model)
			count := 1
			if api == "chat" {
				count = 2
			}
			assertAzureRoutingRequests(t, requests, "azure-openai", "fallback-slot", "2024-06-01", api, count)
		})
	}
}

type azureObservedRequest struct {
	uri  string
	body map[string]any
}

// Only URL and JSON are captured; credentials and headers never enter evidence.
func newAzureRoutingServer(t *testing.T, api string, requests chan<- azureObservedRequest) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode routing body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- azureObservedRequest{r.URL.RequestURI(), body}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/responses") && api == "chat" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"message":"unsupported API","code":"unsupported_api"}}`)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			if api == "responses" {
				_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fixture\",\"status\":\"completed\",\"model\":\"served-model\",\"output\":[]}}\n\n")
			} else {
				_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"served-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}
			return
		}
		if api == "responses" {
			_, _ = fmt.Fprint(w, `{"id":"resp-fixture","status":"completed","model":"served-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`)
		} else {
			_, _ = fmt.Fprint(w, `{"id":"chat-fixture","object":"chat.completion","model":"served-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func assertAzureRoutingRequests(t *testing.T, requests <-chan azureObservedRequest, providerType, model, version, api string, count int) {
	t.Helper()
	require.Len(t, requests, count)
	for range count {
		got := <-requests
		path := "/responses"
		if strings.Contains(got.uri, "/chat/completions") {
			assert.Equal(t, "chat", api)
			path = "/chat/completions"
			if providerType == "azure-openai" {
				path = "/deployments/" + model + path
			}
		}
		if providerType == "azure-openai" {
			path = "/openai" + path + "?api-version=" + version
		}
		assert.Equal(t, path, got.uri)
		assert.Equal(t, model, got.body["model"])
		// Assert the complete payload, not only the deployment field.
		want := fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hello"}]}`, model)
		if strings.Contains(got.uri, "/chat/completions") {
			want = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
		}
		if got.body["max_output_tokens"] != nil {
			want = fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"hi"}],"max_output_tokens":1}`, model)
		} else if got.body["stream"] == true {
			want = strings.TrimSuffix(want, "}") + `,"stream":true}`
			if strings.Contains(got.uri, "/chat/completions") {
				want = strings.TrimSuffix(want, "}") + `,"stream_options":{"include_usage":true}}`
			}
		}
		body, err := json.Marshal(got.body)
		require.NoError(t, err)
		assert.JSONEq(t, want, string(body))
	}
}
