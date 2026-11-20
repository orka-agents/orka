/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwebhook

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

func newTestSigningKey(t *testing.T) *SigningKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewSigningKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

const testAudience = "https://hooks.example.com/orka?route=1"

var testTask = TaskClaim{UID: "0b9e0f43-6f35-4b1b-9d5c-9a2b3f1d7c11", Namespace: "team-a", Name: "build"}

func signTestToken(t *testing.T, key *SigningKey, body []byte, now time.Time) string {
	t.Helper()
	token, err := Sign(key, NewClaims(testAudience, testTask, "delivery-1", "task.succeeded", 2, body, now))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestSignVerifyBindsTaskEventDeliveryAndPayload(t *testing.T) {
	t.Parallel()
	key := newTestSigningKey(t)
	body := []byte(`{"taskName":"build","phase":"Succeeded"}`)
	now := time.Unix(1_800_000_000, 0)
	token := signTestToken(t, key, body, now)

	claims, err := Verify(token, KeySet{Current: key}.JWKS(), VerifyOptions{Audience: testAudience, Body: body, Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if claims.Task != testTask || claims.Subject != testTask.UID {
		t.Fatalf("task claims = %#v / %q, want %#v", claims.Task, claims.Subject, testTask)
	}
	if claims.DeliveryID != "delivery-1" || claims.EventType != "task.succeeded" || claims.Attempt != 2 || claims.JWTID != "delivery-1.2" {
		t.Fatalf("delivery claims = %#v", claims)
	}
	if claims.PayloadSHA256 != PayloadDigest(body) || claims.Expiry-claims.IssuedAt != int64(TokenTTL/time.Second) {
		t.Fatalf("payload/ttl claims = %#v", claims)
	}

	header, _ := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	var decoded map[string]string
	if err := json.Unmarshal(header, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["alg"] != "EdDSA" || decoded["kid"] != key.KeyID || decoded["typ"] != "JWT" {
		t.Fatalf("header = %#v", decoded)
	}
}

// TestSignedTokenVerifiesWithIndependentJOSELibrary checks interoperability:
// a stock JOSE implementation verifies the token against the published JWKS.
func TestSignedTokenVerifiesWithIndependentJOSELibrary(t *testing.T) {
	t.Parallel()
	key := newTestSigningKey(t)
	body := []byte(`{"phase":"Failed"}`)
	token := signTestToken(t, key, body, time.Now())

	published, err := json.Marshal(KeySet{Current: key}.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	set, err := jwk.Parse(published)
	if err != nil {
		t.Fatalf("jwk.Parse() error = %v", err)
	}
	parsed, err := jwt.ParseString(token, jwt.WithKeySet(set), jwt.WithAudience(testAudience), jwt.WithIssuer(Issuer))
	if err != nil {
		t.Fatalf("jwt.ParseString() error = %v", err)
	}
	var digest string
	if err := parsed.Get("payload_sha256", &digest); err != nil || digest != PayloadDigest(body) {
		t.Fatalf("payload_sha256 = %q (%v)", digest, err)
	}
	if _, err := jws.Verify([]byte(token), jws.WithKeySet(set)); err != nil {
		t.Fatalf("jws.Verify() error = %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	t.Parallel()
	key := newTestSigningKey(t)
	body := []byte(`{"phase":"Succeeded"}`)
	now := time.Unix(1_800_000_000, 0)
	token := signTestToken(t, key, body, now)
	keys := KeySet{Current: key}.JWKS()
	options := VerifyOptions{Audience: testAudience, Body: body, Now: now}

	tamperedBody := VerifyOptions{Audience: testAudience, Body: []byte(`{"phase":"Failed"}`), Now: now}
	if _, err := Verify(token, keys, tamperedBody); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered body error = %v, want ErrInvalidSignature", err)
	}

	parts := strings.Split(token, ".")
	var claims map[string]any
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(payload, &claims)
	claims["payload_sha256"] = PayloadDigest([]byte(`{"phase":"Failed"}`))
	forged, _ := json.Marshal(claims)
	forgedToken := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2]
	if _, err := Verify(forgedToken, keys, tamperedBody); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered claims error = %v, want ErrInvalidSignature", err)
	}

	if _, err := Verify(token, keys, VerifyOptions{Audience: "https://other.example.com/", Body: body, Now: now}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong audience error = %v, want ErrInvalidSignature", err)
	}
	if _, err := Verify(token, keys, VerifyOptions{Audience: testAudience, Body: body, Now: now.Add(TokenTTL + 2*MaxClockSkew)}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expired token error = %v, want ErrInvalidSignature", err)
	}
	if _, err := Verify(token, KeySet{Current: newTestSigningKey(t)}.JWKS(), options); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error = %v, want ErrUnknownKey", err)
	}

	// A JWKS entry that claims the right kid but holds another key fails.
	impostor := PublicJWK(newTestSigningKey(t).PublicKey())
	impostor.KeyID = key.KeyID
	if _, err := Verify(token, JWKS{Keys: []JWK{impostor}}, options); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("impostor key error = %v, want ErrInvalidSignature", err)
	}

	header, _ := json.Marshal(map[string]string{"alg": "none", "kid": key.KeyID})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + parts[1] + "."
	if _, err := Verify(unsigned, keys, options); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("alg none error = %v, want ErrInvalidSignature", err)
	}
}

func TestKeyRotationKeepsPreviousKeyVerifiable(t *testing.T) {
	t.Parallel()
	oldKey := newTestSigningKey(t)
	newKey := newTestSigningKey(t)
	body := []byte(`{}`)
	now := time.Unix(1_800_000_000, 0)
	options := VerifyOptions{Audience: testAudience, Body: body, Now: now}

	beforeRotation := signTestToken(t, oldKey, body, now)
	rotated := KeySet{Current: newKey, Previous: oldKey}
	afterRotation := signTestToken(t, rotated.Current, body, now)

	published := rotated.JWKS()
	if len(published.Keys) != 2 || published.Keys[0].KeyID != newKey.KeyID || published.Keys[1].KeyID != oldKey.KeyID {
		t.Fatalf("rotated JWKS = %#v, want current then previous", published)
	}
	for name, token := range map[string]string{"before": beforeRotation, "after": afterRotation} {
		if _, err := Verify(token, published, options); err != nil {
			t.Fatalf("%s-rotation token Verify() error = %v", name, err)
		}
	}

	retired := KeySet{Current: newKey}.JWKS()
	if _, err := Verify(beforeRotation, retired, options); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("retired key error = %v, want ErrUnknownKey", err)
	}
	if _, err := Verify(afterRotation, retired, options); err != nil {
		t.Fatalf("current key Verify() error = %v", err)
	}
}

func TestPrivateKeyPEMRoundTripAndKeyID(t *testing.T) {
	t.Parallel()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodePrivateKeyPEM(private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePrivateKeyPEM(encoded)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM() error = %v", err)
	}
	if !key.PublicKey().Equal(private.Public()) {
		t.Fatal("round-tripped key does not match")
	}

	// RFC 8037 Appendix A.3 thumbprint of the Appendix A.1 key.
	x, _ := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	if got := KeyID(x); got != "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k" {
		t.Fatalf("KeyID() = %q, want RFC 8037 thumbprint", got)
	}

	for name, data := range map[string][]byte{
		"empty":    nil,
		"garbage":  []byte("not pem"),
		"trailing": append(append([]byte{}, encoded...), encoded...),
	} {
		if _, err := ParsePrivateKeyPEM(data); err == nil {
			t.Fatalf("ParsePrivateKeyPEM(%s) succeeded, want error", name)
		}
	}
}
