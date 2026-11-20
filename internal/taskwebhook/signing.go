/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwebhook

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Issuer is the iss claim of every delivery token.
	Issuer = "orka.ai/task-webhook"
	// TokenTTL bounds how long one attempt's token is valid.
	TokenTTL = 5 * time.Minute
	// MaxClockSkew is the leeway verifiers should allow on iat/exp.
	MaxClockSkew = time.Minute

	algorithmEdDSA = "EdDSA"
	tokenType      = "JWT"
	keyTypeOKP     = "OKP"
	curveEd25519   = "Ed25519"
)

// Header names sent with every delivery attempt.
const (
	HeaderSignature      = "X-Orka-Signature"
	HeaderDeliveryID     = "X-Orka-Delivery-ID"
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderEvent          = "X-Orka-Event"
	HeaderAttempt        = "X-Orka-Delivery-Attempt"
)

var (
	// ErrInvalidSignature reports a token that does not verify.
	ErrInvalidSignature = errors.New("webhook signature is invalid")
	// ErrUnknownKey reports a kid that is not in the verifier's key set.
	ErrUnknownKey = errors.New("webhook signing key is unknown")
)

// SigningKey is an Ed25519 private key with its RFC 7638 thumbprint key ID.
type SigningKey struct {
	KeyID   string
	private ed25519.PrivateKey
}

// NewSigningKey wraps an Ed25519 private key.
func NewSigningKey(private ed25519.PrivateKey) (*SigningKey, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("webhook signing key must be an Ed25519 private key")
	}
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("webhook signing key must be an Ed25519 private key")
	}
	return &SigningKey{KeyID: KeyID(public), private: private}, nil
}

// PublicKey returns the verification half of the signing key.
func (k *SigningKey) PublicKey() ed25519.PublicKey {
	public, _ := k.private.Public().(ed25519.PublicKey)
	return public
}

// ParsePrivateKeyPEM decodes a PEM PKCS#8 Ed25519 private key, the format
// `openssl genpkey -algorithm ed25519` writes.
func ParsePrivateKeyPEM(data []byte) (*SigningKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("webhook signing key must be one PEM PRIVATE KEY block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("webhook signing key is not valid PKCS#8")
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("webhook signing key must be an Ed25519 private key")
	}
	return NewSigningKey(private)
}

// EncodePrivateKeyPEM encodes an Ed25519 private key as PEM PKCS#8.
func EncodePrivateKeyPEM(private ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("encode webhook signing key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// KeyID returns the RFC 7638 JWK thumbprint of an Ed25519 public key.
func KeyID(public ed25519.PublicKey) string {
	// RFC 7638 §3.2: required members in lexicographic order, no whitespace.
	canonical := `{"crv":"` + curveEd25519 + `","kty":"` + keyTypeOKP + `","x":"` +
		base64.RawURLEncoding.EncodeToString(public) + `"}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// JWK is a public Ed25519 JSON Web Key.
type JWK struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	X         string `json:"x"`
	KeyID     string `json:"kid"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// PublicJWK returns the published form of an Ed25519 public key.
func PublicJWK(public ed25519.PublicKey) JWK {
	return JWK{
		KeyType:   keyTypeOKP,
		Curve:     curveEd25519,
		X:         base64.RawURLEncoding.EncodeToString(public),
		KeyID:     KeyID(public),
		Algorithm: algorithmEdDSA,
		Use:       "sig",
	}
}

// KeySet holds the signing key and the previous key kept for verification
// during rotation. Only Current ever signs.
type KeySet struct {
	Current  *SigningKey
	Previous *SigningKey
}

// JWKS returns the public keys receivers may use, current first.
func (s KeySet) JWKS() JWKS {
	set := JWKS{Keys: []JWK{}}
	seen := map[string]bool{}
	for _, key := range []*SigningKey{s.Current, s.Previous} {
		if key == nil || seen[key.KeyID] {
			continue
		}
		seen[key.KeyID] = true
		set.Keys = append(set.Keys, PublicJWK(key.PublicKey()))
	}
	return set
}

// TaskClaim identifies the Task a delivery is about.
type TaskClaim struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Claims is the payload of a delivery token. It binds one attempt of one
// delivery to the destination, the Task, the event, and the exact body.
type Claims struct {
	Issuer        string    `json:"iss"`
	Subject       string    `json:"sub"`
	Audience      string    `json:"aud"`
	IssuedAt      int64     `json:"iat"`
	NotBefore     int64     `json:"nbf"`
	Expiry        int64     `json:"exp"`
	JWTID         string    `json:"jti"`
	DeliveryID    string    `json:"delivery_id"`
	Attempt       int32     `json:"attempt"`
	EventType     string    `json:"event"`
	Task          TaskClaim `json:"task"`
	PayloadSHA256 string    `json:"payload_sha256"`
}

type tokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ,omitempty"`
	KeyID     string `json:"kid"`
}

// PayloadDigest returns the lowercase hex SHA-256 digest of body.
func PayloadDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// NewClaims builds the claims for one attempt issued at now.
func NewClaims(audience string, task TaskClaim, deliveryID, eventType string, attempt int32, body []byte, now time.Time) Claims {
	issued := now.Unix()
	return Claims{
		Issuer:        Issuer,
		Subject:       task.UID,
		Audience:      audience,
		IssuedAt:      issued,
		NotBefore:     issued,
		Expiry:        now.Add(TokenTTL).Unix(),
		JWTID:         fmt.Sprintf("%s.%d", deliveryID, attempt),
		DeliveryID:    deliveryID,
		Attempt:       attempt,
		EventType:     eventType,
		Task:          task,
		PayloadSHA256: PayloadDigest(body),
	}
}

// Sign returns a compact JWS (RFC 7515) over claims with alg EdDSA.
func Sign(key *SigningKey, claims Claims) (string, error) {
	if key == nil || len(key.private) != ed25519.PrivateKeySize {
		return "", errors.New("webhook signing key is unavailable")
	}
	header, err := json.Marshal(tokenHeader{Algorithm: algorithmEdDSA, Type: tokenType, KeyID: key.KeyID})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := key.private.Sign(nil, []byte(signingInput), crypto.Hash(0))
	if err != nil {
		return "", fmt.Errorf("sign webhook token: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// VerifyOptions are the receiver-side expectations for a delivery.
type VerifyOptions struct {
	// Audience is the exact webhook URL the receiver is registered as.
	Audience string
	// Body is the raw request body.
	Body []byte
	// Now is the verification time.
	Now time.Time
}

// Verify checks a delivery token against a JWKS and the received body. It is
// the reference receiver algorithm documented for webhook consumers.
func Verify(token string, keys JWKS, options VerifyOptions) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidSignature
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrInvalidSignature
	}
	var header tokenHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Algorithm != algorithmEdDSA || header.KeyID == "" {
		return nil, ErrInvalidSignature
	}
	var public ed25519.PublicKey
	for _, key := range keys.Keys {
		if key.KeyID != header.KeyID || key.KeyType != keyTypeOKP || key.Curve != curveEd25519 {
			continue
		}
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(key.X)
		if decodeErr != nil || len(decoded) != ed25519.PublicKeySize {
			continue
		}
		public = decoded
		break
	}
	if public == nil {
		return nil, ErrUnknownKey
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		return nil, ErrInvalidSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidSignature
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrInvalidSignature
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	skew := int64(MaxClockSkew / time.Second)
	switch {
	case claims.Issuer != Issuer:
		return nil, fmt.Errorf("%w: issuer mismatch", ErrInvalidSignature)
	case claims.Audience != options.Audience:
		return nil, fmt.Errorf("%w: audience mismatch", ErrInvalidSignature)
	case now.Unix() > claims.Expiry+skew:
		return nil, fmt.Errorf("%w: token expired", ErrInvalidSignature)
	case now.Unix()+skew < claims.NotBefore:
		return nil, fmt.Errorf("%w: token not yet valid", ErrInvalidSignature)
	case claims.Expiry-claims.IssuedAt > int64(TokenTTL/time.Second):
		return nil, fmt.Errorf("%w: token lifetime too long", ErrInvalidSignature)
	}
	digest := PayloadDigest(options.Body)
	if subtle.ConstantTimeCompare([]byte(digest), []byte(claims.PayloadSHA256)) != 1 {
		return nil, fmt.Errorf("%w: payload digest mismatch", ErrInvalidSignature)
	}
	return &claims, nil
}
