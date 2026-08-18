package v2

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	OperationCapabilityVersion  = "orka.harness.v2.capability.1"
	OperationCapabilityAudience = "orka.harness.v2"
	MinCapabilitySecretBytes    = 32
)

type OperationCapabilityClaims struct {
	Version       string        `json:"version"`
	Audience      string        `json:"audience"`
	Fence         Fence         `json:"fence"`
	OperationID   OperationID   `json:"operationID"`
	RequestDigest RequestDigest `json:"requestDigest"`
	ExpiresAt     time.Time     `json:"expiresAt"`
}

func (c OperationCapabilityClaims) ValidateAt(now time.Time, requireSession bool) error {
	if c.Version != OperationCapabilityVersion {
		return fmt.Errorf("capability version %q is unsupported", c.Version)
	}
	if c.Audience != OperationCapabilityAudience {
		return fmt.Errorf("capability audience %q is invalid", c.Audience)
	}
	if err := c.Fence.Validate(requireSession); err != nil {
		return fmt.Errorf("capability fence: %w", err)
	}
	if err := requireIdentifier("capability operation ID", string(c.OperationID)); err != nil {
		return err
	}
	if err := ValidateRequestDigest(c.RequestDigest); err != nil {
		return fmt.Errorf("capability request digest: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if c.ExpiresAt.IsZero() || !c.ExpiresAt.After(now) {
		return fmt.Errorf("operation capability is expired")
	}
	return nil
}

func ClaimsForMutation(metadata MutationMetadata) OperationCapabilityClaims {
	return OperationCapabilityClaims{
		Version:       OperationCapabilityVersion,
		Audience:      OperationCapabilityAudience,
		Fence:         metadata.Fence,
		OperationID:   metadata.OperationID,
		RequestDigest: metadata.RequestDigest,
		ExpiresAt:     metadata.ExpiresAt,
	}
}

func signCapabilityClaims(secret []byte, claims any) (string, error) {
	if len(secret) < MinCapabilitySecretBytes {
		return "", fmt.Errorf("operation capability secret must be at least %d bytes", MinCapabilitySecretBytes)
	}
	payload, err := CanonicalValue(claims)
	if err != nil {
		return "", fmt.Errorf("canonicalize operation capability: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	signature := mac.Sum(nil)
	encoding := base64.RawURLEncoding
	return encoding.EncodeToString(payload) + "." + encoding.EncodeToString(signature), nil
}

func verifyCapabilityToken(secret []byte, token string) ([]byte, error) {
	if len(secret) < MinCapabilitySecretBytes {
		return nil, fmt.Errorf("operation capability secret must be at least %d bytes", MinCapabilitySecretBytes)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("operation capability token is malformed")
	}
	encoding := base64.RawURLEncoding
	payload, err := encoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode operation capability payload: %w", err)
	}
	signature, err := encoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode operation capability signature: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, fmt.Errorf("operation capability signature is invalid")
	}
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return nil, fmt.Errorf("operation capability payload is invalid: %w", err)
	}
	if string(canonical) != string(payload) {
		return nil, fmt.Errorf("operation capability payload is not canonical")
	}
	return payload, nil
}

func SignOperationCapability(secret []byte, claims OperationCapabilityClaims) (string, error) {
	if err := claims.ValidateAt(time.Now().UTC(), claims.Fence.RuntimeSessionUID != ""); err != nil {
		return "", err
	}
	return signCapabilityClaims(secret, claims)
}

func VerifyOperationCapability(secret []byte, token string, expected MutationMetadata, requireSession bool, now time.Time) error {
	payload, err := verifyCapabilityToken(secret, token)
	if err != nil {
		return err
	}
	var claims OperationCapabilityClaims
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return fmt.Errorf("decode operation capability claims: %w", err)
	}
	if err := claims.ValidateAt(now, requireSession); err != nil {
		return err
	}
	if claims.OperationID != expected.OperationID || claims.RequestDigest != expected.RequestDigest ||
		CompareFence(expected.Fence, claims.Fence, requireSession) != FenceMatch ||
		!claims.ExpiresAt.Equal(expected.ExpiresAt) {
		return fmt.Errorf("operation capability claims do not match request metadata")
	}
	return nil
}

// StatusCapabilityAudience distinguishes status-read capabilities from
// mutation operation capabilities signed with the same secret.
const StatusCapabilityAudience = "orka.harness.v2/status"

// DefaultStatusCapabilityTTL bounds how long a signed status capability
// remains presentable.
const DefaultStatusCapabilityTTL = time.Minute

// StatusCapabilityClaims prove possession of the per-runtime operation
// capability secret for the status read. Status discloses Task, prompt,
// permission, and fence identifiers, so the controller bearer alone must not
// authorize it. The claims deliberately carry no fence: status is the channel
// through which the controller first learns the supervisor-generated fence
// components (boot ID, instance ID), so exact per-operation fences remain a
// mutation-only requirement, and the capability secret is already scoped to
// exactly one pool or external runtime by construction.
type StatusCapabilityClaims struct {
	Version   string    `json:"version"`
	Audience  string    `json:"audience"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func NewStatusCapabilityClaims(expiresAt time.Time) StatusCapabilityClaims {
	return StatusCapabilityClaims{Version: OperationCapabilityVersion, Audience: StatusCapabilityAudience, ExpiresAt: expiresAt}
}

func (c StatusCapabilityClaims) ValidateAt(now time.Time) error {
	if c.Version != OperationCapabilityVersion {
		return fmt.Errorf("capability version %q is unsupported", c.Version)
	}
	if c.Audience != StatusCapabilityAudience {
		return fmt.Errorf("capability audience %q is invalid", c.Audience)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if c.ExpiresAt.IsZero() || !c.ExpiresAt.After(now) {
		return fmt.Errorf("status capability is expired")
	}
	return nil
}

func SignStatusCapability(secret []byte, claims StatusCapabilityClaims) (string, error) {
	if err := claims.ValidateAt(time.Now().UTC()); err != nil {
		return "", err
	}
	return signCapabilityClaims(secret, claims)
}

func VerifyStatusCapability(secret []byte, token string, now time.Time) error {
	payload, err := verifyCapabilityToken(secret, token)
	if err != nil {
		return err
	}
	var claims StatusCapabilityClaims
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return fmt.Errorf("decode status capability claims: %w", err)
	}
	return claims.ValidateAt(now)
}
