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

// extractAuthToken returns the request's authentication token. An
// Authorization bearer token is preferred; x-api-key is the fallback. An
// Authorization header without the bearer prefix is an authentication format
// error instead of falling through to x-api-key.
func extractAuthToken(ctx fiber.Ctx) (string, error) {
	if value := ctx.Get(AuthHeader); value != "" {
		if !strings.HasPrefix(value, BearerPrefix) {
			return "", fmt.Errorf("%w: %s must start with %q", errInvalidAuthHeaderFormat, AuthHeader, BearerPrefix)
		}
		if token := strings.TrimPrefix(value, BearerPrefix); token != "" {
			return token, nil
		}
	}
	if token := ctx.Get(XAPIKeyHeader); token != "" {
		return token, nil
	}
	return "", errMissingAuthToken
}
