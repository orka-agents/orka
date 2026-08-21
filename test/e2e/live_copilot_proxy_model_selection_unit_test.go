//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLiveCopilotProxyClaudeModelPreferences(t *testing.T) {
	t.Parallel()

	catalog := proxyModelCatalog{AllModelIDs: []string{
		"claude-sonnet-4.5",
		"claude-sonnet-4.6",
		"claude-sonnet-5",
	}}
	actual := firstPreferredProxyModel(
		catalog,
		liveCopilotProxyClaudeModelPreferences,
		liveCopilotProxyClaudeModelPrefixes...,
	)
	if actual != "claude-sonnet-5" {
		t.Fatalf("selected Claude model %q, want %q", actual, "claude-sonnet-5")
	}
}

func TestFirstUsableProxyAnthropicMessagesModel(t *testing.T) {
	t.Parallel()

	var requestedModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/messages" {
			t.Errorf("request path = %q, want /v1/messages", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		if got := req.Header.Get("x-api-key"); got != liveProxyProbeAPIKey {
			t.Errorf("x-api-key = %q, want %q", got, liveProxyProbeAPIKey)
		}
		if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want 2023-06-01", got)
		}

		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requestedModels = append(requestedModels, payload.Model)

		switch payload.Model {
		case "claude-sonnet-4.5":
			http.Error(w, "Claude Sonnet 4 was retired", http.StatusBadRequest)
		case "claude-sonnet-5":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			http.Error(w, "unexpected model", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	catalog := proxyModelCatalog{AllModelIDs: []string{
		"claude-sonnet-4.5",
		"claude-sonnet-5",
	}}
	actual, err := firstUsableProxyAnthropicMessagesModel(
		server.URL,
		catalog,
		[]string{"claude-sonnet-4.5", "claude-sonnet-5"},
		"claude-",
	)
	if err != nil {
		t.Fatalf("select usable Claude model: %v", err)
	}
	if actual != "claude-sonnet-5" {
		t.Fatalf("selected Claude model %q, want %q", actual, "claude-sonnet-5")
	}
	if expected := []string{"claude-sonnet-4.5", "claude-sonnet-5"}; !reflect.DeepEqual(requestedModels, expected) {
		t.Fatalf("requested models = %v, want %v", requestedModels, expected)
	}
}

func TestFirstUsableProxyAnthropicMessagesModelReportsRejectedCandidates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model retired", http.StatusBadRequest)
	}))
	defer server.Close()

	catalog := proxyModelCatalog{AllModelIDs: []string{"claude-sonnet-4.5"}}
	actual, err := firstUsableProxyAnthropicMessagesModel(
		server.URL,
		catalog,
		[]string{"claude-sonnet-4.5"},
		"claude-",
	)
	if err == nil {
		t.Fatal("expected rejected Claude candidates to return an error")
	}
	if actual != "" {
		t.Fatalf("selected Claude model %q, want empty", actual)
	}
	if !strings.Contains(err.Error(), "claude-sonnet-4.5=400") {
		t.Fatalf("error = %q, want rejected model and status", err)
	}
}
