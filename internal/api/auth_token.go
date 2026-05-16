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

// AuthTokenSource describes a request header from which an authentication token
// can be extracted. If Prefix is set, the header value must start with Prefix;
// the extracted token is the remainder of the header value.
type AuthTokenSource struct {
	Header string
	Prefix string
}

// AuthTokenExtractor extracts an authentication token from a request using an
// ordered list of token sources. The first source that yields a non-empty token
// wins.
type AuthTokenExtractor struct {
	Sources []AuthTokenSource
}

func defaultAuthTokenSources() []AuthTokenSource {
	return []AuthTokenSource{
		{Header: AuthHeader, Prefix: BearerPrefix},
		{Header: XAPIKeyHeader},
	}
}

// Extract returns the first token found in the configured token sources. An
// empty source list preserves the default API behavior: Authorization bearer
// tokens are preferred, then x-api-key is used as a fallback.
func (e AuthTokenExtractor) Extract(ctx fiber.Ctx) (string, error) {
	sources := e.Sources
	if len(sources) == 0 {
		sources = defaultAuthTokenSources()
	}

	for _, source := range sources {
		token, found, err := source.Extract(ctx)
		if err != nil {
			return "", err
		}
		if found {
			return token, nil
		}
	}

	return "", errMissingAuthToken
}

// Extract returns a token from this source and reports whether a non-empty token
// was found. A header with a configured but missing prefix is considered an
// authentication format error instead of falling through to later sources.
func (s AuthTokenSource) Extract(ctx fiber.Ctx) (string, bool, error) {
	if s.Header == "" {
		return "", false, nil
	}

	value := ctx.Get(s.Header)
	if value == "" {
		return "", false, nil
	}

	if s.Prefix != "" {
		if !strings.HasPrefix(value, s.Prefix) {
			return "", false, fmt.Errorf("%w: %s must start with %q", errInvalidAuthHeaderFormat, s.Header, s.Prefix)
		}
		value = strings.TrimPrefix(value, s.Prefix)
	}
	if value == "" {
		return "", false, nil
	}

	return value, true, nil
}
