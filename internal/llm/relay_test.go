/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestControllerRelaySendsTheSDKRequestToTheRelay(t *testing.T) {
	var seenPath, seenQuery string
	var seen http.Header
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenQuery, seen = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(relay.Close)

	tokenCalls := 0
	client, err := NewControllerRelayHTTPClient("https://Example.OpenAI.Azure.com/", &ControllerRelay{
		URL: relay.URL + "/internal/v1/tasks/default/task-1/providers/primary",
		Token: func(context.Context) (string, error) {
			tokenCalls++
			return "worker-sa-token", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost,
		"https://example.openai.azure.com/openai/deployments/gpt%2Dx/chat/completions?api-version=2025-03-01-preview", strings.NewReader("{}"))
	request.Header.Set("Api-Key", "leaked-key")
	request.Header.Set("X-Api-Key", "leaked-key")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if want := "/internal/v1/tasks/default/task-1/providers/primary/openai/deployments/gpt%2Dx/chat/completions"; seenPath != want {
		t.Fatalf("relay path = %q, want %q", seenPath, want)
	}
	if seenQuery != "api-version=2025-03-01-preview" {
		t.Fatalf("relay query = %q", seenQuery)
	}
	if got := seen.Get("Authorization"); got != "Bearer worker-sa-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if seen.Get("Api-Key") != "" || seen.Get("X-Api-Key") != "" {
		t.Fatal("an API key header reached the relay")
	}

	for _, outside := range []string{
		"https://attacker.example/openai/responses",
		"http://example.openai.azure.com/openai/responses",
		"https://example.openai.azure.com:8443/openai/responses",
	} {
		request, _ := http.NewRequest(http.MethodPost, outside, strings.NewReader("{}"))
		if _, err := client.Do(request); err == nil {
			t.Fatalf("a request for %s was relayed", outside)
		}
	}
	if tokenCalls != 1 {
		t.Fatalf("token source called %d times, want only for the Provider's endpoint", tokenCalls)
	}
}

func TestControllerRelayNeedsAnOriginBaseURL(t *testing.T) {
	relay := &ControllerRelay{URL: "http://controller:8080/relay", Token: func(context.Context) (string, error) { return "t", nil }}
	for _, baseURL := range []string{"https://example.openai.azure.com", "https://example.openai.azure.com/"} {
		if _, err := NewControllerRelayHTTPClient(baseURL, relay); err != nil {
			t.Fatalf("NewControllerRelayHTTPClient(%s) error = %v", baseURL, err)
		}
	}
	for _, baseURL := range []string{
		"https://gateway.example/azure",
		"https://example.openai.azure.com/?api-version=1",
		"https://example.openai.azure.com/#x",
	} {
		if _, err := NewControllerRelayHTTPClient(baseURL, relay); err == nil {
			t.Fatalf("NewControllerRelayHTTPClient(%s) accepted a baseURL that is not an origin", baseURL)
		}
	}
}

func TestControllerRelayFailsClosed(t *testing.T) {
	token := func(context.Context) (string, error) { return "t", nil }
	for name, tc := range map[string]struct {
		baseURL string
		relay   *ControllerRelay
	}{
		"no relay":           {baseURL: "https://example.openai.azure.com"},
		"no token source":    {baseURL: "https://example.openai.azure.com", relay: &ControllerRelay{URL: "http://controller:8080/r"}},
		"plain http base":    {baseURL: "http://example.openai.azure.com", relay: &ControllerRelay{URL: "http://controller:8080/r", Token: token}},
		"relay without host": {baseURL: "https://example.openai.azure.com", relay: &ControllerRelay{URL: "/internal/v1/r", Token: token}},
		"relay with userinfo": {
			baseURL: "https://example.openai.azure.com", relay: &ControllerRelay{URL: "http://user:pw@controller:8080/r", Token: token},
		},
		"relay with query": {baseURL: "https://example.openai.azure.com", relay: &ControllerRelay{URL: "http://controller:8080/r?x=1", Token: token}},
	} {
		if _, err := NewControllerRelayHTTPClient(tc.baseURL, tc.relay); err == nil {
			t.Errorf("%s: NewControllerRelayHTTPClient accepted", name)
		}
	}

	relayed := false
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		relayed = true
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(relay.Close)
	client, err := NewControllerRelayHTTPClient("https://example.openai.azure.com", &ControllerRelay{
		URL:   relay.URL,
		Token: func(context.Context) (string, error) { return "", errors.New("no token") },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://example.openai.azure.com/openai/responses", strings.NewReader("{}"))
	_, err = client.Do(request)
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || providerErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Do() error = %v, want a 503 provider error", err)
	}
	if relayed {
		t.Fatal("a request without the worker's token reached the relay")
	}
}

func TestControllerRelayRefusesRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(relay.Close)
	client, err := NewControllerRelayHTTPClient("https://example.openai.azure.com", &ControllerRelay{
		URL: relay.URL, Token: func(context.Context) (string, error) { return "t", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://example.openai.azure.com/openai/responses", strings.NewReader("{}"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || redirected {
		t.Fatalf("status = %d, redirected = %v; want the redirect returned, not followed", response.StatusCode, redirected)
	}
}
