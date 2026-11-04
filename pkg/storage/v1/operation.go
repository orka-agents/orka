package v1

import (
	"context"
	"errors"
	"time"
)

// ErrOperationExpired is returned when an operation is too old for the
// request. A write issued more than [MaxOperationAge] before the provider
// clock is refused and commits nothing; the caller must not retry that
// operation, and running the logical operation again is a new operation. An
// earlier attempt of the same operation may still have committed, so after an
// unknown outcome this refusal does not settle the operation; ResolveOperation
// does (see [OperationReceiptStore]). A ResolveOperation
// call that finds no record of an operation issued more than
// [MinReceiptRetention] minus [MaxClockSkew] before the provider clock records
// nothing: whether the operation committed can no longer be established, so
// the caller treats its outcome as permanently unknown and holds the affected
// work for recovery instead of repeating it or reporting success.
var ErrOperationExpired = errors.New("operation expired")

// ErrOperationAborted is returned to a write whose operation
// [OperationReceiptStore.ResolveOperation] has recorded as aborted. Nothing
// was committed and nothing ever will be under that operation ID. Running the
// logical operation again is a new operation, and a deliberate choice of the
// caller.
var ErrOperationAborted = errors.New("operation aborted")

const (
	// MaxOperationIDBytes is the longest accepted operation ID.
	MaxOperationIDBytes = 256
	// MaxOperationAge is how long after its IssuedAt a provider still accepts a
	// new write for an operation. A replay of a committed operation is served
	// for as long as its receipt is kept.
	MaxOperationAge = 15 * time.Minute
	// MaxClockSkew is how far a write's IssuedAt may be ahead of the provider
	// clock, and the clock difference Orka tolerates between itself and a
	// provider.
	MaxClockSkew = 30 * time.Second
	// MinReceiptRetention is the shortest time a provider keeps an operation
	// receipt after recording it. It exceeds MaxOperationAge plus MaxClockSkew,
	// so a receipt outlives every retry the provider can still accept.
	MinReceiptRetention = time.Hour
	// MaxReceiptResultBytes is the largest result a provider needs to keep
	// with a receipt. A provider may instead keep record identifiers and read
	// the records again on replay, and does so for a larger result.
	MaxReceiptResultBytes = 1 << 20
)

// OperationState is the settled state of an operation.
type OperationState string

const (
	// OperationCommitted marks an operation whose change and receipt
	// committed together.
	OperationCommitted OperationState = "committed"
	// OperationAborted marks an operation that ResolveOperation found
	// uncommitted and recorded as aborted, so it can never commit.
	OperationAborted OperationState = "aborted"
)

// IsValidOperationState reports whether state is a defined operation state.
func IsValidOperationState(state OperationState) bool {
	switch state {
	case OperationCommitted, OperationAborted:
		return true
	default:
		return false
	}
}

// OperationOutcome is the settled outcome of one operation, as its receipt
// records it. It carries no result: to read the typed result of a committed
// operation, the caller retries the write with the same envelope, which
// returns it as a replay.
type OperationOutcome struct {
	OperationID   string
	Method        string
	RequestDigest string
	State         OperationState
	// Fence is the envelope fence of the write that committed. It is zero for
	// an operation that ResolveOperation aborted.
	Fence ControllerEpochFence
	// IssuedAt is the envelope IssuedAt of the write that committed, or the
	// IssuedAt of the ResolveOperation request that aborted the operation.
	IssuedAt time.Time
	// RecordedAt is the provider clock when the receipt was recorded.
	RecordedAt time.Time
}

// ResolveOperationRequest names one operation to settle.
type ResolveOperationRequest struct {
	// DatasetID is the dataset the operation targeted. A different dataset
	// returns ErrDatasetMismatch.
	DatasetID     string
	OperationID   string
	Method        string
	RequestDigest string
	// IssuedAt is the operation's envelope IssuedAt. It tells the provider
	// whether a missing receipt means the operation never committed or that
	// its receipt may already have been pruned.
	IssuedAt time.Time
}

// Validate checks that every field is present and well formed.
func (r ResolveOperationRequest) Validate() error {
	if err := ValidateControlIdentifier("resolve operation dataset ID", r.DatasetID); err != nil {
		return err
	}
	if err := ValidateOperationID(r.OperationID); err != nil {
		return err
	}
	if info, ok := LookupMethod(r.Method); !ok || info.Kind != MethodWrite {
		return ValidationErrorf("resolve operation method %q is not a contract write method", r.Method)
	}
	if err := ValidateCanonicalDigest("resolve operation request digest", r.RequestDigest); err != nil {
		return err
	}
	if r.IssuedAt.IsZero() {
		return ValidationErrorf("resolve operation issued-at time is required")
	}
	return nil
}

// OperationReceiptStore settles operations and bounds their receipts. Every
// write that carries a [WriteEnvelope] leaves a receipt in the transaction
// that commits its change: a retry with the same operation ID, method and
// request digest returns the original outcome and changes nothing. A provider
// either keeps the typed result with the receipt or reads it again from the
// records the write made; outputs those records do not hold, such as a created
// flag or a count, are always kept with the receipt. A replay therefore
// returns the original result, including any identifiers the write assigned,
// except that a result read again reflects records that have changed or been
// deleted since; a method whose replay can differ that way says so.
// A successful write that changed nothing, such as a delete that found
// nothing, is a committed outcome and gets a receipt too. A refusal commits
// nothing and leaves no receipt. A receipt never holds plaintext protected
// data.
//
// A refusal settles only the attempt it answers. After an attempt of an
// operation ended with an unknown outcome, the operation is settled only by a
// replayed outcome (committed), by [ErrOperationAborted], or by a
// ResolveOperation result. A refusal of a later attempt leaves the outcome
// unknown, because an earlier attempt may have committed and had its receipt
// pruned since, or may still commit, for example once a migration finishes or
// a precondition holds again.
//
// For one operation ID, a provider makes the receipt check and commit of each
// attempt, and an abort recorded by ResolveOperation, atomic with respect to
// one another, for example by claiming the operation ID before anything else,
// so an operation never both commits and aborts. It measures an operation's
// age on its own clock inside the transaction that would commit it.
//
// Both administrative methods carry no envelope and are not checked against
// the current fence. A provider that imports a dataset from another provider
// either carries its receipts and aborted records with it or gives the
// imported dataset a new dataset ID, so that ResolveOperation never reports a
// committed operation as aborted.
type OperationReceiptStore interface {
	// ResolveOperation returns the committed outcome of an operation, or, when
	// nothing committed, durably records it as aborted so it can never commit
	// later, and returns that. Repeating it returns the same outcome for as
	// long as the provider keeps that record. A receipt with a different method
	// or request digest returns ErrDuplicateMismatch, and a request for another
	// dataset returns ErrDatasetMismatch. While the dataset is not ready, such
	// as while it is migrated or imported, it returns an outcome it already
	// holds and otherwise returns ErrNotReady, recording nothing. Any
	// controller may call it, including one whose fence is stale, because it
	// can only prevent a change.
	//
	// A missing receipt proves that nothing committed only while any receipt
	// the operation could have left is still retained: a receipt recorded at
	// provider time C satisfies IssuedAt <= C + MaxClockSkew and is kept until
	// at least C + MinReceiptRetention, whatever retention the provider used
	// when it was recorded. So when no receipt exists and IssuedAt is more than
	// MinReceiptRetention minus MaxClockSkew before the provider clock,
	// ResolveOperation records nothing and returns ErrOperationExpired. An
	// IssuedAt more than MaxClockSkew ahead of the provider clock returns
	// ErrClockSkew and records nothing. The bound assumes the clock the
	// provider uses for receipt retention never moves backward; a provider
	// whose clock can step back, for example across a failover, retains
	// receipts longer by the largest such step.
	ResolveOperation(ctx context.Context, request ResolveOperationRequest) (OperationOutcome, error)
	// PruneOperationReceipts removes at most limit operation receipts and
	// aborted records older than the provider's receipt retention, measured on
	// the provider clock, and reports how many it removed. It removes no record
	// that another family keeps, such as a Session cleanup completion or a
	// gateway tombstone; those follow their own retention. A limit below 1
	// returns ErrValidation.
	PruneOperationReceipts(ctx context.Context, limit int) (int, error)
}
