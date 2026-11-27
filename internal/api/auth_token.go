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

func extractAuthToken(ctx fiber.Ctx) (string, error) {
	return extractAuthTokenFromHeaders(func(header string) string { return ctx.Get(header) })
}

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
