/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"sigs.k8s.io/controller-runtime/pkg/client"

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
// subject beside the ciphertext. Issuer and subject are opaque claims and
// are hashed exactly as verified; callers reject blank values separately.
func SubjectDigest(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

// ConnectionSubjectLabel indexes Connections by their person without
// exposing the raw subject; the API stamps it on every Connection it creates.
const ConnectionSubjectLabel = "orka.ai/connection-subject"

// connectionSubjectLabelLength keeps the label value well under the 63
// character limit while staying collision-resistant.
const connectionSubjectLabelLength = 32

// ConnectionSubjectLabelValue is the label value for one person's Connections.
func ConnectionSubjectLabelValue(issuer, subject string) string {
	return SubjectDigest(issuer, subject)[:connectionSubjectLabelLength]
}

// ConnectionProviderLabel indexes Connections by provider.
const ConnectionProviderLabel = "orka.ai/connector-provider"

// ConnectionProviderLabelValue is the provider index label value: the
// provider name when it is a valid label value, otherwise a digest of it,
// since an object name may be 253 characters while a label value is
// limited to 63.
func ConnectionProviderLabelValue(provider string) string {
	if len(provider) <= 63 && len(validation.IsValidLabelValue(provider)) == 0 {
		return provider
	}
	sum := sha256.Sum256([]byte(provider))
	return "sha256-" + hex.EncodeToString(sum[:])[:32]
}

// ConnectionLabels are the index labels every Connection should carry; the
// API sets them at creation and the controller repairs them.
func ConnectionLabels(connection *corev1alpha1.Connection) map[string]string {
	if connection == nil {
		return nil
	}
	return map[string]string{
		ConnectionSubjectLabel:  ConnectionSubjectLabelValue(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject),
		ConnectionProviderLabel: ConnectionProviderLabelValue(connection.Spec.ProviderRef.Name),
	}
}

// ListSubjectConnectionsAuthoritative returns the person's Connections in
// namespace without relying on the index label: it lists every Connection
// there and keeps those whose spec names the requester exactly. The
// controller's cached reader makes this cheap; fail-closed decisions use it
// so an object created outside the API, or with its label stripped, can
// never look unlinked.
func ListSubjectConnectionsAuthoritative(ctx context.Context, reader client.Reader, namespace string, requester *corev1alpha1.RequestedBy) ([]corev1alpha1.Connection, error) {
	if reader == nil || requester == nil {
		return nil, nil
	}
	list := &corev1alpha1.ConnectionList{}
	if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var owned []corev1alpha1.Connection
	for i := range list.Items {
		connection := &list.Items[i]
		if connection.Spec.Subject.Issuer == requester.Issuer && connection.Spec.Subject.Subject == requester.Subject {
			owned = append(owned, *connection)
		}
	}
	return owned, nil
}

// ConnectionName derives the deterministic name for one person's Connection
// to one provider, so a person has at most one Connection per provider. The
// digest covers the full provider name, so two providers that share a long
// prefix still yield distinct names after truncation.
func ConnectionName(provider, issuer, subject string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + SubjectDigest(issuer, subject)))
	digest := hex.EncodeToString(sum[:])[:connectionNameDigestLength]
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
