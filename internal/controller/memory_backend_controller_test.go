package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/endpointpolicy"
	"github.com/orka-agents/orka/internal/oms/referenceadapter"
	omsprotocol "github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	testMemoryNamespaceUID           = types.UID("namespace-uid-1")
	testMemoryBackendUID             = types.UID("backend-uid-1")
	testMemoryClusterID              = "cluster-a"
	testMemoryStoreUUID              = "d57d1746-81e9-4f6b-9745-9c7b4f53488d"
	testCoordinatorBinding           = "coordinator.binding"
	testProberFence                  = "prober.fence"
	testProbeToken                   = "probe-token"
	testMemoryBackendActivatedReason = "Activated"
	testMemoryBackendHost            = "memory.example.com"
)

type memoryStaticResolver map[string][]netip.Addr

func (r memoryStaticResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r[host]...), nil
}

type memoryBackendRecordingResolver struct {
	addresses    map[string][]netip.Addr
	stall        bool
	observations []memoryBackendProbeObservation
}

func (r *memoryBackendRecordingResolver) LookupNetIP(ctx context.Context, _ string, host string) ([]netip.Addr, error) {
	observation := observeMemoryBackendProbeContext(ctx)
	if r.stall {
		<-ctx.Done()
		observation.err = ctx.Err()
	}
	r.observations = append(r.observations, observation)
	if observation.err != nil {
		return nil, observation.err
	}
	return append([]netip.Addr(nil), r.addresses[host]...), nil
}

func newMemoryBackendRecordingResolver() *memoryBackendRecordingResolver {
	return &memoryBackendRecordingResolver{addresses: map[string][]netip.Addr{
		testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")},
	}}
}

type memoryRedirectDialer struct {
	target    string
	addresses []string
}

func (d *memoryRedirectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	return (&net.Dialer{}).DialContext(ctx, network, d.target)
}

type memoryBackendProbeParentContextKey struct{}

type memoryBackendProbeObservation struct {
	deadline     time.Time
	remaining    time.Duration
	parentMarker string
	err          error
}

type fakeMemoryBackendProber struct {
	storeResult            MemoryBackendStoreProbeResult
	storeErr               error
	storeCalls             int
	storeTarget            MemoryBackendStoreProbeTarget
	storeObservations      []memoryBackendProbeObservation
	result                 MemoryBackendProbeResult
	err                    error
	calls                  int
	target                 MemoryBackendProbeTarget
	capabilityObservations []memoryBackendProbeObservation
	bindingObservations    []memoryBackendProbeObservation
	fenceResult            MemoryBackendRoutingFenceResult
	fenceErr               error
	fenceCalls             int
	fenceTarget            MemoryBackendRoutingFenceTarget
	fenceTargets           []MemoryBackendRoutingFenceTarget
	fenceObservations      []memoryBackendProbeObservation
	afterStore             func(int)
	events                 *[]string
}

func (p *fakeMemoryBackendProber) AdvanceRoutingFence(
	ctx context.Context,
	target MemoryBackendRoutingFenceTarget,
) (MemoryBackendRoutingFenceResult, error) {
	if p.events != nil {
		*p.events = append(*p.events, testProberFence)
	}
	p.fenceCalls++
	observation := observeMemoryBackendProbeContext(ctx)
	p.fenceObservations = append(p.fenceObservations, observation)
	p.fenceTarget = target
	p.fenceTargets = append(p.fenceTargets, target)
	if observation.err != nil {
		return MemoryBackendRoutingFenceResult{}, observation.err
	}
	result := p.fenceResult
	if result.MaximumRoutingEpoch == 0 {
		result.MaximumRoutingEpoch = target.RoutingEpoch
	}
	if result.ServerCertificateDigest == "" {
		result.ServerCertificateDigest = target.ExpectedServerCertificateDigest
	}
	return result, p.fenceErr
}

func (p *fakeMemoryBackendProber) ResolveStore(
	ctx context.Context,
	target MemoryBackendStoreProbeTarget,
) (MemoryBackendStoreProbeResult, error) {
	if p.events != nil {
		*p.events = append(*p.events, "prober.store")
	}
	p.storeCalls++
	observation := observeMemoryBackendProbeContext(ctx)
	p.storeObservations = append(p.storeObservations, observation)
	p.storeTarget = target
	if observation.err != nil {
		return MemoryBackendStoreProbeResult{}, observation.err
	}
	result := p.storeResult
	if result.StoreUUID == "" {
		result = validMemoryBackendStoreProbeResult()
	}
	if p.afterStore != nil {
		p.afterStore(p.storeCalls)
	}
	return result, p.storeErr
}

func (p *fakeMemoryBackendProber) ProbeCapabilities(
	ctx context.Context,
	target MemoryBackendProbeTarget,
) (MemoryBackendProbeResult, error) {
	if p.events != nil {
		*p.events = append(*p.events, "prober.capabilities")
	}
	p.calls++
	observation := observeMemoryBackendProbeContext(ctx)
	p.capabilityObservations = append(p.capabilityObservations, observation)
	p.target = target
	if observation.err != nil {
		return MemoryBackendProbeResult{}, observation.err
	}
	result := p.result
	result.OwnershipClaimIdentity = ""
	return result, p.err
}

func (p *fakeMemoryBackendProber) ProbeBinding(
	ctx context.Context,
	target MemoryBackendProbeTarget,
) (MemoryBackendProbeResult, error) {
	if p.events != nil {
		*p.events = append(*p.events, "prober.binding")
	}
	p.calls++
	observation := observeMemoryBackendProbeContext(ctx)
	p.bindingObservations = append(p.bindingObservations, observation)
	p.target = target
	if observation.err != nil {
		return MemoryBackendProbeResult{}, observation.err
	}
	return p.result, p.err
}

type fakeMemoryBackendCoordinator struct {
	prepareResult       MemoryBackendValidationBinding
	prepareErr          error
	prepareCalls        int
	prepareRequest      MemoryBackendValidationSnapshot
	claimAttemptErr     error
	claimAttemptCalls   int
	claimAttemptRequest MemoryBackendOwnershipClaimAttemptSnapshot
	retireErr           error
	retireCalls         int
	retireRequest       MemoryBackendValidationCandidateRetirement
	bindingResult       MemoryBackendBindingResult
	bindingErr          error
	bindingCalls        int
	bindingRequest      MemoryBackendBindingSnapshot
	bindingResults      []MemoryBackendBindingResult
	bindingErrors       []error
	deleteResult        MemoryBackendDeletionResult
	deleteErr           error
	deleteCalls         int
	deleteRequest       MemoryBackendDeletionSnapshot
	events              *[]string
}

func (c *fakeMemoryBackendCoordinator) PrepareMemoryBackendValidation(
	_ context.Context,
	snapshot MemoryBackendValidationSnapshot,
) (MemoryBackendValidationBinding, error) {
	if c.events != nil {
		*c.events = append(*c.events, "coordinator.prepare")
	}
	c.prepareCalls++
	c.prepareRequest = snapshot
	result := c.prepareResult
	if result.AuthorityEpoch == 0 && result.RoutingEpoch == 0 {
		result = MemoryBackendValidationBinding{AuthorityEpoch: 1, RoutingEpoch: 1}
	}
	if result.CandidateDigest == "" &&
		(snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive ||
			snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly) {
		result.CandidateDigest = "sha256:" + strings.Repeat("b", 64)
	}
	return result, c.prepareErr
}

func (c *fakeMemoryBackendCoordinator) RecordMemoryBackendOwnershipClaimAttempt(
	_ context.Context,
	snapshot MemoryBackendOwnershipClaimAttemptSnapshot,
) error {
	if c.events != nil {
		*c.events = append(*c.events, "coordinator.claim-attempt")
	}
	c.claimAttemptCalls++
	c.claimAttemptRequest = snapshot
	return c.claimAttemptErr
}

func (c *fakeMemoryBackendCoordinator) RetireMemoryBackendValidationCandidate(
	_ context.Context,
	retirement MemoryBackendValidationCandidateRetirement,
) error {
	c.retireCalls++
	c.retireRequest = retirement
	return c.retireErr
}

func (c *fakeMemoryBackendCoordinator) ReconcileMemoryBackendBinding(
	_ context.Context,
	snapshot MemoryBackendBindingSnapshot,
) (MemoryBackendBindingResult, error) {
	if c.events != nil {
		*c.events = append(*c.events, testCoordinatorBinding)
	}
	c.bindingCalls++
	c.bindingRequest = snapshot
	if index := c.bindingCalls - 1; index < len(c.bindingResults) {
		var err error
		if index < len(c.bindingErrors) {
			err = c.bindingErrors[index]
		}
		return c.bindingResults[index], err
	}
	return c.bindingResult, c.bindingErr
}

func (c *fakeMemoryBackendCoordinator) FinalizeMemoryBackendDeletion(
	_ context.Context,
	snapshot MemoryBackendDeletionSnapshot,
) (MemoryBackendDeletionResult, error) {
	if c.events != nil {
		*c.events = append(*c.events, "coordinator.delete")
	}
	c.deleteCalls++
	c.deleteRequest = snapshot
	return c.deleteResult, c.deleteErr
}

func TestMemoryBackendReconcilerStagedValidatesWithoutCutover(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	coordinator := &fakeMemoryBackendCoordinator{}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)

	reconcileMemoryBackendTwice(t, reconciler, backend.Namespace)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("periodic staged Reconcile() error = %v", err)
	}
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if !updated.Status.Accepted || !updated.Status.Protected || !updated.Status.ResolvedRefs || !updated.Status.Connected || !updated.Status.Ready {
		t.Fatalf("staged status = %+v", updated.Status)
	}
	if updated.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleStaged ||
		updated.Status.AuthorityEpoch != 1 || updated.Status.RoutingEpoch != 1 {
		t.Fatalf("staged backend did not retain its durable validation candidate: %+v", updated.Status)
	}
	if coordinator.prepareCalls != 2 || coordinator.claimAttemptCalls != 0 || coordinator.bindingCalls != 0 {
		t.Fatalf("staged coordinator calls prepare=%d claim-attempt=%d cutover=%d",
			coordinator.prepareCalls, coordinator.claimAttemptCalls, coordinator.bindingCalls)
	}
	if prober.storeCalls != 2 || prober.calls != 2 || prober.target.BearerToken != "test-bearer-token" {
		t.Fatalf("probe calls store=%d binding=%d target=%+v", prober.storeCalls, prober.calls, prober.target)
	}
	if updated.Status.ValidationExpiresAt == nil || !updated.Status.ValidationExpiresAt.Time.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("validation expiry = %v", updated.Status.ValidationExpiresAt)
	}
	if condition := meta.FindStatusCondition(updated.Status.Conditions, string(corev1alpha1.MemoryBackendConditionReady)); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v", condition)
	}
}

func TestMemoryBackendReconcilerStagedUsesFreshTimeoutForEachOMSProbe(t *testing.T) {
	const probeTimeout = 5 * time.Second
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, &fakeMemoryBackendCoordinator{}, backend, namespace, secret)
	resolver := newMemoryBackendRecordingResolver()
	reconciler.EndpointPolicy.Resolver = resolver
	reconciler.ProbeTimeout = probeTimeout
	parentMarker := "staged-validation"
	parentCtx := context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker)

	reconcileMemoryBackendAfterFinalizer(t, reconciler, backend.Namespace, parentCtx)

	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if !updated.Status.Ready {
		t.Fatalf("staged status = %+v", updated.Status)
	}
	if len(resolver.observations) != 1 || len(prober.storeObservations) != 1 || len(prober.capabilityObservations) != 1 {
		t.Fatalf("operation observations resolve=%d store=%d capabilities=%d",
			len(resolver.observations), len(prober.storeObservations), len(prober.capabilityObservations))
	}
	requireFreshMemoryBackendProbeObservations(t, probeTimeout, parentMarker,
		resolver.observations[0], prober.storeObservations[0], prober.capabilityObservations[0])
}

func TestMemoryBackendReconcilerEndpointResolutionHonorsProbeTimeout(t *testing.T) {
	const probeTimeout = 25 * time.Millisecond
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, &fakeMemoryBackendCoordinator{}, backend, namespace, secret)
	resolver := newMemoryBackendRecordingResolver()
	resolver.stall = true
	reconciler.EndpointPolicy.Resolver = resolver
	reconciler.ProbeTimeout = probeTimeout
	parentMarker := "stalled-resolution"
	parentCtx, cancelParent := context.WithTimeout(
		context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker),
		500*time.Millisecond,
	)
	defer cancelParent()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("finalizer Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(parentCtx, request); err != nil {
		t.Fatalf("validation Reconcile() error = %v", err)
	}

	if len(resolver.observations) != 1 {
		t.Fatalf("resolution observations = %#v", resolver.observations)
	}
	observation := resolver.observations[0]
	if !errors.Is(observation.err, context.DeadlineExceeded) || observation.deadline.IsZero() ||
		observation.remaining <= 0 || observation.remaining > probeTimeout || observation.parentMarker != parentMarker {
		t.Fatalf("stalled resolution observation = %#v", observation)
	}
	if prober.storeCalls != 0 {
		t.Fatalf("stalled resolution reached OMS store probe %d times", prober.storeCalls)
	}
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if updated.Status.Reason != "EndpointRejected" || updated.Status.Ready {
		t.Fatalf("stalled resolution status = %+v", updated.Status)
	}
}

func TestMemoryBackendReconcilerEndpointResolutionPreservesParentCancellation(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	reconciler, _ := newMemoryBackendReconciler(t, now, prober, &fakeMemoryBackendCoordinator{}, backend, namespace, secret)
	resolver := newMemoryBackendRecordingResolver()
	resolver.stall = true
	reconciler.EndpointPolicy.Resolver = resolver
	reconciler.ProbeTimeout = 5 * time.Second
	parentMarker := "canceled-resolution"
	parentCtx, cancelParent := context.WithCancel(
		context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker),
	)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("finalizer Reconcile() error = %v", err)
	}
	cancelParent()
	if _, err := reconciler.Reconcile(parentCtx, request); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Reconcile() error = %v", err)
	}

	if len(resolver.observations) != 1 {
		t.Fatalf("resolution observations = %#v", resolver.observations)
	}
	observation := resolver.observations[0]
	if !errors.Is(observation.err, context.Canceled) || observation.parentMarker != parentMarker {
		t.Fatalf("canceled resolution observation = %#v", observation)
	}
	if prober.storeCalls != 0 {
		t.Fatalf("canceled resolution reached OMS store probe %d times", prober.storeCalls)
	}
}

func TestMemoryBackendReconcilerFreshProbeTimeoutsPreserveParentCancellation(t *testing.T) {
	tests := []struct {
		name                 string
		lifecycle            corev1alpha1.MemoryBackendLifecycleState
		remoteFence          bool
		cancelBeforeProbe    bool
		cancelAfterStoreCall int
		observations         func(*fakeMemoryBackendProber) []memoryBackendProbeObservation
	}{
		{
			name:              "initial store resolution",
			lifecycle:         corev1alpha1.MemoryBackendLifecycleStaged,
			cancelBeforeProbe: true,
			observations: func(prober *fakeMemoryBackendProber) []memoryBackendProbeObservation {
				return prober.storeObservations
			},
		},
		{
			name:                 "staged capabilities",
			lifecycle:            corev1alpha1.MemoryBackendLifecycleStaged,
			cancelAfterStoreCall: 1,
			observations: func(prober *fakeMemoryBackendProber) []memoryBackendProbeObservation {
				return prober.capabilityObservations
			},
		},
		{
			name:                 "activation routing fence",
			lifecycle:            corev1alpha1.MemoryBackendLifecycleActive,
			remoteFence:          true,
			cancelAfterStoreCall: 2,
			observations: func(prober *fakeMemoryBackendProber) []memoryBackendProbeObservation {
				return prober.fenceObservations
			},
		},
		{
			name:                 "activation binding",
			lifecycle:            corev1alpha1.MemoryBackendLifecycleActive,
			remoteFence:          true,
			cancelAfterStoreCall: 3,
			observations: func(prober *fakeMemoryBackendProber) []memoryBackendProbeObservation {
				return prober.bindingObservations
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			backend, namespace, secret := memoryBackendTestObjects(t, tt.lifecycle)
			parentMarker := "cancel-" + tt.name
			parentCtx, cancelParent := context.WithCancel(
				context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker),
			)
			defer cancelParent()
			prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
			if tt.cancelAfterStoreCall > 0 {
				prober.afterStore = func(call int) {
					if call == tt.cancelAfterStoreCall {
						cancelParent()
					}
				}
			}
			coordinator := &fakeMemoryBackendCoordinator{}
			if tt.lifecycle == corev1alpha1.MemoryBackendLifecycleActive {
				coordinator.prepareResult = MemoryBackendValidationBinding{
					AuthorityEpoch: 3, RoutingEpoch: 7, RemoteFenceRequired: tt.remoteFence,
				}
				coordinator.bindingResult = MemoryBackendBindingResult{
					EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleActive,
					AuthorityEpoch:          3,
					RoutingEpoch:            7,
					Ready:                   true,
					Reason:                  testMemoryBackendActivatedReason,
				}
			}
			reconciler, _ := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)
			reconciler.ProbeTimeout = 5 * time.Second
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("initial Reconcile() error = %v", err)
			}
			if tt.cancelBeforeProbe {
				cancelParent()
			}
			if _, err := reconciler.Reconcile(parentCtx, request); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled Reconcile() error = %v", err)
			}

			observations := tt.observations(prober)
			if len(observations) != 1 {
				t.Fatalf("canceled probe observations = %#v", observations)
			}
			if !errors.Is(observations[0].err, context.Canceled) {
				t.Fatalf("canceled probe error = %v, want context.Canceled", observations[0].err)
			}
			if observations[0].parentMarker != parentMarker {
				t.Fatalf("canceled probe parent marker = %q, want %q", observations[0].parentMarker, parentMarker)
			}
		})
	}
}

func TestMemoryBackendReconcilerActiveRequiresDurableBindingCoordinator(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleActive)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, nil, backend, namespace, secret)

	reconcileMemoryBackendTwice(t, reconciler, backend.Namespace)
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if updated.Status.Ready || updated.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleStaged {
		t.Fatalf("active backend without coordinator = %+v", updated.Status)
	}
	if updated.Status.Reason != memoryBackendReasonBindingCoordinatorUnavailable || updated.Status.Connected || prober.storeCalls != 1 || prober.calls != 0 {
		t.Fatalf("active backend reason/connection = %+v probes=%d/%d", updated.Status, prober.storeCalls, prober.calls)
	}
}

func TestMemoryBackendReconcilerActivePublishesDurableEpochs(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleActive)
	events := []string{}
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute)), events: &events}
	coordinator := &fakeMemoryBackendCoordinator{
		events: &events, prepareResult: MemoryBackendValidationBinding{AuthorityEpoch: 3, RoutingEpoch: 7},
		bindingResult: MemoryBackendBindingResult{
			EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleActive,
			AuthorityEpoch:          3,
			RoutingEpoch:            7,
			Ready:                   true,
			Reason:                  testMemoryBackendActivatedReason,
			Message:                 "durable activation completed",
		}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)

	reconcileMemoryBackendTwice(t, reconciler, backend.Namespace)
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if !updated.Status.Ready || updated.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive ||
		updated.Status.AuthorityEpoch != 3 || updated.Status.RoutingEpoch != 7 {
		t.Fatalf("active status = %+v", updated.Status)
	}
	if coordinator.prepareCalls != 1 || coordinator.claimAttemptCalls != 1 || coordinator.bindingCalls != 1 {
		t.Fatalf("coordinator calls prepare=%d claim-attempt=%d binding=%d",
			coordinator.prepareCalls, coordinator.claimAttemptCalls, coordinator.bindingCalls)
	}
	if coordinator.claimAttemptRequest.AuthorityEpoch != 3 || coordinator.claimAttemptRequest.RoutingEpoch != 7 ||
		coordinator.claimAttemptRequest.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleActive {
		t.Fatalf("claim-attempt snapshot = %+v", coordinator.claimAttemptRequest)
	}
	if coordinator.bindingRequest.SecretName != "memory-auth" || coordinator.bindingRequest.SecretKey != "token" ||
		coordinator.bindingRequest.SecretUID == "" || coordinator.bindingRequest.SecretResourceVersion == "" ||
		coordinator.bindingRequest.StoreUUID != testMemoryStoreUUID ||
		coordinator.bindingRequest.ClusterID != testMemoryClusterID {
		t.Fatalf("binding snapshot = %+v", coordinator.bindingRequest)
	}
	if coordinator.bindingRequest.TenantID != deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID) {
		t.Fatalf("binding tenant ID = %q", coordinator.bindingRequest.TenantID)
	}
	wantEvents := []string{
		"prober.store", "coordinator.prepare", "coordinator.claim-attempt",
		"prober.store", "prober.binding", "prober.store", testCoordinatorBinding,
	}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("ownership claim ordering = %v, want %v", events, wantEvents)
	}
}

func TestMemoryBackendReconcilerActiveUsesFreshTimeoutForStoreFenceAndBindingProbes(t *testing.T) {
	const probeTimeout = 5 * time.Second
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleActive)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	coordinator := &fakeMemoryBackendCoordinator{
		prepareResult: MemoryBackendValidationBinding{
			AuthorityEpoch: 3, RoutingEpoch: 7, RemoteFenceRequired: true,
		},
		bindingResult: MemoryBackendBindingResult{
			EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleActive,
			AuthorityEpoch:          3,
			RoutingEpoch:            7,
			Ready:                   true,
			Reason:                  testMemoryBackendActivatedReason,
		},
	}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)
	resolver := newMemoryBackendRecordingResolver()
	reconciler.EndpointPolicy.Resolver = resolver
	reconciler.ProbeTimeout = probeTimeout
	parentMarker := "active-validation"
	parentCtx := context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker)

	reconcileMemoryBackendAfterFinalizer(t, reconciler, backend.Namespace, parentCtx)

	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if !updated.Status.Ready {
		t.Fatalf("active status = %+v", updated.Status)
	}
	if len(resolver.observations) != 4 || len(prober.storeObservations) != 4 ||
		len(prober.fenceObservations) != 1 || len(prober.bindingObservations) != 1 {
		t.Fatalf("operation observations resolve=%d store=%d fence=%d binding=%d",
			len(resolver.observations), len(prober.storeObservations), len(prober.fenceObservations), len(prober.bindingObservations))
	}
	observations := append([]memoryBackendProbeObservation(nil), resolver.observations...)
	observations = append(observations, prober.storeObservations...)
	observations = append(observations, prober.fenceObservations[0], prober.bindingObservations[0])
	requireFreshMemoryBackendProbeObservations(t, probeTimeout, parentMarker, observations...)
}

func TestMemoryBackendRetiresStaleCandidateUsingPersistedRoute(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 30, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleActive)
	resolution, err := (endpointpolicy.PublicHTTPSPolicy{Resolver: memoryStaticResolver{
		testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")},
	}}).Resolve(context.Background(), backend.Spec.Deployment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	storeProbe := validMemoryBackendStoreProbeResult()
	specDigest, err := memoryBackendSpecDigest(backend.Spec)
	if err != nil {
		t.Fatal(err)
	}
	candidate := memoryBackendValidationCandidateSnapshot{
		Version: "v1", Namespace: backend.Namespace, NamespaceUID: string(namespace.UID),
		BackendUID: string(backend.UID), BackendGeneration: backend.Generation,
		RequestedLifecycle:    backend.Spec.RequestedLifecycle(),
		LifecycleIntentDigest: strings.TrimSpace(backend.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation]),
		SpecDigest:            specDigest, EndpointIdentity: resolution.Identity, EndpointDigest: resolution.EndpointDigest,
		ResolvedAddressDigest: resolution.ResolvedAddressDigest, ServerCertificateDigest: storeProbe.ServerCertificateDigest,
		SecretName: secret.Name, SecretKey: backend.Spec.ClientAuth.BearerTokenSecretRef.Key,
		SecretUID: string(secret.UID), SecretResourceVersion: secret.ResourceVersion,
		TenantID: deriveMemoryBackendTenantID(testMemoryClusterID, namespace.UID), StoreName: backend.Spec.Store.Name,
		StoreUUID: storeProbe.StoreUUID, Protocol: backend.Spec.Protocol.Profile,
		AuthorityEpoch: 3, RoutingEpoch: 7, CandidateDigest: "candidate-old-route",
	}
	encoded, err := encodeMemoryBackendValidationCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	backend.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation] = encoded
	prober := &fakeMemoryBackendProber{storeResult: storeProbe}
	coordinator := &fakeMemoryBackendCoordinator{}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)
	resolver := newMemoryBackendRecordingResolver()
	reconciler.EndpointPolicy.Resolver = resolver
	reconciler.ProbeTimeout = 5 * time.Second
	parentMarker := "candidate-retirement"
	parentCtx := context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker)

	fresh := getMemoryBackend(t, kubeClient, backend.Namespace)
	fresh.Spec.Deployment.Endpoint = "https://replacement.example.com"
	fresh.Generation++
	newSpecDigest, err := memoryBackendSpecDigest(fresh.Spec)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation] = memoryBackendLifecycleIntentDigest(
		string(fresh.UID), fresh.Generation, fresh.Spec.RequestedLifecycle(), newSpecDigest,
	)
	if err := kubeClient.Update(context.Background(), fresh); err != nil {
		t.Fatalf("update stale backend route: %v", err)
	}

	if err := reconciler.fenceAndRetireMemoryBackendValidationCandidate(parentCtx, fresh, candidate); err != nil {
		t.Fatalf("fenceAndRetireMemoryBackendValidationCandidate: %v", err)
	}
	if prober.fenceCalls != 1 || prober.fenceTarget.EndpointIdentity != candidate.EndpointIdentity ||
		prober.fenceTarget.RoutingEpoch != candidate.RoutingEpoch {
		t.Fatalf("fence target = %+v, calls=%d", prober.fenceTarget, prober.fenceCalls)
	}
	if coordinator.retireCalls != 1 || coordinator.retireRequest.CandidateDigest != candidate.CandidateDigest ||
		!coordinator.retireRequest.RemoteFenceAcknowledged {
		t.Fatalf("retirement = %+v, calls=%d", coordinator.retireRequest, coordinator.retireCalls)
	}
	if len(resolver.observations) != 1 || len(prober.storeObservations) != 1 || len(prober.fenceObservations) != 1 {
		t.Fatalf("retirement observations resolve=%d store=%d fence=%d",
			len(resolver.observations), len(prober.storeObservations), len(prober.fenceObservations))
	}
	requireFreshMemoryBackendProbeObservations(t, reconciler.ProbeTimeout, parentMarker,
		resolver.observations[0], prober.storeObservations[0], prober.fenceObservations[0])
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if updated.Spec.Deployment.Endpoint != "https://replacement.example.com" ||
		strings.TrimSpace(updated.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation]) != "" {
		t.Fatalf("retired backend = %+v", updated)
	}
}

func TestMemoryBackendReconcilerDoesNotProbeWithoutDurableClaimAttempt(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleActive)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(10 * time.Minute))}
	coordinator := &fakeMemoryBackendCoordinator{claimAttemptErr: errors.New("audit unavailable")}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)

	reconcileMemoryBackendTwice(t, reconciler, backend.Namespace)
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if prober.calls != 0 || coordinator.bindingCalls != 0 || updated.Status.Reason != "OwnershipClaimAttemptPersistenceFailed" {
		t.Fatalf("status=%+v probes=%d bindingCalls=%d", updated.Status, prober.calls, coordinator.bindingCalls)
	}
}

func TestMemoryBackendReconcilerRejectsSecretBindingMismatchBeforeProbe(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	secret.Annotations[MemoryBackendAuthStoreNameAnnotation] = "other-store"
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(time.Minute))}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, nil, backend, namespace, secret)

	reconcileMemoryBackendTwice(t, reconciler, backend.Namespace)
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if updated.Status.ResolvedRefs || updated.Status.Connected || updated.Status.Ready || prober.storeCalls != 0 || prober.calls != 0 {
		t.Fatalf("mismatched Secret status/probes = %+v calls=%d/%d", updated.Status, prober.storeCalls, prober.calls)
	}
	if updated.Status.Reason != "SecretBindingRejected" || updated.Status.ValidationExpiresAt == nil || updated.Status.ValidationExpiresAt.After(now) {
		t.Fatalf("mismatched Secret reason/expiry = %+v", updated.Status)
	}
}

func TestMemoryBackendReconcilerUsesProviderCapabilityExpiry(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	prober := &fakeMemoryBackendProber{result: validMemoryBackendProbeResult(now.Add(90 * time.Second))}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, &fakeMemoryBackendCoordinator{}, backend, namespace, secret)

	reconcileMemoryBackendTwice(t, reconciler, backend.Namespace)
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if updated.Status.ValidationExpiresAt == nil || !updated.Status.ValidationExpiresAt.Time.Equal(now.Add(90*time.Second)) {
		t.Fatalf("validation expiry = %v", updated.Status.ValidationExpiresAt)
	}
}

func TestMemoryBackendReconcilerDisabledAdvancesAuthenticatedRemoteFenceAfterLocalBarrier(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleDisabled)
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	seedActivatedMemoryBackendStatus(t, backend, namespace, secret, 7)
	events := []string{}
	prober := &fakeMemoryBackendProber{events: &events}
	route := testMemoryBackendDurableRoute(t, backend, namespace, secret, 3, 8)
	coordinator := &fakeMemoryBackendCoordinator{events: &events, bindingResults: []MemoryBackendBindingResult{
		{EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleRecovering,
			AuthorityEpoch: 3, RoutingEpoch: 8, Reason: "RemoteFencePending", Route: route},
		{EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDisabled,
			AuthorityEpoch: 3, RoutingEpoch: 8, Reason: "Disabled", Message: "local and remote routing fences advanced", Route: route},
	}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)
	resolver := newMemoryBackendRecordingResolver()
	reconciler.EndpointPolicy.Resolver = resolver
	reconciler.ProbeTimeout = 5 * time.Second
	parentMarker := "durable-remote-fence"
	parentCtx := context.WithValue(context.Background(), memoryBackendProbeParentContextKey{}, parentMarker)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(parentCtx, request); err != nil {
		t.Fatal(err)
	}
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if coordinator.bindingCalls != 2 || prober.fenceCalls != 1 || updated.Status.Ready ||
		updated.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDisabled || updated.Status.RoutingEpoch != 8 {
		t.Fatalf("disabled barrier result = %+v, coordinator=%d fence=%d", updated.Status, coordinator.bindingCalls, prober.fenceCalls)
	}
	wantBarrierEvents := []string{testCoordinatorBinding, "prober.store", testProberFence, "prober.store", testCoordinatorBinding}
	if !slices.Equal(events, wantBarrierEvents) {
		t.Fatalf("barrier call order = %v, want %v", events, wantBarrierEvents)
	}
	if prober.fenceTarget.BearerToken != "test-bearer-token" || prober.fenceTarget.RoutingEpoch != 8 ||
		prober.fenceTarget.AuthorityEpoch != 3 || len(prober.fenceTarget.ResolvedAddresses) != 1 {
		t.Fatalf("routing fence target = %+v", prober.fenceTarget)
	}
	if len(resolver.observations) != 2 || len(prober.storeObservations) != 2 || len(prober.fenceObservations) != 1 {
		t.Fatalf("durable route observations resolve=%d store=%d fence=%d",
			len(resolver.observations), len(prober.storeObservations), len(prober.fenceObservations))
	}
	observations := append([]memoryBackendProbeObservation(nil), resolver.observations...)
	observations = append(observations, prober.storeObservations...)
	observations = append(observations, prober.fenceObservations[0])
	requireFreshMemoryBackendProbeObservations(t, reconciler.ProbeTimeout, parentMarker, observations...)
}

func TestMemoryBackendReconcilerDisabledKeepsPreviousLifecycleWhenRemoteFenceUnavailable(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleDisabled)
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	seedActivatedMemoryBackendStatus(t, backend, namespace, secret, 7)
	prober := &fakeMemoryBackendProber{fenceErr: newOMSProbeError("OMSProbeTransportFailed", "OMS routing fence request failed")}
	route := testMemoryBackendDurableRoute(t, backend, namespace, secret, 3, 8)
	coordinator := &fakeMemoryBackendCoordinator{bindingResult: MemoryBackendBindingResult{
		EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleRecovering,
		AuthorityEpoch:          3, RoutingEpoch: 8, Reason: "RemoteFencePending", Route: route,
	}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if updated.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive ||
		updated.Status.RoutingEpoch != 7 || updated.Status.Reason != "OMSProbeTransportFailed" || prober.fenceCalls != 1 {
		t.Fatalf("failed remote fence published unsafe lifecycle: %+v", updated.Status)
	}
}

func TestMemoryBackendReconcilerDecommissioningPersistsFenceBeforeTerminalState(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleDecommissioning)
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	seedActivatedMemoryBackendStatus(t, backend, namespace, secret, 7)
	events := []string{}
	prober := &fakeMemoryBackendProber{events: &events}
	route := testMemoryBackendDurableRoute(t, backend, namespace, secret, 3, 9)
	coordinator := &fakeMemoryBackendCoordinator{events: &events, bindingResults: []MemoryBackendBindingResult{
		{EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioning, AuthorityEpoch: 3, RoutingEpoch: 9,
			Reason: "RemoteFencePending", Route: route},
		{EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned, AuthorityEpoch: 3, RoutingEpoch: 9,
			Reason: "Decommissioned", Route: route},
	}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	terminal := getMemoryBackend(t, kubeClient, backend.Namespace)
	if terminal.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned ||
		terminal.Status.RoutingEpoch != 9 || prober.fenceCalls != 1 {
		t.Fatalf("terminal decommission status/fences = %+v / %d", terminal.Status, prober.fenceCalls)
	}
	if !slices.Equal(events, []string{testCoordinatorBinding, "prober.store", testProberFence, "prober.store", testCoordinatorBinding}) {
		t.Fatalf("decommission barrier order = %v", events)
	}
}

func TestMemoryBackendDeletionFailsClosedWithoutCoordinator(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	deletedAt := metav1.NewTime(now.Add(-time.Minute))
	backend.DeletionTimestamp = &deletedAt
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, nil, nil, backend, namespace, secret)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := getMemoryBackend(t, kubeClient, backend.Namespace)
	if !hasMemoryBackendFinalizer(updated.Finalizers) || updated.Status.Reason != "DeletionBlocked" {
		t.Fatalf("deletion status/finalizers = %+v %v", updated.Status, updated.Finalizers)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, string(corev1alpha1.MemoryBackendConditionDeletionSafe))
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("DeletionSafe condition = %+v", condition)
	}
}

func TestMemoryBackendDeletionRemovesFinalizerOnlyAfterSafeBarrier(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	deletedAt := metav1.NewTime(now.Add(-time.Minute))
	backend.DeletionTimestamp = &deletedAt
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	coordinator := &fakeMemoryBackendCoordinator{deleteResult: MemoryBackendDeletionResult{
		SafeToRemove:            true,
		EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleStaged,
		Reason:                  "NeverActivated",
		Message:                 "no durable remote binding exists",
	}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, nil, coordinator, backend, namespace, secret)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.MemoryBackend{}
	err := kubeClient.Get(context.Background(), request.NamespacedName, updated)
	if err == nil && hasMemoryBackendFinalizer(updated.Finalizers) {
		t.Fatalf("safe deletion retained finalizer: %v", updated.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if coordinator.deleteCalls != 1 {
		t.Fatalf("delete barrier calls = %d", coordinator.deleteCalls)
	}
}

func TestMemoryBackendDeletionRequiresRemoteFenceForDecommissionedAuthority(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleDecommissioning)
	seedActivatedMemoryBackendStatus(t, backend, namespace, secret, 8)
	backend.Status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned
	deletedAt := metav1.NewTime(now.Add(-time.Minute))
	backend.DeletionTimestamp = &deletedAt
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	prober := &fakeMemoryBackendProber{fenceErr: newOMSProbeError("OMSProbeTransportFailed", "remote adapter unavailable")}
	coordinator := &fakeMemoryBackendCoordinator{deleteResult: MemoryBackendDeletionResult{
		SafeToRemove: true, EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned,
		AuthorityEpoch: 3, RoutingEpoch: 8, Reason: "Decommissioned",
	}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace, secret)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.MemoryBackend{}
	err := kubeClient.Get(context.Background(), request.NamespacedName, updated)
	if err == nil && hasMemoryBackendFinalizer(updated.Finalizers) {
		t.Fatalf("decommissioned deletion retained finalizer: %v", updated.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if prober.fenceCalls != 0 {
		t.Fatalf("decommissioned deletion recontacted remote adapter %d times", prober.fenceCalls)
	}
}

func TestMemoryBackendDeletionForceOrphanRemovedBypassesUnavailableRemote(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	backend, namespace, _ := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleDecommissioning)
	backend.Status = corev1alpha1.MemoryBackendStatus{
		EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleActive,
		NamespaceUID:            string(namespace.UID), BackendUID: string(backend.UID), AuthorityEpoch: 3, RoutingEpoch: 7,
	}
	deletedAt := metav1.NewTime(now.Add(-time.Minute))
	backend.DeletionTimestamp = &deletedAt
	backend.Finalizers = []string{MemoryBackendProtectionFinalizer}
	prober := &fakeMemoryBackendProber{fenceErr: newOMSProbeError("OMSProbeTransportFailed", "remote adapter unavailable")}
	coordinator := &fakeMemoryBackendCoordinator{deleteResult: MemoryBackendDeletionResult{
		SafeToRemove: true, EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleRemoved,
		AuthorityEpoch: 3, RoutingEpoch: 10, Reason: "Removed",
	}}
	reconciler, kubeClient := newMemoryBackendReconciler(t, now, prober, coordinator, backend, namespace)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: backend.Namespace, Name: backend.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.MemoryBackend{}
	err := kubeClient.Get(context.Background(), request.NamespacedName, updated)
	if err == nil && hasMemoryBackendFinalizer(updated.Finalizers) {
		t.Fatalf("force-orphan removal retained finalizer: %v", updated.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if prober.fenceCalls != 0 {
		t.Fatalf("force-orphan deletion unexpectedly contacted remote adapter %d times", prober.fenceCalls)
	}
}

func TestMemoryBackendPrimaryWatchPredicate(t *testing.T) {
	watchPredicate := memoryBackendPrimaryWatchPredicate()
	base := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: "memory-test", Generation: 7,
	}}
	generationChanged := base.DeepCopy()
	generationChanged.Generation++
	metadataOnly := base.DeepCopy()
	metadataOnly.ResourceVersion = "2"
	metadataOnly.Annotations = map[string]string{"example.com/changed": "updated"}
	statusOnly := base.DeepCopy()
	statusOnly.Status.Ready = true
	deleting := base.DeepCopy()
	deletedAt := metav1.NewTime(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	deleting.DeletionTimestamp = &deletedAt
	stillDeleting := deleting.DeepCopy()
	stillDeleting.ResourceVersion = "3"
	deletionCleared := deleting.DeepCopy()
	deletionCleared.DeletionTimestamp = nil
	deletionUpdate := event.UpdateEvent{ObjectOld: base, ObjectNew: deleting}
	if (predicate.GenerationChangedPredicate{}).Update(deletionUpdate) {
		t.Fatal("GenerationChangedPredicate unexpectedly accepted a deletionTimestamp-only update")
	}

	tests := []struct {
		name string
		old  client.Object
		new  client.Object
		want bool
	}{
		{name: "generation changed", old: base, new: generationChanged, want: true},
		{name: "deletion timestamp set without generation change", old: base, new: deleting, want: true},
		{name: "deletion timestamp cleared defensively", old: deleting, new: deletionCleared, want: true},
		{name: "metadata only", old: base, new: metadataOnly, want: false},
		{name: "status only", old: base, new: statusOnly, want: false},
		{name: "unchanged deletion timestamp", old: deleting, new: stillDeleting, want: false},
		{name: "missing old object", new: base, want: false},
		{name: "missing new object", old: base, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchPredicate.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}); got != tt.want {
				t.Fatalf("Update() = %t, want %t", got, tt.want)
			}
		})
	}
	if !watchPredicate.Create(event.CreateEvent{Object: base}) {
		t.Fatal("Create() = false, want true")
	}
	if !watchPredicate.Delete(event.DeleteEvent{Object: base}) {
		t.Fatal("Delete() = false, want true")
	}
}

func TestMemoryBackendWatchMappers(t *testing.T) {
	scheme := memoryBackendTestScheme(t)
	backend, namespace, secret := memoryBackendTestObjects(t, corev1alpha1.MemoryBackendLifecycleStaged)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&corev1alpha1.MemoryBackend{}, memoryBackendSecretReferenceField, func(object client.Object) []string {
			return []string{object.(*corev1alpha1.MemoryBackend).Spec.ClientAuth.BearerTokenSecretRef.Name}
		}).
		WithObjects(backend, namespace, secret).
		Build()
	reconciler := &MemoryBackendReconciler{Client: kubeClient}

	requests := reconciler.memoryBackendsForSecret(context.Background(), secret)
	if len(requests) != 1 || requests[0].Namespace != backend.Namespace || requests[0].Name != backend.Name {
		t.Fatalf("memoryBackendsForSecret() = %v", requests)
	}
	requests = reconciler.memoryBackendForNamespace(context.Background(), namespace)
	if len(requests) != 1 || requests[0].Namespace != namespace.Name || requests[0].Name != corev1alpha1.MemoryBackendDefaultName {
		t.Fatalf("memoryBackendForNamespace() = %v", requests)
	}
}

func TestValidateMemoryBackendProbeResultRejectsMissingCapability(t *testing.T) {
	result := validMemoryBackendProbeResult(time.Now().Add(time.Minute))
	result.Capabilities = result.Capabilities[:len(result.Capabilities)-1]
	if err := validateMemoryBackendProbeResult(result, time.Now()); err == nil || !strings.Contains(err.Error(), "missing required capability") {
		t.Fatalf("validateMemoryBackendProbeResult() error = %v", err)
	}
}

func TestOMSHTTPProberPerformsExactProtocolSequence(t *testing.T) {
	now := time.Now().UTC()
	resolver := memoryStaticResolver{testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")}}
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: resolver}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+testProbeToken {
			t.Fatalf("Authorization header = %q", request.Header.Get("Authorization"))
		}
		requestBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var response any
		status := http.StatusOK
		switch request.URL.Path {
		case omsprotocol.PathStoreResolve:
			decoded, err := omsprotocol.DecodeStoreResolveRequest(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			response = omsprotocol.StoreResolveResponse{
				ProtocolVersion: omsprotocol.Version, Binding: decoded.Binding,
				StoreName: decoded.StoreName, StoreUUID: testMemoryStoreUUID,
			}
		case omsprotocol.PathCapabilities:
			decoded, err := omsprotocol.DecodeCapabilitiesRequest(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			response = omsprotocol.CapabilitiesResponse{
				ProtocolVersion: omsprotocol.Version, Binding: decoded.Binding,
				AdapterName: "adapter", AdapterVersion: "v1", Revision: "revision-1",
				ExpiresAt: now.Add(time.Minute), Capabilities: validOMSProtocolCapabilities(),
				Limits: validOMSProtocolCapabilityLimits(),
			}
		case omsprotocol.PathOwnershipClaim:
			decoded, err := omsprotocol.DecodeOwnershipClaimRequest(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			response = omsprotocol.OwnershipClaimResponse{
				ProtocolVersion: omsprotocol.Version, Binding: decoded.Binding, Result: omsprotocol.ResultApplied,
				BindingDigest: omsprotocol.BindingDigest(decoded.Binding), ClaimIdentity: omsprotocol.AuthorityDigest(decoded.Binding),
				MaximumRoutingEpoch: decoded.Binding.RoutingEpoch, ClaimedAt: now,
			}
		case omsprotocol.PathRoutingFence:
			decoded, err := omsprotocol.DecodeRoutingFenceRequest(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			response = omsprotocol.RoutingFenceResponse{
				ProtocolVersion: omsprotocol.Version, Binding: decoded.Binding, Result: omsprotocol.ResultApplied,
				BindingDigest:       omsprotocol.BindingDigest(decoded.Binding),
				MaximumRoutingEpoch: decoded.Binding.RoutingEpoch, CompletedAt: now,
			}
		default:
			t.Fatalf("unexpected probe path %q", request.URL.Path)
		}
		body, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return omsTestHTTPResponse(request, status, body), nil
	})
	prober := &OMSHTTPProber{
		Policy: policy,
		newClient: func(*http.Client, time.Duration, endpointpolicy.Resolution) (*http.Client, error) {
			return &http.Client{Transport: transport}, nil
		},
	}
	store, err := prober.ResolveStore(context.Background(), MemoryBackendStoreProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses,
		BearerToken:       testProbeToken, Profile: corev1alpha1.MemoryBackendProfileV0Alpha1,
		ClusterID: testMemoryClusterID, NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		TenantID: deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID), StoreName: "store-a", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := prober.ProbeBinding(context.Background(), MemoryBackendProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses,
		BearerToken:       testProbeToken, Profile: corev1alpha1.MemoryBackendProfileV0Alpha1,
		ClusterID: testMemoryClusterID, NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		TenantID: deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID), StoreUUID: store.StoreUUID,
		AuthorityEpoch: 3, RoutingEpoch: 7, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.StoreUUID != testMemoryStoreUUID || store.ServerCertificateDigest != result.ServerCertificateDigest ||
		result.OwnershipClaimIdentity == "" || result.CapabilityRevision != "revision-1" ||
		!strings.HasPrefix(result.ServerCertificateDigest, "sha256:") {
		t.Fatalf("store/probe results = %+v %+v", store, result)
	}
	fence, err := prober.AdvanceRoutingFence(context.Background(), MemoryBackendRoutingFenceTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, ExpectedServerCertificateDigest: result.ServerCertificateDigest,
		BearerToken: testProbeToken, Profile: corev1alpha1.MemoryBackendProfileV0Alpha1,
		ClusterID: testMemoryClusterID, NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		TenantID: deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID), StoreUUID: store.StoreUUID,
		AuthorityEpoch: 3, RoutingEpoch: 8, Timeout: time.Second,
	})
	if err != nil || fence.MaximumRoutingEpoch != 8 {
		t.Fatalf("AdvanceRoutingFence() = %+v, %v", fence, err)
	}
}

func TestOMSHTTPProberActualAdapterFenceRejectsStaleMutation(t *testing.T) {
	const token = "adapter-test-token"
	adapter, err := referenceadapter.Open(context.Background(), referenceadapter.Config{
		DatabasePath: filepath.Join(t.TempDir(), "adapter.db"), BearerToken: token,
		CapabilityTTL: time.Minute, SnapshotTTL: time.Minute, MaxSnapshotRecords: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	server := httptest.NewTLSServer(adapter.Handler())
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dialer := &memoryRedirectDialer{target: server.Listener.Addr().String()}
	policy := endpointpolicy.PublicHTTPSPolicy{
		Resolver: memoryStaticResolver{"example.com": {netip.MustParseAddr("8.8.8.8")}},
		Dialer:   dialer,
	}
	resolution, err := policy.Resolve(context.Background(), "https://example.com:"+port)
	if err != nil {
		t.Fatal(err)
	}
	prober := &OMSHTTPProber{Policy: policy, BaseClient: server.Client()}
	tenantID := deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID)
	store, err := prober.ResolveStore(context.Background(), MemoryBackendStoreProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, BearerToken: token,
		Profile: corev1alpha1.MemoryBackendProfileV0Alpha1, ClusterID: testMemoryClusterID,
		NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		TenantID: tenantID, StoreName: "store-a", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := prober.ProbeBinding(context.Background(), MemoryBackendProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, BearerToken: token,
		Profile: corev1alpha1.MemoryBackendProfileV0Alpha1, ClusterID: testMemoryClusterID,
		NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		TenantID: tenantID, StoreUUID: store.StoreUUID, AuthorityEpoch: 1, RoutingEpoch: 1, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prober.AdvanceRoutingFence(context.Background(), MemoryBackendRoutingFenceTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, ExpectedServerCertificateDigest: probe.ServerCertificateDigest,
		BearerToken: token, Profile: corev1alpha1.MemoryBackendProfileV0Alpha1,
		ClusterID: testMemoryClusterID, NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		TenantID: tenantID, StoreUUID: store.StoreUUID, AuthorityEpoch: 1, RoutingEpoch: 2, Timeout: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	staleBinding := omsprotocol.Binding{
		ClusterID: testMemoryClusterID, NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
		AuthorityEpoch: 1, RoutingEpoch: 1, TenantID: tenantID, StoreUUID: store.StoreUUID,
	}
	mutation := omsprotocol.MutationEnvelope{
		ProtocolVersion: omsprotocol.Version, OperationID: "mop-stale-after-controller-fence", Binding: staleBinding,
		MemoryID: "mem-stale-after-controller-fence", Kind: omsprotocol.MutationKindCreate, Generation: 1,
		State: &omsprotocol.MutationState{Content: "must not be applied", Tags: []string{}, Metadata: map[string]string{}},
	}
	if err := omsprotocol.PrepareMutation(&mutation); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(mutation)
	if err != nil {
		t.Fatal(err)
	}
	pinnedClient, err := policy.NewPinnedHTTPClient(server.Client(), time.Second, resolution)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, resolution.BaseURL+omsprotocol.PathMutations, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := pinnedClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, decodeErr := omsprotocol.DecodeMutationReceipt(responseBody)
	if response.StatusCode != http.StatusConflict || decodeErr != nil || decoded.Result != omsprotocol.ResultIdentityConflict ||
		decoded.AppliedGeneration != 0 || decoded.BackendMemoryID != "" {
		t.Fatalf("stale mutation response status=%d body=%s decodeErr=%v", response.StatusCode, responseBody, decodeErr)
	}
	for _, address := range dialer.addresses {
		if address != net.JoinHostPort("8.8.8.8", port) {
			t.Fatalf("credential-bearing request escaped pinned address set: %v", dialer.addresses)
		}
	}
}

func TestOMSHTTPProberRejectsNonProtocolStoreResponses(t *testing.T) {
	resolver := memoryStaticResolver{testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")}}
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: resolver}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for name, responseBody := range map[string][]byte{
		"unknown field": []byte(`{"protocolVersion":"orka.oms.v0alpha1","binding":{"clusterId":"cluster-a","namespaceUid":"namespace-uid-1","backendUid":"backend-uid-1","tenantId":"orka-tenant-sha256:00"},"storeName":"store-a","storeUuid":"` + testMemoryStoreUUID + `","extra":true}`),
		"trailing data": []byte(`{} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			prober := &OMSHTTPProber{
				Policy: policy,
				newClient: func(*http.Client, time.Duration, endpointpolicy.Resolution) (*http.Client, error) {
					return &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
						return omsTestHTTPResponse(request, http.StatusOK, responseBody), nil
					})}, nil
				},
			}
			_, err := prober.ResolveStore(context.Background(), MemoryBackendStoreProbeTarget{
				Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
				EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
				ResolvedAddresses: resolution.Addresses,
				BearerToken:       testProbeToken, Profile: corev1alpha1.MemoryBackendProfileV0Alpha1,
				ClusterID: testMemoryClusterID, NamespaceUID: string(testMemoryNamespaceUID), BackendUID: string(testMemoryBackendUID),
				TenantID: deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID), StoreName: "store-a", Timeout: time.Second,
			})
			if err == nil || !strings.Contains(err.Error(), "strict OMS decoding") {
				t.Fatalf("ResolveStore() error = %v", err)
			}
		})
	}
}

func omsTestHTTPResponse(request *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		TLS: &tls.ConnectionState{
			HandshakeComplete: true,
			PeerCertificates:  []*x509.Certificate{{Raw: []byte("certificate")}},
		},
		ContentLength: int64(len(body)),
		Request:       request,
	}
}

func validOMSProtocolCapabilities() omsprotocol.Capabilities {
	return omsprotocol.Capabilities{
		DurableIdempotency: true, IdempotencyDigestConflicts: true, CreateIfAbsent: true,
		ConditionalMutation: true, MonotonicGenerations: true, DeleteHighWatermark: true,
		DurableRoutingFence: true, OperationLookup: true, ExactGet: true, StablePagination: true,
		ExclusiveOwnership: true, KeywordSearch: true, AuditVersionVisibility: true,
	}
}

func validOMSProtocolCapabilityLimits() omsprotocol.CapabilityLimits {
	return omsprotocol.CapabilityLimits{
		MaxRequestBytes: omsprotocol.MaxHTTPBodyBytes, MaxResponseBytes: omsprotocol.MaxAdapterResponseBytes,
		MaxContentBytes: omsprotocol.MaxContentBytes, MaxTags: omsprotocol.MaxTags, MaxTagBytes: omsprotocol.MaxTagBytes,
		MaxMetadataEntries: omsprotocol.MaxMetadataEntries, MaxMetadataKeyBytes: omsprotocol.MaxMetadataKeyBytes,
		MaxMetadataValueBytes: omsprotocol.MaxMetadataValueBytes, MaxQueryBytes: omsprotocol.MaxQueryBytes,
		MaxPageSize: omsprotocol.MaxPageSize, MaxSnapshotRecords: 256, SnapshotTTLSeconds: 900,
	}
}

func newMemoryBackendReconciler(
	t *testing.T,
	now time.Time,
	prober MemoryBackendOMSProber,
	coordinator MemoryBackendBindingCoordinator,
	objects ...client.Object,
) (*MemoryBackendReconciler, client.Client) {
	t.Helper()
	scheme := memoryBackendTestScheme(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.MemoryBackend{}).
		WithObjects(objects...).
		Build()
	resolver := memoryStaticResolver{testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")}}
	return &MemoryBackendReconciler{
		Client: kubeClient, APIReader: kubeClient,
		EndpointPolicy: endpointpolicy.PublicHTTPSPolicy{Resolver: resolver},
		OMSProber:      prober, BindingCoordinator: coordinator, ClusterIdentity: testMemoryClusterID,
		Now: func() time.Time { return now }, ValidationTTL: 5 * time.Minute,
	}, kubeClient
}

func memoryBackendTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func memoryBackendTestObjects(
	t *testing.T,
	lifecycle corev1alpha1.MemoryBackendLifecycleState,
) (*corev1alpha1.MemoryBackend, *corev1.Namespace, *corev1.Secret) {
	t.Helper()
	backend := &corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{
			Name: corev1alpha1.MemoryBackendDefaultName, Namespace: "memory-test", UID: testMemoryBackendUID, Generation: 1,
		},
		Spec: corev1alpha1.MemoryBackendSpec{
			Protocol: corev1alpha1.MemoryBackendProtocol{
				OMSVersion: corev1alpha1.MemoryBackendOMSVersionV01,
				Profile:    corev1alpha1.MemoryBackendProfileV0Alpha1,
			},
			Deployment: corev1alpha1.MemoryBackendDeployment{
				Mode: corev1alpha1.MemoryBackendDeploymentModeExternalEndpoint, Endpoint: "https://memory.example.com",
			},
			ClientAuth: corev1alpha1.MemoryBackendClientAuth{BearerTokenSecretRef: corev1alpha1.MemoryBackendBearerTokenSecretReference{
				Name: "memory-auth", Key: "token",
			}},
			Store:          corev1alpha1.MemoryBackendStore{Name: "store-a"},
			LifecycleState: lifecycle,
		},
	}
	if protectedMemoryBackendLifecycle(lifecycle) {
		specDigest, err := memoryBackendSpecDigest(backend.Spec)
		if err != nil {
			t.Fatal(err)
		}
		backend.Annotations = map[string]string{
			corev1alpha1.MemoryBackendLifecycleIntentAnnotation: memoryBackendLifecycleIntentDigest(
				string(backend.UID), backend.Generation, lifecycle, specDigest,
			),
		}
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: backend.Namespace, UID: testMemoryNamespaceUID}}
	tenantID := deriveMemoryBackendTenantID(testMemoryClusterID, testMemoryNamespaceUID)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "memory-auth", Namespace: backend.Namespace, UID: types.UID("secret-uid-1"), ResourceVersion: "7",
			Labels: map[string]string{MemoryBackendClientAuthLabel: MemoryBackendClientAuthEnabledValue},
			Annotations: map[string]string{
				MemoryBackendAuthBackendUIDAnnotation:   string(backend.UID),
				MemoryBackendAuthEndpointAnnotation:     "https://memory.example.com",
				MemoryBackendAuthStoreNameAnnotation:    backend.Spec.Store.Name,
				MemoryBackendAuthNamespaceUIDAnnotation: string(namespace.UID),
				MemoryBackendAuthTenantIDAnnotation:     tenantID,
			},
		},
		Data: map[string][]byte{"token": []byte("test-bearer-token")},
	}
	return backend, namespace, secret
}

func testMemoryBackendDurableRoute(
	t *testing.T,
	backend *corev1alpha1.MemoryBackend,
	namespace *corev1.Namespace,
	secret *corev1.Secret,
	authorityEpoch, routingEpoch int64,
) MemoryBackendDurableRoute {
	t.Helper()
	resolution, err := (endpointpolicy.PublicHTTPSPolicy{Resolver: memoryStaticResolver{
		testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")},
	}}).Resolve(context.Background(), backend.Spec.Deployment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return MemoryBackendDurableRoute{
		Namespace: backend.Namespace, NamespaceUID: string(namespace.UID), ClusterID: testMemoryClusterID,
		BackendUID: string(backend.UID), BackendGeneration: backend.Generation,
		AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ServerCertificateDigest: "sha256:" + strings.Repeat("a", 64),
		SecretName:              backend.Spec.ClientAuth.BearerTokenSecretRef.Name,
		SecretKey:               backend.Spec.ClientAuth.BearerTokenSecretRef.Key, SecretUID: string(secret.UID),
		SecretResourceVersion: secret.ResourceVersion,
		TenantID:              deriveMemoryBackendTenantID(testMemoryClusterID, namespace.UID), StoreName: backend.Spec.Store.Name,
		StoreUUID: testMemoryStoreUUID, Protocol: string(backend.Spec.Protocol.Profile),
	}
}

func seedActivatedMemoryBackendStatus(
	t *testing.T,
	backend *corev1alpha1.MemoryBackend,
	namespace *corev1.Namespace,
	secret *corev1.Secret,
	routingEpoch int64,
) {
	t.Helper()
	resolution, err := (endpointpolicy.PublicHTTPSPolicy{Resolver: memoryStaticResolver{
		testMemoryBackendHost: {netip.MustParseAddr("8.8.8.8")},
	}}).Resolve(context.Background(), backend.Spec.Deployment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	specDigest, err := memoryBackendSpecDigest(backend.Spec)
	if err != nil {
		t.Fatal(err)
	}
	backend.Status = corev1alpha1.MemoryBackendStatus{
		Accepted: true, Protected: true, ResolvedRefs: true, Connected: true, Ready: true,
		ObservedGeneration: backend.Generation, EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleActive,
		ValidatedSpecDigest: specDigest, ClusterIdentityDigest: memoryBackendDigest(testMemoryClusterID),
		NamespaceUID: string(namespace.UID), BackendUID: string(backend.UID),
		AuthorityEpoch: 3, RoutingEpoch: routingEpoch,
		SecretUID: string(secret.UID), SecretResourceVersion: secret.ResourceVersion,
		EndpointIdentity: resolution.Identity, EndpointDigest: resolution.EndpointDigest,
		ResolvedAddressDigest:   resolution.ResolvedAddressDigest,
		ServerCertificateDigest: validMemoryBackendStoreProbeResult().ServerCertificateDigest,
		StoreUUID:               testMemoryStoreUUID, OwnershipClaimIdentity: "claim-1",
	}
}

func observeMemoryBackendProbeContext(ctx context.Context) memoryBackendProbeObservation {
	deadline, _ := ctx.Deadline()
	remaining := time.Duration(0)
	if !deadline.IsZero() {
		remaining = time.Until(deadline)
	}
	parentMarker, _ := ctx.Value(memoryBackendProbeParentContextKey{}).(string)
	return memoryBackendProbeObservation{
		deadline: deadline, remaining: remaining, parentMarker: parentMarker, err: ctx.Err(),
	}
}

func requireFreshMemoryBackendProbeObservations(
	t *testing.T,
	probeTimeout time.Duration,
	parentMarker string,
	observations ...memoryBackendProbeObservation,
) {
	t.Helper()
	for i, observation := range observations {
		if observation.err != nil {
			t.Fatalf("probe %d context error = %v", i, observation.err)
		}
		if observation.deadline.IsZero() {
			t.Fatalf("probe %d has no timeout deadline", i)
		}
		if observation.remaining <= probeTimeout/2 || observation.remaining > probeTimeout {
			t.Fatalf("probe %d remaining timeout = %s, want a fresh %s budget", i, observation.remaining, probeTimeout)
		}
		if observation.parentMarker != parentMarker {
			t.Fatalf("probe %d parent marker = %q, want %q", i, observation.parentMarker, parentMarker)
		}
		for j := range i {
			if observation.deadline.Equal(observations[j].deadline) {
				t.Fatalf("outbound OMS probes %d and %d reused the same timeout deadline", j, i)
			}
		}
	}
}

func reconcileMemoryBackendAfterFinalizer(
	t *testing.T,
	reconciler *MemoryBackendReconciler,
	namespace string,
	ctx context.Context,
) {
	t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("finalizer Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("validation Reconcile() error = %v", err)
	}
}

func reconcileMemoryBackendTwice(t *testing.T, reconciler *MemoryBackendReconciler, namespace string) {
	t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}}
	for i := range 2 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile(%d) error = %v", i+1, err)
		}
	}
}

func getMemoryBackend(t *testing.T, kubeClient client.Client, namespace string) *corev1alpha1.MemoryBackend {
	t.Helper()
	backend := &corev1alpha1.MemoryBackend{}
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}, backend); err != nil {
		t.Fatal(err)
	}
	return backend
}

func validMemoryBackendStoreProbeResult() MemoryBackendStoreProbeResult {
	return MemoryBackendStoreProbeResult{
		StoreUUID: testMemoryStoreUUID, ServerCertificateDigest: "sha256:" + strings.Repeat("a", 64),
	}
}

func validMemoryBackendProbeResult(expiry time.Time) MemoryBackendProbeResult {
	return MemoryBackendProbeResult{
		AdapterName: "adapter", AdapterVersion: "v1",
		OwnershipClaimIdentity: "claim-1", Capabilities: validMemoryBackendCapabilities(),
		CapabilityRevision: "revision-1", CapabilityLimits: validMemoryBackendCapabilityLimits(),
		CapabilityExpiresAt: expiry, ServerCertificateDigest: "sha256:" + strings.Repeat("a", 64),
	}
}

func validMemoryBackendCapabilities() []corev1alpha1.MemoryBackendCapability {
	return append([]corev1alpha1.MemoryBackendCapability(nil), requiredMemoryBackendCapabilities...)
}

func validMemoryBackendCapabilityLimits() corev1alpha1.MemoryBackendCapabilityLimits {
	return corev1alpha1.MemoryBackendCapabilityLimits{
		MaxRequestBytes: omsprotocol.MaxHTTPBodyBytes, MaxResponseBytes: omsprotocol.MaxAdapterResponseBytes, MaxContentBytes: omsprotocol.MaxContentBytes,
		MaxTags: 32, MaxTagBytes: 128, MaxMetadataEntries: 32, MaxMetadataKeyBytes: 64,
		MaxMetadataValueBytes: 1024, MaxQueryBytes: 1024, MaxPageSize: 8,
		MaxSnapshotRecords: 1024, SnapshotTTLSeconds: 900,
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func hasMemoryBackendFinalizer(values []string) bool {
	return slices.Contains(values, MemoryBackendProtectionFinalizer)
}
