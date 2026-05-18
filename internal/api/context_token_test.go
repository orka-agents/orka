/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	"github.com/gofiber/fiber/v3"
)

const (
	testContextTokenSubject       = "workload-subject"
	testContextTokenTransactionID = "txn-123"
	testContextTokenTraceID       = "trace-123"
)

func issueTestContextToken(t *testing.T, provider *testOIDCProvider, headerOverrides, claimOverrides map[string]any) string {
	t.Helper()

	now := time.Now()
	header := map[string]any{
		"alg": "RS256",
		"typ": KontxtJWTType,
		"kid": provider.kid,
	}
	claims := map[string]any{
		"iss":    provider.server.URL,
		"sub":    testContextTokenSubject,
		"aud":    provider.aud,
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Unix(),
		"txn":    testContextTokenTransactionID,
		"scope":  "read write",
		"req_wl": "spiffe://example.test/ns/default/sa/client",
		"tctx": map[string]any{
			"trace_id": testContextTokenTraceID,
		},
		"rctx": map[string]any{
			"user": "alice",
		},
	}

	for k, v := range headerOverrides {
		if v == nil {
			delete(header, k)
			continue
		}
		header[k] = v
	}
	for k, v := range claimOverrides {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}

	headerPart := mustBase64JSON(t, header)
	claimsPart := mustBase64JSON(t, claims)
	signedContent := headerPart + "." + claimsPart
	digest := sha256.Sum256([]byte(signedContent))
	signature, err := rsa.SignPKCS1v15(rand.Reader, provider.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("failed to sign JWT: %v", err)
	}

	return signedContent + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func testContextTokenConfig(t *testing.T, provider *testOIDCProvider, headers string) ContextTokenConfig {
	t.Helper()
	cfg, err := NewContextTokenConfig(ContextTokenProfileKontxt, provider.server.URL, provider.aud, provider.server.URL+"/jwks", headers)
	if err != nil {
		t.Fatalf("NewContextTokenConfig returned error: %v", err)
	}
	return cfg
}

func TestContextToken_KontxtGeneratedTokenCompatibility(t *testing.T) {
	provider := newTestOIDCProvider(t)
	profile := testContextTokenConfig(t, provider, "").Profiles[0]

	tokenString, err := kontxttoken.New(kontxttoken.Claims{
		Issuer:             provider.server.URL,
		Audience:           provider.aud,
		Subject:            testContextTokenSubject,
		Scope:              "read write",
		RequestingWorkload: "spiffe://example.test/ns/default/sa/client",
		TransactionContext: map[string]any{
			"trace_id": testContextTokenTraceID,
		},
		RequesterContext: map[string]any{
			"user": "alice",
		},
	}, provider.key, provider.kid, time.Hour)
	if err != nil {
		t.Fatalf("kontxt token.New returned error: %v", err)
	}

	ctxToken, err := validateContextToken(context.Background(), tokenString, profile)
	if err != nil {
		t.Fatalf("validateContextToken returned error for kontxt-generated token: %v", err)
	}
	if ctxToken.Profile != ContextTokenProfileKontxt || ctxToken.Type != kontxttoken.TypeHeader {
		t.Fatalf("unexpected profile/type: %#v", ctxToken)
	}
	if ctxToken.Subject != testContextTokenSubject ||
		ctxToken.Scope != "read write" ||
		ctxToken.RequestingWorkload != "spiffe://example.test/ns/default/sa/client" ||
		ctxToken.TransactionID == "" {
		t.Fatalf("unexpected context token claims: %#v", ctxToken)
	}
	if ctxToken.TransactionContext["trace_id"] != testContextTokenTraceID || ctxToken.RequesterContext["user"] != "alice" {
		t.Fatalf("unexpected context/requester context: %#v", ctxToken)
	}

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{ContextTokens: ContextTokenConfig{Profiles: []ContextTokenProfileConfig{profile}}}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		userInfo := GetUserInfo(ctx)
		if userInfo == nil || userInfo.ContextToken == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "missing context token user info")
		}
		if userInfo.AuthType != AuthTypeContextToken || userInfo.ContextToken.TransactionID != ctxToken.TransactionID {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected context token user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(kontxttoken.HeaderName, tokenString)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestContextToken_KontxtValidViaTxnTokenHeader(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := testContextTokenConfig(t, provider, "")
	token := issueTestContextToken(t, provider, nil, nil)

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{ContextTokens: cfg}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		userInfo := GetUserInfo(ctx)
		if userInfo == nil || userInfo.ContextToken == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "missing context token user info")
		}
		if userInfo.AuthType != AuthTypeContextToken {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected auth type")
		}
		if userInfo.Subject != testContextTokenSubject || userInfo.ContextToken.TransactionID != testContextTokenTransactionID {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected context token claims")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(KontxtHeaderName, token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestContextToken_KontxtValidViaAuthorizationBearerWhenConfigured(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := testContextTokenConfig(t, provider, "Txn-Token,Authorization:Bearer")
	token := issueTestContextToken(t, provider, nil, nil)

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{ContextTokens: cfg}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		if userInfo := GetUserInfo(ctx); userInfo == nil || userInfo.AuthType != AuthTypeContextToken {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(AuthHeader, "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestContextToken_BearerIgnoredWhenNotConfigured(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := testContextTokenConfig(t, provider, "")
	token := issueTestContextToken(t, provider, nil, nil)

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{OIDC: provider.config(), ContextTokens: cfg}))
	app.Get("/test", func(ctx fiber.Ctx) error { return ctx.SendString("OK") })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(AuthHeader, "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll response body: %v", err)
	}
	if !strings.Contains(string(body), "Txn-Token") || !strings.Contains(string(body), "Authorization: Bearer") {
		t.Fatalf("response body = %q, want actionable context-token header guidance", string(body))
	}
}

func TestContextToken_AuthorizationBearerPreservesOIDCFallback(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := testContextTokenConfig(t, provider, "Txn-Token,Authorization:Bearer")
	token := provider.issueToken(t, testOIDCTokenOptions{Username: "oidc-user"})

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{OIDC: provider.config(), ContextTokens: cfg}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		userInfo := GetUserInfo(ctx)
		if userInfo == nil || userInfo.AuthType != AuthTypeOIDC || userInfo.Username != "oidc-user" {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(AuthHeader, "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestValidateContextToken_Kontxt(t *testing.T) {
	provider := newTestOIDCProvider(t)
	profile := testContextTokenConfig(t, provider, "").Profiles[0]
	token := issueTestContextToken(t, provider, nil, nil)

	ctxToken, err := validateContextToken(context.Background(), token, profile)
	if err != nil {
		t.Fatalf("validateContextToken returned error: %v", err)
	}
	if ctxToken.Profile != ContextTokenProfileKontxt || ctxToken.Type != KontxtJWTType {
		t.Fatalf("unexpected profile/type: %#v", ctxToken)
	}
	if ctxToken.TransactionID != testContextTokenTransactionID || ctxToken.Scope != "read write" || ctxToken.RequestingWorkload == "" {
		t.Fatalf("unexpected transaction claims: %#v", ctxToken)
	}
	if !slices.Contains(ctxToken.Scopes, "read") || !slices.Contains(ctxToken.Scopes, "write") {
		t.Fatalf("Scopes = %#v, want read and write", ctxToken.Scopes)
	}
}

func TestValidateContextToken_KontxtUsesDefaultJWKSURL(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg, err := NewContextTokenConfig(ContextTokenProfileKontxt, provider.server.URL, provider.aud, "", "")
	if err != nil {
		t.Fatalf("NewContextTokenConfig returned error: %v", err)
	}
	profile := cfg.Profiles[0]
	if profile.JWKSURL != provider.server.URL+"/.well-known/jwks.json" {
		t.Fatalf("JWKSURL = %q, want kontxt default JWKS endpoint", profile.JWKSURL)
	}

	token := issueTestContextToken(t, provider, nil, nil)
	if _, err := validateContextToken(context.Background(), token, profile); err != nil {
		t.Fatalf("validateContextToken returned error: %v", err)
	}
	if provider.discoveryHits.Load() != 0 {
		t.Fatalf("discoveryHits = %d, want 0 for kontxt default JWKS URL", provider.discoveryHits.Load())
	}
	if provider.jwksHits.Load() == 0 {
		t.Fatal("expected kontxt default JWKS endpoint to be fetched")
	}
}

func TestValidateContextToken_KontxtFailures(t *testing.T) {
	provider := newTestOIDCProvider(t)
	profile := testContextTokenConfig(t, provider, "").Profiles[0]
	now := time.Now()

	tests := []struct {
		name            string
		headerOverrides map[string]any
		claimOverrides  map[string]any
		tamper          bool
		wantErr         string
	}{
		{name: "expired", claimOverrides: map[string]any{"exp": now.Add(-time.Minute).Unix()}, wantErr: "expired"},
		{name: "wrong issuer", claimOverrides: map[string]any{"iss": provider.server.URL + "/other"}, wantErr: "issuer"},
		{name: "wrong audience", claimOverrides: map[string]any{"aud": "not-orka"}, wantErr: "audience"},
		{name: "tampered signature", tamper: true, wantErr: "signature"},
		{name: "missing kid", headerOverrides: map[string]any{"kid": nil}, wantErr: "kid"},
		{name: "unknown kid", headerOverrides: map[string]any{"kid": "unknown"}, wantErr: "kid"},
		{name: "missing typ", headerOverrides: map[string]any{"typ": nil}, wantErr: "typ"},
		{name: "wrong typ", headerOverrides: map[string]any{"typ": "JWT"}, wantErr: "typ"},
		{name: "missing iat", claimOverrides: map[string]any{"iat": nil}, wantErr: "iat"},
		{name: "missing txn", claimOverrides: map[string]any{"txn": nil}, wantErr: "txn"},
		{name: "missing scope", claimOverrides: map[string]any{"scope": nil}, wantErr: "scope"},
		{name: "missing req_wl", claimOverrides: map[string]any{"req_wl": nil}, wantErr: "req_wl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := issueTestContextToken(t, provider, tt.headerOverrides, tt.claimOverrides)
			if tt.tamper {
				token = tamperJWTSignature(t, token)
			}

			_, err := validateContextToken(context.Background(), token, profile)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
				t.Fatalf("validateContextToken error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAllowedCORSHeadersIncludesContextTokenHeaders(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := testContextTokenConfig(t, provider, "Txn-Token,X-Txn-Token,Authorization:Bearer")
	headers := allowedCORSHeaders(cfg)

	for _, want := range []string{KontxtHeaderName, AuthHeader, XAPIKeyHeader, "X-Txn-Token"} {
		if !slices.Contains(headers, want) {
			t.Fatalf("allowedCORSHeaders() = %#v, want %q", headers, want)
		}
	}
}

func TestAllowedCORSHeadersOmitsContextTokenHeadersWhenDisabled(t *testing.T) {
	headers := allowedCORSHeaders(ContextTokenConfig{})
	if slices.Contains(headers, KontxtHeaderName) {
		t.Fatalf("allowedCORSHeaders() = %#v, did not want %q when context-token auth is disabled", headers, KontxtHeaderName)
	}
	for _, want := range []string{AuthHeader, XAPIKeyHeader} {
		if !slices.Contains(headers, want) {
			t.Fatalf("allowedCORSHeaders() = %#v, want %q", headers, want)
		}
	}
}
