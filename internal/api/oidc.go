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
	"strings"
	"time"
)

const oidcHTTPTimeout = 5 * time.Second

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

func validateOIDCToken(ctx context.Context, token string, cfg OIDCConfig) (*UserInfo, error) {
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		discovered, err := discoverOIDCJWKSURL(ctx, cfg.Issuer)
		if err != nil {
			return nil, err
		}
		jwksURL = discovered
	}

	verified, err := verifyJWT(ctx, token, jwtVerificationConfig{
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

	return userInfoFromOIDCClaims(claims), nil
}

func userInfoFromOIDCClaims(claims oidcClaims) *UserInfo {
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

	return &UserInfo{
		Username: username,
		Groups:   append([]string{}, claims.Groups...),
		AuthType: AuthTypeOIDC,
		Subject:  claims.Subject,
		Email:    claims.Email,
		Issuer:   claims.Issuer,
		Roles:    roles,
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
	ctx, cancel := context.WithTimeout(ctx, oidcHTTPTimeout)
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
