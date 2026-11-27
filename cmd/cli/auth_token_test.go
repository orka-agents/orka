/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAuthTokenServer = "https://orka.example"

func runAuthTokenCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"auth", "token"}, args...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func saveOIDCTestConfig(t *testing.T, token string, expiresAt time.Time, tokenEndpoint, refreshToken string) {
	t.Helper()
	if err := saveConfig(orkaConfig{
		Server:            testAuthTokenServer,
		Token:             token,
		TokenKind:         tokenKindOIDC,
		TokenExpiresAt:    &expiresAt,
		SessionServer:     testAuthTokenServer,
		SessionSubject:    "oidc-test-login",
		OIDCIssuer:        "https://idp.example",
		OIDCAudience:      "orka-client-id",
		OIDCClientID:      "orka-client-id",
		OIDCTokenEndpoint: tokenEndpoint,
		OIDCRefreshToken:  refreshToken,
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
}

func TestAuthTokenPrintsSavedOIDCToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	token := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix())
	saveOIDCTestConfig(t, token, time.Now().Add(time.Hour), "https://idp.example/token", "refresh-1")

	stdout, stderr, err := runAuthTokenCLI(t)
	if err != nil {
		t.Fatalf("auth token: %v\n%s", err, stderr)
	}
	if stdout != token+"\n" {
		t.Fatalf("stdout = %q, want the saved token and a newline", stdout)
	}

	stdout, stderr, err = runAuthTokenCLI(t, "--format", "header-json")
	if err != nil {
		t.Fatalf("auth token --format header-json: %v\n%s", err, stderr)
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(stdout), &headers); err != nil {
		t.Fatalf("header-json output %q: %v", stdout, err)
	}
	if len(headers) != 1 || headers["Authorization"] != "Bearer "+token {
		t.Fatalf("headers = %v, want only Authorization: Bearer <token>", headers)
	}
}

func TestAuthTokenRefreshesExpiredOIDCToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	var (
		mu           sync.Mutex
		refreshCalls int
		refreshed    string
	)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		token := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix())
		mu.Lock()
		refreshCalls++
		refreshed = token
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"access_token":  token,
			"refresh_token": "refresh-2",
			"expires_in":    3600,
		}); err != nil {
			t.Errorf("encode token response: %v", err)
		}
	}))
	defer idp.Close()
	stale := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(-time.Minute).Unix())
	saveOIDCTestConfig(t, stale, time.Now().Add(-time.Minute), idp.URL+"/token", "refresh-1")

	stdout, stderr, err := runAuthTokenCLI(t)
	if err != nil {
		t.Fatalf("auth token: %v\n%s", err, stderr)
	}
	mu.Lock()
	gotCalls := refreshCalls
	gotRefreshed := refreshed
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", gotCalls)
	}
	if stdout != gotRefreshed+"\n" {
		t.Fatalf("stdout = %q, want the refreshed token", stdout)
	}
	if saved := loadConfig(); saved.Token != gotRefreshed || saved.OIDCRefreshToken != "refresh-2" {
		t.Fatalf("refreshed session was not saved: token match %v, refresh token %q", saved.Token == gotRefreshed, saved.OIDCRefreshToken)
	}
}

func TestAuthTokenRefusesExpiredOIDCTokenItCannotRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	stale := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(-time.Minute).Unix())
	saveOIDCTestConfig(t, stale, time.Now().Add(-time.Minute), "https://idp.example/token", "")

	stdout, _, err := runAuthTokenCLI(t)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want an expired-token error", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestAuthTokenRefusesTokenPastItsSavedExpiry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	token := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix())
	saveOIDCTestConfig(t, token, time.Now().Add(-time.Minute), "https://idp.example/token", "")

	stdout, _, err := runAuthTokenCLI(t)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want an expired-token error when the saved expiry has passed before the JWT's exp", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestAuthTokenUsesTheSelectedTokensExpiryWhenTheConfigIsReplaced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	replacement := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix())
	saveOIDCTestConfig(t, replacement, time.Now().Add(time.Hour), "https://idp.example/token", "refresh-b")
	pastSaved := time.Now().Add(-time.Minute)
	read := orkaConfig{
		Server:            testAuthTokenServer,
		Token:             testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix()),
		TokenKind:         tokenKindOIDC,
		TokenExpiresAt:    &pastSaved,
		SessionServer:     testAuthTokenServer,
		SessionSubject:    "oidc-replaced-login",
		OIDCClientID:      "orka-client-id",
		OIDCTokenEndpoint: "https://idp.example/token",
	}

	token, err := authTokenFromConfig(newRootCmd(), read)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("token match replacement %v, err = %v; want an expired-token error from the token's own saved expiry",
			token == replacement, err)
	}
}

func TestAuthTokenRefusesExpiredOIDCTokenWhenRefreshIsRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":"invalid_grant"}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer idp.Close()
	stale := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(-time.Minute).Unix())
	saveOIDCTestConfig(t, stale, time.Now().Add(-time.Minute), idp.URL+"/token", "refresh-1")

	stdout, _, err := runAuthTokenCLI(t)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want an expired-token error", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestAuthTokenPrefersGivenTokenOverSavedOIDCSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	saved := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix())
	saveOIDCTestConfig(t, saved, time.Now().Add(time.Hour), "https://idp.example/token", "refresh-1")
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	stdout, stderr, err := runAuthTokenCLI(t, "--token-file", tokenFile)
	if err != nil {
		t.Fatalf("auth token --token-file: %v\n%s", err, stderr)
	}
	if stdout != "file-token\n" {
		t.Fatalf("stdout = %q, want the --token-file token", stdout)
	}
}

func TestAuthTokenWithholdsOIDCTokenFromAnotherServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	token := testCompactJWT(t, "https://idp.example", "orka-client-id", time.Now().Add(time.Hour).Unix())
	saveOIDCTestConfig(t, token, time.Now().Add(time.Hour), "https://idp.example/token", "refresh-1")

	stdout, _, err := runAuthTokenCLI(t, "--server", "https://other.example")
	if err == nil || !strings.Contains(err.Error(), testAuthTokenServer) {
		t.Fatalf("err = %v, want an error naming the signed-in server", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestAuthTokenRefusesGitHubSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	expiresAt := time.Now().Add(time.Hour)
	if err := saveConfig(orkaConfig{
		Server:         testAuthTokenServer,
		Token:          sessionTokenPrefix + "test",
		TokenKind:      tokenKindGitHubSession,
		TokenExpiresAt: &expiresAt,
		SessionServer:  testAuthTokenServer,
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	stdout, _, err := runAuthTokenCLI(t)
	if err == nil || !strings.Contains(err.Error(), "orka auth login") {
		t.Fatalf("err = %v, want a refusal that points at orka auth login", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestAuthTokenRequiresANamedServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "env-token")
	t.Setenv(envServer, "")

	stdout, _, err := runAuthTokenCLI(t)
	if err == nil || !strings.Contains(err.Error(), "no Orka server") {
		t.Fatalf("err = %v, want a missing-server error", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}

func TestAuthTokenPrintsGivenTokenInPrecedenceOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envServer, testAuthTokenServer)
	t.Setenv(envToken, "env-token")
	if err := saveConfig(orkaConfig{Server: testAuthTokenServer, Token: "config-token"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	stdout, stderr, err := runAuthTokenCLI(t)
	if err != nil {
		t.Fatalf("auth token: %v\n%s", err, stderr)
	}
	if stdout != "env-token\n" {
		t.Fatalf("stdout = %q, want the %s token", stdout, envToken)
	}

	t.Setenv(envToken, "")
	stdout, stderr, err = runAuthTokenCLI(t)
	if err != nil {
		t.Fatalf("auth token: %v\n%s", err, stderr)
	}
	if stdout != "config-token\n" {
		t.Fatalf("stdout = %q, want the config token", stdout)
	}
}

func TestAuthTokenRejectsUnknownFormat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envServer, testAuthTokenServer)
	t.Setenv(envToken, "env-token")

	stdout, _, err := runAuthTokenCLI(t, "--format", "yaml")
	if err == nil || !strings.Contains(err.Error(), "--format") {
		t.Fatalf("err = %v, want a --format error", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
}
