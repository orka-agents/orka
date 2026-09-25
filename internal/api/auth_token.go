/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
)

var (
	errMissingAuthToken        = errors.New("missing authorization header")
	errInvalidAuthHeaderFormat = errors.New("invalid authorization header format")
)

// extractAuthToken returns the request's authentication token.
func extractAuthToken(ctx fiber.Ctx) (string, error) {
	return extractAuthTokenFromHeaders(func(header string) string { return ctx.Get(header) })
}

// extractAuthTokenFromHeaders shares header precedence between the installation
// API and the net/http compatibility router. An Authorization bearer token is
// preferred; x-api-key is the fallback. An Authorization header without the
// bearer prefix is an authentication format error instead of falling through to
// x-api-key.
func extractAuthTokenFromHeaders(header func(string) string) (string, error) {
	if value := header(AuthHeader); value != "" {
		if !strings.HasPrefix(value, BearerPrefix) {
			return "", fmt.Errorf("%w: %s must start with %q", errInvalidAuthHeaderFormat, AuthHeader, BearerPrefix)
		}
		if token := strings.TrimPrefix(value, BearerPrefix); token != "" {
			return token, nil
		}
	}
	if token := header(XAPIKeyHeader); token != "" {
		return token, nil
	}
	return "", errMissingAuthToken
}
