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
	MinStateKeyBytes = 32
	ConsentTTL       = 10 * time.Minute
	CallbackPath     = "/api/v1/connections/callback"

	connectionNameDigestLength = 12
	maxConnectionNameLength    = 63
)

func SubjectDigest(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

const ConnectionSubjectLabel = "orka.ai/connection-subject"

const connectionSubjectLabelLength = 32

func ConnectionSubjectLabelValue(issuer, subject string) string {
	return SubjectDigest(issuer, subject)[:connectionSubjectLabelLength]
}

const ConnectionProviderLabel = "orka.ai/connector-provider"

func ConnectionProviderLabelValue(provider string) string {
	if len(provider) <= 63 && len(validation.IsValidLabelValue(provider)) == 0 {
		return provider
	}
	sum := sha256.Sum256([]byte(provider))
	return "sha256-" + hex.EncodeToString(sum[:])[:32]
}

func ConnectionLabels(connection *corev1alpha1.Connection) map[string]string {
	if connection == nil {
		return nil
	}
	return map[string]string{
		ConnectionSubjectLabel:  ConnectionSubjectLabelValue(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject),
		ConnectionProviderLabel: ConnectionProviderLabelValue(connection.Spec.ProviderRef.Name),
	}
}

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

const AgentHostProviderAnnotation = "orka.ai/connector-provider"

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
