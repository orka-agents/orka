/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"context"
	"time"
)

// AgentExecutionSnapshotSchemaVersion is the current immutable execution
// snapshot schema version.
const AgentExecutionSnapshotSchemaVersion int32 = 1

// AgentExecutionSnapshot is the immutable non-secret executable input record
// for one Task binding. The body is sensitive (resolved prompts, Skill
// content, repository identities, endpoint metadata, policy configuration)
// even though raw credentials and TxTokens are prohibited; it is encrypted at
// rest and must never enter logs, events, metrics, or ordinary API output.
type AgentExecutionSnapshot struct {
	TaskUID       string
	Digest        string
	SchemaVersion int32
	// Body is the canonical JSON plaintext. Digest must equal
	// CanonicalAgentExecutionSnapshotDigest(Body).
	Body      []byte
	CreatedAt time.Time
}

// CanonicalAgentExecutionSnapshotDigest returns the canonical digest of a
// plaintext snapshot body.
func CanonicalAgentExecutionSnapshotDigest(body []byte) string {
	return CanonicalBytesDigest(body)
}

// AgentExecutionSnapshotStore persists immutable execution snapshots. The
// storage client implements it: it seals snapshot bodies with Orka's key
// before a provider stores them and fails closed when no key is active.
type AgentExecutionSnapshotStore interface {
	// PersistAgentExecutionSnapshot idempotently stores an immutable snapshot
	// keyed by Task UID and digest. An existing identical snapshot succeeds; an
	// existing snapshot with the same key but different content returns
	// ErrDuplicateMismatch. The snapshot digest must match the body.
	PersistAgentExecutionSnapshot(ctx context.Context, snapshot AgentExecutionSnapshot) error

	// GetAgentExecutionSnapshot decrypts and returns one snapshot, verifying
	// body integrity against the stored digest.
	GetAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) (*AgentExecutionSnapshot, error)

	// DeleteAgentExecutionSnapshots removes every snapshot for a Task UID. The
	// caller is responsible for proving all binding, attempt, lineage,
	// finalizer, and retention references are released first.
	DeleteAgentExecutionSnapshots(ctx context.Context, taskUID string) error
}
