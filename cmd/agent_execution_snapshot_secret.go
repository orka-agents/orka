/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/store/sqlite"
)

const agentExecutionSnapshotSecretWriteAttempts = 3

// agentExecutionSnapshotSecretOptions selects controller-managed snapshot key
// bootstrap. When Name is empty the operator supplies the key through the
// mounted file and the controller never writes the Secret.
type agentExecutionSnapshotSecretOptions struct {
	Name string
	Key  string
}

func (o agentExecutionSnapshotSecretOptions) enabled() bool {
	return strings.TrimSpace(o.Name) != ""
}

func validateAgentExecutionSnapshotSecretOptions(o agentExecutionSnapshotSecretOptions) error {
	if !o.enabled() {
		return nil
	}
	if strings.TrimSpace(o.Key) == "" {
		return errors.New("--agent-execution-snapshot-secret requires --agent-execution-snapshot-secret-key")
	}
	return nil
}

// decodeAgentExecutionSnapshotKey accepts exactly 32 raw bytes or their
// whitespace-padded base64 encoding.
func decodeAgentExecutionSnapshotKey(raw []byte) ([]byte, error) {
	if len(raw) == sqlite.AgentExecutionSnapshotKeyBytes {
		return raw, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil || len(decoded) != sqlite.AgentExecutionSnapshotKeyBytes {
		return nil, fmt.Errorf("snapshot key must be %d raw bytes or their base64 encoding",
			sqlite.AgentExecutionSnapshotKeyBytes)
	}
	return decoded, nil
}

// ensureAgentExecutionSnapshotKey returns the snapshot key stored in the
// chart-created Secret, minting one on the first start. The chart never
// renders key material, so a fresh install finds an empty Secret; the
// controller writes 32 random bytes into it and every later start reuses
// them. When the SQLite store already exists but the Secret holds no key, the
// key was lost and minting a new one would orphan every retained snapshot, so
// startup fails closed instead. Concurrent writers are reconciled through the
// Secret's resourceVersion: whoever loses the update reuses the winner's key.
func ensureAgentExecutionSnapshotKey(
	ctx context.Context,
	reader client.Reader,
	writer client.Writer,
	namespace string,
	o agentExecutionSnapshotSecretOptions,
	storePreexisted bool,
) ([]byte, error) {
	if err := validateAgentExecutionSnapshotSecretOptions(o); err != nil {
		return nil, err
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, errors.New("agent execution snapshot key bootstrap requires the controller Pod namespace")
	}
	name := strings.TrimSpace(o.Name)
	item := strings.TrimSpace(o.Key)
	ref := types.NamespacedName{Namespace: namespace, Name: name}
	var lastErr error
	for range agentExecutionSnapshotSecretWriteAttempts {
		secret := &corev1.Secret{}
		if err := reader.Get(ctx, ref, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("snapshot key Secret %s does not exist; the Helm chart creates it, "+
					"or set controller.agentExecutionSnapshot.existingSecret to supply your own", ref)
			}
			return nil, fmt.Errorf("read snapshot key Secret %s: %w", ref, err)
		}
		if existing := secret.Data[item]; len(existing) > 0 {
			key, err := decodeAgentExecutionSnapshotKey(existing)
			if err != nil {
				return nil, fmt.Errorf("snapshot key Secret %s item %q: %w", ref, item, err)
			}
			return key, nil
		}
		if storePreexisted {
			return nil, fmt.Errorf("the SQLite store already exists but snapshot key Secret %s has no %q item; "+
				"restore the Secret from backup rather than minting a new key, which would make every retained "+
				"execution snapshot unreadable", ref, item)
		}
		key := make([]byte, sqlite.AgentExecutionSnapshotKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate snapshot key: %w", err)
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[item] = []byte(base64.StdEncoding.EncodeToString(key))
		if err := writer.Update(ctx, secret); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("store generated snapshot key in Secret %s: %w", ref, err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("store generated snapshot key in Secret %s: %w", ref, lastErr)
}
