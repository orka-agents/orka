package v1

import (
	"context"
	"strings"
	"time"
)

const MaxSealedRecordPageSize = 100

type AgentExecutionSnapshotKey struct {
	TaskUID string
	Digest  string
}

func (k AgentExecutionSnapshotKey) ID() string {
	return k.TaskUID + "/" + k.Digest
}

func (k AgentExecutionSnapshotKey) Validate() error {
	if strings.TrimSpace(k.TaskUID) == "" {
		return ValidationErrorf("snapshot task UID is required")
	}
	return ValidateCanonicalDigest("snapshot digest", k.Digest)
}

type SealedAgentExecutionSnapshot struct {
	Key           AgentExecutionSnapshotKey
	SchemaVersion int32
	Nonce         []byte
	Ciphertext    []byte
	CreatedAt     time.Time
}

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

type SealedAgentExecutionSnapshotPage struct {
	Snapshots  []SealedAgentExecutionSnapshot
	NextCursor string
}

type SealedAgentExecutionSnapshotStore interface {
	CreateSealedAgentExecutionSnapshot(ctx context.Context, snapshot SealedAgentExecutionSnapshot) error

	GetSealedAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) (*SealedAgentExecutionSnapshot, error)

	ListSealedAgentExecutionSnapshots(ctx context.Context, cursor string, limit int) (*SealedAgentExecutionSnapshotPage, error)

	DeleteAgentExecutionSnapshots(ctx context.Context, taskUID string) error
}

type AgentExecutionSnapshotMetadata struct {
	Key           AgentExecutionSnapshotKey
	SchemaVersion int32
	CreatedAt     time.Time
}

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

type AgentExecutionSnapshotReferenceCounts struct {
	PromptAttempts int64
	SessionTurns   int64
}

func (c AgentExecutionSnapshotReferenceCounts) Total() int64 {
	return c.PromptAttempts + c.SessionTurns
}

type AgentExecutionSnapshotLifecycleStore interface {
	ListAgentExecutionSnapshotMetadataBefore(ctx context.Context, cutoff time.Time) ([]AgentExecutionSnapshotMetadata, error)

	CountAgentExecutionSnapshotReferences(ctx context.Context, key AgentExecutionSnapshotKey) (AgentExecutionSnapshotReferenceCounts, error)

	DeleteAgentExecutionSnapshot(ctx context.Context, key AgentExecutionSnapshotKey) error
}

type SealedNativeSession struct {
	Namespace                string
	SessionName              string
	SessionUID               string
	RecordDigest             string
	DataDigest               string
	SourceOperationID        string
	RuntimeSessionGeneration int64
	MessageCount             int
	ThroughMessageID         string
	Nonce                    []byte
	Ciphertext               []byte
}

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

type SealedNativeSessionPage struct {
	Sessions   []SealedNativeSession
	NextCursor string
}

type SaveSealedNativeSessionRequest struct {
	Record        SealedNativeSession
	RequestDigest string
}

type NativeSessionImportReceipt struct {
	Namespace         string    `json:"namespace"`
	SessionName       string    `json:"sessionName"`
	OperationID       string    `json:"operationID"`
	RequestDigest     string    `json:"requestDigest"`
	DataDigest        string    `json:"dataDigest"`
	ProviderSessionID string    `json:"providerSessionID"`
	CreatedAt         time.Time `json:"createdAt"`
}

type StageSealedNativeSessionImportRequest struct {
	Record  SealedNativeSession
	Receipt NativeSessionImportReceipt
}

type SealedNativeSessionStore interface {
	GetSealedNativeSession(ctx context.Context, namespace, sessionName, sessionUID string) (*SealedNativeSession, error)

	SaveSealedNativeSession(ctx context.Context, request SaveSealedNativeSessionRequest) error

	StageSealedNativeSessionImport(ctx context.Context, request StageSealedNativeSessionImportRequest) (*NativeSessionImportReceipt, error)

	ListSealedNativeSessions(ctx context.Context, cursor string, limit int) (*SealedNativeSessionPage, error)
}

type CreateSessionTurnRecordRequest struct {
	Turn        SessionTurn          `json:"turn"`
	Namespace   string               `json:"namespace"`
	SessionName string               `json:"sessionName"`
	Fence       ControllerEpochFence `json:"fence"`
}

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

type SessionTurnFinalizationBasis struct {
	Turn          SessionTurn
	MessageCount  int
	LastMessageID string
	NativeSession *SealedNativeSession
}

type SealedNativeSessionFinalization struct {
	Record                SealedNativeSession
	Carried               bool
	RequestDigest         string
	ExpectedMessageCount  int
	ExpectedLastMessageID string
	ExpectedRecordDigest  string
}

type SealedSessionTurnFinalization struct {
	Key                  SessionTurnKey                   `json:"key"`
	Namespace            string                           `json:"namespace"`
	SessionName          string                           `json:"sessionName"`
	Fence                ControllerEpochFence             `json:"fence"`
	ExpectedTurnVersion  int64                            `json:"expectedTurnVersion"`
	FinalizationDigest   string                           `json:"finalizationDigest"`
	TerminalKind         SessionTurnTerminalKind          `json:"terminalKind"`
	TerminalContent      string                           `json:"terminalContent"`
	SkipTranscriptAppend bool                             `json:"skipTranscriptAppend,omitempty"`
	SkipUserPromptAppend bool                             `json:"skipUserPromptAppend,omitempty"`
	PublicationID        string                           `json:"publicationId,omitempty"`
	PublicationReceipt   *PublicationReceipt              `json:"publicationReceipt,omitempty"`
	Projection           OutboxProjection                 `json:"projection"`
	FinalizedAt          time.Time                        `json:"finalizedAt"`
	UserMessageID        string                           `json:"userMessageId,omitempty"`
	TerminalMessageID    string                           `json:"terminalMessageId,omitempty"`
	NativeSessionDigest  string                           `json:"nativeSessionDigest,omitempty"`
	NativeSession        *SealedNativeSessionFinalization `json:"-"`
	InputTokens          int64                            `json:"inputTokens,omitempty"`
	OutputTokens         int64                            `json:"outputTokens,omitempty"`
}

type SessionTurnRecordStore interface {
	CreateSessionTurnRecord(ctx context.Context, request CreateSessionTurnRecordRequest) (*SessionTurn, error)

	GetSessionTurn(ctx context.Context, turnID string) (*SessionTurn, error)

	ReadSessionTurnFinalizationBasis(ctx context.Context, turnID, namespace, sessionName string) (*SessionTurnFinalizationBasis, error)

	CommitSealedSessionTurnFinalization(ctx context.Context, finalization SealedSessionTurnFinalization) (*SessionTurn, error)

	ActivateSessionTurnProjection(ctx context.Context, request ActivateSessionTurnProjectionRequest) (*OutboxProjection, error)
}
