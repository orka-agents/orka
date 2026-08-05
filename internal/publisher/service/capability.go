package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	CapabilityVersion   = "orka.workspace-publisher.capability.v1"
	CapabilityAudience  = "orka.workspace-publisher"
	MinSecretBytes      = 32
	MaxCapabilityTTL    = 5 * time.Minute
	maxCapabilityBytes  = 16 << 10
	requestDigestDomain = "orka.workspace-publisher.request.v1"
)

type CapabilityClaims struct {
	Version       string            `json:"version"`
	Audience      string            `json:"audience"`
	Operation     Operation         `json:"operation"`
	Method        string            `json:"method"`
	Path          string            `json:"path"`
	Metadata      OperationMetadata `json:"metadata"`
	RequestDigest string            `json:"requestDigest"`
	IssuedAt      time.Time         `json:"issuedAt"`
	ExpiresAt     time.Time         `json:"expiresAt"`
}

func RequestDigest(method, path string, body []byte) (string, error) {
	canonical, err := harnessv2.CanonicalJSON(body)
	if err != nil {
		return "", invalidRequest("request body is not canonicalizable JSON", err)
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, requestDigestDomain)
	_, _ = io.WriteString(hash, "\n")
	_, _ = io.WriteString(hash, method)
	_, _ = io.WriteString(hash, "\n")
	_, _ = io.WriteString(hash, path)
	_, _ = io.WriteString(hash, "\n")
	_, _ = hash.Write(canonical)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func SignCapability(secret []byte, claims CapabilityClaims) (string, error) {
	if len(secret) < MinSecretBytes {
		return "", fmt.Errorf("capability secret must be at least %d bytes", MinSecretBytes)
	}
	if err := validateClaims(claims, claims.IssuedAt); err != nil {
		return "", err
	}
	payload, err := harnessv2.CanonicalValue(claims)
	if err != nil {
		return "", fmt.Errorf("canonicalize operation capability: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	encoding := base64.RawURLEncoding
	return encoding.EncodeToString(payload) + "." + encoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyCapability(secret []byte, token string, expected CapabilityClaims, now time.Time) error {
	if len(secret) < MinSecretBytes || len(token) == 0 || len(token) > maxCapabilityBytes {
		return ErrUnauthorized
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ErrUnauthorized
	}
	encoding := base64.RawURLEncoding
	payload, err := encoding.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > maxCapabilityBytes {
		return ErrUnauthorized
	}
	signature, err := encoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size {
		return ErrUnauthorized
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return ErrUnauthorized
	}
	canonical, err := harnessv2.CanonicalJSON(payload)
	if err != nil || subtle.ConstantTimeCompare(canonical, payload) != 1 {
		return ErrUnauthorized
	}
	var claims CapabilityClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return ErrUnauthorized
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrUnauthorized
	}
	if err := validateClaims(claims, now); err != nil {
		return ErrUnauthorized
	}
	if !constantEqual(string(claims.Operation), string(expected.Operation)) ||
		!constantEqual(claims.Method, expected.Method) ||
		!constantEqual(claims.Path, expected.Path) ||
		!constantEqual(claims.Metadata.Namespace, expected.Metadata.Namespace) ||
		!constantEqual(claims.Metadata.OperationID, expected.Metadata.OperationID) ||
		!constantEqual(claims.Metadata.TaskID, expected.Metadata.TaskID) ||
		!constantEqual(claims.Metadata.PublicationID, expected.Metadata.PublicationID) ||
		!constantEqual(claims.RequestDigest, expected.RequestDigest) {
		return ErrUnauthorized
	}
	return nil
}

func validateClaims(claims CapabilityClaims, now time.Time) error {
	if claims.Version != CapabilityVersion || claims.Audience != CapabilityAudience || claims.Operation.Path() == "" ||
		claims.Method != "POST" || claims.Path != claims.Operation.Path() || !isDigest(claims.RequestDigest) {
		return ErrUnauthorized
	}
	if err := claims.Metadata.validateFor(claims.Operation); err != nil {
		return ErrUnauthorized
	}
	if claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(claims.IssuedAt) ||
		claims.ExpiresAt.Sub(claims.IssuedAt) > MaxCapabilityTTL {
		return ErrUnauthorized
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if claims.IssuedAt.After(now.Add(30*time.Second)) || !claims.ExpiresAt.After(now) {
		return ErrUnauthorized
	}
	return nil
}

func NewClaims(operation Operation, metadata OperationMetadata, requestDigest string, now time.Time, ttl time.Duration) CapabilityClaims {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if ttl <= 0 || ttl > MaxCapabilityTTL {
		ttl = time.Minute
	}
	return CapabilityClaims{
		Version: CapabilityVersion, Audience: CapabilityAudience, Operation: operation,
		Method: "POST", Path: operation.Path(), Metadata: metadata, RequestDigest: requestDigest,
		IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(ttl),
	}
}

func constantEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func isDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
