/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestAzureTokenFailuresStayRetryable(t *testing.T) {
	newProvider := func(t *testing.T, mode apiMode) *Provider {
		t.Helper()
		provider, err := NewProvider(llm.ProviderConfig{
			BearerToken:         func(context.Context) (string, error) { return "", errors.New("issuer unavailable") },
			BaseURL:             "https://myresource.openai.azure.com",
			ProviderType:        "azure-openai",
			AzureDeploymentName: "gpt-x",
		})
		if err != nil {
			t.Fatal(err)
		}
		provider.mode.Store(int32(mode))
		return provider
	}
	retryable := func(t *testing.T, err error) {
		t.Helper()
		var providerErr *llm.ProviderError
		if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusServiceUnavailable || !llm.ShouldRetry(err) {
			t.Fatalf("error = %#v, want a retryable 503 provider error", err)
		}
	}
	request := &llm.CompletionRequest{Model: "gpt-x", Messages: []llm.Message{{Role: "user", Content: "hi"}}}

	for name, mode := range map[string]apiMode{"chat completions": apiModeChatCompletions, "responses": apiModeResponses} {
		t.Run("complete "+name, func(t *testing.T) {
			_, err := newProvider(t, mode).Complete(context.Background(), request)
			retryable(t, err)
		})
		t.Run("stream "+name, func(t *testing.T) {
			stream, err := newProvider(t, mode).Stream(context.Background(), request)
			if err != nil {
				retryable(t, err)
				return
			}
			var streamErr error
			for chunk := range stream {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			retryable(t, streamErr)
		})
	}
}

func TestAzureBearerProviderCompleteAndStream(t *testing.T) {
	const (
		deployment = "gpt-x"
		apiVersion = "2025-03-01-preview"
		token      = "synthetic-entra-bearer"
	)
	type observed struct {
		path       string
		apiVersion string
		auth       string
		apiKey     string
		xAPIKey    string
	}
	var (
		mu    sync.Mutex
		calls []observed
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, observed{
			path:       r.URL.Path,
			apiVersion: r.URL.Query().Get("api-version"),
			auth:       r.Header.Get("Authorization"),
			apiKey:     r.Header.Get("Api-Key"),
			xAPIKey:    r.Header.Get("X-Api-Key"),
		})
		mu.Unlock()
		if r.URL.Path == "/openai/responses" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"cmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"cmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"cmpl-1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	provider, err := NewProvider(llm.ProviderConfig{
		BearerToken:         func(context.Context) (string, error) { return token, nil },
		BaseURL:             server.URL,
		ProviderType:        "azure-openai",
		AzureDeploymentName: deployment,
		AzureAPIVersion:     apiVersion,
		HTTPTransport:       server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &llm.CompletionRequest{Model: deployment, Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	resp, err := provider.Complete(context.Background(), req)
	if err != nil || resp.Content != "hello" {
		t.Fatalf("Complete() = %+v, %v", resp, err)
	}
	stream, err := provider.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var streamed strings.Builder
	for chunk := range stream {
		if chunk.Error != nil {
			t.Fatalf("Stream() chunk error = %v", chunk.Error)
		}
		streamed.WriteString(chunk.Content)
	}
	if streamed.String() != "hello" {
		t.Fatalf("Stream() = %q, want %q", streamed.String(), "hello")
	}

	mu.Lock()
	defer mu.Unlock()
	wantPath := "/openai/deployments/" + deployment + "/chat/completions"
	chatCalls := 0
	for _, call := range calls {
		if call.auth != "Bearer "+token {
			t.Fatalf("Authorization = %q, want %q", call.auth, "Bearer "+token)
		}
		if call.apiKey != "" || call.xAPIKey != "" {
			t.Fatalf("unexpected API key header: Api-Key=%q X-Api-Key=%q", call.apiKey, call.xAPIKey)
		}
		if call.apiVersion != apiVersion {
			t.Fatalf("api-version = %q, want %q", call.apiVersion, apiVersion)
		}
		if call.path == wantPath {
			chatCalls++
		}
	}
	if chatCalls < 2 {
		t.Fatalf("chat completions calls to %s = %d, want at least 2 (Complete + Stream)", wantPath, chatCalls)
	}
}
