package storagetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// envelopeSequence orders the envelopes this package issues, as a controller
// orders its operations.
var envelopeSequence atomic.Uint64

// NewWriteEnvelope returns a valid write envelope for one call of method with
// request, on datasetID under fence: a new operation ID, the request digest,
// the current time as IssuedAt, and the next issue sequence. Provider tests
// attach it with [storagev1.WithWriteEnvelope]; a retry of the same operation
// reuses the same envelope.
func NewWriteEnvelope(
	t testing.TB, datasetID string, fence storagev1.ControllerEpochFence, method string, request ...any,
) storagev1.WriteEnvelope {
	t.Helper()
	digest, err := storagev1.RequestDigest(method, request...)
	require.NoError(t, err, "request digest for %s", method)
	var id [16]byte
	_, err = rand.Read(id[:])
	require.NoError(t, err)
	envelope := storagev1.WriteEnvelope{
		DatasetID:     datasetID,
		Fence:         fence,
		OperationID:   "op-" + hex.EncodeToString(id[:]),
		RequestDigest: digest,
		IssuedAt:      time.Now().UTC(),
		Sequence:      envelopeSequence.Add(1),
	}
	require.NoError(t, envelope.Validate())
	return envelope
}

// EnvelopeContext returns ctx carrying a new write envelope (see
// [NewWriteEnvelope]) for one call of method with request, on the dataset
// provider describes.
func EnvelopeContext(
	ctx context.Context, t testing.TB, provider storagev1.ProviderDescriber,
	fence storagev1.ControllerEpochFence, method string, request ...any,
) context.Context {
	t.Helper()
	info, err := provider.Describe(ctx)
	require.NoError(t, err)
	return storagev1.WithWriteEnvelope(ctx, NewWriteEnvelope(t, info.DatasetID, fence, method, request...))
}
