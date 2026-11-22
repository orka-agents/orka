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
//go:generate go run ./internal/envelopegen/cmd/envelopegen storagetest ../../pkg/storage/v1/storagetest/envelope_gen.go

type FenceSource interface {
	WriteFence(ctx context.Context, contract string) (datasetID string, fence storagev1.ControllerEpochFence, err error)
}

const (
	DefaultFenceWait      = 10 * time.Second
	DefaultRetryHorizon   = 30 * time.Second
	DefaultResolveTimeout = 5 * time.Second
)

const (
	retryInitialBackoff      = 50 * time.Millisecond
	retryMaxBackoff          = 2 * time.Second
	defaultResolveBackoff    = time.Second
	defaultResolveMaxBackoff = 30 * time.Second
)

type envelopedState struct {
	storagev1.StateProvider
	ops *operations
}

type envelopedArtifacts struct {
	storagev1.ArtifactProvider
	ops *operations
}

type writeCall struct {
	method   string
	contract string
	args     []any
	fence    *storagev1.ControllerEpochFence
}

type operations struct {
	resolvers map[string]storagev1.OperationReceiptStore

	fencesMu    sync.Mutex
	fences      FenceSource
	fencesBound chan struct{}

	fenceWait         time.Duration
	retryHorizon      time.Duration
	resolveTimeout    time.Duration
	resolveBackoff    time.Duration
	resolveMaxBackoff time.Duration
	now               func() time.Time
	newOperationID    func() (string, error)
	sequence          atomic.Uint64

	mu         sync.Mutex
	unresolved map[string]map[string]pendingOperation
	resolving  map[string]bool
	clockSkews map[string]clockSkew
}

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
		clockSkews:        map[string]clockSkew{},
	}
}

func newOperationID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return id.String(), nil
}

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

func (o *operations) writeFence(ctx context.Context, contract string) (string, storagev1.ControllerEpochFence, error) {
	waitCtx, cancel := context.WithTimeout(ctx, o.fenceWait)
	defer cancel()
	notReady := func(cause error) error {
		if ctx.Err() != nil {
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

func (o *operations) run(ctx context.Context, call writeCall, attempt func(context.Context) error) (err error) {
	defer func() {
		if errors.Is(err, storagev1.ErrOutcomeUnknown) {
			if unknown, ok := ctx.Value(taskDataOutcomeKey{}).(*atomic.Bool); ok {
				unknown.Store(true)
			}
		}
	}()
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
	if err := o.requireClockAgreement(datasetID); err != nil {
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
		IssuedAt:      issuedAt(ctx, o.now),
		Sequence:      o.sequence.Add(1),
	}
	return o.drive(ctx, call, envelope, attempt)
}

type issueTimeKey struct{}

func (o *operations) withIssueTime(ctx context.Context) (context.Context, time.Time) {
	now := o.now().UTC()
	return context.WithValue(ctx, issueTimeKey{}, now), now
}

func issuedAt(ctx context.Context, now func() time.Time) time.Time {
	if at, ok := ctx.Value(issueTimeKey{}).(time.Time); ok {
		return at
	}
	return now().UTC()
}

func (o *operations) drive(
	ctx context.Context, call writeCall, envelope storagev1.WriteEnvelope, attempt func(context.Context) error,
) error {
	var deadline, capAt time.Time
	unknown := false
	var last error
	for retry := 0; ; retry++ {
		capped, err := attemptWithin(ctx, capAt, envelope, attempt)
		switch {
		case err == nil:
			return nil
		case capped:
			return o.resolve(ctx, call, envelope, attempt,
				fmt.Errorf("%w: the retry horizon ended an attempt in flight: %v", storagev1.ErrOutcomeUnknown, err))
		case errors.Is(err, storagev1.ErrOutcomeUnknown):
			unknown = true
		case errors.Is(err, storagev1.ErrUnavailable):
		case errors.Is(err, storagev1.ErrOperationAborted):
			return err
		case errors.Is(err, storagev1.ErrClockSkew):
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
		if deadline.IsZero() {
			deadline, capAt = o.retryBounds(ctx, envelope)
		}
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

func (o *operations) retryBounds(ctx context.Context, envelope storagev1.WriteEnvelope) (deadline, capAt time.Time) {
	deadline = o.now().Add(o.retryHorizon)
	if lifetime := envelope.IssuedAt.Add(storagev1.MaxOperationAge - storagev1.MaxClockSkew); lifetime.Before(deadline) {
		deadline = lifetime
	}
	if ctxDeadline, ok := ctx.Deadline(); ok && !deadline.Before(ctxDeadline) {
		return ctxDeadline, time.Time{}
	}
	return deadline, deadline
}

func attemptWithin(
	ctx context.Context, deadline time.Time, envelope storagev1.WriteEnvelope, attempt func(context.Context) error,
) (capped bool, err error) {
	attemptCtx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	err = attempt(storagev1.WithWriteEnvelope(attemptCtx, envelope))
	return err != nil && attemptCtx.Err() != nil && ctx.Err() == nil, err
}

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
		if errors.Is(cause, storagev1.ErrOutcomeUnknown) || errors.Is(cause, storagev1.ErrUnavailable) {
			return fmt.Errorf("%w: operation %s did not commit and never will (%w after %v)",
				storagev1.ErrOperationAborted, envelope.OperationID, storagev1.ErrUnavailable, cause)
		}
		return fmt.Errorf("%w: operation %s did not commit and never will: %w", storagev1.ErrOperationAborted, envelope.OperationID, cause)
	case err == nil:
		err = fmt.Errorf("unexpected operation state %q", outcome.State)
	case errors.Is(err, storagev1.ErrOperationExpired), unsettleable(err):
		return fmt.Errorf("%w: operation %s cannot be settled: %v (after %v)", storagev1.ErrOutcomeUnknown, envelope.OperationID, err, cause)
	}
	o.addUnresolved(envelope.DatasetID, envelope.OperationID, pendingOperation{contract: call.contract, request: request})
	return fmt.Errorf("%w: operation %s is not settled yet: %v (after %v)", storagev1.ErrOutcomeUnknown, envelope.OperationID, err, cause)
}

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

func unsettleable(err error) bool {
	return errors.Is(err, storagev1.ErrDuplicateMismatch) ||
		errors.Is(err, storagev1.ErrDatasetMismatch) ||
		errors.Is(err, storagev1.ErrValidation)
}

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

func (o *operations) unresolvedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for _, operations := range o.unresolved {
		count += len(operations)
	}
	return count
}

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
