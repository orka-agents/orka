/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"context"
	"strings"
	"time"
)

// AgentExecutionSnapshotSchemaVersion is the current immutable execution
// snapshot schema version.
const AgentExecutionSnapshotSchemaVersion int32 = 1

// AgentExecutionSnapshotKey identifies one immutable, content-addressed
// execution snapshot.
type AgentExecutionSnapshotKey struct {
	TaskUID string
	// Digest is the canonical sha256 digest of the plaintext snapshot body.
	Digest string
}

// ID returns the canonical snapshot identity <task-uid>/sha256:<digest>.
func (k AgentExecutionSnapshotKey) ID() string {
	return k.TaskUID + "/" + k.Digest
}

// Validate rejects incomplete snapshot keys.
func (k AgentExecutionSnapshotKey) Validate() error {
	if strings.TrimSpace(k.TaskUID) == "" {
		return ValidationErrorf("snapshot task UID is required")
	}
	if err := ValidateCanonicalDigest("snapshot digest", k.Digest); err != nil {
		return err
	}
	return nil
}

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

// AgentExecutionSnapshotStore persists immutable execution snapshots.
// Implementations must encrypt snapshot bodies at rest and fail closed when no
// encryption key is configured.
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

// AgentExecutionSnapshotMetadata is the non-secret lifecycle view of one
// encrypted snapshot. It intentionally excludes the nonce and ciphertext as
// well as the decrypted body.
type AgentExecutionSnapshotMetadata struct {
	Key           AgentExecutionSnapshotKey
	SchemaVersion int32
	CreatedAt     time.Time
}

// Validate rejects corrupt lifecycle metadata before retention code acts on
// it. Stored snapshot keys remain integrity references, not authorization.
func (m AgentExecutionSnapshotMetadata) Validate() error {
	if err := m.Key.Validate(); err != nil {
		return err
	}
	if m.SchemaVersion < 1 {
		return ValidationErrorf("snapshot metadata schema version must be positive")
	}
	if m.CreatedAt.IsZero() {
		return ValidationErrorf("snapshot metadata creation time is required")
	}
	return nil
}

// AgentExecutionSnapshotReferenceCounts reports every durable SQLite
// reference that can retain a snapshot. SessionTurns retain every snapshot
// scoped to their immutable Task UID because they do not store a snapshot
// digest themselves.
type AgentExecutionSnapshotReferenceCounts struct {
	PromptAttempts int64
	SessionTurns   int64
}

// Total returns the number of durable references across all known sources.
func (c AgentExecutionSnapshotReferenceCounts) Total() int64 {
	return c.PromptAttempts + c.SessionTurns
}

// AgentExecutionSnapshotLifecycleStore is an optional retention/GC extension
// to AgentExecutionSnapshotStore. It exposes metadata only, reports durable
// references, and deletes one exact key without changing the existing broad
// Task-UID deletion contract.
type AgentExecutionSnapshotLifecycleStore interface {
	// ListAgentExecutionSnapshotMetadataBefore returns snapshots created
	// strictly before cutoff, ordered by creation time, Task UID, and digest.
	ListAgentExecutionSnapshotMetadataBefore(ctx context.Context, cutoff time.Time) ([]AgentExecutionSnapshotMetadata, error)

	// CountAgentExecutionSnapshotReferences returns a consistent count across
	// prompt attempts and SessionTurns. Prompt attempts match Task UID and
	// digest; SessionTurns match the immutable Task UID because they do not carry
	// a snapshot digest.
	CountAgentExecutionSnapshotReferences(ctx context.Context, key AgentExecutionSnapshotKey) (AgentExecutionSnapshotReferenceCounts, error)

	// DeleteAgentExecutionSnapshot idempotently deletes one exact Task
	// UID/digest key. The caller must first prove that all references and the
	// configured retention interval have cleared.
	DeleteAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) error
}
