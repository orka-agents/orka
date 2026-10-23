package store

import (
	"context"
	"time"
)

// SessionTurnPersistenceStore is the database half of SessionControlStore.
// The storage client implements it over a provider's SessionTurnRecordStore:
// it persists turn rows, transcript entries, native state and deferred outbox
// records, but never reads or mutates Kubernetes-authoritative control rows.
type SessionTurnPersistenceStore interface {
	CreateSessionTurnRecord(context.Context, CreateSessionTurnRecordRequest) (*SessionTurn, error)
	GetSessionTurn(context.Context, string) (*SessionTurn, error)
	CommitSessionTurnFinalization(context.Context, CommitSessionTurnFinalizationRequest) (*SessionTurn, error)
	ActivateSessionTurnProjection(context.Context, ActivateSessionTurnProjectionRequest) (*OutboxProjection, error)
}

// CommitSessionTurnFinalizationRequest atomically commits only database-owned
// data: optional transcript messages and native state, the finalized
// SessionTurn receipt, and a deferred terminal outbox projection. Kubernetes
// control records are updated by the coordinating Kubernetes store after this
// transaction commits. NativeSession is plaintext; the storage client seals it
// before a provider sees it.
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
	NativeSession        *NativeSessionRecord    `json:"-"`
	NativeSessionCarried bool                    `json:"-"`
	// InputTokens and OutputTokens are the usage Orka attributes to this
	// turn's prompt attempt. The first finalization adds them to the
	// Session's token counters; a repeated finalization adds nothing.
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
}
