package v1

import (
	"context"
	"errors"
)

// ErrDatasetMismatch is returned when a request names a dataset other than
// the one the provider serves. Nothing was committed, and retrying the same
// request cannot succeed: the caller must not treat the provider as empty or
// switch to it.
var ErrDatasetMismatch = errors.New("dataset mismatch")

// WriteEnvelope carries the cross-cutting identity of one controller write:
// the dataset it targets, the controller epoch fence it was issued under, and
// the operation identity a retry must reuse.
//
// The envelope is reserved. Orka does not attach it yet, and providers must
// accept writes without one. Commit-time enforcement arrives with storage
// operation receipts: a provider will then verify DatasetID against its own
// dataset, check Fence against the controller epoch mirror in the same commit
// as the write, and use OperationID with RequestDigest to return the original
// outcome for a retry instead of applying it twice. A network transport
// carries the envelope as request metadata rather than in each record.
//
// The outbox family already takes an explicit [ControllerEpochFence]
// parameter, and that parameter stays authoritative for it. Every other write family has no fence today; the envelope is how they
// will receive one without a second change to each method signature.
// Operation-specific preconditions, such as an expected version or message
// count, stay explicit parameters.
type WriteEnvelope struct {
	// DatasetID is the dataset the caller expects to write to.
	DatasetID string
	// Fence is the controller epoch the write was issued under.
	Fence ControllerEpochFence
	// OperationID identifies the logical operation across retries.
	OperationID string
	// RequestDigest is the canonical digest of the request, so a retry that
	// reuses OperationID with a different request can be rejected.
	RequestDigest string
}

type writeEnvelopeKey struct{}

// WithWriteEnvelope returns a context that carries envelope.
func WithWriteEnvelope(ctx context.Context, envelope WriteEnvelope) context.Context {
	return context.WithValue(ctx, writeEnvelopeKey{}, envelope)
}

// WriteEnvelopeFrom returns the envelope carried by ctx, if any.
func WriteEnvelopeFrom(ctx context.Context) (WriteEnvelope, bool) {
	envelope, ok := ctx.Value(writeEnvelopeKey{}).(WriteEnvelope)
	return envelope, ok
}
