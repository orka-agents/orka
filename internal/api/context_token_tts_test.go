/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"

	"github.com/orka-agents/orka/internal/metrics"
)

func TestNewContextTokenTTSConfig(t *testing.T) {
	cfg, err := NewContextTokenTTSConfig("https://tts.example.test/", "orka", "7s", ContextTokenTTSTokenSourceIncoming, "3m", "30s")
	if err != nil {
		t.Fatalf("NewContextTokenTTSConfig returned error: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("expected TTS config to be enabled")
	}
	if cfg.URL != "https://tts.example.test" {
		t.Fatalf("URL = %q, want trimmed URL", cfg.URL)
	}
	if cfg.Audience != "orka" || cfg.TokenSource != ContextTokenTTSTokenSourceIncoming {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if cfg.Timeout != 7*time.Second || cfg.ChildTokenTTL != 3*time.Minute || cfg.ToolTokenTTL != 30*time.Second {
		t.Fatalf("unexpected durations: %#v", cfg)
	}
}

func TestNewContextTokenTTSConfigDisabledByDefault(t *testing.T) {
	cfg, err := NewContextTokenTTSConfig("", "", "", "", "", "")
	if err != nil {
		t.Fatalf("NewContextTokenTTSConfig returned error: %v", err)
	}
	if cfg.Enabled() {
		t.Fatalf("expected TTS config to be disabled: %#v", cfg)
	}
	if cfg.TokenSource != ContextTokenTTSTokenSourceNone {
		t.Fatalf("TokenSource = %q, want none", cfg.TokenSource)
	}
}

func TestNewContextTokenTTSConfigDefaultsToServiceAccountWithURL(t *testing.T) {
	cfg, err := NewContextTokenTTSConfig("https://tts.example.test", "", "", "", "", "")
	if err != nil {
		t.Fatalf("NewContextTokenTTSConfig returned error: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatalf("expected TTS config to be enabled: %#v", cfg)
	}
	if cfg.TokenSource != ContextTokenTTSTokenSourceServiceAccount {
		t.Fatalf("TokenSource = %q, want %q", cfg.TokenSource, ContextTokenTTSTokenSourceServiceAccount)
	}
}

func TestKontxtTTSClientExchange(t *testing.T) {
	metrics.ContextTokenTTSExchangeTotal.Reset()
	metrics.ContextTokenTTSExchangeDuration.Reset()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token_endpoint" {
			t.Fatalf("path = %q, want /token_endpoint", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.FormValue("grant_type"); got != kontxttoken.GrantType {
			t.Fatalf("grant_type = %q", got)
		}
		if got := r.FormValue("requested_token_type"); got != kontxttoken.RequestedTokenType {
			t.Fatalf("requested_token_type = %q", got)
		}
		if got := r.FormValue("subject_token"); got != "subject-token" {
			t.Fatalf("subject_token = %q", got)
		}
		if got := r.FormValue("subject_token_type"); got != kontxttoken.SubjectTokenTypeTxnToken {
			t.Fatalf("subject_token_type = %q", got)
		}
		if got := r.FormValue("scope"); got != "orka:tasks:create" {
			t.Fatalf("scope = %q", got)
		}
		if got := r.FormValue("audience"); got != "orka" {
			t.Fatalf("audience = %q, want orka", got)
		}
		if got := r.FormValue("requested_expires_in"); got != "8" {
			t.Fatalf("requested_expires_in = %q, want 8", got)
		}

		var details map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("request_details")), &details); err != nil {
			t.Fatalf("request_details JSON error = %v", err)
		}
		if details["operation"] != "createTask" {
			t.Fatalf("request_details = %#v", details)
		}
		var requestContext map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("request_context")), &requestContext); err != nil {
			t.Fatalf("request_context JSON error = %v", err)
		}
		if requestContext["source"] != "test" {
			t.Fatalf("request_context = %#v", requestContext)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tx-token","issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"N_A"}`))
	}))
	defer server.Close()

	cfg, err := NewContextTokenTTSConfig(server.URL, "orka", "", ContextTokenTTSTokenSourceIncoming, "", "")
	if err != nil {
		t.Fatalf("NewContextTokenTTSConfig returned error: %v", err)
	}
	client, err := NewKontxtTTSClient(cfg)
	if err != nil {
		t.Fatalf("NewKontxtTTSClient returned error: %v", err)
	}
	token, err := client.Exchange(context.Background(), ContextTokenExchangeRequest{
		SubjectToken:     "subject-token",
		SubjectTokenType: kontxttoken.SubjectTokenTypeTxnToken,
		Scope:            ContextTokenScopeTaskCreate,
		RequestDetails:   map[string]any{"operation": "createTask"},
		RequestContext:   map[string]any{"source": "test"},
		RequestedTTL:     7500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exchange returned error: %v", err)
	}
	if token != "tx-token" {
		t.Fatalf("token = %q, want tx-token", token)
	}
	if count := testCounterValue(t, metrics.ContextTokenTTSExchangeTotal, "success", "ok"); count != 1 {
		t.Fatalf("TTS exchange success metric = %v, want 1", count)
	}
}
