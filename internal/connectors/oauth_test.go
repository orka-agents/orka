/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// fixtureClient routes every request for the configured public-looking host
// to the local TLS fixture, so the production URL checks stay strict.
func fixtureClient(server *httptest.Server) *http.Client {
	addr := server.Listener.Addr().String()
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- local test fixture
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func testOAuthConfig() OAuthProviderConfig {
	return OAuthProviderConfig{
		AuthorizeURL:         "https://provider.example.test/authorize",
		TokenURL:             "https://provider.example.test/token",
		RevocationURL:        "https://provider.example.test/revoke",
		ClientID:             "client-id",
		ClientSecret:         "client-secret",
		ClientAuthentication: corev1alpha1.ConnectorClientAuthSecretBasic,
		PKCE:                 true,
	}
}

func TestAuthorizeURL(t *testing.T) {
	cfg := testOAuthConfig()
	cfg.AdditionalAuthorizeParameters = map[string]string{"prompt": "consent"}
	raw, err := AuthorizeURL(cfg, "https://orka.example.test/api/v1/connections/callback", "nonce.sig", []string{"read:user", "repo"}, "challenge")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"response_type": "code", "client_id": "client-id", "redirect_uri": "https://orka.example.test/api/v1/connections/callback",
		"state": "nonce.sig", "scope": "read:user repo", "code_challenge": "challenge", "code_challenge_method": "S256", "prompt": "consent",
	} {
		if query.Get(key) != want {
			t.Fatalf("%s = %q, want %q", key, query.Get(key), want)
		}
	}
	cfg.PKCE = false
	raw, err = AuthorizeURL(cfg, "https://orka.example.test/cb", "s", nil, "")
	if err != nil || strings.Contains(raw, "code_challenge") || strings.Contains(raw, "scope=") {
		t.Fatalf("non-PKCE URL = %q, err = %v", raw, err)
	}
	cfg.PKCE = true
	if _, err := AuthorizeURL(cfg, "https://orka.example.test/cb", "s", nil, ""); err == nil {
		t.Fatal("PKCE without a challenge must fail")
	}
	cfg.AdditionalAuthorizeParameters = map[string]string{"Redirect_URI": "https://evil.example"}
	if _, err := AuthorizeURL(cfg, "https://orka.example.test/cb", "s", nil, "c"); err == nil {
		t.Fatal("reserved authorize parameter must fail")
	}
	cfg.AdditionalAuthorizeParameters = nil
	cfg.AuthorizeURL = "http://provider.example.test/authorize"
	if _, err := AuthorizeURL(cfg, "https://orka.example.test/cb", "s", nil, "c"); err == nil {
		t.Fatal("plain http authorize URL must fail")
	}
}

func TestExchangeCodeAndRefresh(t *testing.T) {
	var lastForm url.Values
	var lastAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		lastForm = r.PostForm
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			switch r.PostForm.Get("grant_type") {
			case "authorization_code":
				if r.PostForm.Get("code") != "good-code" {
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code", "error_description": "The code passed is incorrect or expired."})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "gho_access", "refresh_token": "ghr_refresh", "token_type": "bearer",
					"expires_in": 28800, "scope": "repo,read:user",
				})
			case "refresh_token":
				if r.PostForm.Get("refresh_token") != "ghr_refresh" {
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_rotated", "refresh_token": "ghr_rotated", "token_type": "bearer", "expires_in": "3600", "scope": "repo read:user"})
			default:
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
			}
		case "/revoke":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	client := NewOAuthClient(OAuthClientOptions{HTTPClient: fixtureClient(server), Now: func() time.Time { return now }})
	cfg := testOAuthConfig()
	ctx := context.Background()

	token, err := client.ExchangeCode(ctx, cfg, "good-code", "verifier", "https://orka.example.test/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if token.AccessToken != "gho_access" || token.RefreshToken != "ghr_refresh" || token.TokenType != "bearer" {
		t.Fatalf("token = %+v", token)
	}
	if !token.ExpiresAt.Equal(now.Add(8 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v", token.ExpiresAt)
	}
	if len(token.Scopes) != 2 || token.Scopes[0] != "repo" || token.Scopes[1] != "read:user" {
		t.Fatalf("scopes = %v", token.Scopes)
	}
	if lastForm.Get("code_verifier") != "verifier" || lastForm.Get("redirect_uri") != "https://orka.example.test/cb" || lastForm.Get("client_secret") != "" {
		t.Fatalf("form = %v", lastForm)
	}
	if !strings.HasPrefix(lastAuth, "Basic ") {
		t.Fatalf("Authorization = %q, want basic client auth", lastAuth)
	}

	// GitHub-style 200 with an error body is a rejection with a sanitized code.
	_, err = client.ExchangeCode(ctx, cfg, "bad-code", "verifier", "https://orka.example.test/cb")
	var oauthErr *OAuthError
	if !asOAuthError(err, &oauthErr) || oauthErr.Code != "bad_verification_code" || !oauthErr.IsInvalidGrant() {
		t.Fatalf("bad code err = %v", err)
	}
	if strings.Contains(err.Error(), "incorrect or expired") {
		t.Fatalf("error leaked the provider description: %v", err)
	}

	assertRefreshAndRevoke(t, client, cfg, &lastForm, &lastAuth)
}

func assertRefreshAndRevoke(t *testing.T, client *OAuthClient, cfg OAuthProviderConfig, lastForm *url.Values, lastAuth *string) {
	t.Helper()
	ctx := context.Background()
	var oauthErr *OAuthError
	cfg.ClientAuthentication = corev1alpha1.ConnectorClientAuthSecretPost
	refreshed, err := client.Refresh(ctx, cfg, "ghr_refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshed.AccessToken != "gho_rotated" || refreshed.RefreshToken != "ghr_rotated" || refreshed.ExpiresAt.IsZero() {
		t.Fatalf("refreshed = %+v", refreshed)
	}
	if (*lastForm).Get("client_secret") != "client-secret" || (*lastForm).Get("client_id") != "client-id" || *lastAuth != "" {
		t.Fatalf("post client auth form = %v auth = %q", *lastForm, *lastAuth)
	}
	_, err = client.Refresh(ctx, cfg, "stale")
	if !asOAuthError(err, &oauthErr) || oauthErr.StatusCode != http.StatusUnauthorized || !oauthErr.IsInvalidGrant() {
		t.Fatalf("stale refresh err = %v", err)
	}

	if err := client.Revoke(ctx, cfg, "gho_access"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if (*lastForm).Get("token") != "gho_access" {
		t.Fatalf("revoke form = %v", lastForm)
	}
	cfg.RevocationURL = ""
	if err := client.Revoke(ctx, cfg, "gho_access"); err != nil {
		t.Fatalf("Revoke without endpoint must succeed: %v", err)
	}
}

func TestOAuthClientRejectsUnsafeEndpoints(t *testing.T) {
	client := NewOAuthClient(OAuthClientOptions{})
	ctx := context.Background()
	for name, tokenURL := range map[string]string{
		"http":         "http://provider.example.test/token",
		"loopback":     "https://127.0.0.1/token",
		"link-local":   "https://[fe80::1]/token",
		"zone":         "https://[fe80::1%25eth0]/token",
		"trailing dot": "https://provider.example.test./token",
		"userinfo":     "https://u:p@provider.example.test/token",
	} {
		cfg := testOAuthConfig()
		cfg.TokenURL = tokenURL
		if _, err := client.ExchangeCode(ctx, cfg, "code", "verifier", "https://orka.example.test/cb"); err == nil {
			t.Fatalf("%s endpoint must be rejected", name)
		}
	}
	// The validator's host rules apply at call time too, before any DNS
	// lookup or dial, for endpoints stored before a rule existed.
	for name, tokenURL := range map[string]string{
		"metadata name":        "https://metadata.google.internal/token",
		"cluster service":      "https://kubernetes.default.svc/token",
		"loopback shorthand":   "https://127.1/token",
		"hex loopback":         "https://0x7f000001/token",
		"ideographic dot host": "https://127%E3%80%820%E3%80%820%E3%80%821/token",
	} {
		cfg := testOAuthConfig()
		cfg.TokenURL = tokenURL
		if _, err := client.ExchangeCode(ctx, cfg, "code", "verifier", "https://orka.example.test/cb"); err == nil || !strings.Contains(err.Error(), "host is not allowed") {
			t.Fatalf("%s endpoint err = %v, want the host refused before dialing", name, err)
		}
	}
	cfg := testOAuthConfig()
	if _, err := client.ExchangeCode(ctx, cfg, "", "verifier", "https://orka.example.test/cb"); err == nil {
		t.Fatal("empty code must be rejected")
	}
	if _, err := client.ExchangeCode(ctx, cfg, "code", "", "https://orka.example.test/cb"); err == nil {
		t.Fatal("PKCE without a verifier must be rejected")
	}
	if _, err := client.Refresh(ctx, cfg, ""); err == nil {
		t.Fatal("empty refresh token must be rejected")
	}
	cfg.ClientAuthentication = "PrivateKeyJWT"
	if _, err := client.ExchangeCode(ctx, cfg, "code", "verifier", "https://orka.example.test/cb"); err == nil {
		t.Fatal("unsupported client auth must be rejected")
	}
}

func TestTokenResponseEdgeCases(t *testing.T) {
	responses := map[string]func(http.ResponseWriter){
		"/not-json": func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>")) },
		"/no-token": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"token_type":"bearer"}`)) },
		"/too-large": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"` + strings.Repeat("a", maxOAuthResponseBytes) + `"}`))
		},
		"/weird-err": func(w http.ResponseWriter) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"Bad Things<script>"}`))
		},
		"/null-exp": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"Bearer","expires_in":null}`))
		},
		"/mac-type": func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"access_token":"a","token_type":"mac"}`)) },
		"/no-type":  func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"access_token":"a"}`)) },
		"/huge-exp": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","expires_in":9223372036854775807}`))
		},
		"/junk-exp": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","expires_in":"3600oops"}`))
		},
		"/zero-exp": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","expires_in":0}`))
		},
		"/frac-exp": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","expires_in":1.5}`))
		},
		"/str-exp": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","expires_in":"3600"}`))
		},
		"/comma-scope": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","scope":"read,write repo"}`))
		},
		"/empty-scope": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","scope":""}`))
		},
		"/no-scope": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer"}`))
		},
		"/space-token": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":" gho_a","token_type":"bearer"}`))
		},
		"/ctl-token": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"gho\u0001a","token_type":"bearer"}`))
		},
		"/quote-token": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"gho\"a","token_type":"bearer"}`))
		},
		"/ctl-refresh": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"gho_a","refresh_token":"ghr\na","token_type":"bearer"}`))
		},
		"/many-scopes": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","scope":"` + strings.Repeat("s,", 300) + `s"}`))
		},
		"/long-scope": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","scope":"` + strings.Repeat("s", 300) + `"}`))
		},
		"/null-scope": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","scope":null}`))
		},
		"/array-scope": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"access_token":"a","token_type":"bearer","scope":["repo"]}`))
		},
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if respond, ok := responses[r.URL.Path]; ok {
			respond(w)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewOAuthClient(OAuthClientOptions{HTTPClient: fixtureClient(server)})
	ctx := context.Background()
	for path, wantErr := range map[string]bool{
		"/not-json": true, "/no-token": true, "/too-large": true, "/weird-err": true, "/null-exp": false,
		"/mac-type": true, "/no-type": true, "/huge-exp": true,
		"/junk-exp": true, "/zero-exp": true, "/frac-exp": true, "/str-exp": false,
		"/comma-scope": false, "/empty-scope": false, "/no-scope": false, "/null-scope": true, "/array-scope": true,
		"/space-token": true, "/ctl-token": true, "/quote-token": true, "/ctl-refresh": true,
		"/many-scopes": true, "/long-scope": true,
	} {
		cfg := testOAuthConfig()
		cfg.TokenURL = "https://provider.example.test" + path
		token, err := client.ExchangeCode(ctx, cfg, "code", "verifier", "https://orka.example.test/cb")
		if (err != nil) != wantErr {
			t.Fatalf("%s: err = %v, want error %t", path, err, wantErr)
		}
		switch path {
		case "/comma-scope":
			// GitHub delimits granted scopes with commas; spaces are accepted too.
			if strings.Join(token.Scopes, "|") != "read|write|repo" || !token.ScopePresent {
				t.Fatalf("comma scope = %v present %t", token.Scopes, token.ScopePresent)
			}
		case "/empty-scope":
			if len(token.Scopes) != 0 || !token.ScopePresent {
				t.Fatalf("empty scope = %v present %t, want an explicit empty grant", token.Scopes, token.ScopePresent)
			}
		case "/no-scope":
			if len(token.Scopes) != 0 || token.ScopePresent {
				t.Fatalf("omitted scope = %v present %t, want absent", token.Scopes, token.ScopePresent)
			}
		}
		if path == "/weird-err" {
			var oauthErr *OAuthError
			if !asOAuthError(err, &oauthErr) || oauthErr.Code != unknownOAuthErrorCode {
				t.Fatalf("unsanitized code: %v", err)
			}
		}
		if path == "/null-exp" && !token.ExpiresAt.IsZero() {
			t.Fatalf("null expires_in must yield zero expiry, got %v", token.ExpiresAt)
		}
	}
}

func TestProviderOAuthConfigDefaults(t *testing.T) {
	if cfg := ProviderOAuthConfig(nil, "x"); cfg.ClientID != "" {
		t.Fatal("nil provider must yield empty config")
	}
	provider := &corev1alpha1.ConnectorProvider{}
	provider.Spec.OAuth.ClientID = "id"
	cfg := ProviderOAuthConfig(provider, "secret")
	if !cfg.PKCE || cfg.ClientAuthentication != corev1alpha1.ConnectorClientAuthSecretBasic || cfg.ClientSecret != "secret" {
		t.Fatalf("defaults = %+v", cfg)
	}
	disabled := false
	provider.Spec.OAuth.PKCE = &disabled
	provider.Spec.OAuth.ClientAuthentication = corev1alpha1.ConnectorClientAuthSecretPost
	cfg = ProviderOAuthConfig(provider, "secret")
	if cfg.PKCE || cfg.ClientAuthentication != corev1alpha1.ConnectorClientAuthSecretPost {
		t.Fatalf("overrides = %+v", cfg)
	}
}

func TestGeneratePKCEAndNonce(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil || len(verifier) < 43 || challenge == "" || challenge == verifier {
		t.Fatalf("PKCE = %q %q %v", verifier, challenge, err)
	}
	nonce, err := GenerateStateNonce()
	if err != nil || len(nonce) < 43 || strings.Contains(nonce, ".") {
		t.Fatalf("nonce = %q %v", nonce, err)
	}
	other, _ := GenerateStateNonce()
	if other == nonce {
		t.Fatal("nonces must be random")
	}
}

func asOAuthError(err error, target **OAuthError) bool {
	for err != nil {
		if e, ok := err.(*OAuthError); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func TestOAuthClientAllowPrivateEndpointsOption(t *testing.T) {
	cfg := OAuthProviderConfig{ClientID: "c", ClientSecret: "s", TokenURL: "https://10.0.0.1/token"}
	strict := NewOAuthClient(OAuthClientOptions{})
	if _, err := strict.post(context.Background(), cfg, cfg.TokenURL, url.Values{}); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("strict err = %v", err)
	}
	relaxed := NewOAuthClient(OAuthClientOptions{AllowPrivateEndpoints: true, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})}})
	resp, err := relaxed.post(context.Background(), cfg, cfg.TokenURL, url.Values{})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("relaxed = %v %v", resp, err)
	}
	if _, err := relaxed.post(context.Background(), cfg, "http://10.0.0.1/token", url.Values{}); err == nil {
		t.Fatal("plain http stays refused")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestOAuthClientRevokesOnlyAtTheIssuer covers a revocation endpoint on a
// different host than the token endpoint: the token is never sent there.
func TestOAuthClientRevokesOnlyAtTheIssuer(t *testing.T) {
	client := NewOAuthClient(OAuthClientOptions{})
	cfg := testOAuthConfig()
	cfg.TokenURL = "https://provider.example.test/token"
	cfg.RevocationURL = "https://collector.example.test/revoke"
	if err := client.Revoke(context.Background(), cfg, "gho_access"); err == nil || !strings.Contains(err.Error(), "not on the token endpoint's host") {
		t.Fatalf("revoke to another host err = %v, want refusal before any request", err)
	}
	if SameEndpointHost("https://github.com/login/oauth/access_token", "https://GITHUB.com:443/x") != true ||
		SameEndpointHost("https://github.com/a", "https://api.github.com/b") || SameEndpointHost("https://github.com/a", "") {
		t.Fatal("SameEndpointHost must compare host and effective port only")
	}
}
