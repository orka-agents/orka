package storageclient

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

func snapshotAdditionalData(taskUID, digest string, schemaVersion int32) []byte {
	return fmt.Appendf(nil, "orka.agent-execution-snapshot\x00%s\x00%s\x00%d", taskUID, digest, schemaVersion)
}

func openSnapshotBody(custody *keyCustody, sealed storagev1.SealedAgentExecutionSnapshot) ([]byte, error) {
	return custody.open(sealed.Nonce, sealed.Ciphertext,
		snapshotAdditionalData(sealed.Key.TaskUID, sealed.Key.Digest, sealed.SchemaVersion))
}

func (c *Client) PersistAgentExecutionSnapshot(ctx context.Context, snapshot store.AgentExecutionSnapshot) error {
	custody, err := c.activeCustody()
	if err != nil {
		return err
	}
	key := store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest}
	if err := key.Validate(); err != nil {
		return err
	}
	if snapshot.SchemaVersion < 1 {
		return store.ValidationErrorf("snapshot schema version must be positive")
	}
	if len(snapshot.Body) == 0 {
		return store.ValidationErrorf("snapshot body is required")
	}
	if computed := store.CanonicalAgentExecutionSnapshotDigest(snapshot.Body); computed != snapshot.Digest {
		return store.ValidationErrorf("snapshot digest %s does not match body digest %s", snapshot.Digest, computed)
	}
	if done, err := c.matchStoredSnapshot(ctx, snapshot); done || err != nil {
		return err
	}
	nonce, ciphertext, err := custody.seal(snapshotAdditionalData(key.TaskUID, key.Digest, snapshot.SchemaVersion), snapshot.Body)
	if err != nil {
		return fmt.Errorf("seal agent execution snapshot: %w", err)
	}
	createdAt := snapshot.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	err = c.CreateSealedAgentExecutionSnapshot(ctx, storagev1.SealedAgentExecutionSnapshot{
		Key: key, SchemaVersion: snapshot.SchemaVersion, Nonce: nonce, Ciphertext: ciphertext, CreatedAt: createdAt.UTC(),
	})
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	if done, err := c.matchStoredSnapshot(ctx, snapshot); done || err != nil {
		return err
	}
	return fmt.Errorf("persist agent execution snapshot %s: %w", key.ID(), store.ErrConflict)
}

func (c *Client) matchStoredSnapshot(ctx context.Context, snapshot store.AgentExecutionSnapshot) (bool, error) {
	key := store.AgentExecutionSnapshotKey{TaskUID: snapshot.TaskUID, Digest: snapshot.Digest}
	existing, err := c.GetAgentExecutionSnapshot(ctx, key)
	switch {
	case err == nil:
		if existing.SchemaVersion == snapshot.SchemaVersion && subtle.ConstantTimeCompare(existing.Body, snapshot.Body) == 1 {
			return true, nil
		}
		return true, fmt.Errorf("%w: agent execution snapshot %s already exists with different content", store.ErrDuplicateMismatch, key.ID())
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return true, err
	}
}

func (c *Client) GetAgentExecutionSnapshot(ctx context.Context, key store.AgentExecutionSnapshotKey) (*store.AgentExecutionSnapshot, error) {
	custody, err := c.activeCustody()
	if err != nil {
		return nil, err
	}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	sealed, err := c.GetSealedAgentExecutionSnapshot(ctx, key)
	if err != nil {
		return nil, err
	}
	if sealed == nil || sealed.Key != key {
		return nil, fmt.Errorf("agent execution snapshot provider returned another record for %s", key.ID())
	}
	body, err := openSnapshotBody(custody, *sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypt agent execution snapshot %s/%s: %w", key.TaskUID, key.Digest, err)
	}
	if computed := store.CanonicalAgentExecutionSnapshotDigest(body); computed != key.Digest {
		return nil, fmt.Errorf("agent execution snapshot %s failed integrity verification", key.ID())
	}
	return &store.AgentExecutionSnapshot{
		TaskUID:       key.TaskUID,
		Digest:        key.Digest,
		SchemaVersion: sealed.SchemaVersion,
		Body:          body,
		CreatedAt:     sealed.CreatedAt.UTC(),
	}, nil
}
