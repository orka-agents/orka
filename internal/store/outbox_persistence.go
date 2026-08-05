package store

import "context"

// OutboxPersistenceStore is the SQLite-only outbox adapter used behind a
// Kubernetes epoch fence. Its methods persist records but do not treat a
// SQLite controller_epochs row as authoritative.
type OutboxPersistenceStore interface {
	EnqueueOutboxProjectionRecord(context.Context, *OutboxProjection, ControllerEpochFence) (*OutboxProjection, error)
	GetOutboxProjection(context.Context, string) (*OutboxProjection, error)
	ClaimOutboxProjectionRecords(context.Context, ClaimOutboxProjectionsRequest) ([]OutboxProjection, error)
	CompleteOutboxProjectionRecord(context.Context, CompleteOutboxProjectionRequest) (*OutboxProjection, error)
}
