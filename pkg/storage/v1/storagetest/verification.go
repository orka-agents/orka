package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	pv "github.com/orka-agents/orka/pkg/storage/v1/verification"
	"github.com/stretchr/testify/require"
)

func runVerification(t *testing.T, newProvider StateFactory) {
	t.Run("private-records-and-conflicting-evidence", func(t *testing.T) {
		p := newProvider(t)
		createVerificationRun(t, p)
		f := catalogVerification()
		require.NoError(t, p.RecordPatchVerificationEvidence(t.Context(), f.binding, f.evidence[0]))
		conflicting := f.evidence[0]
		conflicting.Observation.StdoutDigest = storagev1.CanonicalBytesDigest([]byte("conflicting output"))
		require.ErrorIs(t, p.RecordPatchVerificationEvidence(t.Context(), f.binding, conflicting), pv.ErrConflict)
		record, err := p.GetPatchVerificationRun(t.Context(), f.binding.RunID)
		require.NoError(t, err)
		require.Equal(t, pv.RunInvalid, record.State)
		require.Len(t, record.Incidents, 1)
		require.Equal(t, f.binding, record.Binding)
		require.NoError(t, p.SaveResult(t.Context(), "verification-tenant", f.binding.OriginalTaskID, []byte("task result")))
		require.NoError(t, p.DeleteResult(t.Context(), "verification-tenant", f.binding.OriginalTaskID))
		retained, err := p.GetPatchVerificationRun(t.Context(), f.binding.RunID)
		require.NoError(t, err)
		require.Equal(t, record, retained, "Task result cleanup cannot delete private verification evidence")
	})
}
