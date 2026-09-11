package artifactcap

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const maxCapabilityTokenBytes = 16 << 10

func Issue(secret []byte, request OperationRequest, now time.Time, ttl time.Duration) (Authorization, error) {
	if len(secret) < MinSecretBytes {
		return Authorization{}, ErrUnauthorized
	}
	if err := request.Validate(); err != nil {
		return Authorization{}, err
	}
	if ttl <= 0 || ttl > MaxCapabilityTTL {
		return Authorization{}, ErrInvalidRequest
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	requestDigest, err := RequestDigest(request)
	if err != nil {
		return Authorization{}, err
	}
	claims := CapabilityClaims{
		Version:       CapabilityVersion,
		Audience:      CapabilityAudience,
		Request:       request,
		RequestDigest: requestDigest,
		IssuedAt:      now,
		ExpiresAt:     now.Add(ttl),
	}
	payload, err := harnessv2.CanonicalValue(claims)
	if err != nil {
		return Authorization{}, ErrInvalidRequest
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	encoding := base64.RawURLEncoding
	return Authorization{
		Capability:    encoding.EncodeToString(payload) + "." + encoding.EncodeToString(mac.Sum(nil)),
		RequestDigest: requestDigest,
	}, nil
}

func Verify(secret []byte, token string, presented PresentedRequest, now time.Time) (CapabilityClaims, error) {
	if len(secret) < MinSecretBytes || len(token) == 0 || len(token) > maxCapabilityTokenBytes {
		return CapabilityClaims{}, ErrUnauthorized
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return CapabilityClaims{}, ErrUnauthorized
	}
	encoding := base64.RawURLEncoding
	payload, err := encoding.DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > maxCapabilityTokenBytes ||
		!constantStringEqual(encoding.EncodeToString(payload), parts[0]) {
		return CapabilityClaims{}, ErrUnauthorized
	}
	signature, err := encoding.DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size ||
		!constantStringEqual(encoding.EncodeToString(signature), parts[1]) {
		return CapabilityClaims{}, ErrUnauthorized
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return CapabilityClaims{}, ErrUnauthorized
	}
	canonical, err := harnessv2.CanonicalJSON(payload)
	if err != nil || subtle.ConstantTimeCompare(canonical, payload) != 1 {
		return CapabilityClaims{}, ErrUnauthorized
	}
	var claims CapabilityClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil || decoder.Decode(&struct{}{}) == nil {
		return CapabilityClaims{}, ErrUnauthorized
	}
	if err := claims.ValidateAt(now); err != nil {
		if errors.Is(err, ErrExpired) {
			return CapabilityClaims{}, ErrExpired
		}
		return CapabilityClaims{}, ErrUnauthorized
	}
	if err := presented.Validate(); err != nil {
		return CapabilityClaims{}, ErrUnauthorized
	}
	expectedDigest, err := RequestDigest(claims.Request)
	if err != nil {
		return CapabilityClaims{}, ErrUnauthorized
	}
	if !constantStringEqual(expectedDigest, claims.RequestDigest) ||
		!constantStringEqual(claims.RequestDigest, presented.RequestDigest) ||
		!constantStringEqual(claims.Request.ObjectDigest, presented.ObjectDigest) ||
		!constantStringEqual(claims.Request.Path(), presented.Path) ||
		!constantStringEqual(claims.Request.Method(), presented.Method) ||
		claims.Request.ContentLength != presented.ContentLength ||
		!constantStringEqual(claims.Request.MediaType, presented.MediaType) {
		return CapabilityClaims{}, ErrUnauthorized
	}
	return claims, nil
}

func constantStringEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
