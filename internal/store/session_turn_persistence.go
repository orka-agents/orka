package store

import (
	"context"
	"time"
)

// SessionTurnPersistenceStore is the SQLite-only half of SessionControlStore.
// Implementations persist turn rows, transcript entries, and deferred outbox
// records, but must not read or mutate Kubernetes-authoritative control rows.
type SessionTurnPersistenceStore interface {
	CreateSessionTurnRecord(context.Context, CreateSessionTurnRecordRequest) (*SessionTurn, error)
	GetSessionTurn(context.Context, string) (*SessionTurn, error)
	CommitSessionTurnFinalization(context.Context, CommitSessionTurnFinalizationRequest) (*SessionTurn, error)
	ActivateSessionTurnProjection(context.Context, ActivateSessionTurnProjectionRequest) (*OutboxProjection, error)
}

// CreateSessionTurnRecordRequest persists an open turn after Kubernetes has
// validated the exact SessionControl mutation lease and PromptAttempt binding.
type CreateSessionTurnRecordRequest struct {
	Turn        SessionTurn          `json:"turn"`
	Namespace   string               `json:"namespace"`
	SessionName string               `json:"sessionName"`
	Fence       ControllerEpochFence `json:"fence"`
}

// CommitSessionTurnFinalizationRequest atomically commits only SQLite-owned
// data: optional transcript messages, the finalized SessionTurn receipt, and a
// deferred terminal outbox projection. Kubernetes control records are updated
// by the coordinating Kubernetes store after this transaction commits.
type CommitSessionTurnFinalizationRequest struct {
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
