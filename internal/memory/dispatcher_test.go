package memory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

type exactClaimTestStore struct {
	store.GovernedMemoryStore
	requestedID string
	reject      bool
}

func (s *exactClaimTestStore) ClaimMemoryOperation(
	ctx context.Context,
	id string,
	claim store.MemoryOperationClaim,
) (*store.MemoryOperationDispatch, error) {
	s.requestedID = id
	if s.reject {
		return nil, store.ErrNotReady
	}
	dispatch, err := s.ClaimNextMemoryOperation(ctx, claim)
	if err != nil {
		return nil, err
	}
	if dispatch.Operation.ID != id {
		return nil, store.ErrConflict
	}
	return dispatch, nil
}

type fakeOMSAdapter struct {
	mu                sync.Mutex
	mutations         int
	failFirst         bool
	failuresRemaining int
	record            *protocol.MemoryRecord
	receipt           *protocol.MutationReceipt
}

func (f *fakeOMSAdapter) Capabilities(context.Context, protocol.CapabilitiesRequest) (*protocol.CapabilitiesResponse, error) {
	return nil, errors.New("unused")
}
func (f *fakeOMSAdapter) ClaimOwnership(context.Context, protocol.OwnershipClaimRequest) (*protocol.OwnershipClaimResponse, error) {
	return nil, errors.New("unused")
}
func (f *fakeOMSAdapter) AdvanceRoutingFence(context.Context, protocol.RoutingFenceRequest) (*protocol.RoutingFenceResponse, error) {
	return nil, errors.New("unused")
}
func (f *fakeOMSAdapter) Mutate(_ context.Context, envelope protocol.MutationEnvelope) (*protocol.MutationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations++
	if f.failuresRemaining > 0 {
		f.failuresRemaining--
		return nil, errors.New("endpoint unavailable")
	}
	if f.failFirst && f.mutations == 1 {
		return nil, errors.New("lost acknowledgement")
	}
	receipt := &protocol.MutationReceipt{
		ProtocolVersion: protocol.Version, Binding: envelope.Binding, Result: protocol.ResultApplied,
		OperationID: envelope.OperationID, BindingDigest: protocol.BindingDigest(envelope.Binding),
		AppliedGeneration: envelope.Generation, BackendVersion: "v1", BackendMemoryID: "backend-1",
		ContentDigest: envelope.ContentDigest, MutationDigest: envelope.MutationDigest,
		CompletedAt: time.Date(2026, 7, 28, 12, 1, 0, 0, time.UTC),
	}
	f.receipt = receipt
	if envelope.State != nil {
		f.record = &protocol.MemoryRecord{
			MemoryID: envelope.MemoryID, UpsertKey: envelope.UpsertKey, State: protocol.RecordStateLive,
			Generation: envelope.Generation, BackendVersion: receipt.BackendVersion, BackendMemoryID: receipt.BackendMemoryID,
			ContentDigest: envelope.ContentDigest, Content: envelope.State.Content,
			Tags: envelope.State.Tags, Metadata: envelope.State.Metadata, UpdatedAt: receipt.CompletedAt,
		}
	}
	return receipt, nil
}
func (f *fakeOMSAdapter) Get(_ context.Context, request protocol.GetRequest) (*protocol.GetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &protocol.GetResponse{ProtocolVersion: protocol.Version, Binding: request.Binding, Found: f.record != nil, Record: f.record}, nil
}
func (f *fakeOMSAdapter) LookupOperation(context.Context, protocol.OperationLookupRequest) (*protocol.OperationLookupResponse, error) {
	return nil, errors.New("unused")
}
func (f *fakeOMSAdapter) Search(context.Context, protocol.SearchRequest) (*protocol.SearchResponse, error) {
	return nil, errors.New("unused")
}

func TestMutationReceiptMatchesEnvelopeCorrelatesGenerationAndContentDigest(t *testing.T) {
	liveDigest := protocol.ContentDigest("remember this")
	tests := []struct {
		name     string
		envelope protocol.MutationEnvelope
		receipt  protocol.MutationReceipt
		want     bool
	}{
		{
			name:     "live match",
			envelope: protocol.MutationEnvelope{Kind: protocol.MutationKindReplace, Generation: 3, ContentDigest: liveDigest},
			receipt:  protocol.MutationReceipt{Result: protocol.ResultApplied, AppliedGeneration: 3, ContentDigest: liveDigest},
			want:     true,
		},
		{
			name:     "generation mismatch",
			envelope: protocol.MutationEnvelope{Kind: protocol.MutationKindReplace, Generation: 3, ContentDigest: liveDigest},
			receipt:  protocol.MutationReceipt{Result: protocol.ResultApplied, AppliedGeneration: 2, ContentDigest: liveDigest},
		},
		{
			name:     "content mismatch",
			envelope: protocol.MutationEnvelope{Kind: protocol.MutationKindReplace, Generation: 3, ContentDigest: liveDigest},
			receipt:  protocol.MutationReceipt{Result: protocol.ResultApplied, AppliedGeneration: 3, ContentDigest: protocol.ContentDigest("other")},
		},
		{
			name:     "delete empty digest",
			envelope: protocol.MutationEnvelope{Kind: protocol.MutationKindDelete, Generation: 4, ContentDigest: protocol.EmptyContentDigest()},
			receipt:  protocol.MutationReceipt{Result: protocol.ResultNotFound, AppliedGeneration: 4, ContentDigest: protocol.EmptyContentDigest()},
			want:     true,
		},
		{
			name:     "delete nonempty digest",
			envelope: protocol.MutationEnvelope{Kind: protocol.MutationKindDelete, Generation: 4, ContentDigest: protocol.EmptyContentDigest()},
			receipt:  protocol.MutationReceipt{Result: protocol.ResultApplied, AppliedGeneration: 4, ContentDigest: liveDigest},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mutationReceiptMatchesEnvelope(test.envelope, &test.receipt); got != test.want {
				t.Fatalf("mutationReceiptMatchesEnvelope() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDispatcherImmediateMaterializationAndReplay(t *testing.T) {
	governed := newMemoryTestStore(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	binding := activateServiceTestBinding(t, governed, now)
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	adapter := &fakeOMSAdapter{}
	authority := &ResolvedAuthority{Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding, Backend: backend, Adapter: adapter}
	resolver := staticAuthorityResolver{authority: authority}
	exactStore := &exactClaimTestStore{GovernedMemoryStore: governed}
	dispatcher := &Dispatcher{Store: exactStore, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time { return now.Add(time.Minute) }}
	service := &Service{Legacy: governed, Proposals: governed, Governed: governed, Resolver: resolver, Dispatcher: dispatcher, Now: func() time.Time { return now.Add(time.Minute) }}
	request := CreateRequest{Content: "materialize me", Tags: []string{"storage"}}
	mutation := MutationContext{Actor: "alice", Principal: "alice", Route: "createMemory", IdempotencyKey: "immediate-key"}
	result, err := service.CreateMemory(context.Background(), binding.Namespace, request, mutation)
	if err != nil {
		t.Fatalf("CreateMemory() error = %v", err)
	}
	if result.StatusCode != 201 || result.Memory == nil || result.Memory.Content != "materialize me" || result.Operation != nil {
		t.Fatalf("result = %#v", result)
	}
	replay, err := service.CreateMemory(context.Background(), binding.Namespace, request, mutation)
	if err != nil {
		t.Fatalf("replay error = %v", err)
	}
	if replay.StatusCode != 201 || replay.Memory == nil || !replay.Replayed {
		t.Fatalf("replay = %#v", replay)
	}
	if adapter.mutations != 1 {
		t.Fatalf("mutations = %d, want 1", adapter.mutations)
	}
	op, err := governed.ListMemoryOperations(context.Background(), store.MemoryOperationFilter{NamespaceUID: binding.NamespaceUID, Limit: 10})
	if err != nil || len(op) != 1 || op[0].State != store.MemoryOperationSucceeded {
		t.Fatalf("operations = %#v, err=%v", op, err)
	}
	if exactStore.requestedID != op[0].ID {
		t.Fatalf("exact claim requested %q, want %q", exactStore.requestedID, op[0].ID)
	}
}

func TestDispatcherLostAcknowledgementReplaysSameOperationAndPreservesDeferredOutcome(t *testing.T) {
	governed := newMemoryTestStore(t)
	base := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	binding := activateServiceTestBinding(t, governed, base)
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	adapter := &fakeOMSAdapter{failFirst: true}
	authority := &ResolvedAuthority{Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding, Backend: backend, Adapter: adapter}
	resolver := staticAuthorityResolver{authority: authority}
	current := base.Add(time.Minute)
	exactStore := &exactClaimTestStore{GovernedMemoryStore: governed}
	dispatcher := &Dispatcher{Store: exactStore, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time { return current }}
	service := &Service{Legacy: governed, Proposals: governed, Governed: governed, Resolver: resolver, Dispatcher: dispatcher, Now: func() time.Time { return current }}
	request := CreateRequest{Content: "retry me"}
	mutation := MutationContext{Actor: "alice", Principal: "alice", Route: "createMemory", IdempotencyKey: "ambiguous-key"}
	result, err := service.CreateMemory(context.Background(), binding.Namespace, request, mutation)
	if err != nil {
		t.Fatalf("CreateMemory() error = %v", err)
	}
	if result.StatusCode != 202 || result.Operation == nil || result.Operation.State != store.MemoryOperationAmbiguous {
		t.Fatalf("result = %#v", result)
	}
	operationID := result.Operation.ID
	current = current.Add(10 * time.Second)
	completed, err := dispatcher.DispatchOne(context.Background(), binding.Namespace)
	if err != nil {
		t.Fatalf("DispatchOne() error = %v", err)
	}
	if completed.ID != operationID || completed.State != store.MemoryOperationSucceeded || adapter.mutations != 2 {
		t.Fatalf("completed = %#v, mutations=%d", completed, adapter.mutations)
	}
	replay, err := service.CreateMemory(context.Background(), binding.Namespace, request, mutation)
	if err != nil {
		t.Fatalf("replay error = %v", err)
	}
	if replay.StatusCode != 202 || replay.Operation == nil || replay.Operation.ID != operationID || !replay.Replayed {
		t.Fatalf("replay = %#v", replay)
	}
}

func TestDispatcherImmediateRequiresExactClaimSupport(t *testing.T) {
	governed := newMemoryTestStore(t)
	now := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	binding := activateServiceTestBinding(t, governed, now)
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	adapter := &fakeOMSAdapter{}
	authority := &ResolvedAuthority{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID,
		Binding: &binding, Backend: backend, Adapter: adapter,
	}
	resolver := staticAuthorityResolver{authority: authority}
	service := &Service{Governed: governed, Resolver: resolver, Now: func() time.Time { return now.Add(time.Minute) }}
	queued, err := service.CreateMemory(context.Background(), binding.Namespace, CreateRequest{Content: "queued"}, MutationContext{
		Actor: "alice", Principal: "alice", Route: "createMemory", IdempotencyKey: "queued-key",
	})
	if err != nil || queued.Operation == nil {
		t.Fatalf("CreateMemory() = %#v, err=%v", queued, err)
	}
	dispatcher := &Dispatcher{Store: governed, Resolver: resolver, Now: func() time.Time { return now.Add(time.Minute) }}
	if _, err := dispatcher.DispatchImmediate(context.Background(), binding.Namespace, "mop-target"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DispatchImmediate() error = %v, want exact-ID ErrNotFound", err)
	}
	operation, err := governed.GetMemoryOperation(context.Background(), binding.NamespaceUID, queued.Operation.ID)
	if err != nil || operation.State != store.MemoryOperationQueued {
		t.Fatalf("queued operation = %#v, err=%v", operation, err)
	}
	if adapter.mutations != 0 {
		t.Fatalf("mutations = %d, want no fallback dispatch", adapter.mutations)
	}
}

func TestRunnerCircuitOpenBacklogExpiresAndRunsHalfOpenProbe(t *testing.T) {
	governed := newMemoryTestStore(t)
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	binding := activateServiceTestBinding(t, governed, base)
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	adapter := &fakeOMSAdapter{failuresRemaining: 6}
	authority := &ResolvedAuthority{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID,
		Binding: &binding, Backend: backend, Adapter: adapter,
	}
	resolver := staticAuthorityResolver{authority: authority}
	current := base.Add(time.Minute)
	service := &Service{Governed: governed, Resolver: resolver, Now: func() time.Time { return current }}
	for i := range 14 {
		result, err := service.CreateMemory(context.Background(), binding.Namespace,
			CreateRequest{Content: fmt.Sprintf("queued-%d", i)}, MutationContext{
				Actor: "alice", Principal: "alice", Route: "createMemory",
				IdempotencyKey: fmt.Sprintf("circuit-backlog-%d", i),
			})
		if err != nil || result.Operation == nil {
			t.Fatalf("queue operation %d: result=%#v err=%v", i, result, err)
		}
	}
	dispatcher := &Dispatcher{
		Store: governed, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time { return current },
		GlobalConcurrency: 1, NamespaceConcurrency: 1, RetryJitterFraction: -1,
	}
	runner := &Runner{Dispatcher: dispatcher, Store: governed}
	runPass := func() {
		t.Helper()
		if err := runner.runPass(context.Background()); err != nil {
			t.Fatalf("runPass() error = %v", err)
		}
	}
	circuitKey := memoryEndpointCircuitKey(&binding)
	circuitState := func() (memoryCircuitState, bool) {
		dispatcher.circuitMu.Lock()
		defer dispatcher.circuitMu.Unlock()
		state, ok := dispatcher.circuits[circuitKey]
		return state, ok
	}

	for range 5 {
		runPass()
		current = current.Add(time.Second)
	}
	state, ok := circuitState()
	if !ok || state.failures != 5 || !state.openedUntil.After(current) || adapter.mutations != 5 {
		t.Fatalf("opened circuit state=%+v exists=%t mutations=%d", state, ok, adapter.mutations)
	}
	firstOpenedUntil := state.openedUntil

	for _, beforeExpiry := range []time.Duration{20 * time.Second, 10 * time.Second, time.Second} {
		current = firstOpenedUntil.Add(-beforeExpiry)
		runPass()
		state, ok = circuitState()
		if !ok || state.failures != 5 || !state.openedUntil.Equal(firstOpenedUntil) || adapter.mutations != 5 {
			t.Fatalf("circuit-open release renewed state=%+v exists=%t mutations=%d", state, ok, adapter.mutations)
		}
	}

	current = firstOpenedUntil
	runPass()
	state, ok = circuitState()
	if !ok || state.failures != 6 || adapter.mutations != 6 {
		t.Fatalf("failed half-open probe state=%+v exists=%t mutations=%d", state, ok, adapter.mutations)
	}
	if want := current.Add(time.Minute); !state.openedUntil.Equal(want) {
		t.Fatalf("failed half-open probe openedUntil=%s, want %s", state.openedUntil, want)
	}
	secondOpenedUntil := state.openedUntil

	for _, beforeExpiry := range []time.Duration{30 * time.Second, time.Second} {
		current = secondOpenedUntil.Add(-beforeExpiry)
		runPass()
		state, ok = circuitState()
		if !ok || state.failures != 6 || !state.openedUntil.Equal(secondOpenedUntil) || adapter.mutations != 6 {
			t.Fatalf("second circuit-open release renewed state=%+v exists=%t mutations=%d", state, ok, adapter.mutations)
		}
	}

	current = secondOpenedUntil
	runPass()
	if _, ok := circuitState(); ok {
		t.Fatal("successful half-open probe did not close the circuit")
	}
	if adapter.mutations != 7 {
		t.Fatalf("adapter mutations=%d, want 7 actual outbound attempts", adapter.mutations)
	}
	operations, err := governed.ListMemoryOperations(context.Background(), store.MemoryOperationFilter{
		NamespaceUID: binding.NamespaceUID, Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListMemoryOperations() error = %v", err)
	}
	succeeded := 0
	for _, operation := range operations {
		if operation.State == store.MemoryOperationSucceeded {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded operations=%d, want one recovery probe materialization", succeeded)
	}
}

func TestDispatcherLeaseOwnerInitializedRaceFree(t *testing.T) {
	dispatcher := &Dispatcher{}
	const workers = 64
	owners := make(chan string, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			owners <- dispatcher.leaseOwner()
		})
	}
	wg.Wait()
	close(owners)
	var want string
	for owner := range owners {
		if owner == "" {
			t.Fatal("lease owner is empty")
		}
		if want == "" {
			want = owner
		}
		if owner != want {
			t.Fatalf("lease owner = %q, want %q", owner, want)
		}
	}
}

type expiryOrderingStore struct {
	store.GovernedMemoryStore
	claimed bool
	retried bool
}

func (s *expiryOrderingStore) ClaimNextMemoryOperation(context.Context, store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	s.claimed = true
	return &store.MemoryOperationDispatch{Operation: store.MemoryOperation{
		ID: "mop-expired", NamespaceUID: "namespace-a", BackendUID: "backend-a", AuthorityEpoch: 1, RoutingEpoch: 1,
		State: store.MemoryOperationLeased, LeaseOwner: "dispatcher-a", LeaseEpoch: 1,
		MaxAgeAt: time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC),
	}}, nil
}

func (s *expiryOrderingStore) RetryMemoryOperation(_ context.Context, retry store.MemoryOperationRetry) (*store.MemoryOperation, error) {
	s.retried = retry.DeadLetter
	return &store.MemoryOperation{ID: retry.ID, State: store.MemoryOperationDeadLettered}, nil
}

type expiryOrderingResolver struct {
	local      *ResolvedAuthority
	freshCalls int
}

func (r *expiryOrderingResolver) ResolveLocal(context.Context, string) (*ResolvedAuthority, error) {
	return r.local, nil
}
func (r *expiryOrderingResolver) Resolve(context.Context, string) (*ResolvedAuthority, error) {
	r.freshCalls++
	return nil, errors.New("remote readiness unavailable")
}

func TestDispatcherDeadLettersExpiredOperationBeforeRemoteReadiness(t *testing.T) {
	binding := &store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "backend-a", AuthorityEpoch: 1, RoutingEpoch: 1, ValidationExpiresAt: time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC),
	}
	resolver := &expiryOrderingResolver{local: &ResolvedAuthority{Namespace: "team-a", NamespaceUID: "namespace-a", Binding: binding}}
	storeImpl := &expiryOrderingStore{}
	dispatcher := &Dispatcher{Store: storeImpl, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time {
		return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	}}
	operation, err := dispatcher.DispatchOne(context.Background(), "team-a")
	if err != nil || operation == nil || operation.State != store.MemoryOperationDeadLettered {
		t.Fatalf("DispatchOne() operation=%#v err=%v", operation, err)
	}
	if !storeImpl.claimed || !storeImpl.retried || resolver.freshCalls != 0 {
		t.Fatalf("ordering claimed=%v retried=%v freshCalls=%d", storeImpl.claimed, storeImpl.retried, resolver.freshCalls)
	}
}

type expiredValidationClaimStore struct {
	store.GovernedMemoryStore
	claim store.MemoryOperationClaim
}

func (s *expiredValidationClaimStore) ClaimNextMemoryOperation(_ context.Context, claim store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	s.claim = claim
	return nil, store.ErrNotReady
}

func TestDispatcherUsesActualNowForExpiredValidationMaintenance(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := &store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "backend-a", AuthorityEpoch: 1, RoutingEpoch: 1, ValidationExpiresAt: now.Add(-time.Minute),
	}
	resolver := &expiryOrderingResolver{local: &ResolvedAuthority{
		Namespace: "team-a", NamespaceUID: "namespace-a", Binding: binding,
	}}
	storeImpl := &expiredValidationClaimStore{}
	dispatcher := &Dispatcher{Store: storeImpl, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time { return now }}
	if _, err := dispatcher.DispatchOne(context.Background(), "team-a"); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("DispatchOne() error = %v, want ErrNotReady", err)
	}
	if !storeImpl.claim.Now.Equal(now) || !storeImpl.claim.AllowExpiredValidationMaintenance {
		t.Fatalf("claim = %#v, want actual now and expired-validation maintenance", storeImpl.claim)
	}
	if resolver.freshCalls != 0 {
		t.Fatalf("fresh resolver calls = %d, want 0", resolver.freshCalls)
	}
}

type unsentReleaseTestStore struct {
	store.GovernedMemoryStore
	retry store.MemoryOperationRetry
}

func (s *unsentReleaseTestStore) ClaimNextMemoryOperation(context.Context, store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	return &store.MemoryOperationDispatch{Operation: store.MemoryOperation{
		ID: "mop-ambiguous", Namespace: "team-a", NamespaceUID: "namespace-a", BackendUID: "backend-a",
		AuthorityEpoch: 1, RoutingEpoch: 1, State: store.MemoryOperationLeased,
		LeaseOwner: "dispatcher-a", LeaseEpoch: 2, LeaseOriginState: store.MemoryOperationAmbiguous,
		Attempts: 50, MaxAgeAt: time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC),
	}}, nil
}

func (s *unsentReleaseTestStore) RetryMemoryOperation(_ context.Context, retry store.MemoryOperationRetry) (*store.MemoryOperation, error) {
	s.retry = retry
	state := store.MemoryOperationQueued
	if retry.Ambiguous {
		state = store.MemoryOperationAmbiguous
	}
	if retry.DeadLetter {
		state = store.MemoryOperationDeadLettered
	}
	return &store.MemoryOperation{ID: retry.ID, State: state}, nil
}

func TestDispatcherUnsentReleasePreservesAmbiguousOriginWithoutSemanticDeadLetter(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := &store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "backend-a", AuthorityEpoch: 1, RoutingEpoch: 1, ValidationExpiresAt: now.Add(time.Hour),
	}
	resolver := &expiryOrderingResolver{local: &ResolvedAuthority{
		Namespace: "team-a", NamespaceUID: "namespace-a", Binding: binding,
	}}
	storeImpl := &unsentReleaseTestStore{}
	dispatcher := &Dispatcher{
		Store: storeImpl, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time { return now },
		RetryJitterFraction: -1,
	}
	operation, err := dispatcher.DispatchOne(context.Background(), "team-a")
	if err == nil || operation == nil || operation.State != store.MemoryOperationAmbiguous {
		t.Fatalf("DispatchOne() operation=%#v err=%v", operation, err)
	}
	if !storeImpl.retry.UnsentRelease || !storeImpl.retry.Ambiguous || storeImpl.retry.DeadLetter {
		t.Fatalf("unsent retry = %#v", storeImpl.retry)
	}
	if want := now.Add(4 * time.Second); !storeImpl.retry.NextRetryAt.Equal(want) {
		t.Fatalf("unsent retry next_retry_at = %s, want %s", storeImpl.retry.NextRetryAt, want)
	}
	if state := dispatcher.circuits[memoryEndpointCircuitKey(binding)]; state.failures != 1 {
		t.Fatalf("endpoint circuit failures = %d, want 1", state.failures)
	}
}

type dispatchSendBoundaryStore struct {
	store.GovernedMemoryStore
	dispatch store.MemoryOperationDispatch
	send     store.MemoryOperationSend
	retry    store.MemoryOperationRetry
}

func (s *dispatchSendBoundaryStore) ClaimNextMemoryOperation(context.Context, store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	copy := s.dispatch
	copy.Payload = append([]byte(nil), s.dispatch.Payload...)
	return &copy, nil
}

func (s *dispatchSendBoundaryStore) MarkMemoryOperationSendStarted(_ context.Context, send store.MemoryOperationSend) (*store.MemoryOperation, error) {
	s.send = send
	operation := s.dispatch.Operation
	operation.State = store.MemoryOperationDispatching
	operation.SendStartedAt = &send.Now
	operation.RequestDeadline = &send.RequestDeadline
	return &operation, nil
}

func (s *dispatchSendBoundaryStore) RetryMemoryOperation(_ context.Context, retry store.MemoryOperationRetry) (*store.MemoryOperation, error) {
	s.retry = retry
	state := store.MemoryOperationQueued
	if retry.Ambiguous {
		state = store.MemoryOperationAmbiguous
	}
	if retry.DeadLetter {
		state = store.MemoryOperationDeadLettered
	}
	return &store.MemoryOperation{ID: retry.ID, State: state}, nil
}

type advancingDispatchResolver struct {
	local     *ResolvedAuthority
	fresh     *ResolvedAuthority
	clock     *time.Time
	freshTime time.Time
}

func (r *advancingDispatchResolver) ResolveLocal(context.Context, string) (*ResolvedAuthority, error) {
	return r.local, nil
}

func (r *advancingDispatchResolver) Resolve(context.Context, string) (*ResolvedAuthority, error) {
	*r.clock = r.freshTime
	return r.fresh, nil
}

type invalidReceiptOMSAdapter struct{ fakeOMSAdapter }

func (a *invalidReceiptOMSAdapter) Mutate(_ context.Context, envelope protocol.MutationEnvelope) (*protocol.MutationReceipt, error) {
	return &protocol.MutationReceipt{
		ProtocolVersion: protocol.Version, Binding: envelope.Binding, Result: protocol.ResultApplied,
		OperationID: "mop-wrong", BindingDigest: protocol.BindingDigest(envelope.Binding),
		AppliedGeneration: envelope.Generation, BackendVersion: "v1", BackendMemoryID: "backend-1",
		ContentDigest: envelope.ContentDigest, MutationDigest: envelope.MutationDigest,
		CompletedAt: time.Date(2026, 7, 30, 12, 0, 10, 0, time.UTC),
	}, nil
}

func TestDispatcherRefreshesSendTimeAfterResolutionAndKeepsInvalidReceiptAmbiguous(t *testing.T) {
	claimTime := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	freshTime := claimTime.Add(5 * time.Second)
	clock := claimTime
	binding := store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "11111111-1111-4111-8111-111111111111",
		ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "22222222-2222-4222-8222-222222222222", AuthorityEpoch: 1, RoutingEpoch: 1,
		TenantID:  protocol.DeriveTenantID("cluster-a", "11111111-1111-4111-8111-111111111111"),
		StoreUUID: "44444444-4444-4444-8444-444444444444", State: store.MemoryBackendBindingAccepting,
		ValidationExpiresAt: claimTime.Add(time.Hour),
	}
	protocolIdentity, err := protocolBinding(&binding)
	if err != nil {
		t.Fatal(err)
	}
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version, OperationID: "mop-valid", Binding: protocolIdentity,
		MemoryID: "mem-valid", Kind: protocol.MutationKindCreate, Generation: 1, ExpectedGeneration: 0,
		State: &protocol.MutationState{Content: "remember", Tags: []string{}, Metadata: map[string]string{}},
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.EncodeJSON(envelope)
	if err != nil {
		t.Fatal(err)
	}
	operation := store.MemoryOperation{
		ID: envelope.OperationID, Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		DesiredGeneration: int64(envelope.Generation), MutationDigest: envelope.MutationDigest,
		State: store.MemoryOperationLeased, LeaseOwner: "dispatcher-a", LeaseEpoch: 1,
		MaxAgeAt: claimTime.Add(time.Hour), Attempts: 1,
	}
	storeImpl := &dispatchSendBoundaryStore{dispatch: store.MemoryOperationDispatch{Operation: operation, Payload: payload}}
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	authority := &ResolvedAuthority{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding,
		Backend: backend, Adapter: &invalidReceiptOMSAdapter{},
	}
	resolver := &advancingDispatchResolver{local: authority, fresh: authority, clock: &clock, freshTime: freshTime}
	dispatcher := &Dispatcher{
		Store: storeImpl, Resolver: resolver, LeaseOwner: "dispatcher-a", Now: func() time.Time { return clock },
		RetryJitterFraction: -1,
	}
	result, err := dispatcher.DispatchOne(context.Background(), binding.Namespace)
	if err != nil || result == nil || result.State != store.MemoryOperationAmbiguous {
		t.Fatalf("DispatchOne() result=%#v err=%v", result, err)
	}
	if !storeImpl.send.Now.Equal(freshTime) || !storeImpl.send.RequestDeadline.Equal(freshTime.Add(defaultMemoryRequestDeadline)) {
		t.Fatalf("send boundary = %#v, want fresh resolution time", storeImpl.send)
	}
	if !storeImpl.retry.Ambiguous || storeImpl.retry.DeadLetter {
		t.Fatalf("retry = %#v, want ambiguous invalid receipt", storeImpl.retry)
	}
}

type abandonmentTestStore struct {
	store.GovernedMemoryStore
	operation   store.MemoryOperation
	abandonment store.MemoryOperationAbandonment
}

func (s *abandonmentTestStore) GetMemoryOperation(context.Context, string, string) (*store.MemoryOperation, error) {
	copy := s.operation
	return &copy, nil
}

func (s *abandonmentTestStore) AbandonMemoryOperation(_ context.Context, abandonment store.MemoryOperationAbandonment) (*store.MemoryOperation, error) {
	s.abandonment = abandonment
	copy := s.operation
	copy.State = store.MemoryOperationAbandoned
	return &copy, nil
}

type abandonmentOMSAdapter struct {
	fakeOMSAdapter
	fence  *protocol.RoutingFenceResponse
	lookup *protocol.OperationLookupResponse
}

func (a *abandonmentOMSAdapter) AdvanceRoutingFence(context.Context, protocol.RoutingFenceRequest) (*protocol.RoutingFenceResponse, error) {
	return a.fence, nil
}

func (a *abandonmentOMSAdapter) LookupOperation(context.Context, protocol.OperationLookupRequest) (*protocol.OperationLookupResponse, error) {
	return a.lookup, nil
}

func TestAbandonMemoryOperationDerivesProviderProofAndFence(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	binding := store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "11111111-1111-4111-8111-111111111111",
		ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "22222222-2222-4222-8222-222222222222", AuthorityEpoch: 1, RoutingEpoch: 2,
		TenantID:  protocol.DeriveTenantID("cluster-a", "11111111-1111-4111-8111-111111111111"),
		StoreUUID: "44444444-4444-4444-8444-444444444444", State: store.MemoryBackendBindingDraining,
	}
	identity, err := protocolBinding(&binding)
	if err != nil {
		t.Fatal(err)
	}
	sentAt := now.Add(-time.Minute)
	storeImpl := &abandonmentTestStore{operation: store.MemoryOperation{
		ID: "mop-dead", Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: 1,
		MutationDigest: protocol.ContentDigest("mutation"), State: store.MemoryOperationDeadLettered,
		SendStartedAt: &sentAt,
	}}
	adapter := &abandonmentOMSAdapter{
		fence: &protocol.RoutingFenceResponse{
			ProtocolVersion: protocol.Version, Binding: identity, Result: protocol.ResultApplied,
			BindingDigest: protocol.BindingDigest(identity), MaximumRoutingEpoch: identity.RoutingEpoch, CompletedAt: now,
		},
		lookup: &protocol.OperationLookupResponse{ProtocolVersion: protocol.Version, Binding: identity, Found: false},
	}
	authority := &ResolvedAuthority{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding, Adapter: adapter,
	}
	service := &Service{Governed: storeImpl, Resolver: staticAuthorityResolver{authority: authority}, Now: func() time.Time { return now }}
	result, err := service.AbandonMemoryOperation(context.Background(), binding.Namespace, storeImpl.operation.ID,
		"operator", "provider outcome reconciled", "request-1")
	if err != nil || result.State != store.MemoryOperationAbandoned {
		t.Fatalf("AbandonMemoryOperation() result=%#v err=%v", result, err)
	}
	if !storeImpl.abandonment.Fenced || !storeImpl.abandonment.ProviderNeverApplied {
		t.Fatalf("abandonment = %#v, want server-derived proof", storeImpl.abandonment)
	}
}

func TestDispatcherConcurrencyRateGuardsAndRetryJitterAreBounded(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	dispatcher := &Dispatcher{
		GlobalConcurrency: 1, NamespaceConcurrency: 1,
		GlobalRPS: 1, NamespaceRPS: 1, GlobalBurst: 1, NamespaceBurst: 1,
		Now: func() time.Time { return now }, RetryJitterFraction: 0.25, RandFloat64: func() float64 { return 0.5 },
	}
	release, err := dispatcher.acquireDispatchGuard("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.acquireDispatchGuard("team-a"); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("second concurrent guard error = %v, want ErrNotReady", err)
	}
	release()
	if _, err := dispatcher.acquireDispatchGuard("team-a"); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("same-tick rate guard error = %v, want ErrNotReady", err)
	}
	now = now.Add(time.Second)
	release, err = dispatcher.acquireDispatchGuard("team-a")
	if err != nil {
		t.Fatalf("guard after refill error = %v", err)
	}
	release()

	retryAt := dispatcher.nextRetry(store.MemoryOperation{Attempts: 1}, 0)
	want := now.Add(2250 * time.Millisecond)
	if !retryAt.Equal(want) {
		t.Fatalf("retryAt = %s, want bounded deterministic jitter %s", retryAt, want)
	}
}

func TestAdapterDependencyFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   bool
	}{
		{name: "transport", status: 0, want: true},
		{name: "unauthorized", status: 401, want: true},
		{name: "forbidden", status: 403, want: true},
		{name: "rate limited", status: 429, want: true},
		{name: "server failure", status: 503, want: true},
		{name: "operation conflict", status: 409, want: false},
		{name: "invalid operation", status: 422, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := adapterDependencyFailure(&AdapterError{StatusCode: test.status}); got != test.want {
				t.Fatalf("adapterDependencyFailure(%d) = %v, want %v", test.status, got, test.want)
			}
		})
	}
}

type dependencyFailureCaptureStore struct {
	store.GovernedMemoryStore
	retry store.MemoryOperationRetry
}

func (s *dependencyFailureCaptureStore) RetryMemoryOperation(
	_ context.Context,
	retry store.MemoryOperationRetry,
) (*store.MemoryOperation, error) {
	s.retry = retry
	return &store.MemoryOperation{ID: retry.ID, State: store.MemoryOperationQueued}, nil
}

func TestDispatcherKeepsAuthenticationFailuresRetryable(t *testing.T) {
	captured := &dependencyFailureCaptureStore{}
	dispatcher := &Dispatcher{Store: captured, LeaseOwner: "dispatcher-a", Now: func() time.Time {
		return time.Date(2026, 7, 30, 19, 0, 0, 0, time.UTC)
	}}
	operation := store.MemoryOperation{
		ID: "mop-auth", NamespaceUID: "namespace-a", BackendUID: "backend-a",
		AuthorityEpoch: 1, RoutingEpoch: 1, LeaseOwner: "dispatcher-a", LeaseEpoch: 1,
		Attempts: 10,
	}
	binding := &store.MemoryBackendBinding{EndpointDigest: "endpoint", BackendUID: operation.BackendUID,
		SecretUID: "secret", SecretResourceVersion: "1"}
	if _, err := dispatcher.handleSendError(context.Background(), operation, binding, &AdapterError{
		StatusCode: http.StatusUnauthorized, Code: protocol.ErrorCodeUnauthorized,
		Message: "OMS adapter authentication failed", Retryable: false,
	}); err != nil {
		t.Fatal(err)
	}
	if captured.retry.DeadLetter || !captured.retry.DependencyFailure || captured.retry.Ambiguous {
		t.Fatalf("authentication retry = %#v, want dependency retry without dead-letter", captured.retry)
	}
}

func TestNamespaceRateRejectionDoesNotConsumeGlobalToken(t *testing.T) {
	now := time.Date(2026, 7, 30, 21, 0, 0, 0, time.UTC)
	dispatcher := &Dispatcher{
		GlobalRPS: 2, GlobalBurst: 2, NamespaceRPS: 1, NamespaceBurst: 1,
		Now: func() time.Time { return now },
	}
	release, err := dispatcher.acquireDispatchGuard("team-a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := dispatcher.acquireDispatchGuard("team-a"); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("second team-a guard error = %v, want namespace rate rejection", err)
	}
	release, err = dispatcher.acquireDispatchGuard("team-b")
	if err != nil {
		t.Fatalf("team-b guard was starved by team-a rejection: %v", err)
	}
	release()
}

func TestDispatcherValidatesStoredEnvelopeBeforeSendBoundary(t *testing.T) {
	now := time.Date(2026, 7, 30, 21, 30, 0, 0, time.UTC)
	binding := store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "11111111-1111-4111-8111-111111111111",
		ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "22222222-2222-4222-8222-222222222222", AuthorityEpoch: 1, RoutingEpoch: 1,
		TenantID:  protocol.DeriveTenantID("cluster-a", "11111111-1111-4111-8111-111111111111"),
		StoreUUID: "44444444-4444-4444-8444-444444444444", State: store.MemoryBackendBindingAccepting,
		ValidationExpiresAt: now.Add(time.Hour),
	}
	operation := store.MemoryOperation{
		ID: "mop-invalid", Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		DesiredGeneration: 1, MutationDigest: protocol.ContentDigest("mutation"),
		State: store.MemoryOperationLeased, LeaseOwner: "dispatcher-a", LeaseEpoch: 1,
		MaxAgeAt: now.Add(time.Hour),
	}
	storeImpl := &dispatchSendBoundaryStore{dispatch: store.MemoryOperationDispatch{
		Operation: operation, Payload: []byte(`{"invalid":`),
	}}
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleActive
	backend.Status.Ready = true
	authority := &ResolvedAuthority{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Binding: &binding,
		Backend: backend, Adapter: &fakeOMSAdapter{},
	}
	dispatcher := &Dispatcher{
		Store: storeImpl, Resolver: staticAuthorityResolver{authority: authority},
		LeaseOwner: "dispatcher-a", Now: func() time.Time { return now },
	}
	result, err := dispatcher.DispatchOne(context.Background(), binding.Namespace)
	if err != nil || result == nil || result.State != store.MemoryOperationDeadLettered {
		t.Fatalf("DispatchOne() result=%#v err=%v", result, err)
	}
	if storeImpl.send.ID != "" {
		t.Fatalf("send boundary was crossed for malformed local payload: %#v", storeImpl.send)
	}
	if !storeImpl.retry.DeadLetter || storeImpl.retry.Ambiguous {
		t.Fatalf("retry = %#v, want provably-unsent dead letter", storeImpl.retry)
	}
}
