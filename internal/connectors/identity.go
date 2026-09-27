/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// MinStateKeyBytes is the minimum HMAC key length for signed state.
	MinStateKeyBytes = 32
	// ConsentTTL bounds how long a consent may stay pending.
	ConsentTTL = 10 * time.Minute
	// CallbackPath is the API path the provider redirects back to. The
	// operator's callback base URL plus this path must match the redirect URI
	// registered with the provider exactly.
	CallbackPath = "/api/v1/connections/callback"

	connectionNameDigestLength = 12
	maxConnectionNameLength    = 63
)

// SubjectDigest returns a stable, non-reversible identifier for a verified
// identity. It binds sealed material to its owner without storing the raw
// subject beside the ciphertext.
func SubjectDigest(issuer, subject string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(issuer) + "\x00" + strings.TrimSpace(subject)))
	return hex.EncodeToString(sum[:])
}

// ConnectionName derives the deterministic name for one person's Connection
// to one provider, so a person has at most one Connection per provider.
func ConnectionName(provider, issuer, subject string) string {
	digest := SubjectDigest(issuer, subject)[:connectionNameDigestLength]
	prefix := strings.ToLower(strings.TrimSpace(provider))
	maxPrefix := maxConnectionNameLength - len(digest) - 1
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-.")
	}
	if prefix == "" {
		prefix = "connection"
	}
	return prefix + "-" + digest
}

// CredentialRef builds the custody binding for a Connection.
func CredentialRef(connection *corev1alpha1.Connection) (store.ConnectorCredentialRef, error) {
	if connection == nil {
		return store.ConnectorCredentialRef{}, errors.New("connection is required")
	}
	ref := store.ConnectorCredentialRef{
		ConnectionUID: string(connection.UID),
		Namespace:     connection.Namespace,
		Name:          connection.Name,
		SubjectDigest: SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject),
		Provider:      connection.Spec.ProviderRef.Name,
	}
	return ref, ref.Validate()
}

// SignState binds a consent nonce to the controller's state key. The state
// travels through the person's browser and the provider; the signature stops
// a forged callback from probing the consent table.
func SignState(key []byte, nonce string) (string, error) {
	if len(key) < MinStateKeyBytes {
		return "", errors.New("connector state key is too short")
	}
	if strings.TrimSpace(nonce) == "" || strings.Contains(nonce, ".") {
		return "", errors.New("connector state nonce is invalid")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(nonce))
	return nonce + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyState checks the signature and returns the nonce.
func VerifyState(key []byte, state string) (string, error) {
	if len(key) < MinStateKeyBytes {
		return "", errors.New("connector state key is too short")
	}
	nonce, signature, ok := strings.Cut(state, ".")
	if !ok || nonce == "" || signature == "" || len(state) > 256 {
		return "", errors.New("connector state is malformed")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return "", errors.New("connector state signature is malformed")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(nonce))
	if !hmac.Equal(decoded, mac.Sum(nil)) {
		return "", errors.New("connector state signature is invalid")
	}
	return nonce, nil
}
