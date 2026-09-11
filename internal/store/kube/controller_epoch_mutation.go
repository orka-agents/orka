package kube

import (
	"context"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// WithControllerEpochMutation uses the same bounded Lease interlock as
// control-record CAS. It releases the interlock when the client callback returns,
// including on ambiguous errors. This does not settle an in-flight server write;
// the callback's target preconditions and ownership checks remain authoritative.
func (s *Store) WithControllerEpochMutation(ctx context.Context, fence store.ControllerEpochFence, mutate func(context.Context) error) error {
	if err := s.requireClient(); err != nil {
		return err
	}
	if mutate == nil {
		return store.ValidationErrorf("controller epoch mutation callback is required")
	}
	_, snapshot, err := s.requireControllerEpoch(ctx, fence)
	if err != nil {
		return err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	mutationCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return mutate(mutationCtx)
}
