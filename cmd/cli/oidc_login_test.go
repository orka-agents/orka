/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testCompactJWT(t *testing.T, iss, aud string, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claimsMap := map[string]any{"iss": iss, "aud": aud, "sub": "alice"}
	if exp > 0 {
		claimsMap["exp"] = exp
	}
	payloadBytes, err := json.Marshal(claimsMap)
	if err != nil {
		t.Fatalf("marshal jwt claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + payload + "." + sig
}

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
				t.Errorf("parse form: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
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
				t.Errorf("parse token form: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
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
					"access_token":  testCompactJWT(t, idp.URL, "orka-client-id", time.Now().Add(time.Hour).Unix()),
					"refresh_token": "oidc-refresh-1",
					"expires_in":    3600,
				})
			case "refresh_token":
				refreshCalls++
				if got := r.Form.Get("refresh_token"); got != "oidc-refresh-1" {
					t.Errorf("refresh_token = %q, want oidc-refresh-1", got)
				}
				if gotScope := r.Form.Get("scope"); gotScope != "" {
					t.Errorf("refresh scope = %q, want empty when provider did not return granted scope", gotScope)
				}
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"access_token":  testCompactJWT(t, idp.URL, "orka-client-id", time.Now().Add(2*time.Hour).Unix()),
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
	if cfg.Token == "" || cfg.TokenKind != tokenKindOIDC || cfg.OIDCRefreshToken != "oidc-refresh-1" {
		t.Fatalf("saved config = %+v", cfg)
	}
	firstToken := cfg.Token

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
	updated := loadConfig()
	if updated.Token == "" || updated.Token == firstToken || updated.OIDCRefreshToken != "oidc-refresh-2" {
		t.Fatalf("updated config = %+v", updated)
	}
	if gotAuthHeader != "Bearer "+updated.Token {
		t.Fatalf("Authorization = %q, want Bearer %s", gotAuthHeader, updated.Token)
	}
	if updated.SessionSubject == "" || updated.SessionSubject != cfg.SessionSubject {
		t.Fatalf("login ID changed across refresh: got %q, want %q", updated.SessionSubject, cfg.SessionSubject)
	}
}

func TestSelectOIDCBearerTokenFallsBackToIDTokenWhenAccessTokenOpaque(t *testing.T) {
	expUnix := time.Now().UTC().Add(30 * time.Minute).Unix()
	idTok := testCompactJWT(t, "https://issuer.example", "orka-api", expUnix)
	tok, exp, err := selectOIDCBearerToken(oidcTokenResponse{
		AccessToken: "opaque-access-token",
		IDToken:     idTok,
		ExpiresIn:   7200,
	}, "https://issuer.example", "orka-api")
	if err != nil {
		t.Fatalf("selectOIDCBearerToken error: %v", err)
	}
	if tok != idTok {
		t.Fatalf("selected token = %q, want id_token", tok)
	}
	if exp == nil || exp.Unix() != expUnix {
		t.Fatalf("derived expiry = %v, want %d", exp, expUnix)
	}
}

func TestFetchOIDCDiscoveryRejectsIssuerMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"issuer":                        "https://evil.example",
			"device_authorization_endpoint": "https://evil.example/device",
			"token_endpoint":                "https://evil.example/token",
		})
	}))
	defer srv.Close()

	if _, err := fetchOIDCDiscovery(t.Context(), srv.URL); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("expected issuer mismatch error, got %v", err)
	}
}

func TestFetchOIDCDiscoveryAndJWTRejectTrailingSlashMismatch(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"issuer":                        srv.URL + "/",
			"device_authorization_endpoint": srv.URL + "/device",
			"token_endpoint":                srv.URL + "/token",
		})
	}))
	defer srv.Close()

	if _, err := fetchOIDCDiscovery(t.Context(), srv.URL); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("expected trailing-slash issuer mismatch error, got %v", err)
	}

	tok := testCompactJWT(t, srv.URL+"/", "orka-api", time.Now().Add(time.Hour).Unix())
	if _, _, err := selectOIDCBearerToken(oidcTokenResponse{AccessToken: tok}, srv.URL, "orka-api"); err == nil {
		t.Fatal("expected selectOIDCBearerToken to reject JWT whose iss differs by a trailing slash")
	}
}

func TestSelectOIDCBearerTokenAcceptsFractionalExpAndRequiresSubAndExp(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))

	noSubPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://issuer.example","aud":"orka-api","exp":1893456000}`))
	noSubTok := header + "." + noSubPayload + "." + sig

	fracExp := float64(time.Now().UTC().Add(45*time.Minute).Unix()) + 0.75
	fracPayloadBytes, _ := json.Marshal(map[string]any{
		"iss": "https://issuer.example",
		"sub": "alice",
		"aud": "orka-api",
		"exp": fracExp,
	})
	fracTok := header + "." + base64.RawURLEncoding.EncodeToString(fracPayloadBytes) + "." + sig

	got, exp, err := selectOIDCBearerToken(oidcTokenResponse{
		AccessToken: noSubTok,
		IDToken:     fracTok,
	}, "https://issuer.example", "orka-api")
	if err != nil {
		t.Fatalf("selectOIDCBearerToken error: %v", err)
	}
	if got != fracTok {
		t.Fatalf("selected token = %q, want id_token with sub and fractional exp", got)
	}
	if exp == nil || exp.Unix() != int64(fracExp) {
		t.Fatalf("exp = %v, want %d", exp, int64(fracExp))
	}
}

func TestOIDCRefreshDoesNotAdoptReplacedAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	freshExpiry := time.Now().UTC().Add(time.Hour)
	if err := saveConfig(orkaConfig{
		Server:            "https://orka.example",
		Token:             "account-b-token",
		TokenKind:         tokenKindOIDC,
		TokenExpiresAt:    &freshExpiry,
		SessionServer:     "https://orka.example",
		SessionSubject:    "oidc-account-b",
		OIDCClientID:      "orka-client",
		OIDCTokenEndpoint: "https://idp.example/token",
		OIDCRefreshToken:  "refresh-b",
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	expired := time.Now().UTC().Add(-time.Minute)
	accountA := orkaConfig{
		Server:            "https://orka.example",
		Token:             "account-a-expired-token",
		TokenKind:         tokenKindOIDC,
		TokenExpiresAt:    &expired,
		SessionServer:     "https://orka.example",
		SessionSubject:    "oidc-account-a",
		OIDCClientID:      "orka-client",
		OIDCTokenEndpoint: "https://idp.example/token",
		OIDCRefreshToken:  "refresh-a",
	}

	cmd := newRootCmd()
	got := configuredOIDCToken(cmd, "https://orka.example", accountA)
	if got != "account-a-expired-token" {
		t.Fatalf("configuredOIDCToken adopted replaced account token: got %q, want account-a-expired-token", got)
	}
}

func TestResolveOIDCLoginTargetsRejectsExplicitOIDCDisabledEvenWithFlags(t *testing.T) {
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"oidc": false}) //nolint:errcheck
	}))
	defer orka.Close()

	_, err := resolveOIDCLoginTargets(t.Context(), orka.URL, "https://issuer.example", "orka-client")
	if err == nil || !strings.Contains(err.Error(), "does not have OIDC authentication enabled") {
		t.Fatalf("expected OIDC disabled error even with manual flags, got %v", err)
	}
}

func TestSelectOIDCBearerTokenSkipsExpiredAccessToken(t *testing.T) {
	expiredAccess := testCompactJWT(t, "https://issuer.example", "orka-api", time.Now().UTC().Add(-time.Hour).Unix())
	validIDExp := time.Now().UTC().Add(time.Hour).Unix()
	validID := testCompactJWT(t, "https://issuer.example", "orka-api", validIDExp)

	tok, exp, err := selectOIDCBearerToken(oidcTokenResponse{
		AccessToken: expiredAccess,
		IDToken:     validID,
	}, "https://issuer.example", "orka-api")
	if err != nil {
		t.Fatalf("selectOIDCBearerToken error: %v", err)
	}
	if tok != validID {
		t.Fatalf("selected token = %q, want unexpired id_token", tok)
	}
	if exp == nil || exp.Unix() != validIDExp {
		t.Fatalf("exp = %v, want %d", exp, validIDExp)
	}
}

func TestOIDCRefreshClearsRefreshTokenOnInvalidGrantAndSavesRotatedTokenOnUnusableBearer(t *testing.T) {
	t.Run("invalid_grant clears refresh token", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"}) //nolint:errcheck
		}))
		defer idp.Close()

		expired := time.Now().UTC().Add(-time.Minute)
		initial := orkaConfig{
			Server:            "https://orka.example",
			Token:             "expired-token",
			TokenKind:         tokenKindOIDC,
			TokenExpiresAt:    &expired,
			SessionServer:     "https://orka.example",
			SessionSubject:    "oidc-login-1",
			OIDCIssuer:        idp.URL,
			OIDCAudience:      "orka-api",
			OIDCClientID:      "orka-api",
			OIDCTokenEndpoint: idp.URL + "/token",
			OIDCRefreshToken:  "dead-refresh-token",
		}
		if err := saveConfig(initial); err != nil {
			t.Fatalf("saveConfig: %v", err)
		}

		cmd := newRootCmd()
		var errBuf bytes.Buffer
		cmd.SetErr(&errBuf)
		_ = configuredOIDCToken(cmd, "https://orka.example", initial)

		after := loadConfig()
		if after.OIDCRefreshToken != "" {
			t.Fatalf("OIDCRefreshToken = %q, want cleared after invalid_grant", after.OIDCRefreshToken)
		}
	})

	t.Run("rotated refresh token saved even when bearer is unusable", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"access_token":  "opaque-access-token",
				"refresh_token": "rotated-refresh-token-2",
				"expires_in":    3600,
			})
		}))
		defer idp.Close()

		expired := time.Now().UTC().Add(-time.Minute)
		initial := orkaConfig{
			Server:            "https://orka.example",
			Token:             "previous-jwt",
			TokenKind:         tokenKindOIDC,
			TokenExpiresAt:    &expired,
			SessionServer:     "https://orka.example",
			SessionSubject:    "oidc-login-2",
			OIDCIssuer:        idp.URL,
			OIDCAudience:      "orka-api",
			OIDCClientID:      "orka-api",
			OIDCTokenEndpoint: idp.URL + "/token",
			OIDCRefreshToken:  "old-refresh-token-1",
		}
		if err := saveConfig(initial); err != nil {
			t.Fatalf("saveConfig: %v", err)
		}

		cmd := newRootCmd()
		var errBuf bytes.Buffer
		cmd.SetErr(&errBuf)
		_ = configuredOIDCToken(cmd, "https://orka.example", initial)

		after := loadConfig()
		if after.OIDCRefreshToken != "rotated-refresh-token-2" {
			t.Fatalf("OIDCRefreshToken = %q, want rotated-refresh-token-2", after.OIDCRefreshToken)
		}
		if after.Token != "previous-jwt" {
			t.Fatalf("Token = %q, want previous-jwt preserved", after.Token)
		}
	})
}
