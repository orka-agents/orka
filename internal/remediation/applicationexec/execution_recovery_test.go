package applicationexec

import (
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
)

func TestRecoverPersistedRootReceiptWithoutNewExecution(t *testing.T) {
	f := newFixture(t)
	f.backend.qualified.Store(true)
	var accepted contract.ExecutionReceipt
	_, err := f.backend.Start(t.Context(), f.request, func(receipt contract.ExecutionReceipt) error {
		accepted = receipt
		return errors.New("coordinator receipt persistence failed")
	})
	require.Error(t, err)
	require.Empty(t, f.created)
	restarted, err := New(f.control, f.control, f.target, f.target, f.backend.targetKube, f.store, f.scheme, f.config)
	require.NoError(t, err)
	restarted.qualified.Store(true)
	recovered, err := restarted.Recover(t.Context(), f.request)
	require.NoError(t, err)
	require.Equal(t, &accepted, recovered.Receipt)
	require.False(t, recovered.Settled)
	require.Empty(t, f.created)
	changed := f.request
	changed.Problem += " changed"
	_, err = restarted.Recover(t.Context(), changed)
	require.Error(t, err)
	require.NoError(t, restarted.Cancel(t.Context(), accepted))
	require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
}

func TestRecoverRootReservationSettlesOnlyProvenNoDispatch(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "never dispatched", true: "unknown create"}[attempted], func(t *testing.T) {
			f := newFixture(t)
			r := reservedRun(t, f)
			r.state.Task.Attempted = attempted
			require.NoError(t, f.backend.save(t.Context(), r))
			recovered, err := f.backend.Recover(t.Context(), f.request)
			if attempted {
				require.ErrorIs(t, err, ErrUncertain)
				require.False(t, recovered.Settled)
				require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
			} else {
				require.NoError(t, err)
				require.True(t, recovered.Settled)
				require.NoError(t, EnsureNoActive(t.Context(), f.control, "control"))
				require.ErrorIs(t, f.backend.ensureTask(t.Context(), r, ""), contract.ErrConflict)
			}
			require.Nil(t, recovered.Receipt)
			require.Empty(t, f.created)
		})
	}
}
