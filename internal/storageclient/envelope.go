package storageclient

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

//go:generate go run ./internal/envelopegen/cmd/envelopegen storageclient envelope_gen.go

// FenceSource tells the storage client which dataset serves a contract and
// the controller epoch fence its writes carry there. The controller epoch
// manager implements it: it opens each dataset's fence once that dataset's
// own epoch advance succeeds, so a write waits only for the dataset it
// targets.
type FenceSource interface {
	// WriteFence waits until the fence of the dataset that serves contract
	// is open and returns that dataset's ID and the fence. It returns ctx's
	// error when ctx ends first.
	WriteFence(ctx context.Context, contract string) (datasetID string, fence storagev1.ControllerEpochFence, err error)
}

const (
	// DefaultFenceWait bounds how long a write waits for its dataset's fence
	// to open before it is refused with ErrNotReady.
	DefaultFenceWait = 10 * time.Second
	// DefaultRetryHorizon bounds how long one write retries after
	// ErrUnavailable or ErrOutcomeUnknown when its context has no earlier
	// deadline. It is far below storagev1.MaxOperationAge, so every retry is
	// still within the operation's lifetime.
	DefaultRetryHorizon = 30 * time.Second
	// DefaultResolveTimeout bounds one ResolveOperation call, and reading the
	// result of an operation it found committed.
	DefaultResolveTimeout = 5 * time.Second
)

const (
	retryInitialBackoff      = 50 * time.Millisecond
	retryMaxBackoff          = 2 * time.Second
	defaultResolveBackoff    = time.Second
	defaultResolveMaxBackoff = 30 * time.Second
)

// envelopedState runs every state.v1 write under a write envelope. Its write
// methods are generated (envelope_gen.go); reads and administrative methods
// are the provider's own.
type envelopedState struct {
	storagev1.StateProvider
	ops *operations
}

// envelopedArtifacts runs every artifacts.v1 write under a write envelope, in
// the same way as envelopedState.
type envelopedArtifacts struct {
	storagev1.ArtifactProvider
	ops *operations
}

// writeCall describes one call of a contract write method.
type writeCall struct {
	method   string
	contract string
	// args are the arguments after the context, which the request digest
	// covers.
	args []any
	// fence is the controller epoch fence the method takes as a parameter,
	// or nil. It is authoritative for the write, so the envelope carries it.
	fence *storagev1.ControllerEpochFence
}

// operations attaches write envelopes, retries writes whose outcome is not
// yet known, and settles them with ResolveOperation.
type operations struct {
	resolvers map[string]storagev1.OperationReceiptStore

	fencesMu    sync.Mutex
	fences      FenceSource
	fencesBound chan struct{}

	fenceWait      time.Duration
	retryHorizon   time.Duration
	resolveTimeout time.Duration
	// resolveBackoff and resolveMaxBackoff pace background resolution.
	resolveBackoff    time.Duration
	resolveMaxBackoff time.Duration
	now               func() time.Time
	newOperationID    func() (string, error)
	sequence          atomic.Uint64

	mu sync.Mutex
	// unresolved holds, for each dataset, the operations whose outcome the
	// client could not settle. While a dataset has any, its new writes are
	// refused.
	unresolved map[string]map[string]pendingOperation
	resolving  map[string]bool
}

// pendingOperation is an operation whose outcome is not settled yet.
type pendingOperation struct {
	contract string
	request  storagev1.ResolveOperationRequest
}

func newOperations(resolvers map[string]storagev1.OperationReceiptStore) *operations {
	return &operations{
		resolvers:         resolvers,
		fencesBound:       make(chan struct{}),
		fenceWait:         DefaultFenceWait,
		retryHorizon:      DefaultRetryHorizon,
		resolveTimeout:    DefaultResolveTimeout,
		resolveBackoff:    defaultResolveBackoff,
		resolveMaxBackoff: defaultResolveMaxBackoff,
		now:               time.Now,
		newOperationID:    newOperationID,
		unresolved:        map[string]map[string]pendingOperation{},
		resolving:         map[string]bool{},
	}
}

// newOperationID returns a new UUIDv7, so operation IDs sort by creation.
func newOperationID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return id.String(), nil
}

// bindFences sets the fence source once.
func (o *operations) bindFences(source FenceSource) error {
	if isNilProvider(source) {
		return storagev1.ValidationErrorf("storage client fence source is required")
	}
	o.fencesMu.Lock()
	defer o.fencesMu.Unlock()
	if o.fences != nil {
		return storagev1.ValidationErrorf("storage client fence source is already bound")
	}
	o.fences = source
	close(o.fencesBound)
	return nil
}

// writeFence waits, for at most fenceWait, until a fence source is bound and
// the fence of the dataset that serves contract is open. Inside a task-data
// transaction it returns the fence the transaction captured before it opened.
func (o *operations) writeFence(ctx context.Context, contract string) (string, storagev1.ControllerEpochFence, error) {
	if captured, ok := ctx.Value(transactionFencesKey{}).(transactionFences); ok {
		if fence, ok := captured[contract]; ok {
			return fence.datasetID, fence.fence, nil
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, o.fenceWait)
	defer cancel()
	notReady := func(cause error) error {
		if ctx.Err() != nil {
			// The call ended first; the write is still only not ready, so a
			// caller can tell it from a definite failure.
			return fmt.Errorf("%w: the controller epoch fence for %s did not open before the call ended: %w",
				storagev1.ErrNotReady, contract, ctx.Err())
		}
		return fmt.Errorf("%w: the controller epoch fence for %s is not open: %w", storagev1.ErrNotReady, contract, cause)
	}
	select {
	case <-o.fencesBound:
	case <-waitCtx.Done():
		return "", storagev1.ControllerEpochFence{}, notReady(errors.New("no fence source is bound"))
	}
	o.fencesMu.Lock()
	source := o.fences
	o.fencesMu.Unlock()
	datasetID, fence, err := source.WriteFence(waitCtx, contract)
	if err != nil {
		return "", storagev1.ControllerEpochFence{}, notReady(err)
	}
	return datasetID, fence, nil
}

// transactionFencesKey carries the write fences a task-data transaction
// captured before it opened.
type transactionFencesKey struct{}

type datasetFence struct {
	datasetID string
	fence     storagev1.ControllerEpochFence
}

type transactionFences map[string]datasetFence

// withTransactionFences waits for the fence of every contract and returns
// ctx carrying them, for a task-data transaction to open with. The writes
// inside the transaction use them, so none of them waits for a fence, or
// calls the fence source, while the transaction holds the store. The
// embedded store serves both contracts, so both fences are open together.
func (o *operations) withTransactionFences(ctx context.Context) (context.Context, error) {
	captured := transactionFences{}
	for _, contract := range []string{storagev1.StateContract, storagev1.ArtifactsContract} {
		datasetID, fence, err := o.writeFence(ctx, contract)
		if err != nil {
			return nil, err
		}
		captured[contract] = datasetFence{datasetID: datasetID, fence: fence}
	}
	return context.WithValue(ctx, transactionFencesKey{}, captured), nil
}

// run runs one write under a write envelope.
//
// A context that already carries an envelope belongs to a caller that owns
// the operation, such as a provider test; the write runs once with it.
// Otherwise run waits for the fence of the dataset that serves the write,
// refuses the write while an earlier write to that dataset has an unknown
// outcome, and creates one operation: a new operation ID, the request digest
// over the arguments as they are now, the issue time and the next sequence.
// Every retry of that operation reuses the whole envelope and the same
// arguments.
func (o *operations) run(ctx context.Context, call writeCall, attempt func(context.Context) error) error {
	if _, ok := storagev1.WriteEnvelopeFrom(ctx); ok {
		return attempt(ctx)
	}
	datasetID, fence, err := o.writeFence(ctx, call.contract)
	if err != nil {
		return err
	}
	if call.fence != nil {
		fence, err = storagev1.NormalizeEpochFence(*call.fence)
		if err != nil {
			return err
		}
	}
	if err := o.requireSettled(datasetID); err != nil {
		return err
	}
	digest, err := storagev1.RequestDigest(call.method, call.args...)
	if err != nil {
		return err
	}
	operationID, err := o.newOperationID()
	if err != nil {
		return err
	}
	envelope := storagev1.WriteEnvelope{
		DatasetID:     datasetID,
		Fence:         fence,
		OperationID:   operationID,
		RequestDigest: digest,
		IssuedAt:      o.now().UTC(),
		Sequence:      o.sequence.Add(1),
	}
	return o.drive(ctx, call, envelope, attempt)
}

// drive makes the attempts of one operation.
//
// ErrUnavailable means the attempt committed nothing, and ErrOutcomeUnknown
// that it may have committed; drive retries the same envelope after either
// until the call's deadline. Any other error settles the operation as
// refused, unless an earlier attempt's outcome is unknown: then the refusal
// settles only that attempt, and drive resolves the operation.
func (o *operations) drive(
	ctx context.Context, call writeCall, envelope storagev1.WriteEnvelope, attempt func(context.Context) error,
) error {
	deadline := o.now().Add(o.retryHorizon)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	unknown := false
	var last error
	for retry := 0; ; retry++ {
		err := attempt(storagev1.WithWriteEnvelope(ctx, envelope))
		switch {
		case err == nil:
			return nil
		case errors.Is(err, storagev1.ErrOutcomeUnknown):
			unknown = true
		case errors.Is(err, storagev1.ErrUnavailable):
		case errors.Is(err, storagev1.ErrOperationAborted):
			return err
		case errors.Is(err, storagev1.ErrClockSkew):
			// The store refuses writes from this controller until the clocks
			// agree again: not ready, also when it ends an attempt after an
			// unknown outcome and resolution finds the operation aborted.
			skew := fmt.Errorf("%w: %w", storagev1.ErrNotReady, err)
			if unknown {
				return o.resolve(ctx, call, envelope, attempt, skew)
			}
			return skew
		case unknown:
			return o.resolve(ctx, call, envelope, attempt, err)
		default:
			return err
		}
		last = err
		wait := backoff(retry, retryInitialBackoff, retryMaxBackoff)
		if ctx.Err() != nil || !o.now().Add(wait).Before(deadline) || !sleep(ctx, wait) {
			break
		}
	}
	if unknown {
		return o.resolve(ctx, call, envelope, attempt, last)
	}
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), last)
	}
	return last
}

// resolve settles an operation whose outcome is unknown. A committed
// operation is attempted once more with the same envelope, which reads its
// original result as a replay. An operation that cannot be settled now joins
// the unresolved set of its dataset, and the write returns ErrOutcomeUnknown.
// An error that leaves the outcome unknown matches only ErrOutcomeUnknown,
// never the refusal or resolution error behind it, so no caller reads it as
// a definite failure.
func (o *operations) resolve(
	ctx context.Context, call writeCall, envelope storagev1.WriteEnvelope, attempt func(context.Context) error, cause error,
) error {
	resolveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.resolveTimeout)
	defer cancel()
	request := storagev1.ResolveOperationRequest{
		DatasetID:     envelope.DatasetID,
		OperationID:   envelope.OperationID,
		Method:        call.method,
		RequestDigest: envelope.RequestDigest,
		IssuedAt:      envelope.IssuedAt,
	}
	resolver := o.resolvers[call.contract]
	if resolver == nil {
		return fmt.Errorf("%w: operation %s: no resolver for %s (after %v)", storagev1.ErrOutcomeUnknown, envelope.OperationID, call.contract, cause)
	}
	outcome, err := resolver.ResolveOperation(resolveCtx, request)
	switch {
	case err == nil && outcome.State == storagev1.OperationCommitted:
		return o.replay(resolveCtx, envelope, attempt)
	case err == nil && outcome.State == storagev1.OperationAborted:
		// The operation is settled: nothing committed, and it never will.
		// When only transient failures ended the attempts, the write is
		// as safe to run again as after ErrUnavailable, so it matches that
		// too; a refusal that ended the attempts stays matchable instead.
		if errors.Is(cause, storagev1.ErrOutcomeUnknown) || errors.Is(cause, storagev1.ErrUnavailable) {
			return fmt.Errorf("%w: operation %s did not commit and never will (%w after %v)",
				storagev1.ErrOperationAborted, envelope.OperationID, storagev1.ErrUnavailable, cause)
		}
		return fmt.Errorf("%w: operation %s did not commit and never will: %w", storagev1.ErrOperationAborted, envelope.OperationID, cause)
	case err == nil:
		err = fmt.Errorf("unexpected operation state %q", outcome.State)
	case errors.Is(err, storagev1.ErrOperationExpired), unsettleable(err):
		// Retrying cannot settle the operation, so it does not block the
		// dataset. Whether it committed stays unknown.
		return fmt.Errorf("%w: operation %s cannot be settled: %v (after %v)", storagev1.ErrOutcomeUnknown, envelope.OperationID, err, cause)
	}
	o.addUnresolved(envelope.DatasetID, envelope.OperationID, pendingOperation{contract: call.contract, request: request})
	return fmt.Errorf("%w: operation %s is not settled yet: %v (after %v)", storagev1.ErrOutcomeUnknown, envelope.OperationID, err, cause)
}

// replay reads the result of a committed operation by attempting it again
// with the same envelope. A replay is served whatever the current fence.
func (o *operations) replay(ctx context.Context, envelope storagev1.WriteEnvelope, attempt func(context.Context) error) error {
	var err error
	for retry := 0; ; retry++ {
		err = attempt(storagev1.WithWriteEnvelope(ctx, envelope))
		if err == nil {
			return nil
		}
		if !errors.Is(err, storagev1.ErrUnavailable) && !errors.Is(err, storagev1.ErrOutcomeUnknown) {
			break
		}
		if !sleep(ctx, backoff(retry, retryInitialBackoff, retryMaxBackoff)) {
			break
		}
	}
	return fmt.Errorf("%w: operation %s committed, but its result could not be read: %v",
		storagev1.ErrOutcomeUnknown, envelope.OperationID, err)
}

// unsettleable reports whether a ResolveOperation error is a definite
// refusal that repeating the request cannot change.
func unsettleable(err error) bool {
	return errors.Is(err, storagev1.ErrDuplicateMismatch) ||
		errors.Is(err, storagev1.ErrDatasetMismatch) ||
		errors.Is(err, storagev1.ErrValidation)
}

// requireSettled refuses a new write while a write to the same dataset has
// an unknown outcome, so nothing that depends on that outcome can run before
// it is settled.
func (o *operations) requireSettled(datasetID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if pending := len(o.unresolved[datasetID]); pending > 0 {
		return fmt.Errorf("%w: %d earlier write(s) to dataset %s have an unknown outcome and are being resolved",
			storagev1.ErrNotReady, pending, datasetID)
	}
	return nil
}

func (o *operations) addUnresolved(datasetID, operationID string, operation pendingOperation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.unresolved[datasetID] == nil {
		o.unresolved[datasetID] = map[string]pendingOperation{}
	}
	o.unresolved[datasetID][operationID] = operation
	if !o.resolving[datasetID] {
		o.resolving[datasetID] = true
		go o.resolveInBackground(datasetID)
	}
}

// unresolvedCount returns how many operations have an unknown outcome.
func (o *operations) unresolvedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, operations := range o.unresolved {
		count += len(operations)
	}
	return count
}

// pending returns the unresolved operations of a dataset in operation ID
// order, or reports that none are left and stops its resolver.
func (o *operations) pending(datasetID string) (map[string]pendingOperation, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	operations := o.unresolved[datasetID]
	if len(operations) == 0 {
		delete(o.unresolved, datasetID)
		delete(o.resolving, datasetID)
		return nil, false
	}
	copied := make(map[string]pendingOperation, len(operations))
	maps.Copy(copied, operations)
	return copied, true
}

func (o *operations) settle(datasetID, operationID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.unresolved[datasetID], operationID)
}

// resolveInBackground keeps resolving the unresolved operations of one
// dataset, with backoff, until none are left.
func (o *operations) resolveInBackground(datasetID string) {
	log := logf.Log.WithName("storage-client").WithValues("dataset", datasetID)
	delay := o.resolveBackoff
	for {
		time.Sleep(delay)
		operations, ok := o.pending(datasetID)
		if !ok {
			return
		}
		ids := make([]string, 0, len(operations))
		for id := range operations {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			operation := operations[id]
			resolver := o.resolvers[operation.contract]
			ctx, cancel := context.WithTimeout(context.Background(), o.resolveTimeout)
			outcome, err := resolver.ResolveOperation(ctx, operation.request)
			cancel()
			switch {
			case err == nil:
				log.Info("Resolved a write whose outcome was unknown", "operation", id,
					"method", operation.request.Method, "state", outcome.State)
				o.settle(datasetID, id)
			case errors.Is(err, storagev1.ErrOperationExpired), unsettleable(err):
				log.Error(err, "A write's outcome cannot be established; the work that issued it needs recovery",
					"operation", id, "method", operation.request.Method)
				o.settle(datasetID, id)
			default:
				log.V(1).Info("A write's outcome is still unknown", "operation", id, "error", err.Error())
			}
		}
		delay = min(delay*2, o.resolveMaxBackoff)
	}
}

// backoff returns the wait before retry number retry: exponential from
// initial, capped at maximum, with up to 20% jitter.
func backoff(retry int, initial, maximum time.Duration) time.Duration {
	wait := initial
	for range retry {
		wait *= 2
		if wait >= maximum {
			wait = maximum
			break
		}
	}
	return wait - time.Duration(rand.Int64N(int64(wait)/5+1)) //nolint:gosec // jitter needs no cryptographic randomness
}

// sleep waits for d and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
