/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package api

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
)

const mcpProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource" + mcpEndpointPath

var defaultOIDCScopes = []string{"openid", "profile", "email", "offline_access"}

type mcpProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// ParseMCPResourceURL returns the canonical form of the MCP endpoint's
// external URL, which MCP clients name as the OAuth resource. Empty input
// returns "".
func ParseMCPResourceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("MCP resource URL %q must be an absolute URL with no credentials, query or fragment", raw)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && (scheme != "http" || !isLoopbackHostname(parsed.Hostname())) {
		return "", fmt.Errorf("MCP resource URL %q must use https, or http only for localhost", raw)
	}
	if strings.ContainsFunc(parsed.Host, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-:[]", r)
	}) {
		return "", fmt.Errorf("MCP resource URL %q has a host Orka cannot quote in a WWW-Authenticate challenge", raw)
	}
	if parsed.EscapedPath() != mcpEndpointPath {
		return "", fmt.Errorf("MCP resource URL %q must end in the endpoint path %s, with no prefix", raw, mcpEndpointPath)
	}
	return scheme + "://" + strings.ToLower(parsed.Host) + mcpEndpointPath, nil
}

// ParseOIDCScopes splits a space- or comma-separated OAuth scope list and
// checks each scope is an RFC 6749 scope token.
func ParseOIDCScopes(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' })
	scopes := make([]string, 0, len(fields))
	for _, scope := range fields {
		for _, r := range scope {
			if r < 0x21 || r == '"' || r == '\\' || r > 0x7e {
				return nil, fmt.Errorf("OIDC scope %q has a character a scope token cannot hold", scope)
			}
		}
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

// ValidateOAuth checks that a configured MCP resource URL can be served:
// clients sign in with the OIDC issuer, so OIDC must be configured too.
func (c MCPServerConfig) ValidateOAuth(oidc OIDCConfig) error {
	if c.ResourceURL == "" {
		return nil
	}
	if !c.Enabled {
		return errors.New("--mcp-resource-url requires --mcp-server-enabled")
	}
	if !oidc.Enabled() {
		return errors.New("--mcp-resource-url requires --oidc-issuer and --oidc-audience: MCP clients sign in with that issuer")
	}
	return nil
}

func mcpOAuthMetadata(mcp MCPServerConfig, oidc OIDCConfig) (mcpProtectedResourceMetadata, string, bool) {
	if !mcp.Enabled || mcp.ResourceURL == "" || !oidc.Enabled() {
		return mcpProtectedResourceMetadata{}, "", false
	}
	scopes := oidc.Scopes
	if len(scopes) == 0 {
		scopes = defaultOIDCScopes
	}
	metadataURL := strings.TrimSuffix(mcp.ResourceURL, mcpEndpointPath) + mcpProtectedResourceMetadataPath
	challenge := fmt.Sprintf(`Bearer resource_metadata="%s", scope="%s"`, metadataURL, strings.Join(scopes, " "))
	return mcpProtectedResourceMetadata{
		Resource:               mcp.ResourceURL,
		AuthorizationServers:   []string{oidc.Issuer},
		ScopesSupported:        scopes,
		BearerMethodsSupported: []string{"header"},
	}, challenge, true
}

func serveMCPProtectedResourceMetadata(metadata mcpProtectedResourceMetadata) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "public, max-age=300")
		return c.JSON(metadata)
	}
}

// withMCPBearerChallenge adds the OAuth challenge to an authentication
// refusal, so an MCP client can find where to sign in.
func withMCPBearerChallenge(auth fiber.Handler, challenge string) fiber.Handler {
	if challenge == "" {
		return auth
	}
	return func(c fiber.Ctx) error {
		err := auth(c)
		var fiberErr *fiber.Error
		if (errors.As(err, &fiberErr) && fiberErr.Code == fiber.StatusUnauthorized) ||
			(err == nil && c.Response().StatusCode() == fiber.StatusUnauthorized) {
			c.Set(fiber.HeaderWWWAuthenticate, challenge)
		}
		return err
	}
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
