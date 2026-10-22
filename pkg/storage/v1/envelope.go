package v1

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
)

// ErrDatasetMismatch is returned when a request names a dataset other than the
// one the provider serves. Nothing was committed. The caller is bound to the
// wrong dataset, so retrying the request cannot succeed.
var ErrDatasetMismatch = errors.New("dataset mismatch")

// ErrClockSkew is returned when a write was issued more than [MaxClockSkew]
// ahead of the provider clock. Nothing was committed. The caller treats the
// store as not ready until the two clocks agree again.
var ErrClockSkew = errors.New("clock skew")

// WriteEnvelope carries the cross-cutting identity of one controller write:
// the dataset it targets, the controller epoch fence it was issued under, and
// the operation identity every retry of it reuses.
//
// Orka does not attach the envelope yet, and providers must accept writes
// without one. When the storage client attaches it, every method that
// [LookupMethod] classifies as [MethodWrite] carries one in its context, and
// reads and [MethodAdmin] methods carry none. A provider runs these checks on
// every write that carries an envelope, in this order, in the transaction
// that commits the write:
//
//  1. DatasetID equals the provider's own dataset, or [ErrDatasetMismatch].
//  2. A receipt for OperationID committed with the same method and
//     RequestDigest returns the original outcome without any further check,
//     so a retry reads the original result even under a stale fence and
//     changes nothing. A receipt with a different method or digest returns
//     [ErrDuplicateMismatch], and an aborted one returns
//     [ErrOperationAborted]. See [OperationReceiptStore].
//  3. IssuedAt is at most [MaxOperationAge] older than the provider clock, or
//     [ErrOperationExpired], and at most [MaxClockSkew] ahead of it, or
//     [ErrClockSkew].
//  4. The dataset is [DatasetReady], or [ErrNotReady].
//  5. Fence equals the controller epoch the dataset holds (name, epoch and
//     holder), or a [FencedError]. This check is linearized with
//     AdvanceControllerEpoch (see [ControllerEpochAdvancer]): once an advance
//     returns, no write carrying an older fence commits.
//  6. The method's own preconditions, such as owner fences, expected
//     versions, dedupe keys and natural keys.
//  7. The change and its operation receipt are written together.
//
// A refusal at any step commits nothing and records no receipt, so a later
// attempt is evaluated again. Once providers require envelopes, a request
// whose envelope fails [WriteEnvelope.Validate], or a write without an
// envelope, returns [ErrValidation].
//
// Each dataset holds its own controller epoch, and Fence is the issuing
// controller's fence for the dataset this write targets. When Orka attaches
// envelopes, it advances each bound dataset separately and opens a dataset's
// fence as soon as that dataset's own advance succeeds, so writes to one store
// never wait for another store's advance. The contract that serves a method
// is [MethodInfo.Contract].
//
// Methods that already take an explicit [ControllerEpochFence], such as the
// outbox family, keep it authoritative; it must equal Fence, or the provider
// returns [ErrValidation]. Operation-specific preconditions, such as an
// expected version or message count, stay explicit parameters. A network
// transport carries the envelope as request metadata rather than in each
// record.
type WriteEnvelope struct {
	// DatasetID is the dataset the caller expects to write to, as Describe
	// reports it.
	DatasetID string
	// Fence is the controller epoch the write was issued under, for the
	// dataset it targets.
	Fence ControllerEpochFence
	// OperationID identifies one operation instance. Every retry of that
	// instance, and its resolution, reuse it. It is at most
	// [MaxOperationIDBytes] bytes.
	OperationID string
	// RequestDigest is [RequestDigest] over the method and its arguments. It
	// never covers the envelope. The storage client computes it once, from
	// the arguments of the first attempt, and every retry resends those
	// arguments unchanged. An in-process provider compares it with the
	// receipt without recomputing it; a transport that crosses a process
	// boundary recomputes it from the decoded request and refuses a mismatch
	// with ErrValidation.
	RequestDigest string
	// IssuedAt is the controller clock when the operation was created. Retries
	// repeat it.
	IssuedAt time.Time
	// Sequence is the operation's issue order within Fence, starting at 1.
	// Retries repeat it.
	Sequence uint64
}

// Validate checks that every envelope field is present and well formed. It
// does not compare the envelope with any stored state.
func (e WriteEnvelope) Validate() error {
	if err := ValidateControlIdentifier("write envelope dataset ID", e.DatasetID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("write envelope fence name", e.Fence.Name); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("write envelope fence holder ID", e.Fence.HolderID); err != nil {
		return err
	}
	if e.Fence.Epoch < 1 {
		return ValidationErrorf("write envelope fence epoch must be at least 1")
	}
	if err := ValidateOperationID(e.OperationID); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("write envelope request digest", e.RequestDigest); err != nil {
		return err
	}
	if e.IssuedAt.IsZero() {
		return ValidationErrorf("write envelope issued-at time is required")
	}
	if e.Sequence < 1 {
		return ValidationErrorf("write envelope sequence must be at least 1")
	}
	return nil
}

// ValidateOperationID checks that id is a non-empty operation identifier of at
// most [MaxOperationIDBytes] bytes of valid UTF-8, without surrounding
// whitespace or Unicode control characters.
func ValidateOperationID(id string) error {
	if id == "" {
		return ValidationErrorf("operation ID is required")
	}
	if len(id) > MaxOperationIDBytes {
		return ValidationErrorf("operation ID exceeds %d bytes", MaxOperationIDBytes)
	}
	if err := validateControlIdentifier("operation ID", id); err != nil {
		return err
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return ValidationErrorf("operation ID contains a control character")
	}
	return nil
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
