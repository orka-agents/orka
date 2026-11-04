package v1

import (
	"context"
)

// OutboxPersistenceStore is the SQLite-only outbox adapter used behind a
// Kubernetes epoch fence. Its methods persist records but do not treat a
// SQLite controller_epochs row as authoritative.
type OutboxPersistenceStore interface {
	EnqueueOutboxProjectionRecord(ctx context.Context, projection *OutboxProjection, fence ControllerEpochFence) (*OutboxProjection, error)
	GetOutboxProjection(ctx context.Context, id string) (*OutboxProjection, error)
	ClaimOutboxProjectionRecords(ctx context.Context, request ClaimOutboxProjectionsRequest) ([]OutboxProjection, error)
	CompleteOutboxProjectionRecord(ctx context.Context, request CompleteOutboxProjectionRequest) (*OutboxProjection, error)
}
