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

var envelopeSequence atomic.Uint64

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

func EnvelopeContext(
	ctx context.Context, t testing.TB, provider storagev1.ProviderDescriber,
	fence storagev1.ControllerEpochFence, method string, request ...any,
) context.Context {
	t.Helper()
	info, err := provider.Describe(ctx)
	require.NoError(t, err)
	return storagev1.WithWriteEnvelope(ctx, NewWriteEnvelope(t, info.DatasetID, fence, method, request...))
}
