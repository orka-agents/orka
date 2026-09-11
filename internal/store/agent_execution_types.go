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

// SessionLineage durably records the execution protocol and runtime identity
// of one conversation Session. LineageGeneration is independent of the Session
// mutation-lease generation and any v2 RuntimeSession generation.
type SessionLineage struct {
	Namespace    string
	SessionName  string
	NamespaceUID string
	SessionUID   string
	// ContractVersion is orka.harness.v2.
	ContractVersion   string
	LineageGeneration int64
	// RuntimeIdentity is the built-in runtime type or the AgentRuntime UID.
	RuntimeIdentity string
	// ConfigDigest is the configuration/snapshot digest recorded when the
	// lineage was established.
	ConfigDigest string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Validate rejects an incomplete authoritative Session lineage.
func (l SessionLineage) Validate() error {
	claim := ClaimSessionLineageRequest{
		Namespace: l.Namespace, SessionName: l.SessionName, NamespaceUID: l.NamespaceUID,
		SessionUID: l.SessionUID, ContractVersion: l.ContractVersion,
		LineageGeneration: l.LineageGeneration, RuntimeIdentity: l.RuntimeIdentity, ConfigDigest: l.ConfigDigest,
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	if l.LineageGeneration < 1 {
		return ValidationErrorf("session lineage generation must be at least 1")
	}
	if l.Version < 1 {
		return ValidationErrorf("session lineage projection version must be at least 1")
	}
	if l.CreatedAt.IsZero() || l.UpdatedAt.IsZero() {
		return ValidationErrorf("session lineage creation and update times are required")
	}
	return nil
}

// ClaimSessionLineageRequest atomically establishes or verifies a Session
// lineage. Callers must invoke it under the same serialization that acquires
// the Session mutation lease so two concurrent first-use Tasks cannot
// establish different runtime identities.
type ClaimSessionLineageRequest struct {
	Namespace       string
	SessionName     string
	NamespaceUID    string
	SessionUID      string
	ContractVersion string
	// LineageGeneration is independent of mutation-lease and runtime-session
	// generations. Ordinary first use establishes generation 1.
	LineageGeneration int64
	RuntimeIdentity   string
	ConfigDigest      string

	// EstablishIfAbsent permits creating the lineage row. It must be true only
	// when the caller has proven the Session is fresh or is a controller-owned
	// Gateway Session whose first message was admitted atomically.
	EstablishIfAbsent bool
}

// Validate rejects incomplete lineage claims.
func (r ClaimSessionLineageRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.Namespace) == "":
		return ValidationErrorf("session lineage namespace is required")
	case strings.TrimSpace(r.SessionName) == "":
		return ValidationErrorf("session lineage session name is required")
	case strings.TrimSpace(r.NamespaceUID) == "":
		return ValidationErrorf("session lineage namespace UID is required")
	case strings.TrimSpace(r.SessionUID) == "":
		return ValidationErrorf("session lineage session UID is required")
	case r.ContractVersion != "orka.harness.v2":
		return ValidationErrorf("session lineage contract version %q must be orka.harness.v2", r.ContractVersion)
	case r.LineageGeneration < 1:
		return ValidationErrorf("session lineage generation must be at least 1")
	case strings.TrimSpace(r.RuntimeIdentity) == "":
		return ValidationErrorf("session lineage runtime identity is required")
	}
	if err := ValidateCanonicalDigest("session lineage config digest", r.ConfigDigest); err != nil {
		return err
	}
	return nil
}

// SessionLineageStore persists a payload projection of Kubernetes-authoritative
// Session protocol/runtime lineage. Projection failure may block dispatch but
// never changes or releases the Kubernetes lineage/Lease authority.
type SessionLineageStore interface {
	// ProjectSessionLineage idempotently stores the exact authoritative record.
	// Any mismatch returns ErrConflict and must be repaired explicitly; SQLite
	// never adjudicates which lineage owns a Session.
	ProjectSessionLineage(ctx context.Context, lineage SessionLineage) (*SessionLineage, error)

	// GetSessionLineage returns the lineage for one Session, or ErrNotFound.
	GetSessionLineage(ctx context.Context, namespace, sessionName string) (*SessionLineage, error)
}
