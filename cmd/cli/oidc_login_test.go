/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthLoginDiscoversOIDCAndRefreshesExpiredToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	prevPoll := oidcPollFloor
	oidcPollFloor = time.Millisecond
	t.Cleanup(func() { oidcPollFloor = prevPoll })

	origBrowser := openBrowserFunc
	var openedURL string
	openBrowserFunc = func(u string) error {
		openedURL = u
		return nil
	}
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	pollCount := 0
	refreshCalls := 0
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"issuer":                        idp.URL,
				"device_authorization_endpoint": idp.URL + "/devicecode",
				"token_endpoint":                idp.URL + "/token",
			})
		case "/devicecode":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse form: %v", err)
			}
			if got := r.Form.Get("client_id"); got != "orka-client-id" {
				t.Errorf("client_id = %q, want orka-client-id", got)
			}
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"device_code":      "dev-code-1",
				"user_code":        "WXYZ-5678",
				"verification_uri": "https://idp.example/device",
				"expires_in":       300,
				"interval":         1,
			})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			switch r.Form.Get("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				pollCount++
				if pollCount == 1 {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"}) //nolint:errcheck
					return
				}
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"access_token":  "oidc-access-1",
					"refresh_token": "oidc-refresh-1",
					"expires_in":    3600,
				})
			case "refresh_token":
				refreshCalls++
				if got := r.Form.Get("refresh_token"); got != "oidc-refresh-1" {
					t.Errorf("refresh_token = %q, want oidc-refresh-1", got)
				}
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"access_token":  "oidc-access-2",
					"refresh_token": "oidc-refresh-2",
					"expires_in":    3600,
				})
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer idp.Close()

	var gotAuthHeader string
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"oidc": true,
				"oidcConfig": map[string]any{
					"issuer":   idp.URL,
					"audience": "orka-client-id",
				},
			})
		case "/api/v1/auth/whoami":
			gotAuthHeader = r.Header.Get("Authorization")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"authenticated": true,
				"username":      "alice@example.com",
				"authType":      "oidc",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"auth", "login", "--server", orka.URL, "--timeout", "10s"})
	if err := root.Execute(); err != nil {
		t.Fatalf("auth login error: %v\n%s", err, out.String())
	}
	if openedURL != "https://idp.example/device" {
		t.Fatalf("openedURL = %q, want https://idp.example/device", openedURL)
	}
	for _, want := range []string{"WXYZ-5678", "Signed in to", "Refresh token saved"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("auth login output missing %q:\n%s", want, out.String())
		}
	}

	cfg := loadConfig()
	if cfg.Token != "oidc-access-1" || cfg.TokenKind != tokenKindOIDC || cfg.OIDCRefreshToken != "oidc-refresh-1" {
		t.Fatalf("saved config = %+v", cfg)
	}

	// Expire the saved access token and verify the next command refreshes it automatically.
	expired := time.Now().UTC().Add(-time.Minute)
	cfg.TokenExpiresAt = &expired
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	out.Reset()
	root2 := newRootCmd()
	root2.SetOut(&out)
	root2.SetErr(&out)
	root2.SetArgs([]string{"auth", "whoami"})
	if err := root2.Execute(); err != nil {
		t.Fatalf("auth whoami error: %v\n%s", err, out.String())
	}
	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	if gotAuthHeader != "Bearer oidc-access-2" {
		t.Fatalf("Authorization = %q, want Bearer oidc-access-2", gotAuthHeader)
	}
	updated := loadConfig()
	if updated.Token != "oidc-access-2" || updated.OIDCRefreshToken != "oidc-refresh-2" {
		t.Fatalf("updated config = %+v", updated)
	}
}
