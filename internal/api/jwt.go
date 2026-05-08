/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

type jwtVerificationConfig struct {
	Issuer            string
	Audience          string
	JWKSURL           string
	AllowedAlgorithms []jwa.SignatureAlgorithm
	RequiredClaims    []string
}

type jwtHeader struct {
	Algorithm jwa.SignatureAlgorithm
	KeyID     string
	Type      string
}

type parsedJWT struct {
	Raw       string
	Header    jwtHeader
	RawClaims json.RawMessage
}

type verifiedJWT struct {
	Token     jwt.Token
	Header    jwtHeader
	RawClaims json.RawMessage
}

func verifyJWT(ctx context.Context, raw string, cfg jwtVerificationConfig) (*verifiedJWT, error) {
	parsed, err := parseCompactJWT(raw)
	if err != nil {
		return nil, err
	}

	if !jwtSigningAlgorithmAllowed(parsed.Header.Algorithm, cfg.AllowedAlgorithms) {
		return nil, fmt.Errorf("unsupported JWT signing algorithm %q", parsed.Header.Algorithm.String())
	}

	fetchCtx, cancel := context.WithTimeout(ctx, oidcHTTPTimeout)
	defer cancel()

	keySet, err := jwk.Fetch(fetchCtx, cfg.JWKSURL, jwk.WithHTTPClient(&http.Client{Timeout: oidcHTTPTimeout}))
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}

	filteredSet, err := filterJWTSigningKeys(keySet, parsed.Header)
	if err != nil {
		return nil, err
	}

	tok, err := jwt.ParseString(raw,
		jwt.WithValidate(false),
		jwt.WithKeySet(filteredSet, jws.WithUseDefault(true)),
	)
	if err != nil {
		return nil, fmt.Errorf("verify JWT signature: %w", err)
	}

	validateOptions := make([]jwt.ValidateOption, 0, 2+len(jwtRequiredClaims(cfg.RequiredClaims)))
	if cfg.Issuer != "" {
		validateOptions = append(validateOptions, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		validateOptions = append(validateOptions, jwt.WithAudience(cfg.Audience))
	}
	for _, claim := range jwtRequiredClaims(cfg.RequiredClaims) {
		validateOptions = append(validateOptions, jwt.WithRequiredClaim(claim))
	}

	if err := jwt.Validate(tok, validateOptions...); err != nil {
		return nil, normalizeJWTValidationError(err)
	}
	if jwtClaimRequired(jwt.SubjectKey, cfg.RequiredClaims) {
		subject, ok := tok.Subject()
		if !ok || subject == "" {
			return nil, errors.New("missing subject")
		}
	}

	return &verifiedJWT{
		Token:     tok,
		Header:    parsed.Header,
		RawClaims: parsed.RawClaims,
	}, nil
}

func parseCompactJWT(raw string) (*parsedJWT, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}

	msg, err := jws.Parse([]byte(raw), jws.WithCompact())
	if err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}

	signatures := msg.Signatures()
	if len(signatures) != 1 {
		return nil, errors.New("invalid JWT format")
	}

	protectedHeaders := signatures[0].ProtectedHeaders()
	if protectedHeaders == nil {
		return nil, errors.New("missing JWT protected header")
	}

	alg, ok := protectedHeaders.Algorithm()
	if !ok {
		return nil, errors.New("missing JWT signing algorithm")
	}
	keyID, _ := protectedHeaders.KeyID()
	typ, _ := protectedHeaders.Type()

	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}

	return &parsedJWT{
		Raw: raw,
		Header: jwtHeader{
			Algorithm: alg,
			KeyID:     keyID,
			Type:      typ,
		},
		RawClaims: json.RawMessage(rawClaims),
	}, nil
}

func jwtSigningAlgorithmAllowed(alg jwa.SignatureAlgorithm, allowed []jwa.SignatureAlgorithm) bool {
	if len(allowed) == 0 {
		allowed = []jwa.SignatureAlgorithm{jwa.RS256()}
	}

	for _, candidate := range allowed {
		if alg.String() == candidate.String() {
			return true
		}
	}
	return false
}

func jwtRequiredClaims(required []string) []string {
	if len(required) == 0 {
		return []string{jwt.SubjectKey, jwt.ExpirationKey}
	}
	return required
}

func jwtClaimRequired(name string, required []string) bool {
	for _, claim := range jwtRequiredClaims(required) {
		if claim == name {
			return true
		}
	}
	return false
}

func filterJWTSigningKeys(keySet jwk.Set, header jwtHeader) (jwk.Set, error) {
	filtered := jwk.NewSet()

	for i := 0; i < keySet.Len(); i++ {
		key, ok := keySet.Key(i)
		if !ok {
			continue
		}
		if header.KeyID != "" {
			keyID, ok := key.KeyID()
			if !ok || keyID != header.KeyID {
				continue
			}
		}
		if key.KeyType().String() != jwa.RSA().String() {
			continue
		}
		if use, ok := key.KeyUsage(); ok && use != "" && use != string(jwk.ForSignature) {
			continue
		}
		if alg, ok := key.Algorithm(); ok && alg != nil && alg.String() != header.Algorithm.String() {
			continue
		}

		clone, err := key.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone JWK: %w", err)
		}
		if alg, ok := clone.Algorithm(); !ok || alg == nil {
			if err := clone.Set(jwk.AlgorithmKey, header.Algorithm); err != nil {
				return nil, fmt.Errorf("set JWK algorithm: %w", err)
			}
		}
		if err := filtered.AddKey(clone); err != nil {
			return nil, fmt.Errorf("add JWK: %w", err)
		}
	}

	if filtered.Len() == 0 {
		if header.KeyID != "" {
			return nil, fmt.Errorf("no usable RSA signing key found for kid %q", header.KeyID)
		}
		return nil, errors.New("no usable RSA signing key found")
	}
	return filtered, nil
}

func normalizeJWTValidationError(err error) error {
	switch {
	case errors.Is(err, jwt.InvalidAudienceError()):
		return fmt.Errorf("invalid audience: %w", err)
	case errors.Is(err, jwt.InvalidIssuerError()):
		return fmt.Errorf("invalid issuer: %w", err)
	case errors.Is(err, jwt.TokenExpiredError()):
		return fmt.Errorf("token expired: %w", err)
	case errors.Is(err, jwt.TokenNotYetValidError()):
		return fmt.Errorf("token not valid yet: %w", err)
	case errors.Is(err, jwt.MissingRequiredClaimError()):
		msg := err.Error()
		switch {
		case strings.Contains(msg, `"`+jwt.SubjectKey+`"`):
			return fmt.Errorf("missing subject: %w", err)
		case strings.Contains(msg, `"`+jwt.ExpirationKey+`"`):
			return fmt.Errorf("invalid expiration: %w", err)
		default:
			return fmt.Errorf("missing required claim: %w", err)
		}
	default:
		return err
	}
}
