/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
)

const oidcHTTPTimeout = 5 * time.Second

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

type jwtAudience []string

func (a *jwtAudience) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*a = nil
		return nil
	}

	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = []string{single}
		return nil
	}

	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a jwtAudience) Contains(audience string) bool {
	return slices.Contains(a, audience)
}

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
	Audience    jwtAudience     `json:"aud"`
	Expiration  json.Number     `json:"exp"`
	NotBefore   json.Number     `json:"nbf,omitempty"`
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

type jwksDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	KeyType   string   `json:"kty"`
	KeyUse    string   `json:"use,omitempty"`
	KeyID     string   `json:"kid,omitempty"`
	Algorithm string   `json:"alg,omitempty"`
	N         string   `json:"n"`
	E         string   `json:"e"`
	X5C       []string `json:"x5c,omitempty"`
}

func validateOIDCToken(ctx context.Context, token string, cfg OIDCConfig) (*UserInfo, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}

	headerBytes, err := decodeJWTPart(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode JWT header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}
	if header.Algorithm != "RS256" {
		return nil, fmt.Errorf("unsupported JWT signing algorithm %q", header.Algorithm)
	}

	claimsBytes, err := decodeJWTPart(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}
	var claims oidcClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, fmt.Errorf("parse JWT claims: %w", err)
	}

	pubKey, err := resolveOIDCPublicKey(ctx, cfg, header.KeyID)
	if err != nil {
		return nil, err
	}

	signature, err := decodeJWTPart(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode JWT signature: %w", err)
	}
	signedContent := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signedContent))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, fmt.Errorf("verify JWT signature: %w", err)
	}

	if err := validateOIDCClaims(claims, cfg, time.Now()); err != nil {
		return nil, err
	}

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
	}, nil
}

func validateOIDCClaims(claims oidcClaims, cfg OIDCConfig, now time.Time) error {
	if claims.Issuer != cfg.Issuer {
		return fmt.Errorf("invalid issuer %q", claims.Issuer)
	}
	if claims.Subject == "" {
		return errors.New("missing subject")
	}
	if !claims.Audience.Contains(cfg.Audience) {
		return fmt.Errorf("invalid audience")
	}

	expiresAt, err := jwtNumberTime(claims.Expiration)
	if err != nil {
		return fmt.Errorf("invalid expiration: %w", err)
	}
	if !now.Before(expiresAt) {
		return errors.New("token expired")
	}

	if claims.NotBefore != "" {
		notBefore, err := jwtNumberTime(claims.NotBefore)
		if err != nil {
			return fmt.Errorf("invalid not-before: %w", err)
		}
		if now.Before(notBefore) {
			return errors.New("token not valid yet")
		}
	}

	return nil
}

func jwtNumberTime(n json.Number) (time.Time, error) {
	if n == "" {
		return time.Time{}, errors.New("missing timestamp")
	}
	unix, err := n.Int64()
	if err != nil {
		f, floatErr := n.Float64()
		if floatErr != nil {
			return time.Time{}, err
		}
		unix = int64(f)
	}
	return time.Unix(unix, 0), nil
}

func resolveOIDCPublicKey(ctx context.Context, cfg OIDCConfig, keyID string) (*rsa.PublicKey, error) {
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		discovered, err := discoverOIDCJWKSURL(ctx, cfg.Issuer)
		if err != nil {
			return nil, err
		}
		jwksURL = discovered
	}

	keys, err := fetchJWKS(ctx, jwksURL)
	if err != nil {
		return nil, err
	}

	for _, key := range keys.Keys {
		if key.KeyType != "RSA" {
			continue
		}
		if key.KeyUse != "" && key.KeyUse != "sig" {
			continue
		}
		if key.Algorithm != "" && key.Algorithm != "RS256" {
			continue
		}
		if keyID != "" && key.KeyID != keyID {
			continue
		}

		pubKey, err := jwkToRSAPublicKey(key)
		if err != nil {
			return nil, err
		}
		return pubKey, nil
	}

	if keyID == "" {
		return nil, errors.New("no usable RSA signing key found")
	}
	return nil, fmt.Errorf("no usable RSA signing key found for kid %q", keyID)
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

func fetchJWKS(ctx context.Context, jwksURL string) (*jwksDocument, error) {
	var keys jwksDocument
	if err := getJSON(ctx, jwksURL, &keys); err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	if len(keys.Keys) == 0 {
		return nil, errors.New("JWKS contains no keys")
	}
	return &keys, nil
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

func jwkToRSAPublicKey(key jwkKey) (*rsa.PublicKey, error) {
	if key.N == "" || key.E == "" {
		return nil, errors.New("JWK missing modulus or exponent")
	}

	nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, fmt.Errorf("decode JWK modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, fmt.Errorf("decode JWK exponent: %w", err)
	}
	exponent := new(big.Int).SetBytes(eBytes).Int64()
	if exponent <= 1 || exponent > int64(^uint(0)>>1) {
		return nil, errors.New("invalid JWK exponent")
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(exponent),
	}, nil
}

func decodeJWTPart(part string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(part)
}
