package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendPreparationGatesAdmissionUntilStartCompletes(t *testing.T) {
	f := newServiceFixture(t)
	release := make(chan struct{})
	var preparing atomic.Bool
	f.service.config.PrepareBackend = func(ctx context.Context) error {
		preparing.Store(true)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.ErrorIs(t, err, contract.ErrUnavailable)
	_, err = f.service.Get(t.Context(), "private", request.RequestID)
	require.ErrorIs(t, err, contract.ErrNotFound)
	f.start()
	require.Eventually(t, preparing.Load, time.Second, time.Millisecond)
	_, err = f.service.Submit(t.Context(), "private", "subject", request)
	require.ErrorIs(t, err, contract.ErrUnavailable)
	close(release)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, err := f.service.Submit(t.Context(), "private", "subject", request)
		require.NoError(c, err)
	}, 5*time.Second, time.Millisecond)
	status := f.awaitStatus(request.RequestID, func(s *contract.Status) bool { return contract.TerminalPhase(s.Phase) })
	require.Equal(t, contract.OutcomeReproduced, status.Outcome)
}

func TestBackendPreparationFailureDoesNotAdmitWork(t *testing.T) {
	f := newServiceFixture(t)
	failure := errors.New("storage fence unavailable")
	f.service.config.PrepareBackend = func(context.Context) error { return failure }
	require.ErrorIs(t, f.service.Start(t.Context()), failure)
	_, err := f.service.Submit(t.Context(), "private", "subject", f.request(contract.Validate))
	require.ErrorIs(t, err, contract.ErrUnavailable)
	require.Zero(t, f.backend.startCalls.Load())
}
