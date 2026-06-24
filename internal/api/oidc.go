/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = nil
		return nil
	}

	var many []string
	if err := json.Unmarshal(data, &many); err == nil {
		*s = many
		return nil
	}

	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []string{single}
		return nil
	}

	return fmt.Errorf("expected string or string array")
}

type oidcClaims struct {
	Issuer      string          `json:"iss"`
	Subject     string          `json:"sub"`
	Email       string          `json:"email,omitempty"`
	Username    string          `json:"preferred_username,omitempty"`
	Name        string          `json:"name,omitempty"`
	Namespace   string          `json:"namespace,omitempty"`
	Groups      stringList      `json:"groups,omitempty"`
	Roles       stringList      `json:"roles,omitempty"`
	RealmAccess oidcRealmAccess `json:"realm_access,omitempty"`
}

type oidcRealmAccess struct {
	Roles stringList `json:"roles,omitempty"`
}

type oidcDiscoveryDocument struct {
	JWKSURI string `json:"jwks_uri"`
}

var errOIDCAuthorization = errors.New("OIDC authorization failed")

func validateOIDCToken(ctx context.Context, token string, cfg OIDCConfig) (*UserInfo, error) {
	parsed, err := parseOIDCTokenCandidate(token, cfg)
	if err != nil {
		return nil, err
	}
	return validateParsedOIDCToken(ctx, parsed, cfg)
}

func validateParsedOIDCToken(ctx context.Context, parsed *parsedJWT, cfg OIDCConfig) (*UserInfo, error) {
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		discovered, err := discoverOIDCJWKSURL(ctx, cfg.Issuer)
		if err != nil {
			return nil, err
		}
		jwksURL = discovered
	}

	verified, err := verifyParsedJWT(ctx, parsed, jwtVerificationConfig{
		Issuer:   cfg.Issuer,
		Audience: cfg.Audience,
		JWKSURL:  jwksURL,
	})
	if err != nil {
		return nil, err
	}

	var claims oidcClaims
	if err := json.Unmarshal(verified.RawClaims, &claims); err != nil {
		return nil, fmt.Errorf("parse JWT claims: %w", err)
	}
	if err := authorizeOIDCClaims(claims, cfg); err != nil {
		return nil, err
	}

	return userInfoFromOIDCClaims(claims, cfg), nil
}

func parseOIDCTokenCandidate(token string, cfg OIDCConfig) (*parsedJWT, error) {
	parsed, err := parseCompactJWT(token)
	if err != nil {
		return nil, err
	}

	if !jwtSigningAlgorithmAllowed(parsed.Header.Algorithm, nil) {
		return nil, fmt.Errorf("unsupported JWT signing algorithm %q", parsed.Header.Algorithm.String())
	}

	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(parsed.RawClaims, &claims); err != nil {
		return nil, fmt.Errorf("parse JWT claims: %w", err)
	}
	if claims.Issuer == "" {
		return nil, errors.New("missing issuer")
	}
	if claims.Issuer != cfg.Issuer {
		return nil, fmt.Errorf("invalid issuer: got %q, want %q", claims.Issuer, cfg.Issuer)
	}

	return parsed, nil
}

func authorizeOIDCClaims(claims oidcClaims, cfg OIDCConfig) error {
	if len(cfg.AllowedSubjects) == 0 {
		return fmt.Errorf("%w: OIDC subject authorization is not configured", errOIDCAuthorization)
	}

	for _, allowed := range cfg.AllowedSubjects {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		matched, err := wildcardMatch(allowed, claims.Subject)
		if err != nil {
			return fmt.Errorf("%w: invalid allowed subject pattern %q: %v", errOIDCAuthorization, allowed, err)
		}
		if matched {
			return nil
		}
	}

	return fmt.Errorf("%w: subject %q is not authorized", errOIDCAuthorization, claims.Subject)
}

func wildcardMatch(pattern, value string) (bool, error) {
	quoted := regexp.QuoteMeta(pattern)
	quoted = strings.ReplaceAll(quoted, `\*`, ".*")
	quoted = strings.ReplaceAll(quoted, `\?`, ".")
	return regexp.MatchString("^"+quoted+"$", value)
}

func userInfoFromOIDCClaims(claims oidcClaims, cfg OIDCConfig) *UserInfo {
	username := claims.Username
	if username == "" {
		username = claims.Email
	}
	if username == "" {
		username = claims.Name
	}
	if username == "" {
		username = claims.Subject
	}

	roles := append([]string{}, claims.Roles...)
	roles = append(roles, claims.RealmAccess.Roles...)

	namespace := cfg.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	if namespace == "" {
		namespace = claims.Namespace
	}
	if namespace == "" {
		namespace = defaultNamespace
	}

	return &UserInfo{
		Username:  username,
		Groups:    append([]string{}, claims.Groups...),
		Namespace: namespace,
		AuthType:  AuthTypeOIDC,
		Subject:   claims.Subject,
		Email:     claims.Email,
		Issuer:    claims.Issuer,
		Roles:     roles,
	}
}

func discoverOIDCJWKSURL(ctx context.Context, issuer string) (string, error) {
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"

	var discovery oidcDiscoveryDocument
	if err := getJSON(ctx, discoveryURL, &discovery); err != nil {
		return "", fmt.Errorf("fetch OIDC discovery document: %w", err)
	}
	if discovery.JWKSURI == "" {
		return "", errors.New("OIDC discovery document missing jwks_uri")
	}
	return discovery.JWKSURI, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, authHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
