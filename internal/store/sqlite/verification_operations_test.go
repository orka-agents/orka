package sqlite

import (
	"context"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func fencedVerificationFixture(t *testing.T) (*Store, string, pv.Manifest, pv.Binding, map[string][]byte, []pv.ExecutionEvidence) {
	t.Helper()
	s := setupTestStore(t)
	require.NoError(t, s.InitializeKubernetesValidationStore(t.Context()))
	dataset := advanceTestEpoch(t, s, testWriteFence)
	s.RequireWriteEnvelopes()
	manifest, binding, provenance, evidence := patchVerificationFixture(t)
	return s, dataset, manifest, binding, provenance, evidence
}

func TestVerificationWritesRequireCurrentEnvelope(t *testing.T) {
	s, dataset, manifest, binding, provenance, evidence := fencedVerificationFixture(t)
	require.ErrorIs(t, s.CreatePatchVerificationRun(t.Context(), manifest, binding, provenance), store.ErrValidation)
	ctx, _ := testEnvelope(t, t.Context(), dataset, "CreatePatchVerificationRun", time.Now(), manifest, binding, provenance)
	require.NoError(t, s.CreatePatchVerificationRun(ctx, manifest, binding, provenance))
	require.NoError(t, s.CreatePatchVerificationRun(ctx, manifest, binding, provenance))
	_, err := s.GetPatchVerificationRun(t.Context(), binding.RunID)
	require.ErrorIs(t, err, store.ErrValidation, "an integrity-checking read can write an incident")

	stale, _ := testEnvelope(t, t.Context(), dataset, "RecordPatchVerificationEvidence", time.Now(), binding, evidence[0])
	advanceTestEpoch(t, s, store.ControllerEpochFence{Name: testWriteFence.Name, Epoch: 2, HolderID: "successor"})
	require.ErrorIs(t, s.RecordPatchVerificationEvidence(stale, binding, evidence[0]), store.ErrFenced)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM patch_verification_evidence`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM patch_verification_runs`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestVerificationEnvelopeReplayReturnsOriginalSnapshot(t *testing.T) {
	s, dataset, manifest, binding, provenance, evidence := fencedVerificationFixture(t)
	create, _ := testEnvelope(t, t.Context(), dataset, "CreatePatchVerificationRun", time.Now(), manifest, binding, provenance)
	require.NoError(t, s.CreatePatchVerificationRun(create, manifest, binding, provenance))
	read, _ := testEnvelope(t, t.Context(), dataset, "GetPatchVerificationRun", time.Now(), binding.RunID)
	before, err := s.GetPatchVerificationRun(read, binding.RunID)
	require.NoError(t, err)
	record, _ := testEnvelope(t, t.Context(), dataset, "RecordPatchVerificationEvidence", time.Now(), binding, evidence[0])
	require.NoError(t, s.RecordPatchVerificationEvidence(record, binding, evidence[0]))
	replayed, err := s.GetPatchVerificationRun(read, binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, replayed)
	fresh, _ := testEnvelope(t, t.Context(), dataset, "GetPatchVerificationRun", time.Now(), binding.RunID)
	current, err := s.GetPatchVerificationRun(fresh, binding.RunID)
	require.NoError(t, err)
	require.Len(t, current.Evidence, 1)
	require.Empty(t, replayed.Evidence)
}

func TestVerificationDomainRefusalCommitsAndReplaysInvalidation(t *testing.T) {
	s, dataset, manifest, binding, provenance, evidence := fencedVerificationFixture(t)
	create, _ := testEnvelope(t, t.Context(), dataset, "CreatePatchVerificationRun", time.Now(), manifest, binding, provenance)
	require.NoError(t, s.CreatePatchVerificationRun(create, manifest, binding, provenance))
	record, _ := testEnvelope(t, t.Context(), dataset, "RecordPatchVerificationEvidence", time.Now(), binding, evidence[0])
	require.NoError(t, s.RecordPatchVerificationEvidence(record, binding, evidence[0]))
	conflicting := evidence[0]
	conflicting.Observation.StdoutDigest = pv.Digest([]byte("different observation"))
	conflict, envelope := testEnvelope(t, t.Context(), dataset, "RecordPatchVerificationEvidence", time.Now(), binding, conflicting)
	for range 2 {
		require.ErrorIs(t, s.RecordPatchVerificationEvidence(conflict, binding, conflicting), pv.ErrConflict)
	}
	var state string
	require.NoError(t, s.db.QueryRow(`SELECT state FROM patch_verification_runs WHERE run_id=?`, binding.RunID).Scan(&state))
	require.Equal(t, string(pv.RunInvalid), state)
	var incidents int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM patch_verification_incidents WHERE run_id=?`, binding.RunID).Scan(&incidents))
	require.Equal(t, 1, incidents)
	resolution, err := s.ResolveOperation(t.Context(), storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: envelope.OperationID, Method: "RecordPatchVerificationEvidence",
		RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationCommitted, resolution.State)
}

func TestVerificationWriteFailureRollsBackEvidenceAndReceipt(t *testing.T) {
	s, dataset, manifest, binding, provenance, evidence := fencedVerificationFixture(t)
	create, _ := testEnvelope(t, t.Context(), dataset, "CreatePatchVerificationRun", time.Now(), manifest, binding, provenance)
	require.NoError(t, s.CreatePatchVerificationRun(create, manifest, binding, provenance))
	var before int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM patch_verification_blobs`).Scan(&before))
	_, err := s.db.Exec(`CREATE TRIGGER fail_verification_evidence BEFORE INSERT ON patch_verification_evidence
		BEGIN SELECT RAISE(ABORT, 'synthetic verification write failure'); END`)
	require.NoError(t, err)
	ctx, envelope := testEnvelope(t, context.Background(), dataset, "RecordPatchVerificationEvidence", time.Now(), binding, evidence[0])
	require.ErrorContains(t, s.RecordPatchVerificationEvidence(ctx, binding, evidence[0]), "synthetic verification write failure")
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM patch_verification_blobs`).Scan(&count))
	require.Equal(t, before, count)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM patch_verification_evidence`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM operation_receipts WHERE operation_id=?`, envelope.OperationID).Scan(&count))
	require.Zero(t, count)
}
