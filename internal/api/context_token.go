/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	"github.com/gofiber/fiber/v3"
)

const (
	// AuthTypeContextToken identifies generic transaction/context token authentication.
	AuthTypeContextToken = "contextToken"

	// ContextTokenProfileKontxt identifies the built-in kontxt transaction token profile.
	ContextTokenProfileKontxt = "kontxt"

	// KontxtHeaderName is the default HTTP header used by kontxt transaction tokens.
	KontxtHeaderName = kontxttoken.HeaderName

	// KontxtJWTType is the JWT typ header expected by kontxt transaction tokens.
	KontxtJWTType = kontxttoken.TypeHeader
)

var kontxtRequiredClaims = []string{"sub", "exp", "iat", "txn", "scope", "req_wl"}

// TokenHeaderConfig describes one HTTP header location from which a token can be extracted.
type TokenHeaderConfig struct {
	Name   string
	Scheme string
}

// ContextTokenProfileConfig describes validation settings for a context-token profile.
type ContextTokenProfileConfig struct {
	Name           string
	Issuer         string
	Audience       string
	JWKSURL        string
	Headers        []TokenHeaderConfig
	ExpectedType   string
	RequiredClaims []string
}

// ContextTokenConfig holds generic transaction/context token validation settings.
type ContextTokenConfig struct {
	Profiles []ContextTokenProfileConfig
}

// Enabled reports whether context-token authentication is configured.
func (c ContextTokenConfig) Enabled() bool {
	return len(c.Profiles) > 0
}

// ContextToken contains verified transaction/context token identity and metadata.
type ContextToken struct {
	Profile            string
	Type               string
	Issuer             string
	Subject            string
	Audience           []string
	TransactionID      string
	Scope              string
	Scopes             []string
	RequestingWorkload string
	TransactionContext map[string]any
	RequesterContext   map[string]any
	Claims             map[string]any
}

type contextTokenClaims struct {
	Issuer             string         `json:"iss"`
	IssuedAt           json.Number    `json:"iat"`
	Subject            string         `json:"sub"`
	Audience           stringList     `json:"aud"`
	Expiration         json.Number    `json:"exp"`
	NotBefore          json.Number    `json:"nbf,omitempty"`
	TransactionID      string         `json:"txn"`
	Scope              string         `json:"scope"`
	RequestingWorkload string         `json:"req_wl"`
	TransactionContext map[string]any `json:"tctx,omitempty"`
	RequesterContext   map[string]any `json:"rctx,omitempty"`
}

// NewContextTokenConfig builds context-token configuration for a named profile.
func NewContextTokenConfig(profile, issuer, audience, jwksURL, headers string) (ContextTokenConfig, error) {
	if strings.TrimSpace(profile) == "" {
		if issuer != "" || audience != "" || jwksURL != "" || headers != "" {
			return ContextTokenConfig{}, errors.New("context-token-profile is required when context-token settings are provided")
		}
		return ContextTokenConfig{}, nil
	}

	profile = strings.TrimSpace(strings.ToLower(profile))
	if issuer == "" {
		return ContextTokenConfig{}, errors.New("context-token issuer is required")
	}
	if audience == "" {
		return ContextTokenConfig{}, errors.New("context-token audience is required")
	}

	headerCfg, err := parseTokenHeaderConfigs(headers)
	if err != nil {
		return ContextTokenConfig{}, err
	}

	switch profile {
	case ContextTokenProfileKontxt:
		if len(headerCfg) == 0 {
			headerCfg = []TokenHeaderConfig{{Name: KontxtHeaderName}}
		}
		if jwksURL == "" {
			jwksURL = strings.TrimRight(issuer, "/") + "/.well-known/jwks.json"
		}
		return ContextTokenConfig{Profiles: []ContextTokenProfileConfig{{
			Name:           ContextTokenProfileKontxt,
			Issuer:         issuer,
			Audience:       audience,
			JWKSURL:        jwksURL,
			Headers:        headerCfg,
			ExpectedType:   KontxtJWTType,
			RequiredClaims: append([]string{}, kontxtRequiredClaims...),
		}}}, nil
	default:
		return ContextTokenConfig{}, fmt.Errorf("unsupported context-token profile %q", profile)
	}
}

func parseTokenHeaderConfigs(headers string) ([]TokenHeaderConfig, error) {
	if strings.TrimSpace(headers) == "" {
		return nil, nil
	}

	parts := strings.Split(headers, ",")
	configs := make([]TokenHeaderConfig, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		headerName, scheme, _ := strings.Cut(part, ":")
		headerName = strings.TrimSpace(headerName)
		scheme = strings.TrimSpace(scheme)
		if headerName == "" || !validHTTPHeaderName(headerName) {
			return nil, fmt.Errorf("invalid context-token header %q", part)
		}
		configs = append(configs, TokenHeaderConfig{Name: headerName, Scheme: scheme})
	}
	return configs, nil
}

func validHTTPHeaderName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		case r == '!', r == '#', r == '$', r == '%', r == '&', r == '\'', r == '*', r == '+', r == '-', r == '.', r == '^', r == '_', r == '`', r == '|', r == '~':
			continue
		default:
			return false
		}
	}
	return name != ""
}

func extractContextTokenCandidate(ctx fiber.Ctx, cfg ContextTokenConfig) (string, ContextTokenProfileConfig, bool, error) {
	if !cfg.Enabled() {
		return "", ContextTokenProfileConfig{}, false, nil
	}

	for _, profile := range cfg.Profiles {
		for _, header := range profile.Headers {
			headerValue := ctx.Get(header.Name)
			if headerValue == "" {
				continue
			}
			token, err := extractTokenFromHeaderValue(headerValue, header)
			if err != nil {
				if !strings.EqualFold(header.Name, AuthHeader) {
					continue
				}
				return "", ContextTokenProfileConfig{}, false, err
			}
			if strings.EqualFold(header.Name, AuthHeader) && !shouldTreatBearerAsContextToken(token, profile) {
				continue
			}
			return token, profile, true, nil
		}
	}

	return "", ContextTokenProfileConfig{}, false, nil
}

func isUnconfiguredBearerContextToken(ctx fiber.Ctx, cfg ContextTokenConfig) (bool, error) {
	if !cfg.Enabled() {
		return false, nil
	}

	authHeader := ctx.Get(AuthHeader)
	if authHeader == "" {
		return false, nil
	}
	token, err := extractTokenFromHeaderValue(authHeader, TokenHeaderConfig{Name: AuthHeader, Scheme: strings.TrimSpace(BearerPrefix)})
	if err != nil {
		return false, err
	}

	for _, profile := range cfg.Profiles {
		if contextTokenHeaderConfigured(profile, AuthHeader) {
			continue
		}
		if shouldTreatBearerAsContextToken(token, profile) {
			return true, nil
		}
	}
	return false, nil
}

func extractTokenFromHeaderValue(value string, header TokenHeaderConfig) (string, error) {
	if header.Scheme == "" {
		token := strings.TrimSpace(value)
		if token == "" {
			return "", errors.New("empty token header")
		}
		return token, nil
	}

	fields := strings.Fields(value)
	if len(fields) != 2 || !strings.EqualFold(fields[0], header.Scheme) {
		return "", fmt.Errorf("invalid %s header format", header.Name)
	}
	return fields[1], nil
}

func validateContextToken(ctx context.Context, token string, profile ContextTokenProfileConfig) (*ContextToken, error) {
	if profile.Name == "" {
		return nil, errors.New("missing context-token profile name")
	}
	if profile.Issuer == "" {
		return nil, errors.New("missing context-token issuer")
	}
	if profile.Audience == "" {
		return nil, errors.New("missing context-token audience")
	}

	parsed, err := parseCompactJWT(token)
	if err != nil {
		return nil, err
	}
	if parsed.Header.KeyID == "" {
		return nil, errors.New("missing JWT kid")
	}
	if profile.ExpectedType != "" && parsed.Header.Type != profile.ExpectedType {
		return nil, fmt.Errorf("invalid JWT type %q", parsed.Header.Type)
	}

	jwksURL := profile.JWKSURL
	if jwksURL == "" {
		discovered, err := discoverOIDCJWKSURL(ctx, profile.Issuer)
		if err != nil {
			return nil, err
		}
		jwksURL = discovered
	}

	verified, err := verifyParsedJWT(ctx, parsed, jwtVerificationConfig{
		Issuer:         profile.Issuer,
		Audience:       profile.Audience,
		JWKSURL:        jwksURL,
		RequiredClaims: profile.RequiredClaims,
	})
	if err != nil {
		return nil, err
	}

	claimsMap, err := decodeContextTokenClaimsMap(verified.RawClaims)
	if err != nil {
		return nil, fmt.Errorf("parse context-token claims map: %w", err)
	}
	if err := validateRequiredContextTokenClaims(claimsMap, profile.RequiredClaims); err != nil {
		return nil, err
	}

	var claims contextTokenClaims
	if err := json.Unmarshal(verified.RawClaims, &claims); err != nil {
		return nil, fmt.Errorf("parse context-token claims: %w", err)
	}

	return &ContextToken{
		Profile:            profile.Name,
		Type:               verified.Header.Type,
		Issuer:             claims.Issuer,
		Subject:            claims.Subject,
		Audience:           append([]string{}, claims.Audience...),
		TransactionID:      claims.TransactionID,
		Scope:              claims.Scope,
		Scopes:             strings.Fields(claims.Scope),
		RequestingWorkload: claims.RequestingWorkload,
		TransactionContext: cloneMap(claims.TransactionContext),
		RequesterContext:   cloneMap(claims.RequesterContext),
		Claims:             cloneMap(claimsMap),
	}, nil
}

func decodeContextTokenClaimsMap(raw json.RawMessage) (map[string]any, error) {
	var claims map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func validateRequiredContextTokenClaims(claims map[string]any, required []string) error {
	for _, name := range required {
		if !claimPresent(claims[name]) {
			return fmt.Errorf("missing required context-token claim %q", name)
		}
	}
	return nil
}

func claimPresent(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case json.Number:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

func contextTokenToUserInfo(token *ContextToken) *UserInfo {
	username := token.Subject
	if username == "" {
		username = token.RequestingWorkload
	}

	return &UserInfo{
		Username:     username,
		AuthType:     AuthTypeContextToken,
		Subject:      token.Subject,
		Issuer:       token.Issuer,
		Roles:        append([]string{}, token.Scopes...),
		ContextToken: token,
	}
}

func shouldTreatBearerAsContextToken(token string, profile ContextTokenProfileConfig) bool {
	if profile.ExpectedType == "" {
		return false
	}
	parsed, err := parseCompactJWT(token)
	if err != nil {
		return false
	}
	return parsed.Header.Type == profile.ExpectedType
}

func contextTokenHeaderConfigured(profile ContextTokenProfileConfig, name string) bool {
	return slices.ContainsFunc(profile.Headers, func(header TokenHeaderConfig) bool {
		return strings.EqualFold(header.Name, name)
	})
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}
