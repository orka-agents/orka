package v1_test

import (
	"context"
	"errors"
	"testing"

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
	require.GreaterOrEqual(t, storagev1.ContractRevision, 1, "revisions start at 1")
}
