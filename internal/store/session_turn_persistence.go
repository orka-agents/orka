package store

import (
	"context"
	"time"
)

type SessionTurnPersistenceStore interface {
	CreateSessionTurnRecord(context.Context, CreateSessionTurnRecordRequest) (*SessionTurn, error)
	GetSessionTurn(context.Context, string) (*SessionTurn, error)
	CommitSessionTurnFinalization(context.Context, CommitSessionTurnFinalizationRequest) (*SessionTurn, error)
	ActivateSessionTurnProjection(context.Context, ActivateSessionTurnProjectionRequest) (*OutboxProjection, error)
}

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
	InputTokens          int64                   `json:"inputTokens,omitempty"`
	OutputTokens         int64                   `json:"outputTokens,omitempty"`
}
