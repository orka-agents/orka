package v1

import (
	"context"
	"strings"
	"time"
)

// Protected records
//
// Execution snapshots, native session state and connector custody (see
// [SealedConnectorCustodyStore]) are sealed by Orka before a provider sees
// them. Orka owns the encryption key and the sealing format: it
// encrypts each record with AES-256-GCM under a fresh nonce and binds the
// record's identity into the associated data. A provider stores the nonce and
// ciphertext as opaque bytes, never holds the key and never receives the
// plaintext. It decides conditional commits only from the plain metadata that
// travels beside the sealed bytes, and Orka authenticates that metadata
// against the plaintext when it opens a record.

// MaxSealedRecordPageSize bounds the records one page of a sealed record
// listing returns.
const MaxSealedRecordPageSize = 100

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
	return ValidateCanonicalDigest("snapshot digest", k.Digest)
}

// SealedAgentExecutionSnapshot is an execution snapshot as a provider stores
// it. Nonce and Ciphertext are opaque to the provider.
type SealedAgentExecutionSnapshot struct {
	Key           AgentExecutionSnapshotKey
	SchemaVersion int32
	Nonce         []byte
	Ciphertext    []byte
	CreatedAt     time.Time
}

// Validate rejects an incomplete sealed snapshot.
func (s SealedAgentExecutionSnapshot) Validate() error {
	if err := s.Key.Validate(); err != nil {
		return err
	}
	if s.SchemaVersion < 1 {
		return ValidationErrorf("snapshot schema version must be positive")
	}
	if len(s.Nonce) == 0 || len(s.Ciphertext) == 0 {
		return ValidationErrorf("sealed snapshot requires a nonce and ciphertext")
	}
	if s.CreatedAt.IsZero() {
		return ValidationErrorf("snapshot creation time is required")
	}
	return nil
}

// SealedAgentExecutionSnapshotPage is one page of sealed snapshots.
type SealedAgentExecutionSnapshotPage struct {
	Snapshots []SealedAgentExecutionSnapshot
	// NextCursor continues the listing; it is empty after the last page.
	NextCursor string
}

// SealedAgentExecutionSnapshotStore persists sealed, immutable execution
// snapshots.
type SealedAgentExecutionSnapshotStore interface {
	// CreateSealedAgentExecutionSnapshot stores a new snapshot. When a
	// snapshot with the same key already exists it returns ErrConflict and
	// changes nothing; Orka then reads the stored snapshot to decide whether
	// the request repeats it.
	CreateSealedAgentExecutionSnapshot(ctx context.Context, snapshot SealedAgentExecutionSnapshot) error

	// GetSealedAgentExecutionSnapshot returns one sealed snapshot or
	// ErrNotFound.
	GetSealedAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) (*SealedAgentExecutionSnapshot, error)

	// ListSealedAgentExecutionSnapshots returns retained snapshots ordered by
	// Task UID and digest, at most limit (1 through MaxSealedRecordPageSize)
	// per page, so Orka can authenticate every retained record with its key
	// before it serves requests. The cursor is the NextCursor of the previous
	// page, or empty for the first page; an invalid cursor returns
	// ErrValidation. Records can be large, so a provider holds at most one
	// page of them at a time.
	ListSealedAgentExecutionSnapshots(ctx context.Context, cursor string, limit int) (*SealedAgentExecutionSnapshotPage, error)

	// DeleteAgentExecutionSnapshots removes every snapshot for a Task UID.
	// The caller is responsible for proving all binding, attempt, lineage,
	// finalizer and retention references are released first.
	DeleteAgentExecutionSnapshots(ctx context.Context, taskUID string) error
}

// AgentExecutionSnapshotMetadata is the non-secret lifecycle view of one
// sealed snapshot. It excludes the nonce and ciphertext.
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

// AgentExecutionSnapshotReferenceCounts reports every durable reference that
// can retain a snapshot. SessionTurns retain every snapshot scoped to their
// immutable Task UID because they do not store a snapshot digest themselves.
type AgentExecutionSnapshotReferenceCounts struct {
	PromptAttempts int64
	SessionTurns   int64
}

// Total returns the number of durable references across all known sources.
func (c AgentExecutionSnapshotReferenceCounts) Total() int64 {
	return c.PromptAttempts + c.SessionTurns
}

// AgentExecutionSnapshotLifecycleStore is the retention view of execution
// snapshots. It exposes metadata only, reports durable references, and
// deletes one exact key.
type AgentExecutionSnapshotLifecycleStore interface {
	// ListAgentExecutionSnapshotMetadataBefore returns snapshots created
	// strictly before cutoff, ordered by creation time, Task UID, and digest.
	ListAgentExecutionSnapshotMetadataBefore(ctx context.Context, cutoff time.Time) ([]AgentExecutionSnapshotMetadata, error)

	// CountAgentExecutionSnapshotReferences returns a consistent count across
	// prompt attempts and SessionTurns. Prompt attempts match Task UID and
	// digest; SessionTurns match the immutable Task UID because they do not
	// carry a snapshot digest.
	CountAgentExecutionSnapshotReferences(ctx context.Context, key AgentExecutionSnapshotKey) (AgentExecutionSnapshotReferenceCounts, error)

	// DeleteAgentExecutionSnapshot idempotently deletes one exact Task
	// UID/digest key. The caller must first prove that all references and the
	// configured retention interval have cleared.
	DeleteAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) error
}

// SealedNativeSession is a Session's private native continuity record as a
// provider stores it. The plain fields repeat facts from inside the sealed
// record so the provider can enforce ownership, transcript boundaries and
// monotonic history without opening it; Orka checks them against the
// plaintext when it opens the record.
type SealedNativeSession struct {
	Namespace   string
	SessionName string
	// SessionUID is the canonical Session owner, or empty for an import
	// that no Session has bound yet.
	SessionUID string
	// RecordDigest is the canonical digest of the sealed plaintext record.
	RecordDigest string
	// DataDigest is the canonical digest of the native bundle.
	DataDigest               string
	SourceOperationID        string
	RuntimeSessionGeneration int64
	// MessageCount and ThroughMessageID are the transcript boundary the
	// record covers: the Session's message count and the ID of its last
	// message when the record was sealed.
	MessageCount     int
	ThroughMessageID string
	Nonce            []byte
	Ciphertext       []byte
}

// Validate rejects an incomplete sealed native session. A staged import has
// no owner and no runtime generation.
func (s SealedNativeSession) Validate() error {
	for field, value := range map[string]string{
		"session namespace":   s.Namespace,
		"session name":        s.SessionName,
		"native operation ID": s.SourceOperationID,
	} {
		if err := ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if s.SessionUID == "" {
		if s.RuntimeSessionGeneration != 0 {
			return ValidationErrorf("staged native import must not carry runtime ownership")
		}
	} else {
		if err := ValidateControlIdentifier("session UID", s.SessionUID); err != nil {
			return err
		}
		if s.RuntimeSessionGeneration < 1 {
			return ValidationErrorf("native runtime generation must be positive")
		}
	}
	if err := ValidateCanonicalDigest("native record digest", s.RecordDigest); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("native data digest", s.DataDigest); err != nil {
		return err
	}
	if s.MessageCount < 0 || (s.MessageCount == 0) != (s.ThroughMessageID == "") {
		return ValidationErrorf("native snapshot transcript boundary is incomplete")
	}
	if len(s.Nonce) == 0 || len(s.Ciphertext) == 0 {
		return ValidationErrorf("sealed native session requires a nonce and ciphertext")
	}
	return nil
}

// SealedNativeSessionPage is one page of sealed native sessions.
type SealedNativeSessionPage struct {
	Sessions []SealedNativeSession
	// NextCursor continues the listing; it is empty after the last page.
	NextCursor string
}

// SaveSealedNativeSessionRequest records a native capture. RequestDigest
// identifies the capture for retries: Orka computes it from the plaintext
// record without its creation time. A repeated source operation with the
// same digest succeeds without a change; with another digest it returns
// ErrDuplicateMismatch.
type SaveSealedNativeSessionRequest struct {
	Record        SealedNativeSession
	RequestDigest string
}

// NativeSessionImportReceipt is the public outcome of staging a native
// session import.
type NativeSessionImportReceipt struct {
	Namespace         string    `json:"namespace"`
	SessionName       string    `json:"sessionName"`
	OperationID       string    `json:"operationID"`
	RequestDigest     string    `json:"requestDigest"`
	DataDigest        string    `json:"dataDigest"`
	ProviderSessionID string    `json:"providerSessionID"`
	CreatedAt         time.Time `json:"createdAt"`
}

// StageSealedNativeSessionImportRequest reserves a new Session and stages a
// sealed import for its first Task. Receipt is the receipt the provider
// records; its CreatedAt is also the new Session's creation time.
type StageSealedNativeSessionImportRequest struct {
	Record  SealedNativeSession
	Receipt NativeSessionImportReceipt
}

// SealedNativeSessionStore persists sealed native continuity state
// independently of public Task artifacts and Session projections.
type SealedNativeSessionStore interface {
	// GetSealedNativeSession returns the Session's native record when
	// sessionUID is its canonical owner and the record covers the Session's
	// current transcript. It returns ErrNotFound when there is no record,
	// ErrConflict for another owner, a deleted name or a stale boundary,
	// and ErrGatewayOwnedSession for a gateway Session. An empty sessionUID
	// reads only an import that no Session has bound yet.
	GetSealedNativeSession(ctx context.Context, namespace, sessionName, sessionUID string) (*SealedNativeSession, error)

	// SaveSealedNativeSession records a capture for the record's canonical
	// owner. The record must cover the Session's current transcript and
	// advance its runtime generation or transcript boundary over the stored
	// record; otherwise it returns ErrConflict.
	SaveSealedNativeSession(ctx context.Context, request SaveSealedNativeSessionRequest) error

	// StageSealedNativeSessionImport creates the Session named by the
	// record and stores the record with the receipt in one transaction. A
	// repeated operation returns the stored receipt when its request digest
	// matches and ErrDuplicateMismatch otherwise. A name that exists, is being
	// cleaned up or was deleted with native state returns ErrConflict.
	StageSealedNativeSessionImport(ctx context.Context, request StageSealedNativeSessionImportRequest) (*NativeSessionImportReceipt, error)

	// ListSealedNativeSessions returns every retained native record ordered by
	// namespace and Session name, paged like
	// ListSealedAgentExecutionSnapshots.
	ListSealedNativeSessions(ctx context.Context, cursor string, limit int) (*SealedNativeSessionPage, error)
}

// CreateSessionTurnRecordRequest persists an open turn after Kubernetes has
// validated the exact SessionControl mutation lease and PromptAttempt binding.
type CreateSessionTurnRecordRequest struct {
	Turn        SessionTurn          `json:"turn"`
	Namespace   string               `json:"namespace"`
	SessionName string               `json:"sessionName"`
	Fence       ControllerEpochFence `json:"fence"`
}

// ActivateSessionTurnProjectionRequest releases the deferred outbox record
// only after Kubernetes SessionControl/BranchClaim finalization has committed.
type ActivateSessionTurnProjectionRequest struct {
	TurnID                 string               `json:"turnId"`
	ProjectionID           string               `json:"projectionId"`
	Fence                  ControllerEpochFence `json:"fence"`
	FinalizationDigest     string               `json:"finalizationDigest"`
	ExpectedAggregateKind  string               `json:"expectedAggregateKind"`
	ExpectedProjectionKind string               `json:"expectedProjectionKind"`
	ExpectedPayloadDigest  string               `json:"expectedPayloadDigest"`
	AvailableAt            time.Time            `json:"availableAt"`
	UpdatedAt              time.Time            `json:"updatedAt"`
}

// SessionTurnFinalizationBasis is what Orka reads before it seals a turn's
// native state: the turn, the Session's transcript boundary and the stored
// native record, all from one snapshot.
type SessionTurnFinalizationBasis struct {
	Turn SessionTurn
	// MessageCount and LastMessageID are the Session's transcript length and
	// the ID of its last message. LastMessageID is empty when MessageCount is
	// zero, and for a last message stored before messages carried IDs.
	MessageCount  int
	LastMessageID string
	// NativeSession is the stored native record, whatever transcript boundary
	// it covers, or nil.
	NativeSession *SealedNativeSession
}

// SealedNativeSessionFinalization is the native state a turn finalization
// commits. Orka sealed Record against the transcript boundary the
// finalization produces, after reading the Session at the expected boundary.
type SealedNativeSessionFinalization struct {
	Record SealedNativeSession
	// Carried marks Record as the Session's stored checkpoint carried across
	// a non-success outcome marker with only its transcript boundary
	// advanced, rather than a new capture.
	Carried bool
	// RequestDigest identifies a capture as in SaveSealedNativeSessionRequest.
	// It is empty for a carried checkpoint.
	RequestDigest string
	// ExpectedMessageCount and ExpectedLastMessageID are the transcript
	// boundary Orka read before sealing. The provider commits only if the
	// Session's transcript still has that boundary before the finalization
	// appends its messages.
	ExpectedMessageCount  int
	ExpectedLastMessageID string
	// ExpectedRecordDigest is the RecordDigest of the stored record a carried
	// checkpoint replaces. The provider commits only if it is unchanged.
	ExpectedRecordDigest string
}

// SealedSessionTurnFinalization atomically commits a turn's terminal
// transcript messages, its optional sealed native state, the finalized
// SessionTurn receipt and a deferred terminal outbox projection. Kubernetes
// control records are updated by Orka after this transaction commits.
type SealedSessionTurnFinalization struct {
	Key                  SessionTurnKey          `json:"key"`
	Namespace            string                  `json:"namespace"`
	SessionName          string                  `json:"sessionName"`
	Fence                ControllerEpochFence    `json:"fence"`
	ExpectedTurnVersion  int64                   `json:"expectedTurnVersion"`
	FinalizationDigest   string                  `json:"finalizationDigest"`
	TerminalKind         SessionTurnTerminalKind `json:"terminalKind"`
	TerminalContent      string                  `json:"terminalContent"`
	SkipTranscriptAppend bool                    `json:"skipTranscriptAppend,omitempty"`
	SkipUserPromptAppend bool                    `json:"skipUserPromptAppend,omitempty"`
	PublicationID        string                  `json:"publicationId,omitempty"`
	PublicationReceipt   *PublicationReceipt     `json:"publicationReceipt,omitempty"`
	Projection           OutboxProjection        `json:"projection"`
	FinalizedAt          time.Time               `json:"finalizedAt"`
	// UserMessageID and TerminalMessageID are the IDs Orka assigned to the
	// transcript messages the finalization appends. Each is required exactly
	// when its message is appended.
	UserMessageID     string `json:"userMessageId,omitempty"`
	TerminalMessageID string `json:"terminalMessageId,omitempty"`
	// NativeSessionDigest identifies the native state the finalization
	// binds, and is recorded on the turn. It is empty without native state.
	NativeSessionDigest string `json:"nativeSessionDigest,omitempty"`
	// NativeSession is the sealed native state to commit. Orka omits it when
	// the turn was already finalized, so a repeated finalization only
	// compares the recorded receipt.
	NativeSession *SealedNativeSessionFinalization `json:"-"`
	// InputTokens and OutputTokens are the usage Orka attributes to this
	// turn's prompt attempt. The first finalization adds them to the
	// Session's token counters; a repeated finalization adds nothing.
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
}

// SessionTurnRecordStore persists SessionTurn records, their terminal
// transcript additions and deferred outbox projections. It never reads or
// changes Kubernetes-authoritative control records.
type SessionTurnRecordStore interface {
	// CreateSessionTurnRecord persists an open turn. A repeated create with
	// the same immutable input returns the stored turn.
	CreateSessionTurnRecord(ctx context.Context, request CreateSessionTurnRecordRequest) (*SessionTurn, error)

	// GetSessionTurn returns one turn or ErrNotFound.
	GetSessionTurn(ctx context.Context, turnID string) (*SessionTurn, error)

	// ReadSessionTurnFinalizationBasis returns the turn with the Session's
	// transcript boundary and stored native record.
	ReadSessionTurnFinalizationBasis(ctx context.Context, turnID, namespace, sessionName string) (*SessionTurnFinalizationBasis, error)

	// CommitSealedSessionTurnFinalization commits a finalization. A repeated
	// finalization with the same terminal data returns the finalized turn;
	// different data returns ErrConflict.
	CommitSealedSessionTurnFinalization(ctx context.Context, finalization SealedSessionTurnFinalization) (*SessionTurn, error)

	// ActivateSessionTurnProjection makes the deferred terminal projection
	// claimable after Kubernetes finalization has committed.
	ActivateSessionTurnProjection(ctx context.Context, request ActivateSessionTurnProjectionRequest) (*OutboxProjection, error)
}
