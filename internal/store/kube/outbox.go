package kube

import (
	"context"

	"github.com/orka-agents/orka/internal/store"
)

// EnqueueOutboxProjection validates the Kubernetes controller epoch and then
// persists the SQLite-owned outbox record without consulting SQLite controls.
func (s *Store) EnqueueOutboxProjection(ctx context.Context, projection *store.OutboxProjection, fence store.ControllerEpochFence) (*store.OutboxProjection, error) {
	if s.outbox == nil {
		return nil, ErrOutboxStoreNotConfigured
	}
	normalized, snapshot, err := s.requireControllerEpoch(ctx, fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	return s.outbox.EnqueueOutboxProjectionRecord(ctx, projection, normalized)
}

// GetOutboxProjection reads the SQLite-owned outbox record.
func (s *Store) GetOutboxProjection(ctx context.Context, id string) (*store.OutboxProjection, error) {
	if s.outbox == nil {
		return nil, ErrOutboxStoreNotConfigured
	}
	return s.outbox.GetOutboxProjection(ctx, id)
}

// ClaimOutboxProjections validates the Kubernetes controller epoch before the
// SQLite adapter atomically claims due records.
func (s *Store) ClaimOutboxProjections(ctx context.Context, request store.ClaimOutboxProjectionsRequest) ([]store.OutboxProjection, error) {
	if s.outbox == nil {
		return nil, ErrOutboxStoreNotConfigured
	}
	normalized, snapshot, err := s.requireControllerEpoch(ctx, request.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	request.Fence = normalized
	return s.outbox.ClaimOutboxProjectionRecords(ctx, request)
}

// CompleteOutboxProjection validates the Kubernetes controller epoch before
// the SQLite adapter completes, retries, or dead-letters the exact claim.
func (s *Store) CompleteOutboxProjection(ctx context.Context, request store.CompleteOutboxProjectionRequest) (*store.OutboxProjection, error) {
	if s.outbox == nil {
		return nil, ErrOutboxStoreNotConfigured
	}
	normalized, snapshot, err := s.requireControllerEpoch(ctx, request.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	request.Fence = normalized
	return s.outbox.CompleteOutboxProjectionRecord(ctx, request)
}
