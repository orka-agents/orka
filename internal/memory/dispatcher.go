package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	defaultMemoryOperationLease  = 45 * time.Second
	defaultMemoryRequestDeadline = 15 * time.Second
	maxMemorySemanticAttempts    = 10
	maxMemoryRetryDelay          = 15 * time.Minute
	memoryDispatcherActor        = "orka-memory-dispatcher"
	memoryOperationExpiredCode   = "MEMORY_OPERATION_EXPIRED"
	memoryOperationRetryableCode = "MEMORY_ADAPTER_RETRYABLE"
	memoryOperationRejectedCode  = "MEMORY_ADAPTER_REJECTED"
)

// Dispatcher durably advances one operation at a time through the OMS send boundary.
type Dispatcher struct {
	Store                store.GovernedMemoryStore
	Resolver             AuthorityResolver
	LeaseOwner           string
	LeaseDuration        time.Duration
	RequestDeadline      time.Duration
	GlobalConcurrency    int
	NamespaceConcurrency int
	GlobalRPS            float64
	NamespaceRPS         float64
	GlobalBurst          int
	NamespaceBurst       int
	RetryJitterFraction  float64
	RandFloat64          func() float64
	Now                  func() time.Time
	circuitMu            sync.Mutex
	circuits             map[string]memoryCircuitState
	limitMu              sync.Mutex
	globalSlots          chan struct{}
	namespaceSlots       map[string]chan struct{}
	globalRate           *rate.Limiter
	namespaceRates       map[string]*rate.Limiter
	limitsInitialized    bool
	leaseOwnerOnce       sync.Once
	generatedOwner       string
}

type memoryCircuitState struct {
	failures    int
	openedUntil time.Time
}

// DispatchOne claims and attempts one due background operation for a namespace.
func (d *Dispatcher) DispatchOne(ctx context.Context, namespace string) (*store.MemoryOperation, error) {
	return d.dispatchOne(ctx, namespace, "", false)
}

// DispatchImmediate attempts exactly one newly admitted operation and atomically
// records the immediate success response shape for caller-idempotency replay.
func (d *Dispatcher) DispatchImmediate(ctx context.Context, namespace, operationID string) (*store.MemoryOperation, error) {
	if strings.TrimSpace(operationID) == "" {
		return nil, store.ErrValidation
	}
	return d.dispatchOne(ctx, namespace, operationID, true)
}

// dispatchOne sends no work unless the binding, CR, endpoint, capability status,
// and Secret are all freshly revalidated.
//
//nolint:gocyclo // Durable dispatch intentionally validates every state and receipt boundary.
func (d *Dispatcher) dispatchOne(ctx context.Context, namespace, operationID string, finalizeImmediate bool) (*store.MemoryOperation, error) {
	if d == nil || d.Store == nil || d.Resolver == nil {
		return nil, fmt.Errorf("memory dispatcher is not configured")
	}
	release, err := d.acquireDispatchGuard(namespace)
	if err != nil {
		return nil, err
	}
	defer release()

	localAuthority, err := d.Resolver.ResolveLocal(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if !localAuthority.Remote() || localAuthority.Binding == nil {
		return nil, store.ErrNotFound
	}

	now := d.now()
	claimRequest := store.MemoryOperationClaim{
		NamespaceUID: localAuthority.NamespaceUID, BackendUID: localAuthority.Binding.BackendUID,
		AuthorityEpoch: localAuthority.Binding.AuthorityEpoch, RoutingEpoch: localAuthority.Binding.RoutingEpoch,
		LeaseOwner: d.leaseOwner(), Now: now, LeaseDuration: d.leaseDuration(),
		AllowExpiredValidationMaintenance: !localAuthority.Binding.ValidationExpiresAt.UTC().After(now),
	}
	var claim *store.MemoryOperationDispatch
	if operationID == "" {
		claim, err = d.Store.ClaimNextMemoryOperation(ctx, claimRequest)
	} else {
		claim, err = d.Store.ClaimMemoryOperation(ctx, operationID, claimRequest)
	}
	if err != nil {
		return nil, err
	}
	operation := claim.Operation
	if operationID != "" && operation.ID != operationID {
		return nil, fmt.Errorf("%w: exact memory operation claim returned a different operation", store.ErrConflict)
	}
	if !operation.MaxAgeAt.After(now) {
		return d.retry(ctx, operation, true, false, memoryOperationExpiredCode, "memory operation exceeded its maximum age", time.Time{})
	}

	authority, err := d.Resolver.Resolve(ctx, namespace)
	if err != nil {
		return d.releaseUnsentClaim(ctx, operation, localAuthority.Binding, err)
	}
	if !authority.Remote() || authority.Backend == nil || authority.Adapter == nil || authority.Binding == nil ||
		authority.Binding.BackendUID != operation.BackendUID || authority.Binding.AuthorityEpoch != operation.AuthorityEpoch ||
		authority.Binding.RoutingEpoch != operation.RoutingEpoch {
		return d.releaseUnsentClaim(ctx, operation, localAuthority.Binding, store.ErrNotReady)
	}
	effective := authority.Backend.Status.EffectiveLifecycleState
	if effective != corev1alpha1.MemoryBackendEffectiveLifecycleActive &&
		effective != corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly &&
		effective != corev1alpha1.MemoryBackendEffectiveLifecycleDraining &&
		effective != corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioning {
		return d.releaseUnsentClaim(ctx, operation, authority.Binding, store.ErrNotReady)
	}
	if (effective == corev1alpha1.MemoryBackendEffectiveLifecycleActive ||
		effective == corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly) && !authority.Backend.Status.Ready {
		return d.releaseUnsentClaim(ctx, operation, authority.Binding, store.ErrNotReady)
	}
	now = d.now()
	if !operation.MaxAgeAt.After(now) {
		return d.retry(ctx, operation, true, false, memoryOperationExpiredCode,
			"memory operation exceeded its maximum age", time.Time{})
	}
	endpointCircuitKey := memoryEndpointCircuitKey(authority.Binding)
	credentialCircuitKey := memoryCredentialCircuitKey(authority.Binding)
	if d.circuitOpen(endpointCircuitKey, now) || d.circuitOpen(credentialCircuitKey, now) {
		return d.releaseUnsentClaimWithoutCircuitFailure(ctx, operation, authority.Binding, store.ErrNotReady)
	}

	envelope, err := protocol.DecodeMutationEnvelope(claim.Payload)
	if err != nil {
		return d.retry(ctx, operation, true, false, memoryOperationRejectedCode,
			"stored mutation envelope failed strict validation", time.Time{})
	}
	binding, err := protocolBinding(authority.Binding)
	if err != nil || envelope.Binding != binding || envelope.OperationID != operation.ID ||
		envelope.MutationDigest != operation.MutationDigest || int64(envelope.Generation) != operation.DesiredGeneration {
		return d.retry(ctx, operation, true, false, ReasonIdentityMismatch,
			"stored mutation no longer matches the active binding", time.Time{})
	}

	marked, err := d.Store.MarkMemoryOperationSendStarted(ctx, store.MemoryOperationSend{
		NamespaceUID:    operation.NamespaceUID,
		ID:              operation.ID,
		BackendUID:      operation.BackendUID,
		AuthorityEpoch:  operation.AuthorityEpoch,
		RoutingEpoch:    operation.RoutingEpoch,
		LeaseOwner:      d.leaseOwner(),
		LeaseEpoch:      operation.LeaseEpoch,
		Now:             now,
		RequestDeadline: now.Add(d.requestDeadline()),
	})
	if err != nil {
		return nil, err
	}
	operation = *marked

	requestCtx, cancel := context.WithDeadline(ctx, memoryOperationRequestDeadline(operation, now.Add(d.requestDeadline())))
	receipt, sendErr := authority.Adapter.Mutate(requestCtx, *envelope)
	cancel()
	if sendErr != nil {
		return d.handleSendError(ctx, operation, authority.Binding, sendErr)
	}
	if receipt.Binding != binding || receipt.OperationID != operation.ID || receipt.MutationDigest != operation.MutationDigest ||
		!mutationReceiptMatchesEnvelope(*envelope, receipt) {
		return d.retry(ctx, operation, false, true, ReasonIdentityMismatch,
			"OMS receipt identity did not match the dispatched operation", d.nextRetry(operation, 0))
	}

	switch receipt.Result {
	case protocol.ResultApplied:
		d.recordCircuitSuccess(endpointCircuitKey)
		d.recordCircuitSuccess(credentialCircuitKey)
		return d.complete(ctx, operation, receipt, finalizeImmediate)
	case protocol.ResultNotFound:
		if operation.Kind != store.MemoryOperationDelete {
			return d.retry(ctx, operation, false, true, memoryOperationRejectedCode,
				"OMS returned notFound for a non-delete mutation", d.nextRetry(operation, 0))
		}
		d.recordCircuitSuccess(endpointCircuitKey)
		d.recordCircuitSuccess(credentialCircuitKey)
		return d.complete(ctx, operation, receipt, finalizeImmediate)
	case protocol.ResultRetryableError:
		return d.retry(ctx, operation, false, false, memoryOperationRetryableCode, "OMS returned a retryable operation result", d.nextRetry(operation, 0))
	case protocol.ResultPreconditionFailed, protocol.ResultIdempotencyConflict,
		protocol.ResultIdentityConflict, protocol.ResultNonRetryableError:
		return d.retry(ctx, operation, true, false, memoryOperationRejectedCode, "OMS rejected the operation", time.Time{})
	default:
		return d.retry(ctx, operation, false, true, memoryOperationRejectedCode,
			"OMS returned an unknown result", d.nextRetry(operation, 0))
	}
}

func mutationReceiptMatchesEnvelope(envelope protocol.MutationEnvelope, receipt *protocol.MutationReceipt) bool {
	if receipt == nil {
		return false
	}
	if receipt.Result != protocol.ResultApplied && receipt.Result != protocol.ResultNotFound {
		return true
	}
	expectedContentDigest := envelope.ContentDigest
	if envelope.Kind == protocol.MutationKindDelete {
		expectedContentDigest = protocol.EmptyContentDigest()
	}
	return receipt.AppliedGeneration == envelope.Generation && receipt.ContentDigest == expectedContentDigest
}

func (d *Dispatcher) complete(
	ctx context.Context,
	operation store.MemoryOperation,
	receipt *protocol.MutationReceipt,
	finalizeImmediate bool,
) (*store.MemoryOperation, error) {
	outcome := store.MemoryIdempotencyOutcome{}
	if finalizeImmediate {
		outcome = immediateIdempotencyOutcome(operation, receipt)
	}
	memoryDispatchTotal.WithLabelValues("succeeded").Inc()
	if !operation.CreatedAt.IsZero() {
		memoryMaterializationLatency.Observe(d.now().Sub(operation.CreatedAt).Seconds())
	}
	return d.Store.CompleteMemoryOperation(ctx, store.MemoryOperationCompletion{
		NamespaceUID:   operation.NamespaceUID,
		ID:             operation.ID,
		BackendUID:     operation.BackendUID,
		AuthorityEpoch: operation.AuthorityEpoch,
		RoutingEpoch:   operation.RoutingEpoch,
		LeaseOwner:     d.leaseOwner(),
		LeaseEpoch:     operation.LeaseEpoch,
		Receipt: store.MemoryOperationReceipt{
			BindingIdentityDigest: receipt.BindingDigest,
			AppliedGeneration:     int64(receipt.AppliedGeneration),
			BackendVersion:        receipt.BackendVersion,
			BackendMemoryID:       receipt.BackendMemoryID,
			ContentDigest:         receipt.ContentDigest,
			MutationDigest:        receipt.MutationDigest,
			CompletedAt:           receipt.CompletedAt,
		},
		FinalizeIdempotencyOutcome: finalizeImmediate,
		IdempotencyOutcome:         outcome,
		Now:                        d.now(),
		Actor:                      memoryDispatcherActor,
		Reason:                     "OMS operation materialized",
	})
}

func immediateIdempotencyOutcome(operation store.MemoryOperation, receipt *protocol.MutationReceipt) store.MemoryIdempotencyOutcome {
	status := http.StatusOK
	responseType := store.MemoryIdempotencyMemory
	switch operation.Kind {
	case store.MemoryOperationCreate:
		if operation.ProposalID == "" {
			status = http.StatusCreated
		}
	case store.MemoryOperationReplace:
		status = http.StatusOK
	case store.MemoryOperationDelete:
		status = http.StatusNoContent
		responseType = store.MemoryIdempotencyEmpty
	}
	responseDigest := ""
	if responseType == store.MemoryIdempotencyMemory {
		responseDigest = digestString(operation.MemoryID + "\x00" + receipt.ContentDigest + "\x00" + receipt.MutationDigest)
	}
	return store.MemoryIdempotencyOutcome{Status: status, ResponseType: responseType, ResponseDigest: responseDigest}
}

func (d *Dispatcher) releaseUnsentClaim(
	ctx context.Context,
	operation store.MemoryOperation,
	binding *store.MemoryBackendBinding,
	cause error,
) (*store.MemoryOperation, error) {
	return d.releaseUnsentClaimWithCircuitFailure(ctx, operation, binding, cause, true)
}

func (d *Dispatcher) releaseUnsentClaimWithoutCircuitFailure(
	ctx context.Context,
	operation store.MemoryOperation,
	binding *store.MemoryBackendBinding,
	cause error,
) (*store.MemoryOperation, error) {
	return d.releaseUnsentClaimWithCircuitFailure(ctx, operation, binding, cause, false)
}

func (d *Dispatcher) releaseUnsentClaimWithCircuitFailure(
	ctx context.Context,
	operation store.MemoryOperation,
	binding *store.MemoryBackendBinding,
	cause error,
	recordFailure bool,
) (*store.MemoryOperation, error) {
	now := d.now()
	if recordFailure {
		d.recordCircuitFailure(memoryEndpointCircuitKey(binding), now)
	}
	deadLetter := !operation.MaxAgeAt.After(now)
	ambiguous := operation.LeaseOriginState == store.MemoryOperationAmbiguous
	outcome := "retry"
	if deadLetter {
		outcome = "dead_letter"
	} else if ambiguous {
		outcome = "ambiguous"
	}
	memoryDispatchTotal.WithLabelValues(outcome).Inc()
	released, err := d.Store.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID: operation.NamespaceUID, ID: operation.ID, BackendUID: operation.BackendUID,
		AuthorityEpoch: operation.AuthorityEpoch, RoutingEpoch: operation.RoutingEpoch,
		LeaseOwner: d.leaseOwner(), LeaseEpoch: operation.LeaseEpoch, Ambiguous: ambiguous,
		DeadLetter: deadLetter, UnsentRelease: true, ErrorCode: memoryOperationRetryableCode,
		ErrorMessage: "memory backend was unavailable before send", NextRetryAt: d.nextUnsentRetry(operation),
		Actor: memoryDispatcherActor, Reason: "memory backend was unavailable before send", Now: now,
	})
	if err != nil {
		return nil, err
	}
	return released, cause
}

func (d *Dispatcher) handleSendError(
	ctx context.Context,
	operation store.MemoryOperation,
	binding *store.MemoryBackendBinding,
	err error,
) (*store.MemoryOperation, error) {
	endpointKey := memoryEndpointCircuitKey(binding)
	credentialKey := memoryCredentialCircuitKey(binding)
	var adapterErr *AdapterError
	if errors.As(err, &adapterErr) {
		if dependencyFailure := adapterDependencyFailure(adapterErr); dependencyFailure {
			circuitKey := endpointKey
			if adapterErr.StatusCode == http.StatusUnauthorized || adapterErr.StatusCode == http.StatusForbidden ||
				adapterErr.StatusCode == http.StatusTooManyRequests {
				circuitKey = credentialKey
			}
			d.recordCircuitFailure(circuitKey, d.now())
			return d.retry(ctx, operation, false, adapterErr.Retryable, adapterErr.Code, adapterErr.Message,
				d.nextRetry(operation, time.Duration(adapterErr.RetryAfterSeconds)*time.Second), true)
		}
		if adapterErr.Retryable {
			return d.retry(ctx, operation, false, true, adapterErr.Code, adapterErr.Message,
				d.nextRetry(operation, time.Duration(adapterErr.RetryAfterSeconds)*time.Second))
		}
		return d.retry(ctx, operation, true, false, adapterErr.Code, adapterErr.Message, time.Time{})
	}
	// The durable send boundary was crossed before the transport error. Replay of
	// the same operation ID is safe, but the local state must remain ambiguous.
	d.recordCircuitFailure(endpointKey, d.now())
	return d.retry(ctx, operation, false, true, memoryOperationRetryableCode,
		"OMS acknowledgement was ambiguous", d.nextRetry(operation, 0), true)
}

func adapterDependencyFailure(err *AdapterError) bool {
	if err == nil {
		return false
	}
	return err.StatusCode == 0 || err.StatusCode == http.StatusUnauthorized || err.StatusCode == http.StatusForbidden ||
		err.StatusCode == http.StatusTooManyRequests || err.StatusCode >= http.StatusInternalServerError
}

func (d *Dispatcher) retry(
	ctx context.Context,
	operation store.MemoryOperation,
	deadLetter, ambiguous bool,
	code, message string,
	next time.Time,
	dependencyFailure ...bool,
) (*store.MemoryOperation, error) {
	isDependencyFailure := len(dependencyFailure) > 0 && dependencyFailure[0]
	if !isDependencyFailure && !deadLetter && !ambiguous && operation.Attempts >= maxMemorySemanticAttempts {
		deadLetter = true
		ambiguous = false
		code = memoryOperationRejectedCode
		message = "memory operation exceeded its semantic retry budget"
	}
	if next.IsZero() {
		next = d.now()
	}
	outcome := "retry"
	if deadLetter {
		outcome = "dead_letter"
	} else if ambiguous {
		outcome = "ambiguous"
	}
	memoryDispatchTotal.WithLabelValues(outcome).Inc()
	return d.Store.RetryMemoryOperation(ctx, store.MemoryOperationRetry{
		NamespaceUID:      operation.NamespaceUID,
		ID:                operation.ID,
		BackendUID:        operation.BackendUID,
		AuthorityEpoch:    operation.AuthorityEpoch,
		RoutingEpoch:      operation.RoutingEpoch,
		LeaseOwner:        d.leaseOwner(),
		LeaseEpoch:        operation.LeaseEpoch,
		Ambiguous:         ambiguous,
		DeadLetter:        deadLetter,
		DependencyFailure: isDependencyFailure,
		ErrorCode:         code,
		ErrorMessage:      message,
		NextRetryAt:       next,
		Actor:             memoryDispatcherActor,
		Reason:            message,
		Now:               d.now(),
	})
}

func (d *Dispatcher) nextRetry(operation store.MemoryOperation, providerDelay time.Duration) time.Time {
	return d.nextRetryForAttempt(int64(max(1, operation.Attempts)), providerDelay)
}

func (d *Dispatcher) nextUnsentRetry(operation store.MemoryOperation) time.Time {
	return d.nextRetryForAttempt(max(int64(1), operation.LeaseEpoch), 0)
}

func (d *Dispatcher) nextRetryForAttempt(attempt int64, providerDelay time.Duration) time.Time {
	delay := min(max(providerDelay, time.Duration(math.Pow(2, float64(min(attempt, int64(10)))))*time.Second), maxMemoryRetryDelay)
	fraction := d.retryJitterFraction()
	if fraction > 0 && delay < maxMemoryRetryDelay {
		jitterCap := min(time.Duration(float64(delay)*fraction), maxMemoryRetryDelay-delay)
		if jitterCap > 0 {
			delay += time.Duration(d.randomFloat64() * float64(jitterCap))
		}
	}
	return d.now().Add(delay)
}

func (d *Dispatcher) retryJitterFraction() float64 {
	if d.RetryJitterFraction < 0 {
		return 0
	}
	if d.RetryJitterFraction == 0 {
		return 0.2
	}
	return min(d.RetryJitterFraction, 0.5)
}

func (d *Dispatcher) randomFloat64() float64 {
	if d.RandFloat64 != nil {
		value := d.RandFloat64()
		return min(max(value, 0), math.Nextafter(1, 0))
	}
	return rand.Float64()
}

func (d *Dispatcher) acquireDispatchGuard(namespace string) (func(), error) {
	globalSlots, namespaceSlots, globalRate, namespaceRate := d.dispatchGuards(namespace)
	releaseGlobal := false
	releaseNamespace := false
	release := func() {
		if releaseNamespace {
			<-namespaceSlots
		}
		if releaseGlobal {
			<-globalSlots
		}
	}
	if globalSlots != nil {
		select {
		case globalSlots <- struct{}{}:
			releaseGlobal = true
		default:
			return func() {}, store.ErrNotReady
		}
	}
	if namespaceSlots != nil {
		select {
		case namespaceSlots <- struct{}{}:
			releaseNamespace = true
		default:
			release()
			return func() {}, store.ErrNotReady
		}
	}
	now := d.now()
	var namespaceReservation *rate.Reservation
	if namespaceRate != nil {
		namespaceReservation = namespaceRate.ReserveN(now, 1)
		if !namespaceReservation.OK() || namespaceReservation.DelayFrom(now) > 0 {
			namespaceReservation.CancelAt(now)
			release()
			return func() {}, store.ErrNotReady
		}
	}
	if globalRate != nil {
		globalReservation := globalRate.ReserveN(now, 1)
		if !globalReservation.OK() || globalReservation.DelayFrom(now) > 0 {
			globalReservation.CancelAt(now)
			if namespaceReservation != nil {
				namespaceReservation.CancelAt(now)
			}
			release()
			return func() {}, store.ErrNotReady
		}
	}
	return release, nil
}

func (d *Dispatcher) dispatchGuards(namespace string) (chan struct{}, chan struct{}, *rate.Limiter, *rate.Limiter) {
	d.limitMu.Lock()
	defer d.limitMu.Unlock()
	if !d.limitsInitialized {
		if d.GlobalConcurrency > 0 {
			d.globalSlots = make(chan struct{}, d.GlobalConcurrency)
		}
		if d.NamespaceConcurrency > 0 {
			d.namespaceSlots = make(map[string]chan struct{})
		}
		if d.GlobalRPS > 0 {
			d.globalRate = rate.NewLimiter(rate.Limit(d.GlobalRPS), dispatchBurst(d.GlobalBurst))
		}
		if d.NamespaceRPS > 0 {
			d.namespaceRates = make(map[string]*rate.Limiter)
		}
		d.limitsInitialized = true
	}
	key := strings.TrimSpace(namespace)
	var namespaceSlots chan struct{}
	if d.NamespaceConcurrency > 0 {
		namespaceSlots = d.namespaceSlots[key]
		if namespaceSlots == nil {
			namespaceSlots = make(chan struct{}, d.NamespaceConcurrency)
			d.namespaceSlots[key] = namespaceSlots
		}
	}
	var namespaceRate *rate.Limiter
	if d.NamespaceRPS > 0 {
		namespaceRate = d.namespaceRates[key]
		if namespaceRate == nil {
			namespaceRate = rate.NewLimiter(rate.Limit(d.NamespaceRPS), dispatchBurst(d.NamespaceBurst))
			d.namespaceRates[key] = namespaceRate
		}
	}
	return d.globalSlots, namespaceSlots, d.globalRate, namespaceRate
}

func dispatchBurst(configured int) int {
	if configured > 0 {
		return configured
	}
	return 1
}

func memoryEndpointCircuitKey(binding *store.MemoryBackendBinding) string {
	if binding == nil {
		return ""
	}
	return "endpoint:" + binding.EndpointDigest
}

func memoryCredentialCircuitKey(binding *store.MemoryBackendBinding) string {
	if binding == nil {
		return ""
	}
	return "credential:" + digestString(strings.Join([]string{
		binding.EndpointDigest, binding.BackendUID, binding.SecretUID, binding.SecretResourceVersion,
	}, "\x00"))
}

func (d *Dispatcher) circuitOpen(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	d.circuitMu.Lock()
	defer d.circuitMu.Unlock()
	state := d.circuits[key]
	return state.openedUntil.After(now)
}

func (d *Dispatcher) recordCircuitFailure(key string, now time.Time) {
	if key == "" {
		return
	}
	d.circuitMu.Lock()
	defer d.circuitMu.Unlock()
	if d.circuits == nil {
		d.circuits = make(map[string]memoryCircuitState)
	}
	state := d.circuits[key]
	state.failures++
	if state.failures >= 5 {
		delay := time.Duration(min(state.failures-4, 6)) * 30 * time.Second
		state.openedUntil = now.Add(delay)
	}
	d.circuits[key] = state
}

func (d *Dispatcher) recordCircuitSuccess(key string) {
	if key == "" {
		return
	}
	d.circuitMu.Lock()
	defer d.circuitMu.Unlock()
	delete(d.circuits, key)
}

func (d *Dispatcher) leaseOwner() string {
	if owner := strings.TrimSpace(d.LeaseOwner); owner != "" {
		return owner
	}
	d.leaseOwnerOnce.Do(func() {
		d.generatedOwner = "memory-dispatcher-" + uuid.NewString()
	})
	return d.generatedOwner
}

func (d *Dispatcher) leaseDuration() time.Duration {
	if d.LeaseDuration <= d.requestDeadline() {
		return defaultMemoryOperationLease
	}
	return d.LeaseDuration
}

func (d *Dispatcher) requestDeadline() time.Duration {
	if d.RequestDeadline <= 0 || d.RequestDeadline >= defaultMemoryOperationLease {
		return defaultMemoryRequestDeadline
	}
	return d.RequestDeadline
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func memoryOperationRequestDeadline(operation store.MemoryOperation, fallback time.Time) time.Time {
	if operation.RequestDeadline != nil && !operation.RequestDeadline.IsZero() {
		return operation.RequestDeadline.UTC()
	}
	return fallback.UTC()
}
