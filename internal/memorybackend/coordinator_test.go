package memorybackend

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/store"
	storesqlite "github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	candidateFencePendingReason = "CandidateFencePending"
	refreshedCapabilityRevision = "cap-b"
)

//nolint:gocyclo // This integration-style test exercises activation, refresh, drain, and resume in one sequence.
func TestStoreCoordinatorActivationGateAndCutover(t *testing.T) {
	governed := newCoordinatorStore(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	validation := controller.MemoryBackendValidationSnapshot{
		Namespace: "team-a", NamespaceUID: "namespace-a", BackendUID: "backend-a", BackendGeneration: 1,
		ClusterID: "cluster-a", TenantID: "tenant-a", RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive,
		SpecDigest: "sha256:spec", EndpointDigest: "sha256:endpoint",
		ResolvedAddressDigest: "sha256:addresses", ServerCertificateDigest: "sha256:certificate",
		SecretName: "memory-auth", SecretKey: "token",
		SecretUID: "secret-a", SecretResourceVersion: "1", StoreName: "store-a", StoreUUID: "store-uuid-a", Protocol: "orka.oms.v0alpha1",
	}
	coordinator := &StoreCoordinator{Store: governed, ActivationEnabled: false, Now: func() time.Time { return now }}
	if _, err := coordinator.PrepareMemoryBackendValidation(context.Background(), validation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("activation-disabled PrepareMemoryBackendValidation() error = %v, want conflict", err)
	}
	if _, err := governed.GetMemoryBackendBinding(context.Background(), validation.NamespaceUID); err == nil {
		t.Fatal("activation-disabled coordinator created a binding")
	}

	if err := governed.UpsertControllerFeatureHeartbeat(context.Background(), store.ControllerFeatureHeartbeat{
		InstanceID: "replica-a", Role: "serving_dispatching", FeatureEpoch: FoundationFeatureEpoch,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := governed.UpsertControllerFeatureHeartbeat(context.Background(), store.ControllerFeatureHeartbeat{
		InstanceID: "replica-a", Role: "serving_dispatching", FeatureEpoch: ActivationFeatureEpoch,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	coordinator.ActivationEnabled = true
	authorizeValidationLifecycle(t, governed, &validation)
	prepared, err := coordinator.PrepareMemoryBackendValidation(context.Background(), validation)
	if err != nil || prepared.AuthorityEpoch != 1 || prepared.RoutingEpoch != 1 {
		t.Fatalf("PrepareMemoryBackendValidation() = %#v, %v", prepared, err)
	}
	claimPreparedCandidate(t, coordinator, validation, prepared)
	snapshot := coordinatorSnapshot(validation, prepared, now)
	recordCoordinatorActivationRecoveryReceipt(t, governed, bindingFromSnapshot(snapshot, store.MemoryBackendBindingAccepting), now)
	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive {
		t.Fatalf("activation result = %#v", result)
	}
	binding, err := governed.GetMemoryBackendBinding(context.Background(), validation.NamespaceUID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Mode != store.MemoryBackendModeRemote || binding.State != store.MemoryBackendBindingAccepting {
		t.Fatalf("binding = %#v", binding)
	}

	prepared, err = coordinator.PrepareMemoryBackendValidation(context.Background(), validation)
	if err != nil {
		t.Fatal(err)
	}
	claimPreparedCandidate(t, coordinator, validation, prepared)
	snapshot = coordinatorSnapshot(validation, prepared, now.Add(time.Minute))
	snapshot.Capabilities.Revision = refreshedCapabilityRevision
	result, err = coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready || result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDraining ||
		result.RoutingEpoch != prepared.RoutingEpoch {
		t.Fatalf("capability refresh did not install the old-route drain barrier: %#v", result)
	}
	prepared, err = coordinator.PrepareMemoryBackendValidation(context.Background(), validation)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.RemoteFenceRequired || prepared.RoutingEpoch <= result.RoutingEpoch {
		t.Fatalf("post-drain validation did not require a remote routing fence: %#v", prepared)
	}
	claimPreparedCandidate(t, coordinator, validation, prepared)
	snapshot = coordinatorSnapshot(validation, prepared, now.Add(2*time.Minute))
	snapshot.Capabilities.Revision = refreshedCapabilityRevision
	result, err = coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil || !result.Ready || result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive {
		t.Fatalf("post-fence validation result = %#v, %v", result, err)
	}
}

func claimPreparedCandidate(
	t *testing.T,
	coordinator *StoreCoordinator,
	validation controller.MemoryBackendValidationSnapshot,
	prepared controller.MemoryBackendValidationBinding,
) {
	t.Helper()
	if validation.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleActive &&
		validation.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleReadOnly {
		return
	}
	if err := coordinator.RecordMemoryBackendOwnershipClaimAttempt(context.Background(), controller.MemoryBackendOwnershipClaimAttemptSnapshot{
		Namespace: validation.Namespace, NamespaceUID: validation.NamespaceUID, BackendUID: validation.BackendUID,
		RequestedLifecycle: validation.RequestedLifecycle, AuthorityEpoch: prepared.AuthorityEpoch,
		RoutingEpoch: prepared.RoutingEpoch, CandidateDigest: prepared.CandidateDigest,
	}); err != nil {
		t.Fatal(err)
	}
}

func authorizeBindingReconcile(
	t *testing.T,
	coordinator *StoreCoordinator,
	governed store.GovernedMemoryStore,
	snapshot *controller.MemoryBackendBindingSnapshot,
) {
	t.Helper()
	authorizeBindingLifecycle(t, governed, snapshot)
	if snapshot.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleActive &&
		snapshot.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleReadOnly {
		return
	}
	validation := controller.MemoryBackendValidationSnapshot{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
		BackendGeneration: snapshot.BackendGeneration, ClusterID: snapshot.ClusterID, TenantID: snapshot.TenantID,
		RequestedLifecycle: snapshot.RequestedLifecycle, SpecDigest: snapshot.SpecDigest,
		EndpointIdentity: snapshot.EndpointIdentity, EndpointDigest: snapshot.EndpointDigest,
		ResolvedAddressDigest: snapshot.ResolvedAddressDigest, ServerCertificateDigest: snapshot.ServerCertificateDigest,
		SecretName: snapshot.SecretName, SecretKey: snapshot.SecretKey, SecretUID: snapshot.SecretUID,
		SecretResourceVersion: snapshot.SecretResourceVersion, StoreName: snapshot.StoreName,
		StoreUUID: snapshot.StoreUUID, Protocol: snapshot.Protocol,
	}
	digest := validationCandidateDigest(validation)
	snapshot.CandidateDigest = digest
	if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: "test",
		Action: memoryValidationCandidateAuditAction, Reason: "test candidate", PreviousState: memoryValidationCandidateTrackingState,
		NewState: string(snapshot.RequestedLifecycle), AuthorityEpoch: snapshot.AuthorityEpoch,
		RoutingEpoch: snapshot.RoutingEpoch, RequestDigest: digest, RequestID: snapshot.BackendUID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	claimPreparedCandidate(t, coordinator, validation, controller.MemoryBackendValidationBinding{
		AuthorityEpoch: snapshot.AuthorityEpoch, RoutingEpoch: snapshot.RoutingEpoch, CandidateDigest: digest,
	})
	if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive {
		if _, err := governed.GetMemoryBackendBinding(context.Background(), snapshot.NamespaceUID); errors.Is(err, store.ErrNotFound) {
			recordCoordinatorActivationRecoveryReceipt(
				t, governed, bindingFromSnapshot(*snapshot, store.MemoryBackendBindingAccepting), time.Now().UTC(),
			)
		}
	}
}

func recordCoordinatorActivationRecoveryReceipt(
	t *testing.T,
	governed store.GovernedMemoryStore,
	binding store.MemoryBackendBinding,
	now time.Time,
) {
	t.Helper()
	if _, err := governed.RecordMemoryActivationRecoveryReceipt(context.Background(), store.MemoryActivationRecoveryReceipt{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		RouteDigest: binding.RecoveryRouteIdentity().Digest(), StoreUUID: binding.StoreUUID,
		ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor:          "test", Reason: "test recovery prerequisite", VerifiedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func coordinatorSnapshot(
	validation controller.MemoryBackendValidationSnapshot,
	prepared controller.MemoryBackendValidationBinding,
	now time.Time,
) controller.MemoryBackendBindingSnapshot {
	return controller.MemoryBackendBindingSnapshot{
		Namespace: validation.Namespace, NamespaceUID: validation.NamespaceUID, BackendUID: validation.BackendUID,
		BackendGeneration: validation.BackendGeneration, ClusterID: validation.ClusterID, TenantID: validation.TenantID,
		RequestedLifecycle: validation.RequestedLifecycle, SpecDigest: validation.SpecDigest,
		EndpointDigest: validation.EndpointDigest, ResolvedAddressDigest: validation.ResolvedAddressDigest,
		ServerCertificateDigest: validation.ServerCertificateDigest,
		SecretName:              validation.SecretName, SecretKey: validation.SecretKey,
		SecretUID: validation.SecretUID, SecretResourceVersion: validation.SecretResourceVersion, StoreName: validation.StoreName,
		Protocol: validation.Protocol, StoreUUID: validation.StoreUUID, OwnershipClaimIdentity: "claim-a",
		Capabilities:        corev1alpha1.MemoryBackendObservedCapabilities{Revision: "cap-a"},
		ValidationExpiresAt: now.Add(time.Hour), AuthorityEpoch: prepared.AuthorityEpoch, RoutingEpoch: prepared.RoutingEpoch,
		CandidateDigest: prepared.CandidateDigest, LifecycleIntentDigest: validation.LifecycleIntentDigest,
		RemoteFenceAcknowledged:  prepared.RemoteFenceRequired,
		AcknowledgedRoutingEpoch: map[bool]int64{true: prepared.RoutingEpoch}[prepared.RemoteFenceRequired],
	}
}

func authorizeValidationLifecycle(
	t *testing.T,
	governed store.GovernedMemoryStore,
	validation *controller.MemoryBackendValidationSnapshot,
) {
	t.Helper()
	if validation == nil || !protectedMemoryBackendLifecycle(validation.RequestedLifecycle) {
		return
	}
	digest := lifecycleIntentDigestForValidation(*validation)
	validation.LifecycleIntentDigest = digest
	for _, action := range []string{memoryBackendLifecycleIntentAuditAction, memoryBackendLifecycleRequestedAuditAction} {
		if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
			Namespace: validation.Namespace, NamespaceUID: validation.NamespaceUID, Actor: "test",
			Action: action, Reason: "test lifecycle authorization", NewState: string(validation.RequestedLifecycle),
			RequestDigest: digest, RequestID: "test-request", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func authorizeBindingLifecycle(
	t *testing.T,
	governed store.GovernedMemoryStore,
	snapshot *controller.MemoryBackendBindingSnapshot,
) {
	t.Helper()
	if snapshot == nil || !protectedMemoryBackendLifecycle(snapshot.RequestedLifecycle) {
		return
	}
	digest := lifecycleIntentDigestForBinding(*snapshot)
	snapshot.LifecycleIntentDigest = digest
	for _, action := range []string{memoryBackendLifecycleIntentAuditAction, memoryBackendLifecycleRequestedAuditAction} {
		if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
			Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: "test",
			Action: action, Reason: "test lifecycle authorization", NewState: string(snapshot.RequestedLifecycle),
			RequestDigest: digest, RequestID: "test-request", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func newCoordinatorStore(t *testing.T) *storesqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coordinator.db")
	db, err := storesqlite.NewDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return storesqlite.NewStore(db, path)
}

type coordinatorBindingStore struct {
	store.GovernedMemoryStore
	binding      store.MemoryBackendBinding
	getErr       error
	unresolved   bool
	transitioned bool
	refreshed    bool
	resolved     bool
	listedState  store.MemoryBackendBindingState
	events       []string
}

func (s *coordinatorBindingStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	binding := s.binding
	return &binding, nil
}

func (s *coordinatorBindingStore) TransitionMemoryBackendBinding(
	_ context.Context,
	transition store.MemoryBackendTransition,
) (*store.MemoryBackendBinding, error) {
	s.transitioned = true
	s.events = append(s.events, "transition:"+string(transition.State))
	s.binding.State = transition.State
	s.binding.RoutingEpoch = transition.RoutingEpoch
	binding := s.binding
	return &binding, nil
}

func (s *coordinatorBindingStore) RefreshMemoryBackendBinding(
	_ context.Context,
	refresh store.MemoryBackendBindingRefresh,
) (*store.MemoryBackendBinding, error) {
	s.refreshed = true
	s.events = append(s.events, "refresh:"+string(refresh.Binding.State))
	s.binding = refresh.Binding
	binding := s.binding
	return &binding, nil
}

func (s *coordinatorBindingStore) ListMemoryOperations(context.Context, store.MemoryOperationFilter) ([]store.MemoryOperation, error) {
	s.listedState = s.binding.State
	s.events = append(s.events, "list:"+string(s.binding.State))
	if s.unresolved {
		return []store.MemoryOperation{{ID: "mop-1", State: store.MemoryOperationQueued}}, nil
	}
	return nil, nil
}

func (s *coordinatorBindingStore) ResolveMemoryOperationsForDecommission(
	context.Context,
	store.MemoryDecommissionResolution,
) (int, error) {
	s.resolved = true
	s.unresolved = false
	s.events = append(s.events, "resolve:decommission")
	return 1, nil
}

func TestStoreCoordinatorRejectsDisabledWithoutRemoteAuthority(t *testing.T) {
	tests := []struct {
		name  string
		store *coordinatorBindingStore
	}{
		{
			name: "never activated",
			store: &coordinatorBindingStore{
				GovernedMemoryStore: newCoordinatorStore(t),
				getErr:              store.ErrNotFound,
			},
		},
		{
			name: "legacy authority",
			store: &coordinatorBindingStore{
				GovernedMemoryStore: newCoordinatorStore(t),
				binding: store.MemoryBackendBinding{
					Namespace: "team-a", NamespaceUID: "namespace-a", BackendUID: "backend-a",
					Mode: store.MemoryBackendModeLegacy, State: store.MemoryBackendBindingLegacy,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := coordinatorBindingSnapshot(coordinatorTestBinding(), corev1alpha1.MemoryBackendLifecycleDisabled)
			snapshot.OwnershipClaimIdentity = ""
			coordinator := &StoreCoordinator{Store: test.store}
			authorizeBindingReconcile(t, coordinator, test.store.GovernedMemoryStore, &snapshot)
			result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("ReconcileMemoryBackendBinding() error = %v, want ErrConflict", err)
			}
			if result.EffectiveLifecycleState == corev1alpha1.MemoryBackendEffectiveLifecycleDisabled {
				t.Fatalf("ReconcileMemoryBackendBinding() reported Disabled without remote authority: %#v", result)
			}
		})
	}
}

func TestStoreCoordinatorDisablesRemoteAuthorityWithoutNewOwnershipClaim(t *testing.T) {
	binding := coordinatorTestBinding()
	backing := newCoordinatorStore(t)
	wrapper := &coordinatorBindingStore{GovernedMemoryStore: backing, binding: binding}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleDisabled)
	snapshot.OwnershipClaimIdentity = ""
	coordinator := &StoreCoordinator{Store: wrapper}
	authorizeBindingReconcile(t, coordinator, backing, &snapshot)

	pending, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("ReconcileMemoryBackendBinding() error = %v", err)
	}
	if pending.Reason != "RemoteFencePending" || pending.RoutingEpoch != binding.RoutingEpoch+1 {
		t.Fatalf("pending disable result = %#v", pending)
	}
	if !wrapper.transitioned || wrapper.binding.State != store.MemoryBackendBindingRecovering {
		t.Fatalf("remote binding did not enter the durable Disabled barrier: %#v", wrapper.binding)
	}
	snapshot.RemoteFenceAcknowledged = true
	snapshot.AcknowledgedRoutingEpoch = pending.RoutingEpoch
	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDisabled || result.Ready ||
		result.RoutingEpoch != pending.RoutingEpoch {
		t.Fatalf("acknowledged disable result = %#v", result)
	}
}

func TestStoreCoordinatorEntersDrainingBeforeWaitingForReadOnlyOperations(t *testing.T) {
	binding := coordinatorTestBinding()
	backing := newCoordinatorStore(t)
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: backing, binding: binding, unresolved: true}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleReadOnly)
	authorizeBindingReconcile(t, coordinator, backing, &snapshot)
	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !wrapped.transitioned || wrapped.binding.State != store.MemoryBackendBindingDraining ||
		wrapped.listedState != store.MemoryBackendBindingDraining ||
		result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDraining ||
		result.RoutingEpoch != binding.RoutingEpoch || wrapped.binding.RoutingEpoch != binding.RoutingEpoch {
		t.Fatalf("drain result = %#v, binding = %#v", result, wrapped.binding)
	}
}

func TestPrepareReadOnlyPersistsDrainingBarrierBeforeUnresolvedCheck(t *testing.T) {
	binding := coordinatorTestBinding()
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding, unresolved: true}
	snapshot := controller.MemoryBackendValidationSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		BackendGeneration: binding.BackendGeneration, ClusterID: binding.ClusterID, TenantID: binding.TenantID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleReadOnly, SpecDigest: binding.SpecDigest,
		EndpointDigest: binding.EndpointDigest, SecretName: binding.SecretName, SecretKey: binding.SecretKey,
		SecretUID: binding.SecretUID, SecretResourceVersion: binding.SecretResourceVersion,
		StoreName: binding.StoreName, StoreUUID: binding.StoreUUID, Protocol: binding.Protocol,
		PreviousEffectiveLifecycle: corev1alpha1.MemoryBackendEffectiveLifecycleActive,
		PreviousAuthorityEpoch:     binding.AuthorityEpoch, PreviousRoutingEpoch: binding.RoutingEpoch,
	}
	authorizeValidationLifecycle(t, wrapped.GovernedMemoryStore, &snapshot)
	prepared, err := (&StoreCoordinator{Store: wrapped, ActivationEnabled: true}).PrepareMemoryBackendValidation(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.DrainRequired || !wrapped.transitioned || wrapped.listedState != store.MemoryBackendBindingDraining ||
		wrapped.binding.State != store.MemoryBackendBindingDraining || wrapped.binding.RoutingEpoch != binding.RoutingEpoch ||
		prepared.RoutingEpoch != binding.RoutingEpoch {
		t.Fatalf("prepared=%#v binding=%#v listedState=%q", prepared, wrapped.binding, wrapped.listedState)
	}
	wrapped.unresolved = false
	snapshot.PreviousEffectiveLifecycle = corev1alpha1.MemoryBackendEffectiveLifecycleDraining
	second, err := (&StoreCoordinator{Store: wrapped, ActivationEnabled: true}).PrepareMemoryBackendValidation(
		context.Background(), snapshot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !second.RemoteFenceRequired || second.RoutingEpoch != binding.RoutingEpoch+1 {
		t.Fatalf("post-drain ReadOnly preparation = %#v, want required routing fence", second)
	}
}

func TestStoreCoordinatorDecommissioningPersistsDrainingBarrierBeforeUnresolvedCheck(t *testing.T) {
	binding := coordinatorTestBinding()
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding, unresolved: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleDecommissioning)
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	authorizeBindingReconcile(t, coordinator, wrapped.GovernedMemoryStore, &snapshot)
	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !wrapped.transitioned || wrapped.listedState != "" ||
		wrapped.binding.State != store.MemoryBackendBindingDraining || wrapped.binding.RoutingEpoch != binding.RoutingEpoch ||
		result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioning ||
		result.Reason != "RemoteFencePending" || result.RoutingEpoch != binding.RoutingEpoch+1 {
		t.Fatalf("result=%#v binding=%#v listedState=%q", result, wrapped.binding, wrapped.listedState)
	}
}

func TestStoreCoordinatorReadOnlyPersistsDrainingAndRefreshedMetadataBeforeReady(t *testing.T) {
	binding := coordinatorTestBinding()
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleReadOnly)
	snapshot.RoutingEpoch = binding.RoutingEpoch + 1
	snapshot.SecretResourceVersion = "3"
	snapshot.Capabilities.Revision = refreshedCapabilityRevision
	snapshot.RemoteFenceAcknowledged = true
	snapshot.AcknowledgedRoutingEpoch = snapshot.RoutingEpoch
	authorizeBindingReconcile(t, coordinator, wrapped.GovernedMemoryStore, &snapshot)

	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly ||
		!wrapped.transitioned || !wrapped.refreshed || wrapped.binding.State != store.MemoryBackendBindingDraining ||
		wrapped.binding.RoutingEpoch != snapshot.RoutingEpoch || wrapped.binding.SecretResourceVersion != "3" ||
		wrapped.binding.CapabilityRevision != refreshedCapabilityRevision {
		t.Fatalf("ReadOnly result = %#v, binding = %#v", result, wrapped.binding)
	}
}

func TestStoreCoordinatorReadOnlyRefreshesAlreadyDrainingBinding(t *testing.T) {
	binding := coordinatorTestBinding()
	binding.State = store.MemoryBackendBindingDraining
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleReadOnly)
	snapshot.RoutingEpoch = binding.RoutingEpoch + 1
	snapshot.SecretResourceVersion = "3"
	snapshot.RemoteFenceAcknowledged = true
	snapshot.AcknowledgedRoutingEpoch = snapshot.RoutingEpoch
	authorizeBindingReconcile(t, coordinator, wrapped.GovernedMemoryStore, &snapshot)

	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || !wrapped.refreshed || wrapped.transitioned ||
		wrapped.binding.State != store.MemoryBackendBindingDraining || wrapped.binding.RoutingEpoch != snapshot.RoutingEpoch ||
		wrapped.binding.SecretResourceVersion != "3" {
		t.Fatalf("ReadOnly refresh result = %#v, binding = %#v", result, wrapped.binding)
	}
}

func TestStoreCoordinatorPersistsValidationExpiryOnlyRenewalWithoutRoutingAdvance(t *testing.T) {
	binding := coordinatorTestBinding()
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleActive)
	snapshot.ValidationExpiresAt = binding.ValidationExpiresAt.Add(time.Hour)
	authorizeBindingReconcile(t, coordinator, wrapped.GovernedMemoryStore, &snapshot)

	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || !wrapped.refreshed || result.RoutingEpoch != binding.RoutingEpoch ||
		wrapped.binding.RoutingEpoch != binding.RoutingEpoch ||
		!wrapped.binding.ValidationExpiresAt.Equal(snapshot.ValidationExpiresAt) {
		t.Fatalf("expiry-only refresh result = %#v, binding = %#v", result, wrapped.binding)
	}
}

func TestStoreCoordinatorWaitsForPersistedRemoteFenceBeforeTerminalDecommission(t *testing.T) {
	binding := coordinatorTestBinding()
	binding.State = store.MemoryBackendBindingDraining
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleDecommissioning)
	authorizeBindingReconcile(t, coordinator, wrapped.GovernedMemoryStore, &snapshot)
	pending, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if pending.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioning ||
		pending.RoutingEpoch != binding.RoutingEpoch+1 || wrapped.transitioned {
		t.Fatalf("pending result = %#v binding=%#v", pending, wrapped.binding)
	}
	snapshot.RemoteFenceAcknowledged = true
	snapshot.AcknowledgedRoutingEpoch = pending.RoutingEpoch
	terminal, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned ||
		wrapped.binding.State != store.MemoryBackendBindingDecommissioned || wrapped.binding.RoutingEpoch != pending.RoutingEpoch ||
		!wrapped.resolved {
		t.Fatalf("terminal result = %#v binding=%#v", terminal, wrapped.binding)
	}
}

func TestStoreCoordinatorRejectsActiveForUnsafeBindingStates(t *testing.T) {
	for _, state := range []store.MemoryBackendBindingState{
		store.MemoryBackendBindingDecommissioned,
		store.MemoryBackendBindingRemoved,
	} {
		t.Run(string(state), func(t *testing.T) {
			binding := coordinatorTestBinding()
			binding.State = state
			coordinator := &StoreCoordinator{Store: &coordinatorBindingStore{
				GovernedMemoryStore: newCoordinatorStore(t), binding: binding,
			}, ActivationEnabled: true}
			validation := controller.MemoryBackendValidationSnapshot{
				Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
				BackendGeneration: binding.BackendGeneration, ClusterID: binding.ClusterID, TenantID: binding.TenantID,
				RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive, SpecDigest: binding.SpecDigest,
				EndpointDigest: binding.EndpointDigest, SecretUID: binding.SecretUID,
				SecretResourceVersion: binding.SecretResourceVersion, StoreName: binding.StoreName,
				StoreUUID: binding.StoreUUID, Protocol: binding.Protocol,
			}
			authorizeValidationLifecycle(t, coordinator.Store, &validation)
			if _, err := coordinator.PrepareMemoryBackendValidation(context.Background(), validation); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("PrepareMemoryBackendValidation() error = %v, want ErrConflict", err)
			}
			if _, err := coordinator.ReconcileMemoryBackendBinding(
				context.Background(), coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleActive),
			); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("ReconcileMemoryBackendBinding() error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestStoreCoordinatorAllowsNeverActivatedStagedDeletionOverLegacyBinding(t *testing.T) {
	binding := coordinatorTestBinding()
	binding.Mode = store.MemoryBackendModeLegacy
	binding.State = store.MemoryBackendBindingLegacy
	binding.BackendUID = "previous-backend"
	coordinator := &StoreCoordinator{Store: &coordinatorBindingStore{
		GovernedMemoryStore: newCoordinatorStore(t), binding: binding,
	}}
	result, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), controller.MemoryBackendDeletionSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: "new-staged-backend",
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleStaged,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SafeToRemove || result.Reason != "NeverActivatedReplacement" {
		t.Fatalf("deletion result = %#v", result)
	}
}

func TestStoreCoordinatorFencesClaimedCandidateBeforeDeletingOverLegacyBinding(t *testing.T) {
	governed := newCoordinatorStore(t)
	binding := coordinatorTestBinding()
	binding.Mode = store.MemoryBackendModeLegacy
	binding.State = store.MemoryBackendBindingLegacy
	binding.BackendUID = "previous-backend"
	if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: "test",
		Action: memoryValidationClaimedAuditAction, PreviousState: string(store.MemoryBackendBindingValidating),
		NewState: "claimed", AuthorityEpoch: 2, RoutingEpoch: 4,
		RequestID: "new-backend", CreatedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &StoreCoordinator{Store: &coordinatorBindingStore{GovernedMemoryStore: governed, binding: binding}}
	result, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), controller.MemoryBackendDeletionSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: "new-backend",
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SafeToRemove || result.Reason != candidateFencePendingReason || result.AuthorityEpoch != 2 || result.RoutingEpoch != 4 {
		t.Fatalf("deletion result = %#v", result)
	}
}

func TestStoreCoordinatorDeletionFencesEveryClaimedValidationCandidate(t *testing.T) {
	governed := newCoordinatorStore(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	backendUID := "backend-a"
	for index, candidate := range []struct {
		authority int64
		routing   int64
		claimed   bool
	}{
		{authority: 1, routing: 1, claimed: true},
		{authority: 2, routing: 3, claimed: true},
		{authority: 3, routing: 4, claimed: false},
	} {
		requested := corev1alpha1.MemoryBackendLifecycleActive
		if !candidate.claimed {
			requested = corev1alpha1.MemoryBackendLifecycleStaged
		}
		if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
			Namespace: "team-a", NamespaceUID: "namespace-a", Actor: "test",
			Action: memoryValidationCandidateAuditAction, PreviousState: memoryValidationCandidateTrackingState,
			NewState: string(requested), AuthorityEpoch: candidate.authority, RoutingEpoch: candidate.routing,
			RequestID: backendUID, CreatedAt: now.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
		if candidate.claimed {
			if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
				Namespace: "team-a", NamespaceUID: "namespace-a", Actor: "test",
				Action: memoryValidationClaimedAuditAction, PreviousState: string(store.MemoryBackendBindingValidating),
				NewState: "claimed", AuthorityEpoch: candidate.authority, RoutingEpoch: candidate.routing,
				RequestID: backendUID, CreatedAt: now.Add(time.Duration(index)*time.Second + time.Millisecond),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	coordinator := &StoreCoordinator{Store: governed, Now: func() time.Time { return now.Add(time.Minute) }}
	snapshot := controller.MemoryBackendDeletionSnapshot{
		Namespace: "team-a", NamespaceUID: "namespace-a", BackendUID: backendUID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive,
	}
	first, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if first.SafeToRemove || first.Reason != candidateFencePendingReason || first.AuthorityEpoch != 1 || first.RoutingEpoch != 1 {
		t.Fatalf("first deletion barrier = %#v", first)
	}

	if err := coordinator.RetireMemoryBackendValidationCandidate(context.Background(), controller.MemoryBackendValidationCandidateRetirement{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
		AuthorityEpoch: first.AuthorityEpoch, RoutingEpoch: first.RoutingEpoch,
		CandidateDigest: first.CandidateDigest, RemoteFenceAcknowledged: true,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if second.SafeToRemove || second.Reason != candidateFencePendingReason || second.AuthorityEpoch != 2 || second.RoutingEpoch != 3 {
		t.Fatalf("second deletion barrier = %#v", second)
	}

	if err := coordinator.RetireMemoryBackendValidationCandidate(context.Background(), controller.MemoryBackendValidationCandidateRetirement{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
		AuthorityEpoch: second.AuthorityEpoch, RoutingEpoch: second.RoutingEpoch,
		CandidateDigest: second.CandidateDigest, RemoteFenceAcknowledged: true,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.SafeToRemove {
		t.Fatalf("completed deletion barrier = %#v", completed)
	}
}

func coordinatorTestBinding() store.MemoryBackendBinding {
	return store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, BackendUID: "backend-a", BackendGeneration: 2,
		AuthorityEpoch: 1, RoutingEpoch: 2, SpecDigest: "sha256:spec", EndpointDigest: "sha256:endpoint",
		ResolvedAddressDigest: "sha256:addresses", ServerCertificateDigest: "sha256:certificate",
		SecretName: "memory-auth", SecretKey: "token", SecretUID: "secret-a", SecretResourceVersion: "2",
		TenantID: "tenant-a", StoreName: "store-a",
		StoreUUID: "store-uuid-a", OwnershipClaim: "claim-a", CapabilityRevision: "cap-a",
		Protocol: "orka.oms.v0alpha1", State: store.MemoryBackendBindingAccepting,
		ValidationExpiresAt: time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC),
	}
}

func coordinatorBindingSnapshot(
	binding store.MemoryBackendBinding,
	lifecycle corev1alpha1.MemoryBackendLifecycleState,
) controller.MemoryBackendBindingSnapshot {
	return controller.MemoryBackendBindingSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, ClusterID: binding.ClusterID,
		BackendUID: binding.BackendUID, BackendGeneration: binding.BackendGeneration,
		RequestedLifecycle: lifecycle, SpecDigest: binding.SpecDigest, EndpointDigest: binding.EndpointDigest,
		ResolvedAddressDigest: binding.ResolvedAddressDigest, ServerCertificateDigest: binding.ServerCertificateDigest,
		SecretName: binding.SecretName, SecretKey: binding.SecretKey, SecretUID: binding.SecretUID,
		SecretResourceVersion: binding.SecretResourceVersion,
		TenantID:              binding.TenantID, StoreName: binding.StoreName, StoreUUID: binding.StoreUUID,
		OwnershipClaimIdentity: binding.OwnershipClaim,
		Capabilities:           corev1alpha1.MemoryBackendObservedCapabilities{Revision: binding.CapabilityRevision},
		Protocol:               binding.Protocol, AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		ValidationExpiresAt: binding.ValidationExpiresAt,
	}
}

func TestPrepareValidationPersistsCurrentGenerationWithOldRouteWhileDraining(t *testing.T) {
	binding := coordinatorTestBinding()
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding, unresolved: true}
	validation := controller.MemoryBackendValidationSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		BackendGeneration: binding.BackendGeneration + 1, ClusterID: binding.ClusterID, TenantID: binding.TenantID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive, SpecDigest: "sha256:new-spec",
		EndpointDigest: "sha256:new-endpoint", SecretName: "new-auth", SecretKey: "new-token",
		SecretUID: "new-secret", SecretResourceVersion: "9",
		StoreName: binding.StoreName, StoreUUID: binding.StoreUUID, Protocol: binding.Protocol,
	}
	authorizeValidationLifecycle(t, wrapped.GovernedMemoryStore, &validation)
	prepared, err := (&StoreCoordinator{Store: wrapped, ActivationEnabled: true}).PrepareMemoryBackendValidation(context.Background(), validation)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.DrainRequired || !wrapped.refreshed || wrapped.binding.BackendGeneration != validation.BackendGeneration {
		t.Fatalf("prepared=%#v binding=%#v", prepared, wrapped.binding)
	}
	if wrapped.binding.RoutingEpoch != binding.RoutingEpoch || wrapped.binding.SpecDigest != binding.SpecDigest ||
		wrapped.binding.EndpointDigest != binding.EndpointDigest || wrapped.binding.SecretName != binding.SecretName ||
		wrapped.binding.SecretKey != binding.SecretKey || wrapped.binding.SecretUID != binding.SecretUID ||
		wrapped.binding.SecretResourceVersion != binding.SecretResourceVersion {
		t.Fatalf("drain-compatible refresh replaced the validated old route: %#v", wrapped.binding)
	}
}

func TestPrepareIdentityRotationPersistsDrainingBeforeUnresolvedOperations(t *testing.T) {
	binding := coordinatorTestBinding()
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding, unresolved: true}
	validation := controller.MemoryBackendValidationSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		BackendGeneration: binding.BackendGeneration + 1, ClusterID: binding.ClusterID, TenantID: binding.TenantID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive, SpecDigest: "sha256:rotated-spec",
		EndpointDigest: "sha256:rotated-endpoint", SecretName: "rotated-auth", SecretKey: "rotated-token",
		SecretUID: "rotated-secret", SecretResourceVersion: "9",
		StoreName: binding.StoreName, StoreUUID: binding.StoreUUID, Protocol: binding.Protocol,
	}

	authorizeValidationLifecycle(t, wrapped.GovernedMemoryStore, &validation)
	prepared, err := (&StoreCoordinator{Store: wrapped, ActivationEnabled: true}).PrepareMemoryBackendValidation(context.Background(), validation)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.DrainRequired || prepared.RoutingEpoch != binding.RoutingEpoch {
		t.Fatalf("prepared = %#v, want old-route drain", prepared)
	}
	if wrapped.binding.State != store.MemoryBackendBindingDraining || wrapped.binding.RoutingEpoch != binding.RoutingEpoch ||
		wrapped.listedState != store.MemoryBackendBindingDraining {
		t.Fatalf("binding = %#v, listed state = %q", wrapped.binding, wrapped.listedState)
	}
	if len(wrapped.events) < 2 || wrapped.events[0] != "transition:draining" || wrapped.events[1] != "list:draining" {
		t.Fatalf("events = %v, want durable drain before unresolved-operation scan", wrapped.events)
	}
	if wrapped.binding.EndpointDigest != binding.EndpointDigest || wrapped.binding.SecretName != binding.SecretName ||
		wrapped.binding.SecretKey != binding.SecretKey || wrapped.binding.SecretResourceVersion != binding.SecretResourceVersion {
		t.Fatalf("old route metadata changed before drain completed: %#v", wrapped.binding)
	}
}

func TestStoreCoordinatorReadOnlyToActiveResumePersistsValidatedSnapshotBeforeReady(t *testing.T) {
	binding := coordinatorTestBinding()
	binding.State = store.MemoryBackendBindingDraining
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleActive)
	snapshot.BackendGeneration++
	snapshot.RoutingEpoch++
	snapshot.SpecDigest = "sha256:resumed-spec"
	snapshot.EndpointDigest = "sha256:resumed-endpoint"
	snapshot.SecretName = "resumed-auth"
	snapshot.SecretKey = "resumed-token"
	snapshot.SecretUID = "resumed-secret"
	snapshot.SecretResourceVersion = "7"
	snapshot.OwnershipClaimIdentity = "resumed-claim"
	snapshot.Capabilities.Revision = "resumed-capabilities"
	snapshot.ValidationExpiresAt = binding.ValidationExpiresAt.Add(2 * time.Hour)
	snapshot.RemoteFenceAcknowledged = true
	snapshot.AcknowledgedRoutingEpoch = snapshot.RoutingEpoch
	authorizeBindingReconcile(t, coordinator, wrapped.GovernedMemoryStore, &snapshot)

	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive {
		t.Fatalf("result = %#v", result)
	}
	if wrapped.binding.State != store.MemoryBackendBindingAccepting ||
		wrapped.binding.BackendGeneration != snapshot.BackendGeneration || wrapped.binding.RoutingEpoch != snapshot.RoutingEpoch ||
		wrapped.binding.SpecDigest != snapshot.SpecDigest || wrapped.binding.EndpointDigest != snapshot.EndpointDigest ||
		wrapped.binding.SecretName != snapshot.SecretName || wrapped.binding.SecretKey != snapshot.SecretKey ||
		wrapped.binding.SecretUID != snapshot.SecretUID || wrapped.binding.SecretResourceVersion != snapshot.SecretResourceVersion ||
		wrapped.binding.OwnershipClaim != snapshot.OwnershipClaimIdentity ||
		wrapped.binding.CapabilityRevision != snapshot.Capabilities.Revision ||
		!wrapped.binding.ValidationExpiresAt.Equal(snapshot.ValidationExpiresAt) {
		t.Fatalf("resumed binding did not persist validated snapshot: %#v", wrapped.binding)
	}
	if len(wrapped.events) != 1 || wrapped.events[0] != "refresh:accepting" || wrapped.transitioned {
		t.Fatalf("events = %v, want one atomic validated refresh and accepting transition", wrapped.events)
	}
}

func TestPrepareValidationCandidateDoesNotReuseStaleOrRetiredEpoch(t *testing.T) {
	binding := coordinatorTestBinding()
	governed := newCoordinatorStore(t)
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: governed, binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	base := controller.MemoryBackendValidationSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		BackendGeneration: binding.BackendGeneration + 1, ClusterID: binding.ClusterID, TenantID: binding.TenantID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive, SpecDigest: "sha256:spec-a",
		EndpointDigest: "sha256:endpoint-a", SecretName: binding.SecretName, SecretKey: binding.SecretKey,
		SecretUID: binding.SecretUID, SecretResourceVersion: "3", StoreName: binding.StoreName,
		StoreUUID: binding.StoreUUID, Protocol: binding.Protocol,
	}
	authorizeValidationLifecycle(t, governed, &base)
	a, err := coordinator.PrepareMemoryBackendValidation(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	b := base
	b.SpecDigest = "sha256:spec-b"
	b.EndpointDigest = "sha256:endpoint-b"
	b.SecretResourceVersion = "4"
	authorizeValidationLifecycle(t, governed, &b)
	second, err := coordinator.PrepareMemoryBackendValidation(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	secondReplay, err := coordinator.PrepareMemoryBackendValidation(context.Background(), b)
	if err != nil || secondReplay != second {
		t.Fatalf("current candidate replay=%#v want=%#v err=%v", secondReplay, second, err)
	}
	aAgain, err := coordinator.PrepareMemoryBackendValidation(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if a.RoutingEpoch >= second.RoutingEpoch || second.RoutingEpoch >= aAgain.RoutingEpoch {
		t.Fatalf("candidate epochs A=%#v B=%#v A2=%#v", a, second, aAgain)
	}
	if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: "test",
		Action: memoryValidationRemovedAuditAction, Reason: "retired", PreviousState: "claimed",
		NewState: string(store.MemoryBackendBindingRemoved), AuthorityEpoch: aAgain.AuthorityEpoch,
		RoutingEpoch: aAgain.RoutingEpoch, RequestID: binding.BackendUID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	aAfterRetirement, err := coordinator.PrepareMemoryBackendValidation(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if aAfterRetirement.RoutingEpoch <= aAgain.RoutingEpoch {
		t.Fatalf("retired candidate was reused: before=%#v after=%#v", aAgain, aAfterRetirement)
	}
}

func TestStoreCoordinatorDeletionFencesClaimAttemptCrashWindow(t *testing.T) {
	for _, lifecycle := range []corev1alpha1.MemoryBackendLifecycleState{
		corev1alpha1.MemoryBackendLifecycleActive,
	} {
		t.Run(string(lifecycle), func(t *testing.T) {
			governed := newCoordinatorStore(t)
			now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			coordinator := &StoreCoordinator{Store: governed, ActivationEnabled: true, Now: func() time.Time { return now }}
			validation := controller.MemoryBackendValidationSnapshot{
				Namespace: "team-a", NamespaceUID: "namespace-a", BackendUID: "backend-a", BackendGeneration: 1,
				ClusterID: "cluster-a", TenantID: "tenant-a", RequestedLifecycle: lifecycle,
				SpecDigest: "sha256:spec", EndpointDigest: "sha256:endpoint", SecretName: "memory-auth", SecretKey: "token",
				SecretUID: "secret-a", SecretResourceVersion: "1", StoreName: "store-a",
				StoreUUID: "store-uuid-a", Protocol: "orka.oms.v0alpha1",
			}
			authorizeValidationLifecycle(t, governed, &validation)
			prepared, err := coordinator.PrepareMemoryBackendValidation(context.Background(), validation)
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.RecordMemoryBackendOwnershipClaimAttempt(context.Background(), controller.MemoryBackendOwnershipClaimAttemptSnapshot{
				Namespace: validation.Namespace, NamespaceUID: validation.NamespaceUID, BackendUID: validation.BackendUID,
				RequestedLifecycle: lifecycle, AuthorityEpoch: prepared.AuthorityEpoch, RoutingEpoch: prepared.RoutingEpoch,
				CandidateDigest: prepared.CandidateDigest,
			}); err != nil {
				t.Fatal(err)
			}

			// Simulate a process crash after the remote claim request may have been sent,
			// but before ReconcileMemoryBackendBinding could record claimed success.
			deletion := controller.MemoryBackendDeletionSnapshot{
				Namespace: validation.Namespace, NamespaceUID: validation.NamespaceUID, BackendUID: validation.BackendUID,
			}
			pending, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), deletion)
			if err != nil {
				t.Fatal(err)
			}
			if pending.SafeToRemove || pending.Reason != candidateFencePendingReason ||
				pending.AuthorityEpoch != prepared.AuthorityEpoch || pending.RoutingEpoch != prepared.RoutingEpoch {
				t.Fatalf("deletion barrier = %#v", pending)
			}
		})
	}
}

func TestFinalizeDeletionBlocksOutstandingCandidateForSameTerminalBackend(t *testing.T) {
	governed := newCoordinatorStore(t)
	now := time.Date(2026, 7, 30, 17, 0, 0, 0, time.UTC)
	binding := coordinatorTestBinding()
	binding.State = store.MemoryBackendBindingDecommissioned
	if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: "test",
		Action: memoryValidationCandidateAuditAction, PreviousState: memoryValidationCandidateTrackingState,
		NewState: string(corev1alpha1.MemoryBackendLifecycleActive), AuthorityEpoch: 2, RoutingEpoch: 4,
		RequestID: binding.BackendUID, RequestDigest: "sha256:candidate", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := governed.AppendMemoryAudit(context.Background(), store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: "test",
		Action: memoryValidationClaimedAuditAction, PreviousState: string(store.MemoryBackendBindingValidating),
		NewState: "claimed", AuthorityEpoch: 2, RoutingEpoch: 4,
		RequestID: binding.BackendUID, RequestDigest: "sha256:candidate", CreatedAt: now.Add(time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := &StoreCoordinator{Store: &coordinatorBindingStore{GovernedMemoryStore: governed, binding: binding}}
	result, err := coordinator.FinalizeMemoryBackendDeletion(context.Background(), controller.MemoryBackendDeletionSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleDecommissioning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SafeToRemove || result.Reason != candidateFencePendingReason || result.CandidateDigest != "sha256:candidate" {
		t.Fatalf("deletion result = %#v, want outstanding candidate fence", result)
	}
}

func TestReconcileRejectsBindingSnapshotThatDoesNotMatchCandidateDigest(t *testing.T) {
	binding := coordinatorTestBinding()
	governed := newCoordinatorStore(t)
	coordinator := &StoreCoordinator{Store: &coordinatorBindingStore{GovernedMemoryStore: governed, binding: binding}, ActivationEnabled: true}
	snapshot := coordinatorBindingSnapshot(binding, corev1alpha1.MemoryBackendLifecycleActive)
	authorizeBindingReconcile(t, coordinator, governed, &snapshot)
	snapshot.CandidateDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("candidate mismatch error = %v, want ErrConflict", err)
	}
}

func TestStoreCoordinatorResumesRecoveringAuthorityWithAcknowledgedFence(t *testing.T) {
	binding := coordinatorTestBinding()
	binding.State = store.MemoryBackendBindingRecovering
	wrapped := &coordinatorBindingStore{GovernedMemoryStore: newCoordinatorStore(t), binding: binding}
	coordinator := &StoreCoordinator{Store: wrapped, ActivationEnabled: true}
	validation := controller.MemoryBackendValidationSnapshot{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		BackendGeneration: binding.BackendGeneration + 1, ClusterID: binding.ClusterID, TenantID: binding.TenantID,
		RequestedLifecycle: corev1alpha1.MemoryBackendLifecycleActive, SpecDigest: binding.SpecDigest,
		EndpointDigest: binding.EndpointDigest, ResolvedAddressDigest: binding.ResolvedAddressDigest,
		ServerCertificateDigest: binding.ServerCertificateDigest, SecretName: binding.SecretName, SecretKey: binding.SecretKey,
		SecretUID: binding.SecretUID, SecretResourceVersion: binding.SecretResourceVersion,
		StoreName: binding.StoreName, StoreUUID: binding.StoreUUID, Protocol: binding.Protocol,
	}
	authorizeValidationLifecycle(t, wrapped.GovernedMemoryStore, &validation)
	prepared, err := coordinator.PrepareMemoryBackendValidation(context.Background(), validation)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.RemoteFenceRequired || prepared.RoutingEpoch != binding.RoutingEpoch+1 {
		t.Fatalf("prepared = %#v", prepared)
	}
	claimPreparedCandidate(t, coordinator, validation, prepared)
	snapshot := coordinatorSnapshot(validation, prepared, time.Now().UTC())
	result, err := coordinator.ReconcileMemoryBackendBinding(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ready || result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive ||
		wrapped.binding.State != store.MemoryBackendBindingAccepting || wrapped.binding.RoutingEpoch != prepared.RoutingEpoch {
		t.Fatalf("result=%#v binding=%#v", result, wrapped.binding)
	}
}
