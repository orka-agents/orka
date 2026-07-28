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
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/transactiontoken"

	"github.com/orka-agents/orka/internal/metrics"
)

func TestNewContextTokenTTSConfig(t *testing.T) {
	cfg, err := NewContextTokenTTSConfig("https://tts.example.test/token", "orka", "7s", ContextTokenTTSTokenSourceIncoming, "3m", "30s")
	if err != nil {
		t.Fatalf("NewContextTokenTTSConfig returned error: %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("expected TTS config to be enabled")
	}
	if cfg.Endpoint != "https://tts.example.test/token" {
		t.Fatalf("Endpoint = %q, want exact endpoint", cfg.Endpoint)
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

func TestContextTokenTTSClientExchange(t *testing.T) {
	metrics.ContextTokenTTSExchangeTotal.Reset()
	metrics.ContextTokenTTSExchangeDuration.Reset()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token_endpoint" {
			t.Fatalf("path = %q, want /token_endpoint", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.FormValue("grant_type"); got != transactiontoken.GrantTypeTokenExchange {
			t.Fatalf("grant_type = %q", got)
		}
		if got := r.FormValue("requested_token_type"); got != transactiontoken.RequestedTokenType {
			t.Fatalf("requested_token_type = %q", got)
		}
		if got := r.FormValue("subject_token"); got != "subject-token" {
			t.Fatalf("subject_token = %q", got)
		}
		if got := r.FormValue("subject_token_type"); got != transactiontoken.SubjectTokenTypeTransactionToken {
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

	cfg, err := NewContextTokenTTSConfig(server.URL+"/token_endpoint", "orka", "", ContextTokenTTSTokenSourceIncoming, "", "")
	if err != nil {
		t.Fatalf("NewContextTokenTTSConfig returned error: %v", err)
	}
	client, err := NewContextTokenTTSClient(cfg)
	if err != nil {
		t.Fatalf("NewContextTokenTTSClient returned error: %v", err)
	}
	token, err := client.Exchange(context.Background(), ContextTokenExchangeRequest{
		SubjectToken:     "subject-token",
		SubjectTokenType: transactiontoken.SubjectTokenTypeTransactionToken,
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

func TestNewContextTokenTTSConfigRejectsCredentialBearingEndpoint(t *testing.T) {
	_, err := NewContextTokenTTSConfig("https://client:secret@issuer.example.test/token", "", "", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "without userinfo") {
		t.Fatalf("NewContextTokenTTSConfig() error = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "secret") {
		t.Fatalf("NewContextTokenTTSConfig() leaked endpoint credentials: %v", err)
	}
}

func TestContextTokenTTSClientRejectsNonTransactionResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "bearer result", body: `{"access_token":"token","issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"Bearer"}`, want: "token_type"},
		{name: "missing issued type", body: `{"access_token":"token","token_type":"N_A"}`, want: "issued_token_type"},
		{name: "empty token", body: `{"access_token":"","issued_token_type":"urn:ietf:params:oauth:token-type:txn_token","token_type":"N_A"}`, want: "access_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			cfg, err := NewContextTokenTTSConfig(server.URL, "", "", ContextTokenTTSTokenSourceIncoming, "", "")
			if err != nil {
				t.Fatal(err)
			}
			client, err := NewContextTokenTTSClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Exchange(context.Background(), ContextTokenExchangeRequest{SubjectToken: "subject", Scope: "read"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Exchange() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
