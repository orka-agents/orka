package v1_test

import (
	"context"
	"errors"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestFencedErrorMatchesFencedAndConflict(t *testing.T) {
	err := storagev1.FencedErrorf("controller epoch fence %d is stale", 3)
	require.ErrorIs(t, err, storagev1.ErrFenced)
	require.ErrorIs(t, err, storagev1.ErrConflict, "existing callers keep treating a fence rejection as a conflict")
	require.NotErrorIs(t, err, storagev1.ErrValidation)
	require.EqualError(t, err, "controller epoch fence 3 is stale")

	var fenced storagev1.FencedError
	require.True(t, errors.As(err, &fenced))
	require.NotErrorIs(t, storagev1.ErrConflict, storagev1.ErrFenced, "an ordinary conflict is not a fence rejection")
}

func TestPreconditionFailedErrorMatchesPreconditionAndConflict(t *testing.T) {
	err := storagev1.PreconditionFailedErrorf("proposal %s changed", "p-1")
	require.ErrorIs(t, err, storagev1.ErrPreconditionFailed)
	require.ErrorIs(t, err, storagev1.ErrConflict, "callers that retry concurrent changes keep working")
	require.NotErrorIs(t, err, storagev1.ErrFenced)
	require.EqualError(t, err, "proposal p-1 changed")
	require.NotErrorIs(t, storagev1.ErrConflict, storagev1.ErrPreconditionFailed, "an ordinary conflict is not a precondition failure")
}

func TestTaskDataFenceTravelsInContext(t *testing.T) {
	_, ok := storagev1.TaskDataFenceFrom(context.Background())
	require.False(t, ok, "a context without a fence carries none")

	jobs := []storagev1.TaskJobIdentity{{Namespace: "ns", TaskUID: "task-uid", JobUID: "job-uid"}}
	fence := storagev1.TaskDataFence{Namespace: "ns", TaskName: "task", Generation: 3, Jobs: jobs}
	ctx := storagev1.WithTaskDataFence(context.Background(), fence)
	jobs[0].JobUID = "changed after attaching"
	got, ok := storagev1.TaskDataFenceFrom(ctx)
	require.True(t, ok)
	require.Equal(t, "job-uid", got.Jobs[0].JobUID, "the context keeps its own copy of the Jobs")
	got.Jobs[0].JobUID = "changed after reading"
	again, _ := storagev1.TaskDataFenceFrom(ctx)
	require.Equal(t, "job-uid", again.Jobs[0].JobUID, "a reader cannot change the carried fence")
}

func TestScanIngestionReadsCompareByValue(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("AEST", 10*60*60))
	stored := storagev1.ScanIngestionReadResult{ReviewSlice: &storagev1.ReviewSlice{
		ID: "slice", Tags: []string{}, OwnedFiles: []storagev1.ReviewSliceFile{}, CreatedAt: at.UTC(),
	}}
	decoded := storagev1.ScanIngestionReadResult{ReviewSlice: &storagev1.ReviewSlice{ID: "slice", CreatedAt: at}}
	require.True(t, storagev1.ScanIngestionReadsEqual(stored, decoded),
		"an empty list equals an absent one and a time equals the same instant in another zone")
	decoded.ReviewSlice.Status = "reviewed"
	require.False(t, storagev1.ScanIngestionReadsEqual(stored, decoded))
	require.True(t, storagev1.ScanIngestionReadsEqual(
		storagev1.ScanIngestionReadResult{Findings: []storagev1.Finding{}}, storagev1.ScanIngestionReadResult{}))
	require.False(t, storagev1.ScanIngestionReadsEqual(
		storagev1.ScanIngestionReadResult{NotFound: true}, storagev1.ScanIngestionReadResult{}))

	run := storagev1.ScanRun{ID: "run", StartedAt: at}
	cancelled := run
	cancelled.CancellationVersion = 1
	require.False(t, storagev1.ScanRunsEqual(run, cancelled), "fields outside the JSON form still count")
	utc := run
	utc.StartedAt = at.UTC()
	require.True(t, storagev1.ScanRunsEqual(run, utc))
}

func TestTooLargeErrorReportsTheLimit(t *testing.T) {
	err := error(storagev1.TooLargeError{What: `artifact "out.bin"`, Size: 11, Limit: 10})
	require.ErrorIs(t, err, storagev1.ErrTooLarge)
	require.NotErrorIs(t, err, storagev1.ErrValidation)
	require.EqualError(t, err, `artifact "out.bin" is 11 bytes, which exceeds the 10 byte limit`)
}

func TestWriteEnvelopeTravelsInContext(t *testing.T) {
	_, ok := storagev1.WriteEnvelopeFrom(context.Background())
	require.False(t, ok, "a context without an envelope carries none")

	envelope := storagev1.WriteEnvelope{
		DatasetID:     "dataset-1",
		Fence:         storagev1.ControllerEpochFence{Name: "orka-controller", Epoch: 4, HolderID: "holder"},
		OperationID:   "operation-1",
		RequestDigest: storagev1.CanonicalBytesDigest([]byte("request")),
	}
	got, ok := storagev1.WriteEnvelopeFrom(storagev1.WithWriteEnvelope(context.Background(), envelope))
	require.True(t, ok)
	require.Equal(t, envelope, got)
}

func TestDescribeVocabulary(t *testing.T) {
	for _, origin := range []storagev1.DatasetOrigin{
		storagev1.DatasetOriginNew, storagev1.DatasetOriginAdopted, storagev1.DatasetOriginMigrated,
	} {
		require.True(t, storagev1.IsValidDatasetOrigin(origin), origin)
	}
	require.False(t, storagev1.IsValidDatasetOrigin(""))
	for _, class := range []storagev1.DurabilityClass{
		storagev1.DurabilitySynchronousReplicated, storagev1.DurabilityLocalFsync,
		storagev1.DurabilityLocalBuffered, storagev1.DurabilityAsyncReplicated,
	} {
		require.True(t, storagev1.IsValidDurabilityClass(class), class)
	}
	require.False(t, storagev1.IsValidDurabilityClass("eventual"))
}
