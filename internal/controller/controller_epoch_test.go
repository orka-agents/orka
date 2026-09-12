package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestControllerEpochManagerCreatesAndAdvances(t *testing.T) {
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")

	first := NewControllerEpochManager(controlStore, "controller-a")
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- first.Start(ctx1) }()
	fence1, err := fenceWithTimeout(first)
	if err != nil {
		t.Fatal(err)
	}
	if fence1.Epoch != 1 || fence1.HolderID != "controller-a" {
		t.Fatalf("first fence = %#v", fence1)
	}
	cancel1()
	if err := <-done1; err != nil {
		t.Fatal(err)
	}

	second := NewControllerEpochManager(controlStore, "controller-b")
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- second.Start(ctx2) }()
	fence2, err := fenceWithTimeout(second)
	if err != nil {
		t.Fatal(err)
	}
	if fence2.Epoch != 2 || fence2.HolderID != "controller-b" {
		t.Fatalf("second fence = %#v", fence2)
	}
	cancel2()
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerMirrorsAuthoritativeFenceBeforeReadiness(t *testing.T) {
	authorityDB, err := sqlite.NewDB(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authorityDB.Close() })
	mirrorDB, err := sqlite.NewDB(filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirrorDB.Close() })
	authority := sqlite.NewStore(authorityDB, "authority")
	mirror := sqlite.NewStore(mirrorDB, "mirror")
	manager := NewControllerEpochManager(authority, "controller-a").WithMirror(mirror)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	fence, err := fenceWithTimeout(manager)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	mirrored, err := mirror.GetControllerEpoch(context.Background(), fence.Name)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if mirrored.Epoch != fence.Epoch || mirrored.HolderID != fence.HolderID {
		cancel()
		t.Fatalf("mirrored epoch = %#v, want fence %#v", mirrored, fence)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerAdoptsCommittedIntentAfterConflict(t *testing.T) {
	authority := newRetainedIntentEpochStore(epochConflictCommitThenReportConflict)
	mirror := &recordingControllerEpochMirror{}
	manager := NewControllerEpochManager(authority, controllerEpochTestTakeoverHolder).WithMirror(mirror)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()

	fence, err := fenceWithTimeout(manager)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if fence.Epoch != 8 || fence.HolderID != controllerEpochTestTakeoverHolder {
		cancel()
		t.Fatalf("fence after ambiguous commit = %#v, want exact acquired epoch 8", fence)
	}
	if len(authority.changes) != 1 {
		cancel()
		t.Fatalf("CAS submissions after committed conflict = %d, want 1 (no epoch 9 intent)", len(authority.changes))
	}
	if len(mirror.synced) != 1 || !controllerEpochMatchesCAS(&mirror.synced[0], authority.changes[0]) {
		cancel()
		t.Fatalf("mirrored authority = %#v, submitted intent = %#v", mirror.synced, authority.changes[0])
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerRetriesExactIntentWhenPredecessorIsUnchanged(t *testing.T) {
	authority := newRetainedIntentEpochStore(epochConflictLeavesPredecessor)
	manager := NewControllerEpochManager(authority, controllerEpochTestTakeoverHolder)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()

	fence, err := fenceWithTimeout(manager)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if fence.Epoch != 8 || fence.HolderID != controllerEpochTestTakeoverHolder {
		cancel()
		t.Fatalf("fence after predecessor retry = %#v", fence)
	}
	if len(authority.changes) != 2 {
		cancel()
		t.Fatalf("CAS submissions = %d, want conflict plus one retry", len(authority.changes))
	}
	if authority.changes[0] != authority.changes[1] {
		cancel()
		t.Fatalf("retry changed CAS intent:\nfirst=%#v\nretry=%#v", authority.changes[0], authority.changes[1])
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerTakesOverOnceAfterCompetingAdvance(t *testing.T) {
	authority := newRetainedIntentEpochStore(epochConflictCommitsCompetitor)
	manager := NewControllerEpochManager(authority, controllerEpochTestTakeoverHolder)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()

	fence, err := fenceWithTimeout(manager)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if fence.Epoch != 9 || fence.HolderID != controllerEpochTestTakeoverHolder {
		cancel()
		t.Fatalf("fence after competing advance = %#v, want one takeover at epoch 9", fence)
	}
	if len(authority.changes) != 2 {
		cancel()
		t.Fatalf("CAS submissions = %d, want original epoch 8 intent and one epoch 9 takeover", len(authority.changes))
	}
	first, second := authority.changes[0], authority.changes[1]
	if first.ExpectedEpoch != 7 || first.NewEpoch != 8 || second.ExpectedEpoch != 8 || second.NewEpoch != 9 {
		cancel()
		t.Fatalf("CAS sequence = %#v, want 7->8 then 8->9", authority.changes)
	}
	if second.RequestDigest != controllerEpochDigest(second.Name, second.HolderID, second.ExpectedVersion, second.ExpectedEpoch, second.NewEpoch) {
		cancel()
		t.Fatalf("takeover digest %q does not bind the replanned epoch 9 intent", second.RequestDigest)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerFailsClosedOnIncomparableConflictState(t *testing.T) {
	authority := newRetainedIntentEpochStore(epochConflictCommitsSameHolderDifferentIntent)
	manager := NewControllerEpochManager(authority, controllerEpochTestTakeoverHolder)
	err := manager.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not the exact predecessor") {
		t.Fatalf("manager error = %v, want incomparable-authority failure", err)
	}
	if _, ready := manager.Current(); ready {
		t.Fatal("manager became ready after incomparable conflict state")
	}
	if len(authority.changes) != 1 {
		t.Fatalf("CAS submissions after incomparable state = %d, want no retry", len(authority.changes))
	}
}

func TestControllerEpochManagerFailsClosedWhenRestoredLeaseIsMissing(t *testing.T) {
	ctx := context.Background()
	authority, kubeClient := newKubernetesControllerEpochAuthority(t)
	restored := advanceKubernetesControllerEpoch(t, ctx, authority, 7)
	object, lease := kubernetesControllerEpochObjects(t, ctx, kubeClient)
	if object.Status.Epoch != restored.Epoch || object.Status.Version != restored.Version {
		t.Fatalf("restored controller epoch object = %#v, want epoch/version %d/%d", object.Status, restored.Epoch, restored.Version)
	}
	if err := kubeClient.Delete(ctx, lease); err != nil {
		t.Fatalf("delete authoritative Lease to simulate incomplete restore: %v", err)
	}

	manager := NewControllerEpochManager(authority, "controller-after-incomplete-restore")
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "authoritative Lease is missing") {
			t.Fatalf("manager startup error = %v, want missing-authority failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("manager startup did not fail closed for missing restored Lease")
	}
	if _, ready := manager.Current(); ready {
		t.Fatal("manager became ready without the restored authoritative Lease")
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.CurrentFence(readyCtx); err == nil {
		t.Fatal("CurrentFence succeeded without the restored authoritative Lease")
	}

	var leases coordinationv1.LeaseList
	if err := kubeClient.List(ctx, &leases, client.InNamespace(controllerEpochManagerTestNamespace)); err != nil {
		t.Fatalf("list controller epoch Leases after rejected startup: %v", err)
	}
	if len(leases.Items) != 0 {
		t.Fatalf("manager recreated %d Lease(s) after incomplete restore", len(leases.Items))
	}
	var preserved corev1alpha1.ControllerEpoch
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(object), &preserved); err != nil {
		t.Fatalf("get preserved controller epoch status: %v", err)
	}
	if preserved.Status.Epoch != 7 || preserved.Status.Version != 7 || preserved.Status.HolderID != restored.HolderID {
		t.Fatalf("manager changed restored status after rejected startup: %#v", preserved.Status)
	}
}

func TestControllerEpochManagerAdvancesRestoredAuthorityAndMirrorExactlyOnce(t *testing.T) {
	ctx := context.Background()
	authority, kubeClient := newKubernetesControllerEpochAuthority(t)
	restored := advanceKubernetesControllerEpoch(t, ctx, authority, 7)
	if restored.Epoch != 7 || restored.Version != 7 {
		t.Fatalf("restored authority = %#v", restored)
	}

	mirrorDB, err := sqlite.NewDB(filepath.Join(t.TempDir(), "restored-mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirrorDB.Close() })
	mirror := sqlite.NewStore(mirrorDB, "restored-mirror")
	manager := NewControllerEpochManager(authority, "controller-after-restore").WithMirror(mirror)
	managerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(managerCtx) }()

	fence, err := fenceWithTimeout(manager)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if fence.Epoch != 8 || fence.HolderID != "controller-after-restore" {
		cancel()
		t.Fatalf("restored startup fence = %#v, want exact epoch 8", fence)
	}
	authoritative, err := authority.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		cancel()
		t.Fatalf("read advanced Kubernetes authority: %v", err)
	}
	mirrored, err := mirror.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		cancel()
		t.Fatalf("read synchronized durable mirror: %v", err)
	}
	if authoritative.Epoch != 8 || authoritative.Version != 8 || mirrored.Epoch != 8 || mirrored.Version != 8 ||
		authoritative.HolderID != fence.HolderID || mirrored.HolderID != fence.HolderID ||
		authoritative.RequestDigest != mirrored.RequestDigest {
		cancel()
		t.Fatalf("advanced authority/mirror mismatch: authority=%#v mirror=%#v fence=%#v", authoritative, mirrored, fence)
	}
	object, _ := kubernetesControllerEpochObjects(t, ctx, kubeClient)
	if object.Status.Epoch != 8 || object.Status.Version != 8 || object.Status.HolderID != fence.HolderID ||
		object.Status.RequestDigest != authoritative.RequestDigest {
		cancel()
		t.Fatalf("advanced controller epoch status = %#v", object.Status)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerFailsClosedOnCorruptRestoredAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, context.Context, client.Client, *corev1alpha1.ControllerEpoch, *coordinationv1.Lease)
	}{
		{
			name: "blank ControllerEpoch status",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, object *corev1alpha1.ControllerEpoch, _ *coordinationv1.Lease) {
				t.Helper()
				object.Status = corev1alpha1.ControllerEpochStatus{}
				if err := kubeClient.Status().Update(ctx, object); err != nil {
					t.Fatalf("blank restored ControllerEpoch status: %v", err)
				}
			},
		},
		{
			name: "stale ControllerEpoch status",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, object *corev1alpha1.ControllerEpoch, lease *coordinationv1.Lease) {
				t.Helper()
				staleAt := metav1.NewTime(time.Date(2026, time.August, 10, 10, 0, 0, 0, time.UTC).Add(6 * time.Minute))
				staleHolder := "restored-controller-6"
				object.Status = corev1alpha1.ControllerEpochStatus{
					Epoch: 6, Version: 6, HolderID: staleHolder,
					RequestDigest: controllerEpochDigest(store.DefaultControllerEpochName, staleHolder, 5, 5, 6),
					AcquiredAt:    &staleAt, UpdatedAt: &staleAt,
					LeaseName: lease.Name, LeaseResourceVersion: lease.ResourceVersion,
				}
				if err := kubeClient.Status().Update(ctx, object); err != nil {
					t.Fatalf("restore stale ControllerEpoch status: %v", err)
				}
			},
		},
		{
			name: "missing Lease holder",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, _ *corev1alpha1.ControllerEpoch, lease *coordinationv1.Lease) {
				t.Helper()
				lease.Spec.HolderIdentity = nil
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("clear restored Lease holder: %v", err)
				}
			},
		},
		{
			name: "invalid Lease digest",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, _ *corev1alpha1.ControllerEpoch, lease *coordinationv1.Lease) {
				t.Helper()
				lease.Annotations["core.orka.ai/request-digest"] = "sha256:invalid"
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("corrupt restored Lease digest: %v", err)
				}
			},
		},
		{
			name: "missing Lease epoch annotation",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, _ *corev1alpha1.ControllerEpoch, lease *coordinationv1.Lease) {
				t.Helper()
				delete(lease.Annotations, "core.orka.ai/controller-epoch")
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("remove restored Lease epoch annotation: %v", err)
				}
			},
		},
		{
			name: "status holder mismatch",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, object *corev1alpha1.ControllerEpoch, _ *coordinationv1.Lease) {
				t.Helper()
				object.Status.HolderID = "mismatched-restored-holder"
				if err := kubeClient.Status().Update(ctx, object); err != nil {
					t.Fatalf("mismatch restored status holder: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			authority, kubeClient := newKubernetesControllerEpochAuthority(t)
			advanceKubernetesControllerEpoch(t, ctx, authority, 7)
			object, lease := kubernetesControllerEpochObjects(t, ctx, kubeClient)
			test.mutate(t, ctx, kubeClient, object, lease)

			manager := NewControllerEpochManager(authority, "controller-after-corrupt-restore")
			done := make(chan error, 1)
			go func() { done <- manager.Start(ctx) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("manager accepted corrupt restored epoch authority")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("manager did not fail closed for corrupt restored epoch authority")
			}
			if _, ready := manager.Current(); ready {
				t.Fatal("manager became ready with corrupt restored epoch authority")
			}
		})
	}
}

func TestControllerEpochManagerRequiresHolderAndStore(t *testing.T) {
	manager := NewControllerEpochManager(nil, "holder")
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("missing store unexpectedly accepted")
	}
	manager = NewControllerEpochManager(&conflictingEpochStore{}, "")
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("missing holder unexpectedly accepted")
	}
}

func TestControllerEpochDigestChangesAcrossTargets(t *testing.T) {
	first := controllerEpochDigest("epoch", "holder", 1, 1, 2)
	second := controllerEpochDigest("epoch", "holder", 2, 2, 3)
	if first == second {
		t.Fatal("epoch digests unexpectedly equal")
	}
	if err := store.ValidateCanonicalDigest("epoch digest", first); err != nil {
		t.Fatal(err)
	}
}

func fenceWithTimeout(manager *ControllerEpochManager) (store.ControllerEpochFence, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return manager.CurrentFence(ctx)
}

type conflictingEpochStore struct{}

func (*conflictingEpochStore) GetControllerEpoch(context.Context, string) (*store.ControllerEpoch, error) {
	return nil, store.ErrNotFound
}
func (*conflictingEpochStore) CompareAndSwapControllerEpoch(context.Context, store.ControllerEpochCAS) (*store.ControllerEpoch, error) {
	return nil, store.ErrConflict
}

var _ store.ControllerEpochStore = (*conflictingEpochStore)(nil)

type retainedIntentEpochConflict int

const (
	epochConflictCommitThenReportConflict retainedIntentEpochConflict = iota
	epochConflictLeavesPredecessor
	epochConflictCommitsCompetitor
	epochConflictCommitsSameHolderDifferentIntent
)

type retainedIntentEpochStore struct {
	current  *store.ControllerEpoch
	changes  []store.ControllerEpochCAS
	conflict retainedIntentEpochConflict
}

func newRetainedIntentEpochStore(conflict retainedIntentEpochConflict) *retainedIntentEpochStore {
	acquiredAt := time.Date(2026, time.August, 11, 12, 7, 0, 0, time.UTC)
	holderID := "controller-before-takeover"
	return &retainedIntentEpochStore{
		current: &store.ControllerEpoch{
			Name: store.DefaultControllerEpochName, Epoch: 7, Version: 7, HolderID: holderID,
			RequestDigest: controllerEpochDigest(store.DefaultControllerEpochName, holderID, 6, 6, 7),
			AcquiredAt:    acquiredAt, UpdatedAt: acquiredAt,
		},
		conflict: conflict,
	}
}

func (s *retainedIntentEpochStore) GetControllerEpoch(context.Context, string) (*store.ControllerEpoch, error) {
	if s.current == nil {
		return nil, store.ErrNotFound
	}
	copy := *s.current
	return &copy, nil
}

func (s *retainedIntentEpochStore) CompareAndSwapControllerEpoch(
	_ context.Context,
	change store.ControllerEpochCAS,
) (*store.ControllerEpoch, error) {
	s.changes = append(s.changes, change)
	if len(s.changes) == 1 {
		switch s.conflict {
		case epochConflictCommitThenReportConflict:
			s.current = controllerEpochFromTestCAS(change)
			return nil, store.ErrConflict
		case epochConflictLeavesPredecessor:
			copy := *s.current
			return &copy, store.ErrConflict
		case epochConflictCommitsCompetitor:
			competitor := change
			competitor.HolderID = "competing-controller"
			competitor.RequestDigest = controllerEpochDigest(
				competitor.Name,
				competitor.HolderID,
				competitor.ExpectedVersion,
				competitor.ExpectedEpoch,
				competitor.NewEpoch,
			)
			competitor.UpdatedAt = competitor.UpdatedAt.Add(time.Millisecond)
			s.current = controllerEpochFromTestCAS(competitor)
			copy := *s.current
			return &copy, store.ErrConflict
		case epochConflictCommitsSameHolderDifferentIntent:
			corrupt := change
			corrupt.RequestDigest = controllerEpochDigest(
				corrupt.Name,
				corrupt.HolderID,
				corrupt.ExpectedVersion+1,
				corrupt.ExpectedEpoch,
				corrupt.NewEpoch,
			)
			s.current = controllerEpochFromTestCAS(corrupt)
			return nil, store.ErrConflict
		}
	}
	if s.current == nil || s.current.Version != change.ExpectedVersion || s.current.Epoch != change.ExpectedEpoch {
		return nil, store.ErrConflict
	}
	s.current = controllerEpochFromTestCAS(change)
	copy := *s.current
	return &copy, nil
}

func controllerEpochFromTestCAS(change store.ControllerEpochCAS) *store.ControllerEpoch {
	return &store.ControllerEpoch{
		Name: change.Name, Epoch: change.NewEpoch, Version: change.ExpectedVersion + 1,
		HolderID: change.HolderID, RequestDigest: change.RequestDigest,
		AcquiredAt: change.UpdatedAt, UpdatedAt: change.UpdatedAt,
	}
}

type recordingControllerEpochMirror struct {
	synced []store.ControllerEpoch
}

func (m *recordingControllerEpochMirror) SyncControllerEpochMirror(_ context.Context, epoch store.ControllerEpoch) error {
	m.synced = append(m.synced, epoch)
	return nil
}

var (
	_ store.ControllerEpochStore  = (*retainedIntentEpochStore)(nil)
	_ store.ControllerEpochMirror = (*recordingControllerEpochMirror)(nil)
)

const (
	controllerEpochManagerTestNamespace = "orka-system"
	controllerEpochTestTakeoverHolder   = "controller-takeover"
)

var controllerEpochTestLeaseUIDSequence atomic.Int64

func withControllerEpochLeaseUIDs(t testing.TB, baseClient client.Client) client.WithWatch {
	t.Helper()
	withWatch, ok := baseClient.(client.WithWatch)
	if !ok {
		t.Fatal("controller epoch test client does not implement client.WithWatch")
	}
	return interceptor.NewClient(withWatch, interceptor.Funcs{
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if lease, ok := object.(*coordinationv1.Lease); ok && lease.UID == "" {
				lease.UID = types.UID(fmt.Sprintf("controller-test-lease-uid-%d", controllerEpochTestLeaseUIDSequence.Add(1)))
			}
			return delegate.Create(ctx, object, options...)
		},
	})
}

func newKubernetesControllerEpochAuthority(t *testing.T) (*kubestore.Store, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Orka scheme: %v", err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.ControllerEpoch{}).
		Build()
	kubeClient := withControllerEpochLeaseUIDs(t, baseClient)
	authority, err := kubestore.New(kubeClient, controllerEpochManagerTestNamespace)
	if err != nil {
		t.Fatalf("create Kubernetes controller epoch store: %v", err)
	}
	return authority, kubeClient
}

func advanceKubernetesControllerEpoch(
	t *testing.T,
	ctx context.Context,
	authority store.ControllerEpochStore,
	target int64,
) *store.ControllerEpoch {
	t.Helper()
	var current *store.ControllerEpoch
	for next := int64(1); next <= target; next++ {
		expectedVersion, expectedEpoch := int64(0), int64(0)
		if current != nil {
			expectedVersion, expectedEpoch = current.Version, current.Epoch
		}
		holder := fmt.Sprintf("restored-controller-%d", next)
		var err error
		current, err = authority.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
			Name: store.DefaultControllerEpochName, ExpectedVersion: expectedVersion, ExpectedEpoch: expectedEpoch, NewEpoch: next,
			HolderID: holder, RequestDigest: controllerEpochDigest(store.DefaultControllerEpochName, holder, expectedVersion, expectedEpoch, next),
			UpdatedAt: time.Date(2026, time.August, 10, 10, 0, 0, 0, time.UTC).Add(time.Duration(next) * time.Minute),
		})
		if err != nil {
			t.Fatalf("advance Kubernetes controller epoch to %d: %v", next, err)
		}
	}
	return current
}

func kubernetesControllerEpochObjects(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
) (*corev1alpha1.ControllerEpoch, *coordinationv1.Lease) {
	t.Helper()
	var objects corev1alpha1.ControllerEpochList
	if err := kubeClient.List(ctx, &objects, client.InNamespace(controllerEpochManagerTestNamespace)); err != nil {
		t.Fatalf("list controller epoch objects: %v", err)
	}
	if len(objects.Items) != 1 {
		t.Fatalf("controller epoch object count = %d, want 1", len(objects.Items))
	}
	var leases coordinationv1.LeaseList
	if err := kubeClient.List(ctx, &leases, client.InNamespace(controllerEpochManagerTestNamespace)); err != nil {
		t.Fatalf("list controller epoch Leases: %v", err)
	}
	if len(leases.Items) != 1 {
		t.Fatalf("controller epoch Lease count = %d, want 1", len(leases.Items))
	}
	return objects.Items[0].DeepCopy(), leases.Items[0].DeepCopy()
}
