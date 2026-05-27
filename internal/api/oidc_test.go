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
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

type testOIDCProvider struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	kid           string
	aud           string
	extraJWKSKeys []testJWKKey

	discoveryHits atomic.Int64
	jwksHits      atomic.Int64
}

type testOIDCTokenOptions struct {
	Issuer     string
	Subject    string
	Audience   any
	ExpiresAt  time.Time
	NotBefore  *time.Time
	Kid        string
	Username   string
	Email      string
	Name       string
	Groups     []string
	Roles      []string
	RealmRoles []string
}

func newTestOIDCProvider(t *testing.T) *testOIDCProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	p := &testOIDCProvider{
		key: key,
		kid: "test-key-1",
		aud: "orka-api",
	}

	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			p.discoveryHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":   p.server.URL,
				"jwks_uri": p.server.URL + "/jwks",
			})
		case "/jwks", "/.well-known/jwks.json":
			p.jwksHits.Add(1)
			keys := append([]testJWKKey{testJWKFromPublicKey(&p.key.PublicKey, p.kid)}, p.extraJWKSKeys...)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": keys,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.server.Close)

	return p
}

func (p *testOIDCProvider) config() OIDCConfig {
	return OIDCConfig{
		Issuer:   p.server.URL,
		Audience: p.aud,
		JWKSURL:  p.server.URL + "/jwks",
	}
}

func (p *testOIDCProvider) configWithoutJWKSURL() OIDCConfig {
	return OIDCConfig{
		Issuer:   p.server.URL,
		Audience: p.aud,
	}
}

func (p *testOIDCProvider) issueToken(t *testing.T, opts testOIDCTokenOptions) string {
	t.Helper()

	issuer := opts.Issuer
	if issuer == "" {
		issuer = p.server.URL
	}
	subject := opts.Subject
	if subject == "" {
		subject = "user-123"
	}
	audience := opts.Audience
	if audience == nil {
		audience = p.aud
	}
	expiresAt := opts.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Hour)
	}
	kid := opts.Kid
	if kid == "" {
		kid = p.kid
	}

	header := map[string]any{
		"alg": "RS256",
		"typ": "JWT",
		"kid": kid,
	}
	claims := map[string]any{
		"iss": issuer,
		"sub": subject,
		"aud": audience,
		"exp": expiresAt.Unix(),
	}
	if opts.NotBefore != nil {
		claims["nbf"] = opts.NotBefore.Unix()
	}
	if opts.Username != "" {
		claims["preferred_username"] = opts.Username
	}
	if opts.Email != "" {
		claims["email"] = opts.Email
	}
	if opts.Name != "" {
		claims["name"] = opts.Name
	}
	if opts.Groups != nil {
		claims["groups"] = opts.Groups
	}
	if opts.Roles != nil {
		claims["roles"] = opts.Roles
	}
	if opts.RealmRoles != nil {
		claims["realm_access"] = map[string]any{"roles": opts.RealmRoles}
	}

	headerPart := mustBase64JSON(t, header)
	claimsPart := mustBase64JSON(t, claims)
	signedContent := headerPart + "." + claimsPart
	digest := sha256.Sum256([]byte(signedContent))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("failed to sign JWT: %v", err)
	}

	return signedContent + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func mustBase64JSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JWT part: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type testJWKKey struct {
	KeyType   string   `json:"kty"`
	KeyUse    string   `json:"use,omitempty"`
	KeyID     string   `json:"kid,omitempty"`
	Algorithm string   `json:"alg,omitempty"`
	N         string   `json:"n"`
	E         string   `json:"e"`
	X5C       []string `json:"x5c,omitempty"`
}

func testJWKFromPublicKey(key *rsa.PublicKey, kid string) testJWKKey {
	return testJWKKey{
		KeyType:   "RSA",
		KeyUse:    "sig",
		KeyID:     kid,
		Algorithm: "RS256",
		N:         base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		E:         base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func tamperJWTSignature(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[2]) == 0 {
		t.Fatalf("test helper received invalid JWT")
	}
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	parts[2] = string(sig)
	return strings.Join(parts, ".")
}

func TestValidateOIDCToken_Valid(t *testing.T) {
	provider := newTestOIDCProvider(t)
	cfg := provider.config()
	token := provider.issueToken(t, testOIDCTokenOptions{
		Subject:    "subject-123",
		Username:   "jane",
		Email:      "jane@example.test",
		Groups:     []string{"devs", "admins"},
		Roles:      []string{"submitter"},
		RealmRoles: []string{"reviewer"},
	})

	userInfo, err := validateOIDCToken(context.Background(), token, cfg)
	if err != nil {
		t.Fatalf("validateOIDCToken returned error: %v", err)
	}

	if userInfo.AuthType != AuthTypeOIDC {
		t.Fatalf("AuthType = %q, want %q", userInfo.AuthType, AuthTypeOIDC)
	}
	if userInfo.Subject != "subject-123" {
		t.Fatalf("Subject = %q, want %q", userInfo.Subject, "subject-123")
	}
	if userInfo.Issuer != provider.server.URL {
		t.Fatalf("Issuer = %q, want %q", userInfo.Issuer, provider.server.URL)
	}
	if userInfo.Username != "jane" {
		t.Fatalf("Username = %q, want %q", userInfo.Username, "jane")
	}
	if userInfo.Email != "jane@example.test" {
		t.Fatalf("Email = %q, want %q", userInfo.Email, "jane@example.test")
	}
	if strings.Join(userInfo.Groups, ",") != "devs,admins" {
		t.Fatalf("Groups = %#v, want [devs admins]", userInfo.Groups)
	}
	if strings.Join(userInfo.Roles, ",") != "submitter,reviewer" {
		t.Fatalf("Roles = %#v, want [submitter reviewer]", userInfo.Roles)
	}
}

func TestValidateOIDCToken_Expired(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{ExpiresAt: time.Now().Add(-time.Minute)})

	_, err := validateOIDCToken(context.Background(), token, provider.config())
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("validateOIDCToken error = %v, want token expired", err)
	}
}

func TestValidateOIDCToken_WrongAudience(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Audience: "not-orka"})

	_, err := validateOIDCToken(context.Background(), token, provider.config())
	if err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("validateOIDCToken error = %v, want audience error", err)
	}
}

func TestValidateOIDCToken_WrongIssuer(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Issuer: provider.server.URL + "/other"})

	_, err := validateOIDCToken(context.Background(), token, provider.config())
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("validateOIDCToken error = %v, want issuer error", err)
	}
}

func TestValidateOIDCToken_TamperedSignature(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := tamperJWTSignature(t, provider.issueToken(t, testOIDCTokenOptions{}))

	_, err := validateOIDCToken(context.Background(), token, provider.config())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("validateOIDCToken error = %v, want signature error", err)
	}
}

func TestValidateOIDCToken_UnknownKid(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Kid: "unknown-key"})

	_, err := validateOIDCToken(context.Background(), token, provider.config())
	if err == nil || !strings.Contains(err.Error(), "kid") {
		t.Fatalf("validateOIDCToken error = %v, want unknown kid error", err)
	}
}

func TestValidateOIDCToken_KidSelectsMatchingJWKSKey(t *testing.T) {
	provider := newTestOIDCProvider(t)
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate wrong RSA key: %v", err)
	}
	provider.extraJWKSKeys = []testJWKKey{testJWKFromPublicKey(&wrongKey.PublicKey, "wrong-key")}

	token := provider.issueToken(t, testOIDCTokenOptions{Kid: "wrong-key"})
	_, err = validateOIDCToken(context.Background(), token, provider.config())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("validateOIDCToken error = %v, want signature error from matching wrong kid key", err)
	}
}

func TestValidateOIDCToken_DiscoveryJWKSURL(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{})

	_, err := validateOIDCToken(context.Background(), token, provider.configWithoutJWKSURL())
	if err != nil {
		t.Fatalf("validateOIDCToken with discovery returned error: %v", err)
	}
	if provider.discoveryHits.Load() == 0 {
		t.Fatal("expected OIDC discovery endpoint to be called")
	}
	if provider.jwksHits.Load() == 0 {
		t.Fatal("expected JWKS endpoint to be called")
	}
}

func TestValidateOIDCToken_InvalidJWTSkipsDiscovery(t *testing.T) {
	provider := newTestOIDCProvider(t)

	_, err := validateOIDCToken(context.Background(), "not-a-compact-jwt", provider.configWithoutJWKSURL())
	if err == nil || !strings.Contains(err.Error(), "invalid JWT format") {
		t.Fatalf("validateOIDCToken error = %v, want invalid JWT format", err)
	}
	if got := provider.discoveryHits.Load(); got != 0 {
		t.Fatalf("OIDC discovery hits = %d, want 0", got)
	}
	if got := provider.jwksHits.Load(); got != 0 {
		t.Fatalf("JWKS hits = %d, want 0", got)
	}
}

func TestValidateOIDCToken_WrongIssuerSkipsDiscovery(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Issuer: "https://kubernetes.default.svc"})

	_, err := validateOIDCToken(context.Background(), token, provider.configWithoutJWKSURL())
	if err == nil || !strings.Contains(err.Error(), "invalid issuer") {
		t.Fatalf("validateOIDCToken error = %v, want invalid issuer", err)
	}
	if got := provider.discoveryHits.Load(); got != 0 {
		t.Fatalf("OIDC discovery hits = %d, want 0", got)
	}
	if got := provider.jwksHits.Load(); got != 0 {
		t.Fatalf("JWKS hits = %d, want 0", got)
	}
}

func TestNewAuthMiddleware_OIDC_ValidToken(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{Username: "oidc-user"})

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{OIDC: provider.config()}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		userInfo := GetUserInfo(ctx)
		if userInfo == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "missing user info")
		}
		if userInfo.AuthType != AuthTypeOIDC || userInfo.Username != "oidc-user" {
			return fiber.NewError(fiber.StatusInternalServerError, "unexpected user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestNewAuthMiddleware_OIDC_MissingToken(t *testing.T) {
	provider := newTestOIDCProvider(t)

	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{OIDC: provider.config()}))
	app.Get("/test", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/test", nil))
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}
