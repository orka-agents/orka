package executor

import (
	"context"
	"errors"
	"io"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestTransientPreparationObservationResumesExactWorkload(t *testing.T) {
	for _, failure := range []error{apierrors.NewInternalError(errors.New("synthetic outage")), io.ErrUnexpectedEOF} {
		f := newFixture(t, true)
		read := f.backend.readReport
		reads := 0
		f.backend.readReport = func(ctx context.Context, namespace, name string) (pv.PreparationReport, error) {
			reads++
			if reads == 1 {
				return pv.PreparationReport{}, failure
			}
			return read(ctx, namespace, name)
		}
		var receipt contract.ExecutionReceipt
		record, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error { receipt = r; return nil })
		require.Error(t, err)
		require.Nil(t, record)
		var retryable interface{ Retryable() bool }
		require.ErrorAs(t, err, &retryable)
		require.True(t, retryable.Retryable())
		var noEffects interface{ NoEffects() bool }
		require.False(t, errors.As(err, &noEffects))
		record, err = f.backend.Resume(t.Context(), f.request, receipt)
		require.NoError(t, err)
		require.Equal(t, pv.Reproduced, record.Assessment.Conclusion)
		require.Len(t, f.jobs, 1)
		require.Equal(t, receipt.TaskUID, string(f.jobs[0].OwnerReferences[0].UID))
	}
}
