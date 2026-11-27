package kube

import (
	"context"

	"github.com/orka-agents/orka/internal/store"
)

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

func (s *Store) GetOutboxProjection(ctx context.Context, id string) (*store.OutboxProjection, error) {
	if s.outbox == nil {
		return nil, ErrOutboxStoreNotConfigured
	}
	return s.outbox.GetOutboxProjection(ctx, id)
}

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
