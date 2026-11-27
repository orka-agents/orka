/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestNewProvider_RelayIsAzureOnlyAndCarriesNoCredential(t *testing.T) {
	relay := &llm.ControllerRelay{URL: "http://controller:8080/r", Token: func(context.Context) (string, error) { return "t", nil }}
	if _, err := NewProvider(llm.ProviderConfig{Relay: relay, BaseURL: "https://myresource.openai.azure.com", ProviderType: "azure-openai"}); err != nil {
		t.Fatalf("NewProvider(azure relay) error = %v", err)
	}
	token := func(context.Context) (string, error) { return "entra-token", nil }
	for name, config := range map[string]llm.ProviderConfig{
		"openai type":     {Relay: relay, BaseURL: "https://api.example.com", ProviderType: testProviderOpenAI},
		"relay and key":   {Relay: relay, APIKey: "k", BaseURL: "https://myresource.openai.azure.com", ProviderType: "azure-openai"},
		"relay and token": {Relay: relay, BearerToken: token, BaseURL: "https://myresource.openai.azure.com", ProviderType: "azure-openai"},
	} {
		if _, err := NewProvider(config); err == nil {
			t.Fatalf("NewProvider(%s) was accepted", name)
		}
	}
}

func TestRelayedAzureRequestsUseOnlyTheRelayedPaths(t *testing.T) {
	const prefix = "/internal/v1/tasks/default/task-1/providers/primary"
	allowed := regexp.MustCompile(`^` + prefix + `(/openai/deployments/[A-Za-z0-9_.-]+/chat/completions|/openai/responses)$`)
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer worker-sa-token" || r.Header.Get("Api-Key") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.RawQuery, "api-version=") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/openai/responses") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":{"message":"Not Found","type":"invalid_request_error","code":"invalid_url"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(server.Close)

	provider, err := NewProvider(llm.ProviderConfig{
		Relay: &llm.ControllerRelay{
			URL:   server.URL + prefix,
			Token: func(context.Context) (string, error) { return "worker-sa-token", nil },
		},
		BaseURL:             "https://myresource.openai.azure.com",
		ProviderType:        "azure-openai",
		AzureDeploymentName: "gpt-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &llm.CompletionRequest{Model: "gpt-x", Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	response, err := provider.Complete(context.Background(), request)
	if err != nil || response.Content != "Hello!" {
		t.Fatalf("Complete() = %+v, %v", response, err)
	}
	provider.mode.Store(int32(apiModeChatCompletions))
	stream, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	for range stream {
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("no request reached the relay")
	}
	for _, path := range paths {
		if !allowed.MatchString(path) {
			t.Fatalf("the SDK sent %s, which the relay does not admit", path)
		}
	}
}
