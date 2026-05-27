/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
)

func TestVerifyJWT_MissingJWKSURL(t *testing.T) {
	provider := newTestOIDCProvider(t)
	token := provider.issueToken(t, testOIDCTokenOptions{})

	_, err := verifyJWT(context.Background(), token, jwtVerificationConfig{
		Issuer:   provider.server.URL,
		Audience: provider.aud,
	})
	if err == nil || !strings.Contains(err.Error(), "missing JWKS URL") {
		t.Fatalf("verifyJWT error = %v, want missing JWKS URL error", err)
	}
}

func TestFilterJWTSigningKeys_AllowsECKeyForES256(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate EC key: %v", err)
	}

	key, err := jwk.Import(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to import EC key: %v", err)
	}
	if err := key.Set(jwk.KeyIDKey, "ec-key"); err != nil {
		t.Fatalf("failed to set JWK kid: %v", err)
	}

	keySet := jwk.NewSet()
	if err := keySet.AddKey(key); err != nil {
		t.Fatalf("failed to add EC key: %v", err)
	}

	filtered, err := filterJWTSigningKeys(keySet, jwtHeader{
		Algorithm: jwa.ES256(),
		KeyID:     "ec-key",
	})
	if err != nil {
		t.Fatalf("filterJWTSigningKeys returned error: %v", err)
	}
	if filtered.Len() != 1 {
		t.Fatalf("filtered key count = %d, want 1", filtered.Len())
	}

	filteredKey, ok := filtered.Key(0)
	if !ok {
		t.Fatal("filtered key missing at index 0")
	}
	if got := filteredKey.KeyType().String(); got != jwa.EC().String() {
		t.Fatalf("filtered key type = %s, want %s", got, jwa.EC().String())
	}
	alg, ok := filteredKey.Algorithm()
	if !ok || alg == nil || alg.String() != jwa.ES256().String() {
		t.Fatalf("filtered key algorithm = %v, want %s", alg, jwa.ES256().String())
	}
}
