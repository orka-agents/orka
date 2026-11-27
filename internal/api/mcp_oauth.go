/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package api

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
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

func ParseMCPResourceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("MCP resource URL must be an absolute URL with no credentials, query or fragment")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return "", errors.New("MCP resource URL has an empty port")
	}
	var portNum uint64
	if port := parsed.Port(); port != "" {
		n, portErr := strconv.ParseUint(port, 10, 16)
		if portErr != nil || n == 0 || strconv.FormatUint(n, 10) != port {
			return "", errors.New("MCP resource URL has an invalid port")
		}
		portNum = n
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if scheme != connectorSchemeHTTPS && (scheme != connectorSchemeHTTP || !isLoopbackHostname(hostname)) {
		return "", errors.New("MCP resource URL must use https, or http only for localhost")
	}
	if (scheme == connectorSchemeHTTPS && portNum == 443) || (scheme == connectorSchemeHTTP && portNum == 80) {
		return "", errors.New("MCP resource URL must omit the default port so clients and metadata match under RFC 9728")
	}
	if strings.Contains(hostname, ":") {
		ip := net.ParseIP(hostname)
		if ip == nil || ip.To4() != nil || ip.String() != hostname {
			return "", errors.New("MCP resource URL must use a canonical IPv6 literal")
		}
	}
	if strings.ContainsFunc(parsed.Host, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-:[]", r)
	}) {
		return "", errors.New("MCP resource URL has a host Orka cannot quote in a WWW-Authenticate challenge")
	}
	if parsed.EscapedPath() != mcpEndpointPath {
		return "", fmt.Errorf("MCP resource URL must end in the endpoint path %s, with no prefix", mcpEndpointPath)
	}
	return scheme + "://" + strings.ToLower(parsed.Host) + mcpEndpointPath, nil
}

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

func withMCPBearerChallenge(auth fiber.Handler, challenge string) fiber.Handler {
	if challenge == "" {
		return auth
	}
	return func(c fiber.Ctx) error {
		err := auth(c)
		var fiberErr *fiber.Error
		if (errors.As(err, &fiberErr) && fiberErr.Code == fiber.StatusUnauthorized) ||
			(err == nil && c.Response().StatusCode() == fiber.StatusUnauthorized) {
			header := challenge
			if hasBearerAuthorization(c.Get(fiber.HeaderAuthorization)) {
				header += `, error="invalid_token"`
			}
			c.Set(fiber.HeaderWWWAuthenticate, header)
		}
		return err
	}
}

func hasBearerAuthorization(raw string) bool {
	fields := strings.Fields(raw)
	return len(fields) >= 2 && strings.EqualFold(fields[0], "bearer")
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
