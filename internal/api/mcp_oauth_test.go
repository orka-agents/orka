/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

const (
	mcpOAuthTestIssuer      = "https://issuer.example/dex"
	mcpOAuthTestResource    = "https://orka.example.com/mcp"
	mcpOAuthTestMetadataURL = "https://orka.example.com/.well-known/oauth-protected-resource/mcp"
)

func TestParseMCPResourceURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                  "",
		"https://orka.example.com/mcp":      "https://orka.example.com/mcp",
		" HTTPS://Orka.Example.com/mcp ":    "https://orka.example.com/mcp",
		"https://orka.example.com:8443/mcp": "https://orka.example.com:8443/mcp",
		"http://localhost:8080/mcp":         "http://localhost:8080/mcp",
		"http://127.0.0.1:8080/mcp":         "http://127.0.0.1:8080/mcp",
		"http://[::1]:8080/mcp":             "http://[::1]:8080/mcp",
	} {
		got, err := ParseMCPResourceURL(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{
		"http://orka.example.com/mcp",
		"https://orka.example.com",
		"https://orka.example.com/",
		"https://orka.example.com/mcp/",
		"https://orka.example.com/orka/mcp",
		"https://orka.example.com/MCP",
		"https://orka.example.com/mcp?x=1",
		"https://orka.example.com/mcp?",
		"https://orka.example.com/mcp#x",
		"https://someone@orka.example.com/mcp",
		`https://orka"x.example.com/mcp`,
		"https://orka%22x.example.com/mcp",
		"/mcp",
		"orka.example.com/mcp",
		"ftp://orka.example.com/mcp",
	} {
		_, err := ParseMCPResourceURL(raw)
		require.Error(t, err, raw)
	}
}

func TestParseOIDCScopes(t *testing.T) {
	scopes, err := ParseOIDCScopes(" openid profile,email  offline_access,groups ")
	require.NoError(t, err)
	require.Equal(t, []string{"openid", "profile", "email", "offline_access", "groups"}, scopes)

	scopes, err = ParseOIDCScopes("  ")
	require.NoError(t, err)
	require.Empty(t, scopes)

	for _, raw := range []string{`openid "x`, `openid a\b`, "openid\tprofile", "openid é"} {
		_, err := ParseOIDCScopes(raw)
		require.Error(t, err, raw)
	}
}

func TestMCPServerConfigValidateOAuth(t *testing.T) {
	oidc := OIDCConfig{Issuer: mcpOAuthTestIssuer, Audience: "orka-test"}
	require.NoError(t, MCPServerConfig{}.ValidateOAuth(OIDCConfig{}))
	require.NoError(t, MCPServerConfig{Enabled: true}.ValidateOAuth(OIDCConfig{}))
	require.NoError(t, MCPServerConfig{Enabled: true, ResourceURL: mcpOAuthTestResource}.ValidateOAuth(oidc))
	require.ErrorContains(t, MCPServerConfig{ResourceURL: mcpOAuthTestResource}.ValidateOAuth(oidc), "--mcp-server-enabled")
	require.ErrorContains(t, MCPServerConfig{Enabled: true, ResourceURL: mcpOAuthTestResource}.ValidateOAuth(OIDCConfig{Issuer: mcpOAuthTestIssuer}), "--oidc-audience")
}

func newMCPOAuthFixture(t *testing.T, mcp MCPServerConfig, oidc OIDCConfig) *externalAuthorizationFixture {
	t.Helper()
	f := newExternalAuthorizationFixture(t)
	f.config.MCP = mcp
	f.config.OIDC = oidc
	f.server = NewServer(f.kube, nil, f.config)
	return f
}

func mcpOAuthRequest(t *testing.T, f *externalAuthorizationFixture, method, path, authorization string) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := f.server.app.Test(request)
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response, string(data)
}

func TestMCPServesProtectedResourceMetadataAndChallenge(t *testing.T) {
	f := newMCPOAuthFixture(t,
		MCPServerConfig{Enabled: true, ResourceURL: mcpOAuthTestResource},
		OIDCConfig{Issuer: mcpOAuthTestIssuer, Audience: "orka-test", Scopes: []string{"openid", "email", "groups", "offline_access"}})

	response, body := mcpOAuthRequest(t, f, http.MethodGet, mcpProtectedResourceMetadataPath, "")
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, response.Header.Get(fiber.HeaderContentType), "application/json")
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &metadata))
	require.Equal(t, map[string]any{
		"resource":                 mcpOAuthTestResource,
		"authorization_servers":    []any{mcpOAuthTestIssuer},
		"scopes_supported":         []any{"openid", "email", "groups", "offline_access"},
		"bearer_methods_supported": []any{"header"},
	}, metadata)
	require.Zero(t, f.tokenReviews, "the metadata route must not authenticate")

	wantChallenge := `Bearer resource_metadata="` + mcpOAuthTestMetadataURL + `", scope="openid email groups offline_access"`
	for _, authorization := range []string{"", "Basic not-a-credential"} {
		response, body = mcpOAuthRequest(t, f, http.MethodPost, mcpEndpointPath, authorization)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
		require.Equal(t, wantChallenge, response.Header.Get(fiber.HeaderWWWAuthenticate), authorization)
		require.Contains(t, body, `"code":401`)
	}

	cors := httptest.NewRequest(http.MethodPost, mcpEndpointPath, nil)
	cors.Header.Set("Origin", "https://app.example")
	corsResponse, err := f.server.app.Test(cors)
	require.NoError(t, err)
	require.NoError(t, corsResponse.Body.Close())
	require.Equal(t, http.StatusUnauthorized, corsResponse.StatusCode)
	require.Equal(t, fiber.HeaderWWWAuthenticate, corsResponse.Header.Get(fiber.HeaderAccessControlExposeHeaders),
		"a browser client must be able to read the challenge")

	response, body = mcpOAuthRequest(t, f, http.MethodPost, mcpEndpointPath, "Bearer fixture-token")
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Empty(t, response.Header.Get(fiber.HeaderWWWAuthenticate))
	require.Contains(t, body, `"protocolVersion":"2025-11-25"`)

	response, _ = mcpOAuthRequest(t, f, http.MethodGet, "/api/v1/agents", "")
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.Empty(t, response.Header.Get(fiber.HeaderWWWAuthenticate), "only the MCP endpoint names the MCP resource")
}

func TestMCPProtectedResourceMetadataDefaultsScopes(t *testing.T) {
	f := newMCPOAuthFixture(t,
		MCPServerConfig{Enabled: true, ResourceURL: mcpOAuthTestResource},
		OIDCConfig{Issuer: mcpOAuthTestIssuer, Audience: "orka-test"})

	response, body := mcpOAuthRequest(t, f, http.MethodGet, mcpProtectedResourceMetadataPath, "")
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"scopes_supported":["openid","profile","email","offline_access"]`)

	response, _ = mcpOAuthRequest(t, f, http.MethodPost, mcpEndpointPath, "")
	require.Equal(t, `Bearer resource_metadata="`+mcpOAuthTestMetadataURL+`", scope="openid profile email offline_access"`,
		response.Header.Get(fiber.HeaderWWWAuthenticate))
}

func TestMCPProtectedResourceMetadataNeedsResourceURLAndOIDC(t *testing.T) {
	oidc := OIDCConfig{Issuer: mcpOAuthTestIssuer, Audience: "orka-test"}
	for name, tc := range map[string]struct {
		mcp  MCPServerConfig
		oidc OIDCConfig
	}{
		"no resource URL": {MCPServerConfig{Enabled: true}, oidc},
		"no OIDC":         {MCPServerConfig{Enabled: true, ResourceURL: mcpOAuthTestResource}, OIDCConfig{}},
		"MCP disabled":    {MCPServerConfig{ResourceURL: mcpOAuthTestResource}, oidc},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPOAuthFixture(t, tc.mcp, tc.oidc)
			response, _ := mcpOAuthRequest(t, f, http.MethodGet, mcpProtectedResourceMetadataPath, "")
			require.Equal(t, http.StatusNotFound, response.StatusCode)
			response, _ = mcpOAuthRequest(t, f, http.MethodPost, mcpEndpointPath, "")
			if tc.mcp.Enabled {
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			}
			require.Empty(t, response.Header.Get(fiber.HeaderWWWAuthenticate))
		})
	}
}

func TestWithMCPBearerChallengeOnlyOnUnauthorized(t *testing.T) {
	for status, wantChallenge := range map[int]bool{
		fiber.StatusUnauthorized:       true,
		fiber.StatusForbidden:          false,
		fiber.StatusServiceUnavailable: false,
	} {
		app := fiber.New()
		app.Post("/", withMCPBearerChallenge(func(fiber.Ctx) error {
			return fiber.NewError(status, "refused")
		}, `Bearer resource_metadata="x"`))
		response, err := app.Test(httptest.NewRequest(http.MethodPost, "/", nil))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, status, response.StatusCode)
		require.Equal(t, wantChallenge, response.Header.Get(fiber.HeaderWWWAuthenticate) != "", status)
	}
}

func TestAuthConfigAdvertisesOnlyConfiguredOIDCScopes(t *testing.T) {
	for name, tc := range map[string]struct {
		scopes []string
		want   string
	}{
		"configured":     {[]string{"openid", "groups"}, `{"oidc":true,"oidcConfig":{"issuer":"` + mcpOAuthTestIssuer + `","audience":"orka-test","scopes":["openid","groups"]}}`},
		"not configured": {nil, `{"oidc":true,"oidcConfig":{"issuer":"` + mcpOAuthTestIssuer + `","audience":"orka-test"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPOAuthFixture(t, MCPServerConfig{}, OIDCConfig{Issuer: mcpOAuthTestIssuer, Audience: "orka-test", Scopes: tc.scopes})
			response, body := mcpOAuthRequest(t, f, http.MethodGet, "/api/v1/auth/config", "")
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			require.JSONEq(t, tc.want, body)
		})
	}
}
