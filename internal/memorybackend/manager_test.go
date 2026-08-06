package memorybackend

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

func TestForceOrphanRetryTreatsRemovedAsTerminal(t *testing.T) {
	governed := newCoordinatorStore(t)
	now := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
	if err := governed.UpsertControllerFeatureHeartbeat(context.Background(), store.ControllerFeatureHeartbeat{
		InstanceID: "replica", Role: "serving_dispatching", FeatureEpoch: FoundationFeatureEpoch,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := governed.UpsertControllerFeatureHeartbeat(context.Background(), store.ControllerFeatureHeartbeat{
		InstanceID: "replica", Role: "serving_dispatching", FeatureEpoch: ActivationFeatureEpoch,
		LastHeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	binding := store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "backend-a", BackendGeneration: 1, AuthorityEpoch: 1, RoutingEpoch: 1,
		SpecDigest: "sha256:spec", EndpointDigest: "sha256:endpoint", ResolvedAddressDigest: "sha256:addresses", ServerCertificateDigest: "sha256:certificate", SecretName: "memory-auth", SecretKey: "token",
		SecretUID: "secret-a", SecretResourceVersion: "1",
		TenantID: "tenant-a", StoreName: "store-a", StoreUUID: "store-a-uuid", OwnershipClaim: "claim-a",
		CapabilityRevision: "cap-a", Protocol: "orka.oms.v0alpha1", State: store.MemoryBackendBindingAccepting,
		ActivationEpoch: 1, MinimumFeatureEpoch: ActivationFeatureEpoch, ValidationExpiresAt: now.Add(time.Hour),
	}
	if _, err := governed.RecordMemoryActivationRecoveryReceipt(context.Background(), store.MemoryActivationRecoveryReceipt{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		RouteDigest: binding.RecoveryRouteIdentity().Digest(), StoreUUID: binding.StoreUUID,
		ManifestDigest: protocol.ContentDigest("manager test recovery manifest"),
		Actor:          "test", Reason: "test recovery prerequisite", VerifiedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	activated, err := governed.ActivateMemoryBackend(context.Background(), store.MemoryBackendActivation{
		Binding: binding, RequiredFeatureEpoch: ActivationFeatureEpoch, Actor: "test", Reason: "activate", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding = activated.Binding
	for _, state := range []store.MemoryBackendBindingState{
		store.MemoryBackendBindingDraining,
		store.MemoryBackendBindingDecommissioned,
		store.MemoryBackendBindingRemoved,
	} {
		bindingPtr, transitionErr := governed.TransitionMemoryBackendBinding(context.Background(), store.MemoryBackendTransition{
			NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
			ExpectedState: binding.State, State: state,
			ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
			Actor: "test", Reason: "advance", Now: now,
		})
		if transitionErr != nil {
			t.Fatal(transitionErr)
		}
		binding = *bindingPtr
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: binding.Namespace, UID: types.UID(binding.NamespaceUID)}}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: binding.Namespace, UID: types.UID(binding.BackendUID),
	}, Spec: corev1alpha1.MemoryBackendSpec{LifecycleState: corev1alpha1.MemoryBackendLifecycleDecommissioning}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, backend).Build()
	manager := &Manager{Client: client, Reader: client, Store: governed, Now: func() time.Time { return now.Add(time.Minute) }}
	if _, err := manager.ForceOrphan(context.Background(), binding.Namespace, "operator", "retry cleanup", "request-a", false); err != nil {
		t.Fatalf("ForceOrphan() retry error = %v", err)
	}
	current, err := governed.GetMemoryBackendBinding(context.Background(), binding.NamespaceUID)
	if err != nil || current.State != store.MemoryBackendBindingRemoved {
		t.Fatalf("binding = %#v, err=%v", current, err)
	}
}

type managerBindingStore struct {
	store.GovernedMemoryStore
	binding store.MemoryBackendBinding
}

func (s *managerBindingStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	copy := s.binding
	return &copy, nil
}

func TestSetLifecycleRejectsReactivationOfTerminalBinding(t *testing.T) {
	for _, state := range []store.MemoryBackendBindingState{
		store.MemoryBackendBindingRecovering,
		store.MemoryBackendBindingDecommissioned,
		store.MemoryBackendBindingRemoved,
	} {
		for _, dryRun := range []bool{false, true} {
			t.Run(string(state)+"/dry-run="+map[bool]string{false: "false", true: "true"}[dryRun], func(t *testing.T) {
				scheme := runtime.NewScheme()
				if err := corev1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				if err := corev1alpha1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
				backend := &corev1alpha1.MemoryBackend{
					ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name},
					Spec:       corev1alpha1.MemoryBackendSpec{LifecycleState: corev1alpha1.MemoryBackendLifecycleDecommissioning},
				}
				client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, backend).Build()
				manager := &Manager{
					Client: client,
					Reader: client,
					Store: &managerBindingStore{binding: store.MemoryBackendBinding{
						Namespace: namespace.Name, NamespaceUID: string(namespace.UID), State: state,
					}},
				}

				_, err := manager.SetLifecycle(
					context.Background(), namespace.Name, corev1alpha1.MemoryBackendLifecycleActive,
					"operator", "reactivate", "request-a", dryRun,
				)
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("SetLifecycle() error = %v, want ErrConflict", err)
				}
				current := &corev1alpha1.MemoryBackend{}
				if err := client.Get(context.Background(), clientObjectKey(namespace.Name), current); err != nil {
					t.Fatal(err)
				}
				if current.Spec.LifecycleState != corev1alpha1.MemoryBackendLifecycleDecommissioning {
					t.Fatalf("lifecycle = %q, want terminal request unchanged", current.Spec.LifecycleState)
				}
			})
		}
	}
}

func TestSetLifecycleRejectsDisabledWithoutRemoteAuthority(t *testing.T) {
	for _, test := range []struct {
		name  string
		store store.GovernedMemoryStore
	}{
		{
			name: "never activated",
			store: &managerBindingErrorStore{
				GovernedMemoryStore: newCoordinatorStore(t),
				err:                 store.ErrNotFound,
			},
		},
		{
			name: "legacy authority",
			store: &managerBindingStore{
				GovernedMemoryStore: newCoordinatorStore(t),
				binding: store.MemoryBackendBinding{
					Namespace: "team-a", NamespaceUID: "namespace-a",
					Mode: store.MemoryBackendModeLegacy, State: store.MemoryBackendBindingLegacy,
				},
			},
		},
	} {
		for _, dryRun := range []bool{false, true} {
			t.Run(test.name+"/dry-run="+map[bool]string{false: "false", true: "true"}[dryRun], func(t *testing.T) {
				scheme := runtime.NewScheme()
				if err := corev1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				if err := corev1alpha1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
				backend := &corev1alpha1.MemoryBackend{
					ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name},
					Spec:       corev1alpha1.MemoryBackendSpec{LifecycleState: corev1alpha1.MemoryBackendLifecycleStaged},
				}
				kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, backend).Build()
				manager := &Manager{Client: kubeClient, Reader: kubeClient, Store: test.store}

				_, err := manager.SetLifecycle(
					context.Background(), namespace.Name, corev1alpha1.MemoryBackendLifecycleDisabled,
					"operator", "disable", "request-a", dryRun,
				)
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("SetLifecycle() error = %v, want ErrConflict", err)
				}
				current := &corev1alpha1.MemoryBackend{}
				if err := kubeClient.Get(context.Background(), clientObjectKey(namespace.Name), current); err != nil {
					t.Fatal(err)
				}
				if current.Spec.LifecycleState != corev1alpha1.MemoryBackendLifecycleStaged {
					t.Fatalf("lifecycle = %q, want Staged", current.Spec.LifecycleState)
				}
			})
		}
	}
}

func TestRecordIntentPropagatesBindingReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()
	wantErr := errors.New("binding read failed")
	manager := &Manager{
		Client: client,
		Reader: client,
		Store: &managerBindingErrorStore{
			GovernedMemoryStore: newCoordinatorStore(t),
			err:                 wantErr,
		},
	}
	if err := manager.RecordIntent(context.Background(), namespace.Name, "operator", "backend.test", "test", "request-a"); !errors.Is(err, wantErr) {
		t.Fatalf("RecordIntent() error = %v, want %v", err, wantErr)
	}
}

func TestTerminalActionsRejectBackendUIDMismatch(t *testing.T) {
	for _, action := range []struct {
		name string
		run  func(*Manager) error
	}{
		{name: "force-orphan", run: func(manager *Manager) error {
			_, err := manager.ForceOrphan(context.Background(), "team-a", "operator", "test", "request-a", true)
			return err
		}},
		{name: "restore-legacy", run: func(manager *Manager) error {
			_, err := manager.RestoreLegacy(context.Background(), "team-a", "operator", "test", "request-a", true)
			return err
		}},
	} {
		t.Run(action.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
			backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
				Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name, UID: types.UID("backend-current"),
			}}
			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, backend).Build()
			manager := &Manager{
				Client: client,
				Reader: client,
				Store: &managerBindingStore{
					GovernedMemoryStore: newCoordinatorStore(t),
					binding: store.MemoryBackendBinding{
						Namespace: namespace.Name, NamespaceUID: string(namespace.UID), BackendUID: "backend-previous",
						Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingDecommissioned,
					},
				},
			}
			if err := action.run(manager); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("terminal action error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestRestoreLegacyRetriesDeletionAfterLocalCommit(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name, UID: types.UID("backend-a"),
	}}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, backend).Build()
	deleteClient := &managerFailingDeleteClient{Client: baseClient, failures: 1}
	restoreStore := &managerRestoreStore{
		GovernedMemoryStore: newCoordinatorStore(t),
		binding: store.MemoryBackendBinding{
			Namespace: namespace.Name, NamespaceUID: string(namespace.UID), BackendUID: string(backend.UID),
			Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingDecommissioned,
			AuthorityEpoch: 1, RoutingEpoch: 2,
		},
	}
	manager := &Manager{Client: deleteClient, Reader: baseClient, Store: restoreStore}

	if _, err := manager.RestoreLegacy(context.Background(), namespace.Name, "operator", "restore", "request-a", false); err == nil {
		t.Fatal("first RestoreLegacy() error = nil, want delete failure")
	}
	if restoreStore.restoreCalls != 1 || restoreStore.binding.Mode != store.MemoryBackendModeLegacy ||
		restoreStore.binding.State != store.MemoryBackendBindingLegacy {
		t.Fatalf("first restore state = calls:%d binding:%#v", restoreStore.restoreCalls, restoreStore.binding)
	}

	if _, err := manager.RestoreLegacy(context.Background(), namespace.Name, "operator", "restore retry", "request-b", false); err != nil {
		t.Fatalf("second RestoreLegacy() error = %v", err)
	}
	if restoreStore.restoreCalls != 1 || restoreStore.previewCalls != 1 {
		t.Fatalf("restore replayed local commit: preview=%d restore=%d", restoreStore.previewCalls, restoreStore.restoreCalls)
	}
	current := &corev1alpha1.MemoryBackend{}
	if err := baseClient.Get(context.Background(), clientObjectKey(namespace.Name), current); !apierrors.IsNotFound(err) {
		t.Fatalf("backend Get() error = %v, want NotFound", err)
	}
}

func TestManagerDeleteUsesObjectPreconditionsAndRejectsRecreatedDefault(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
	staleBackend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name,
		UID: types.UID("backend-old"), ResourceVersion: "17",
	}}
	recreatedBackend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name,
		UID: types.UID("backend-new"), ResourceVersion: "23",
	}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, staleBackend).Build()
	writerBase := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace.DeepCopy(), recreatedBackend).Build()
	writer := &managerPreconditionClient{Client: writerBase}
	manager := &Manager{Client: writer, Reader: reader, Store: newCoordinatorStore(t)}

	err := manager.RequestDelete(context.Background(), namespace.Name, "operator", "delete stale object", "request-a")
	if !apierrors.IsConflict(err) {
		t.Fatalf("RequestDelete() error = %v, want Conflict", err)
	}
	if writer.preconditions == nil || writer.preconditions.UID == nil || *writer.preconditions.UID != staleBackend.UID {
		t.Fatalf("delete UID precondition = %#v, want %q", writer.preconditions, staleBackend.UID)
	}
	if writer.preconditions.ResourceVersion == nil || *writer.preconditions.ResourceVersion != staleBackend.ResourceVersion {
		t.Fatalf("delete resourceVersion precondition = %#v, want %q", writer.preconditions, staleBackend.ResourceVersion)
	}
	current := &corev1alpha1.MemoryBackend{}
	if err := writerBase.Get(context.Background(), clientObjectKey(namespace.Name), current); err != nil {
		t.Fatalf("recreated backend was deleted: %v", err)
	}
	if current.UID != recreatedBackend.UID {
		t.Fatalf("current backend UID = %q, want %q", current.UID, recreatedBackend.UID)
	}
}

func TestRequestDeletePropagatesBindingReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name, UID: types.UID("backend-a"),
	}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, backend).Build()
	wantErr := errors.New("binding read failed")
	manager := &Manager{
		Client: kubeClient,
		Reader: kubeClient,
		Store: &managerBindingErrorStore{
			GovernedMemoryStore: newCoordinatorStore(t),
			err:                 wantErr,
		},
	}
	if err := manager.RequestDelete(context.Background(), namespace.Name, "operator", "delete", "request-a"); !errors.Is(err, wantErr) {
		t.Fatalf("RequestDelete() error = %v, want %v", err, wantErr)
	}
	current := &corev1alpha1.MemoryBackend{}
	if err := kubeClient.Get(context.Background(), clientObjectKey(namespace.Name), current); err != nil {
		t.Fatalf("backend was deleted after binding read failure: %v", err)
	}
}

type managerBindingErrorStore struct {
	store.GovernedMemoryStore
	err error
}

func (s *managerBindingErrorStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	return nil, s.err
}

type managerRestoreStore struct {
	store.GovernedMemoryStore
	binding      store.MemoryBackendBinding
	previewCalls int
	restoreCalls int
}

func (s *managerRestoreStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	copy := s.binding
	return &copy, nil
}

func (s *managerRestoreStore) PreviewLegacyMemoryRestore(context.Context, string, string) (*store.LegacyMemoryRestorePreview, error) {
	s.previewCalls++
	return &store.LegacyMemoryRestorePreview{Namespace: s.binding.Namespace, NamespaceUID: s.binding.NamespaceUID, Restorable: true}, nil
}

func (s *managerRestoreStore) RestoreLegacyMemories(_ context.Context, _ store.LegacyMemoryRestore) (*store.LegacyMemoryRestoreResult, error) {
	s.restoreCalls++
	s.binding.Mode = store.MemoryBackendModeLegacy
	s.binding.State = store.MemoryBackendBindingLegacy
	s.binding.RoutingEpoch++
	return &store.LegacyMemoryRestoreResult{Binding: s.binding, RestoredMemories: 2}, nil
}

type managerFailingDeleteClient struct {
	crclient.Client
	failures    int
	deleteCalls int
}

func (c *managerFailingDeleteClient) Delete(ctx context.Context, object crclient.Object, options ...crclient.DeleteOption) error {
	c.deleteCalls++
	if c.deleteCalls <= c.failures {
		return errors.New("injected delete failure")
	}
	return c.Client.Delete(ctx, object, options...)
}

type managerPreconditionClient struct {
	crclient.Client
	preconditions *metav1.Preconditions
}

func (c *managerPreconditionClient) Delete(ctx context.Context, object crclient.Object, options ...crclient.DeleteOption) error {
	deleteOptions := (&crclient.DeleteOptions{}).ApplyOptions(options)
	c.preconditions = deleteOptions.Preconditions
	if c.preconditions == nil || c.preconditions.UID == nil {
		return errors.New("delete UID precondition is missing")
	}
	current := &corev1alpha1.MemoryBackend{}
	if err := c.Get(ctx, crclient.ObjectKeyFromObject(object), current); err != nil {
		return err
	}
	if current.UID != *c.preconditions.UID ||
		(c.preconditions.ResourceVersion != nil && current.ResourceVersion != *c.preconditions.ResourceVersion) {
		return apierrors.NewConflict(
			corev1alpha1.GroupVersion.WithResource("memorybackends").GroupResource(),
			object.GetName(),
			errors.New("delete preconditions do not match the current object"),
		)
	}
	return c.Client.Delete(ctx, object, options...)
}

func clientObjectKey(namespace string) crclient.ObjectKey {
	return crclient.ObjectKey{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}
}

func TestRecordIntentRequiresReaderOrClient(t *testing.T) {
	manager := &Manager{Store: newCoordinatorStore(t)}
	if err := manager.RecordIntent(context.Background(), "team-a", "operator", "backend.test", "test", "request-a"); err == nil {
		t.Fatal("RecordIntent() accepted a manager without an effective reader")
	}
}

func TestValidateManagedLifecycleSourceMatchesCoordinatorMatrix(t *testing.T) {
	remote := func(state store.MemoryBackendBindingState) *store.MemoryBackendBinding {
		return &store.MemoryBackendBinding{Mode: store.MemoryBackendModeRemote, State: state}
	}
	legacy := &store.MemoryBackendBinding{Mode: store.MemoryBackendModeLegacy, State: store.MemoryBackendBindingLegacy}
	for _, test := range []struct {
		name      string
		binding   *store.MemoryBackendBinding
		lifecycle corev1alpha1.MemoryBackendLifecycleState
		wantErr   bool
	}{
		{name: "new active", lifecycle: corev1alpha1.MemoryBackendLifecycleActive},
		{name: "legacy active", binding: legacy, lifecycle: corev1alpha1.MemoryBackendLifecycleActive},
		{name: "draining active", binding: remote(store.MemoryBackendBindingDraining), lifecycle: corev1alpha1.MemoryBackendLifecycleActive},
		{name: "recovering active", binding: remote(store.MemoryBackendBindingRecovering), lifecycle: corev1alpha1.MemoryBackendLifecycleActive},
		{name: "accepting read only", binding: remote(store.MemoryBackendBindingAccepting), lifecycle: corev1alpha1.MemoryBackendLifecycleReadOnly},
		{name: "recovering read only rejected", binding: remote(store.MemoryBackendBindingRecovering), lifecycle: corev1alpha1.MemoryBackendLifecycleReadOnly, wantErr: true},
		{name: "draining disabled", binding: remote(store.MemoryBackendBindingDraining), lifecycle: corev1alpha1.MemoryBackendLifecycleDisabled},
		{name: "removed disabled rejected", binding: remote(store.MemoryBackendBindingRemoved), lifecycle: corev1alpha1.MemoryBackendLifecycleDisabled, wantErr: true},
		{name: "decommissioned decommission", binding: remote(store.MemoryBackendBindingDecommissioned), lifecycle: corev1alpha1.MemoryBackendLifecycleDecommissioning},
		{name: "removed decommission rejected", binding: remote(store.MemoryBackendBindingRemoved), lifecycle: corev1alpha1.MemoryBackendLifecycleDecommissioning, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateManagedLifecycleSource(test.binding, test.lifecycle)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateManagedLifecycleSource() error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}
