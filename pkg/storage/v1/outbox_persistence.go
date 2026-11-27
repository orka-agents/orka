package v1

import (
	"context"
)

type OutboxPersistenceStore interface {
	EnqueueOutboxProjectionRecord(context.Context, *OutboxProjection, ControllerEpochFence) (*OutboxProjection, error)
	GetOutboxProjection(context.Context, string) (*OutboxProjection, error)
	ClaimOutboxProjectionRecords(context.Context, ClaimOutboxProjectionsRequest) ([]OutboxProjection, error)
	CompleteOutboxProjectionRecord(context.Context, CompleteOutboxProjectionRequest) (*OutboxProjection, error)
}
