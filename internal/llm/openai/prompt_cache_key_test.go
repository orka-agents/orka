package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/llm"
)

func TestPromptCacheKeyOnlyForOpenAIAPI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config llm.ProviderConfig
		want   bool
	}{
		{"default endpoint", llm.ProviderConfig{APIKey: "k"}, true},
		{"openai endpoint", llm.ProviderConfig{APIKey: "k", BaseURL: "https://api.openai.com/v1"}, true},
		{"compatible server", llm.ProviderConfig{APIKey: "k", BaseURL: "http://vekil.default.svc:1337/v1"}, false},
		{"azure", llm.ProviderConfig{APIKey: "k", ProviderType: providerTypeAzureOpenAI, BaseURL: "https://example.openai.azure.com"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProvider(tc.config)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.promptCacheKey(&llm.CompletionRequest{Model: "m"}) != ""; got != tc.want {
				t.Errorf("sends prompt_cache_key = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPromptCacheKeyFollowsConversation(t *testing.T) {
	p := &Provider{sendPromptCacheKey: true}
	step := &llm.CompletionRequest{Model: "m", SystemPrompt: "sys", Messages: []llm.Message{{Role: "user", Content: "hi"}}}
	next := &llm.CompletionRequest{Model: "m", SystemPrompt: "sys", Messages: []llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "more"},
	}}
	key := p.promptCacheKey(step)
	if got := p.promptCacheKey(next); got != key {
		t.Errorf("next step key = %q, want %q", got, key)
	}
	split := &llm.CompletionRequest{Model: "m", SystemPrompt: "sys catalog-1", SystemPromptStable: len("sys "), Messages: step.Messages}
	changed := &llm.CompletionRequest{Model: "m", SystemPrompt: "sys catalog-2", SystemPromptStable: len("sys "), Messages: step.Messages}
	if p.promptCacheKey(split) != p.promptCacheKey(changed) {
		t.Error("a change after the stable system prompt changed the key")
	}
	for name, other := range map[string]*llm.CompletionRequest{
		"model":         {Model: "m2", SystemPrompt: "sys", Messages: step.Messages},
		"system":        {Model: "m", SystemPrompt: "other", Messages: step.Messages},
		"first message": {Model: "m", SystemPrompt: "sys", Messages: []llm.Message{{Role: "user", Content: "bye"}}},
	} {
		if p.promptCacheKey(other) == key {
			t.Errorf("a different %s shares the key", name)
		}
	}
}

func TestPromptCacheKeyOnProviderWire(t *testing.T) {
	for _, mode := range []apiMode{apiModeResponses, apiModeChatCompletions} {
		for _, send := range []bool{true, false} {
			captured := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				captured <- body
				w.Header().Set("Content-Type", "application/json")
				if mode == apiModeResponses {
					_, _ = w.Write([]byte(`{"id":"resp-test","object":"response","status":"completed","model":"test-model","output":[{"id":"msg-test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}]}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"chat-test","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
				}
			}))
			provider, err := NewProvider(llm.ProviderConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			provider.mode.Store(int32(mode))
			provider.sendPromptCacheKey = send
			req := &llm.CompletionRequest{Model: "test-model", SystemPrompt: "sys", Messages: []llm.Message{{Role: "user", Content: "hi"}}}
			_, err = provider.Complete(t.Context(), req)
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			got, present := (<-captured)["prompt_cache_key"]
			if present != send {
				t.Fatalf("mode %d send=%v: prompt_cache_key present = %v", mode, send, present)
			}
			if send && got != provider.promptCacheKey(req) {
				t.Errorf("mode %d: prompt_cache_key = %v, want %q", mode, got, provider.promptCacheKey(req))
			}
		}
	}
}
