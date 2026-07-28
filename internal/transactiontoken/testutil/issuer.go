/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package testutil provides local standards-based transaction-token fixtures.
// It intentionally does not depend on any provider SDK.
package testutil

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/transactiontoken"
)

type testingT interface {
	Helper()
	Fatalf(string, ...any)
}

// Issuer is an in-memory RSA transaction-token issuer for tests.
type Issuer struct {
	key *rsa.PrivateKey
	kid string
}

// NewIssuer creates a local issuer.
func NewIssuer(t testingT) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate transaction-token test key: %v", err)
	}
	return &Issuer{key: key, kid: "transaction-token-test-key"}
}

// KeyID returns the fixture signing key ID.
func (i *Issuer) KeyID() string { return i.kid }

// PrivateKey returns the fixture key for tests that build their own JWTs.
func (i *Issuer) PrivateKey() *rsa.PrivateKey { return i.key }

// JWKSHandler serves the issuer public key as a JWKS.
func (i *Issuer) JWKSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": i.kid,
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(i.key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(i.key.PublicKey.E)).Bytes()),
			}},
		})
	})
}

// Sign returns a strict transaction-token JWT. Missing temporal and transaction
// identifiers are populated locally.
func (i *Issuer) Sign(t testingT, claims transactiontoken.Claims, ttl time.Duration) string {
	t.Helper()
	token, err := i.SignClaims(claims, ttl)
	if err != nil {
		t.Fatalf("sign transaction token: %v", err)
	}
	return token
}

// SignClaims signs the supplied claim set.
func (i *Issuer) SignClaims(claims transactiontoken.Claims, ttl time.Duration) (string, error) {
	if i == nil || i.key == nil {
		return "", errors.New("transaction-token issuer is not configured")
	}
	now := time.Now().UTC()
	if claims.IssuedAt == 0 {
		claims.IssuedAt = now.Unix()
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	if claims.Expiration == 0 {
		claims.Expiration = now.Add(ttl).Unix()
	}
	if claims.TransactionID == "" {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", err
		}
		claims.TransactionID = base64.RawURLEncoding.EncodeToString(random)
	}
	header := map[string]any{"alg": "RS256", "typ": transactiontoken.JWTType, "kid": i.kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify fetches a JWKS, verifies a strict RS256 transaction token, and checks
// its audience and expiry. It is intentionally small and used only by tests.
func Verify(ctx context.Context, jwksURL, audience, token string) (*transactiontoken.Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("token must contain three JWT segments")
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return nil, err
	}
	if header.Algorithm != "RS256" || header.Type != transactiontoken.JWTType || header.KeyID == "" {
		return nil, errors.New("invalid transaction-token JWT header")
	}
	publicKey, err := fetchRSAKey(ctx, jwksURL, header.KeyID)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, fmt.Errorf("verify transaction-token signature: %w", err)
	}
	var claims transactiontoken.Claims
	if err := decodePart(parts[1], &claims); err != nil {
		return nil, err
	}
	if claims.Audience != audience {
		return nil, errors.New("transaction-token audience mismatch")
	}
	if claims.Expiration <= time.Now().Unix() {
		return nil, errors.New("transaction token is expired")
	}
	return &claims, nil
}

func decodePart(part string, destination any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func fetchRSAKey(ctx context.Context, jwksURL, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("JWKS returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []struct {
			KeyID string `json:"kid"`
			N     string `json:"n"`
			E     string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	for _, key := range set.Keys {
		if key.KeyID != kid {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 + int(b)
		}
		if e == 0 {
			return nil, errors.New("JWKS RSA exponent is invalid")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
	}
	return nil, fmt.Errorf("JWKS key %q not found", kid)
}
