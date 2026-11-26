package kube

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/storageclient"
	controlstore "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const protocolInstallation = "installation-protocol-faults"

type storeFault int

const (
	faultLose storeFault = iota + 1
	faultDelay
)

type faultyStoreHalf struct {
	SQLitePersistence

	mu      sync.Mutex
	faults  map[string][]storeFault
	calls   map[string]int
	delayed []func() error
}

func newFaultyStoreHalf(persistence SQLitePersistence) *faultyStoreHalf {
	return &faultyStoreHalf{SQLitePersistence: persistence, faults: map[string][]storeFault{}, calls: map[string]int{}}
}

func (f *faultyStoreHalf) inject(method string, faults ...storeFault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[method] = append(f.faults[method], faults...)
}

func (f *faultyStoreHalf) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *faultyStoreHalf) step(method string, call func() error) error {
	f.mu.Lock()
	f.calls[method]++
	var fault storeFault
	if queued := f.faults[method]; len(queued) > 0 {
		fault, f.faults[method] = queued[0], queued[1:]
	}
	if fault == faultDelay {
		f.delayed = append(f.delayed, call)
		f.mu.Unlock()
		return fmt.Errorf("%w: %s is still in flight", storagev1.ErrOutcomeUnknown, method)
	}
	f.mu.Unlock()
	if err := call(); err != nil {
		return err
	}
	if fault == faultLose {
		return fmt.Errorf("%w: %s response lost", storagev1.ErrOutcomeUnknown, method)
	}
	return nil
}

func (f *faultyStoreHalf) deliverDelayed() []error {
	f.mu.Lock()
	delayed := f.delayed
	f.delayed = nil
	f.mu.Unlock()
	errs := make([]error, 0, len(delayed))
	for _, call := range delayed {
		errs = append(errs, call())
	}
	return errs
}

func (f *faultyStoreHalf) EnqueueOutboxProjectionRecord(
	ctx context.Context, projection *controlstore.OutboxProjection, fence controlstore.ControllerEpochFence,
) (*controlstore.OutboxProjection, error) {
	var stored *controlstore.OutboxProjection
	err := f.step("EnqueueOutboxProjectionRecord", func() error {
		var err error
		stored, err = f.SQLitePersistence.EnqueueOutboxProjectionRecord(context.WithoutCancel(ctx), projection, fence)
		return err
	})
	return stored, err
}

func (f *faultyStoreHalf) ClaimOutboxProjectionRecords(
	ctx context.Context, request controlstore.ClaimOutboxProjectionsRequest,
) ([]controlstore.OutboxProjection, error) {
	var claimed []controlstore.OutboxProjection
	err := f.step("ClaimOutboxProjectionRecords", func() error {
		var err error
		claimed, err = f.SQLitePersistence.ClaimOutboxProjectionRecords(context.WithoutCancel(ctx), request)
		return err
	})
	return claimed, err
}

func (f *faultyStoreHalf) CompleteOutboxProjectionRecord(
	ctx context.Context, request controlstore.CompleteOutboxProjectionRequest,
) (*controlstore.OutboxProjection, error) {
	var completed *controlstore.OutboxProjection
	err := f.step("CompleteOutboxProjectionRecord", func() error {
		var err error
		completed, err = f.SQLitePersistence.CompleteOutboxProjectionRecord(context.WithoutCancel(ctx), request)
		return err
	})
	return completed, err
}

func (f *faultyStoreHalf) PrepareSessionCleanup(
	ctx context.Context, intent controlstore.SessionCleanupIntent,
) (*controlstore.SessionCleanupIntent, error) {
	var prepared *controlstore.SessionCleanupIntent
	err := f.step("PrepareSessionCleanup", func() error {
		var err error
		prepared, err = f.SQLitePersistence.PrepareSessionCleanup(context.WithoutCancel(ctx), intent)
		return err
	})
	return prepared, err
}

func (f *faultyStoreHalf) CompleteSessionCleanup(ctx context.Context, request controlstore.CompleteSessionCleanupRequest) error {
	return f.step("CompleteSessionCleanup", func() error {
		return f.SQLitePersistence.CompleteSessionCleanup(context.WithoutCancel(ctx), request)
	})
}

type fixedFence struct {
	datasetID string
	fence     storagev1.ControllerEpochFence
}

func (f fixedFence) WriteFence(context.Context, string) (string, storagev1.ControllerEpochFence, error) {
	return f.datasetID, f.fence, nil
}

type protocolController struct {
	store   *Store
	storage *storageclient.Client
	half    *faultyStoreHalf
	fence   controlstore.ControllerEpochFence
}

type protocolFaultFixture struct {
	kubeClient client.Client
	backend    *sqlitestore.Store
	datasetID  string
	replaced   *protocolController
	successor  *protocolController
	runtime    *runtimeCleanupCounter
}

type runtimeCleanupCounter struct {
	mu    sync.Mutex
	calls int
}

func (c *runtimeCleanupCounter) cleanup(context.Context, controlstore.SessionCleanupIntent, controlstore.ControllerEpochFence) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil
}

func (c *runtimeCleanupCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newProtocolFaultFixture(t *testing.T) *protocolFaultFixture {
	t.Helper()
	ctx := context.Background()
	_, kubeClient, fence := newTestStoreWithEpoch(t)
	backend, err := sqlitestore.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()
	info, err := backend.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &protocolFaultFixture{
		kubeClient: kubeClient, backend: backend, datasetID: info.DatasetID, runtime: &runtimeCleanupCounter{},
	}
	fixture.advanceDataset(t, fence)
	fixture.replaced = fixture.controller(t, fence)
	return fixture
}

func (f *protocolFaultFixture) controller(t *testing.T, fence controlstore.ControllerEpochFence) *protocolController {
	t.Helper()
	storage, err := storageclient.New(storageclient.Options{
		State: f.backend, Artifacts: f.backend, Embedded: f.backend,
		Fences: fixedFence{datasetID: f.datasetID, fence: fence},
	})
	if err != nil {
		t.Fatalf("create storage client: %v", err)
	}
	half := newFaultyStoreHalf(storage)
	kubeStore, err := NewComposite(f.kubeClient, testControlNamespace, half, WithSessionRuntimeCleanup(f.runtime.cleanup))
	if err != nil {
		t.Fatalf("NewComposite: %v", err)
	}
	return &protocolController{store: kubeStore, storage: storage, half: half, fence: fence}
}

func (f *protocolFaultFixture) advanceDataset(t *testing.T, fence controlstore.ControllerEpochFence) {
	t.Helper()
	ctx := context.Background()
	reader, err := New(f.kubeClient, testControlNamespace)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := reader.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		t.Fatal(err)
	}
	if epoch.Epoch != fence.Epoch || epoch.HolderID != fence.HolderID {
		t.Fatalf("Kubernetes holds epoch %d of %s, want %d of %s", epoch.Epoch, epoch.HolderID, fence.Epoch, fence.HolderID)
	}
	if _, err := f.backend.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
		DatasetID: f.datasetID, Epoch: *epoch, InstallationID: protocolInstallation,
	}); err != nil {
		t.Fatalf("advance the dataset to epoch %d: %v", fence.Epoch, err)
	}
}

func (f *protocolFaultFixture) takeover(t *testing.T) {
	t.Helper()
	epoch, err := f.replaced.store.CompareAndSwapControllerEpoch(context.Background(), controlstore.ControllerEpochCAS{
		ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2, HolderID: "controller-b",
		RequestDigest: testDigest("protocol-takeover"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("take over the controller epoch: %v", err)
	}
	fence := controlstore.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
	f.advanceDataset(t, fence)
	f.successor = f.controller(t, fence)
}

func requireAllFenced(t *testing.T, errs []error, want int) {
	t.Helper()
	if len(errs) != want {
		t.Fatalf("delivered %d delayed steps, want %d", len(errs), want)
	}
	for _, err := range errs {
		if !errors.Is(err, controlstore.ErrFenced) {
			t.Fatalf("delayed step after the takeover = %v, want ErrFenced", err)
		}
	}
}

func protocolProjection(id string) *controlstore.OutboxProjection {
	payload := []byte(`{"phase":"Succeeded"}`)
	return &controlstore.OutboxProjection{
		ID: id, AggregateKind: "Task", AggregateID: "task-" + id, ProjectionKind: "TaskTerminalStatus",
		Payload: payload, PayloadDigest: controlstore.CanonicalBytesDigest(payload),
		CreatedAt: testNow, AvailableAt: testNow,
	}
}

func TestOutboxProtocolConvergesAfterLostResponses(t *testing.T) {
	ctx := context.Background()
	fixture := newProtocolFaultFixture(t)
	controller := fixture.replaced
	half := controller.half

	half.inject("EnqueueOutboxProjectionRecord", faultLose)
	if _, err := controller.store.EnqueueOutboxProjection(ctx, protocolProjection("p3-lost"), controller.fence); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
		t.Fatalf("enqueue with a lost response = %v, want ErrOutcomeUnknown", err)
	}
	enqueued, err := controller.store.EnqueueOutboxProjection(ctx, protocolProjection("p3-lost"), controller.fence)
	if err != nil || enqueued.Version != 1 {
		t.Fatalf("enqueue run again = %#v, %v; want the stored projection", enqueued, err)
	}

	claim := controlstore.ClaimOutboxProjectionsRequest{
		Fence: controller.fence, WorkerID: "projector-a", Limit: 10, LeaseDuration: time.Minute, Now: testNow,
	}
	half.inject("ClaimOutboxProjectionRecords", faultLose)
	if _, err := controller.store.ClaimOutboxProjections(ctx, claim); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
		t.Fatalf("claim with a lost response = %v, want ErrOutcomeUnknown", err)
	}
	early := claim
	early.Now = testNow.Add(30 * time.Second)
	if claimed, err := controller.store.ClaimOutboxProjections(ctx, early); err != nil || len(claimed) != 0 {
		t.Fatalf("claim while the lost claim's lease runs = %d projections, %v; want none", len(claimed), err)
	}
	late := claim
	late.Now = testNow.Add(2 * time.Minute)
	claimed, err := controller.store.ClaimOutboxProjections(ctx, late)
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("claim after the lease expired = %#v, %v; want the projection on its second attempt", claimed, err)
	}

	complete := controlstore.CompleteOutboxProjectionRequest{
		ID: "p3-lost", Fence: controller.fence, ExpectedVersion: claimed[0].Version, LeaseOwner: "projector-a",
		OperationID: "complete-p3-lost", OperationDigest: testDigest("complete-p3-lost"),
		NewState: controlstore.OutboxProjectionDelivered, DeliveryDigest: testDigest("delivered"),
		UpdatedAt: testNow.Add(3 * time.Minute),
	}
	half.inject("CompleteOutboxProjectionRecord", faultLose)
	if _, err := controller.store.CompleteOutboxProjection(ctx, complete); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
		t.Fatalf("completion with a lost response = %v, want ErrOutcomeUnknown", err)
	}
	completed, err := controller.store.CompleteOutboxProjection(ctx, complete)
	if err != nil || completed.State != controlstore.OutboxProjectionDelivered {
		t.Fatalf("completion run again = %#v, %v; want the stored delivered projection", completed, err)
	}
	stored, err := controller.store.GetOutboxProjection(ctx, "p3-lost")
	if err != nil || stored.Version != completed.Version || stored.Attempts != 2 {
		t.Fatalf("projection after convergence = %#v, %v", stored, err)
	}
	if claimed, err := controller.store.ClaimOutboxProjections(ctx, controlstore.ClaimOutboxProjectionsRequest{
		Fence: controller.fence, WorkerID: "projector-a", Limit: 10, LeaseDuration: time.Minute, Now: testNow.Add(time.Hour),
	}); err != nil || len(claimed) != 0 {
		t.Fatalf("claim after delivery = %d projections, %v; want none", len(claimed), err)
	}
}

func TestOutboxProtocolRefusesCompletionDelayedPastTakeover(t *testing.T) {
	ctx := context.Background()
	fixture := newProtocolFaultFixture(t)
	replaced := fixture.replaced
	if _, err := replaced.store.EnqueueOutboxProjection(ctx, protocolProjection("p3-delayed"), replaced.fence); err != nil {
		t.Fatal(err)
	}
	claimed, err := replaced.store.ClaimOutboxProjections(ctx, controlstore.ClaimOutboxProjectionsRequest{
		Fence: replaced.fence, WorkerID: "projector-a", Limit: 10, LeaseDuration: time.Minute, Now: testNow,
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	replaced.half.inject("CompleteOutboxProjectionRecord", faultDelay)
	if _, err := replaced.store.CompleteOutboxProjection(ctx, controlstore.CompleteOutboxProjectionRequest{
		ID: "p3-delayed", Fence: replaced.fence, ExpectedVersion: claimed[0].Version, LeaseOwner: "projector-a",
		OperationID: "complete-a", OperationDigest: testDigest("complete-a"),
		NewState: controlstore.OutboxProjectionDelivered, DeliveryDigest: testDigest("delivered-a"),
		UpdatedAt: testNow.Add(time.Second),
	}); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
		t.Fatalf("delayed completion = %v, want ErrOutcomeUnknown", err)
	}

	fixture.takeover(t)
	requireAllFenced(t, replaced.half.deliverDelayed(), 1)
	successor := fixture.successor
	stored, err := successor.store.GetOutboxProjection(ctx, "p3-delayed")
	if err != nil || stored.State != controlstore.OutboxProjectionDelivering || stored.Version != claimed[0].Version {
		t.Fatalf("projection after the delayed completion = %#v, %v; want the claim unchanged", stored, err)
	}
	reclaimed, err := successor.store.ClaimOutboxProjections(ctx, controlstore.ClaimOutboxProjectionsRequest{
		Fence: successor.fence, WorkerID: "projector-b", Limit: 10, LeaseDuration: time.Minute, Now: testNow.Add(2 * time.Minute),
	})
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("successor claim = %#v, %v", reclaimed, err)
	}
	completed, err := successor.store.CompleteOutboxProjection(ctx, controlstore.CompleteOutboxProjectionRequest{
		ID: "p3-delayed", Fence: successor.fence, ExpectedVersion: reclaimed[0].Version, LeaseOwner: "projector-b",
		OperationID: "complete-b", OperationDigest: testDigest("complete-b"),
		NewState: controlstore.OutboxProjectionDelivered, DeliveryDigest: testDigest("delivered-b"),
		UpdatedAt: testNow.Add(3 * time.Minute),
	})
	if err != nil || completed.State != controlstore.OutboxProjectionDelivered || completed.DeliveryDigest != testDigest("delivered-b") {
		t.Fatalf("successor completion = %#v, %v", completed, err)
	}
	storeSteps := replaced.half.count("CompleteOutboxProjectionRecord")
	if _, err := replaced.store.CompleteOutboxProjection(ctx, controlstore.CompleteOutboxProjectionRequest{
		ID: "p3-delayed", Fence: replaced.fence, ExpectedVersion: completed.Version, LeaseOwner: "projector-a",
		OperationID: "complete-a-again", OperationDigest: testDigest("complete-a-again"),
		NewState: controlstore.OutboxProjectionDelivered, DeliveryDigest: testDigest("delivered-a"),
		UpdatedAt: testNow.Add(4 * time.Minute),
	}); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("replaced controller completion after the takeover = %v, want a fence conflict", err)
	}
	if got := replaced.half.count("CompleteOutboxProjectionRecord"); got != storeSteps {
		t.Fatalf("a completion the replaced controller started after the takeover reached the store")
	}
}

func seedProtocolSession(t *testing.T, fixture *protocolFaultFixture, name string) controlstore.ReclaimSessionRequest {
	t.Helper()
	ctx := context.Background()
	controller := fixture.replaced
	session := &controlstore.SessionRecord{Namespace: "tenant-a", Name: name, SessionType: "task", CreatedAt: testNow, UpdatedAt: testNow}
	if err := controller.storage.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	control, err := controller.store.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: name, SessionUID: name + "-uid",
		RequestDigest: testDigest(name + "-control"), LeaseGeneration: 1,
		Availability: controlstore.SessionAvailable, CreatedAt: testNow,
	}, controller.fence)
	if err != nil {
		t.Fatalf("CreateSessionControl: %v", err)
	}
	if _, err := controller.store.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/" + name,
		OwnerKind: controlstore.BranchClaimOwnerSession, OwnerUID: control.SessionUID,
		LastVerified:  controlstore.RemoteRefState{SHA: "0123456789012345678901234567890123456789"},
		RequestDigest: testDigest(name + "-branch"), CreatedAt: testNow,
	}, controller.fence); err != nil {
		t.Fatalf("CreateBranchClaim: %v", err)
	}
	return controlstore.ReclaimSessionRequest{
		Namespace: "tenant-a", SessionName: name, Fence: controller.fence,
		OperationID: "delete-" + name, OperationDigest: testDigest("delete-" + name), RequestedAt: testNow,
	}
}

func requireSessionReclaimed(t *testing.T, fixture *protocolFaultFixture, request controlstore.ReclaimSessionRequest) {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.backend.GetSession(ctx, request.Namespace, request.SessionName); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("transcript after cleanup: %v", err)
	}
	reader, err := New(fixture.kubeClient, testControlNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetSessionControl(ctx, request.Namespace, request.SessionName); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("Session control after cleanup: %v", err)
	}
	uid := request.SessionName + "-uid"
	if err := fixture.kubeClient.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: runtimeSessionLeaseName(uid)}, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Session Lease after cleanup: %v", err)
	}
	if err := reader.ensureNoSessionBranchClaims(ctx, uid); err != nil {
		t.Fatalf("Session BranchClaims after cleanup: %v", err)
	}
	completion, err := fixture.backend.GetSessionCleanupCompletion(ctx, request.Namespace, request.SessionName)
	if err != nil || completion.OperationID != request.OperationID || completion.OperationDigest != request.OperationDigest {
		t.Fatalf("cleanup completion = %#v, %v", completion, err)
	}
	if _, err := fixture.backend.GetSessionCleanupIntent(ctx, request.Namespace, request.SessionName); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("cleanup intent after completion: %v", err)
	}
}

func TestSessionCleanupProtocolConvergesAfterLostResponses(t *testing.T) {
	for _, step := range []string{"PrepareSessionCleanup", "CompleteSessionCleanup"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			fixture := newProtocolFaultFixture(t)
			controller := fixture.replaced
			request := seedProtocolSession(t, fixture, "p4-lost")

			controller.half.inject(step, faultLose)
			if err := controller.store.ReclaimSession(ctx, request); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
				t.Fatalf("cleanup with a lost %s response = %v, want ErrOutcomeUnknown", step, err)
			}
			if err := controller.store.ReclaimSession(ctx, request); err != nil {
				t.Fatalf("cleanup run again: %v", err)
			}
			requireSessionReclaimed(t, fixture, request)
			if got := fixture.runtime.count(); got != 1 {
				t.Fatalf("runtime retired %d times, want once", got)
			}
			if got := controller.half.count("CompleteSessionCleanup"); got != 1 {
				t.Fatalf("completion ran %d times, want once", got)
			}
			if err := controller.store.ReclaimSession(ctx, request); err != nil {
				t.Fatalf("completed cleanup run again: %v", err)
			}
			if got := fixture.runtime.count(); got != 1 || controller.half.count("CompleteSessionCleanup") != 1 {
				t.Fatal("a completed cleanup ran again")
			}
		})
	}
}

func TestSessionCleanupProtocolRefusesCompletionDelayedPastTakeover(t *testing.T) {
	ctx := context.Background()
	fixture := newProtocolFaultFixture(t)
	replaced := fixture.replaced
	request := seedProtocolSession(t, fixture, "p4-delayed")

	replaced.half.inject("CompleteSessionCleanup", faultDelay)
	if err := replaced.store.ReclaimSession(ctx, request); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
		t.Fatalf("cleanup with a delayed completion = %v, want ErrOutcomeUnknown", err)
	}
	fixture.takeover(t)
	requireAllFenced(t, replaced.half.deliverDelayed(), 1)
	if _, err := fixture.backend.GetSession(ctx, request.Namespace, request.SessionName); err != nil {
		t.Fatalf("a delayed completion deleted the transcript: %v", err)
	}
	if _, err := fixture.backend.GetSessionCleanupIntent(ctx, request.Namespace, request.SessionName); err != nil {
		t.Fatalf("a delayed completion removed the intent: %v", err)
	}

	successor := fixture.successor
	if err := successor.store.ResumeSessionCleanups(ctx, successor.fence); err != nil {
		t.Fatalf("successor resumes the cleanup: %v", err)
	}
	requireSessionReclaimed(t, fixture, request)
	if got := fixture.runtime.count(); got != 2 {
		t.Fatalf("runtime retired %d times, want once per controller", got)
	}
}

func TestSessionCleanupProtocolRefusesPreparationDelayedPastTakeover(t *testing.T) {
	ctx := context.Background()
	fixture := newProtocolFaultFixture(t)
	replaced := fixture.replaced
	request := seedProtocolSession(t, fixture, "p4-prepare")

	replaced.half.inject("PrepareSessionCleanup", faultDelay)
	if err := replaced.store.ReclaimSession(ctx, request); !errors.Is(err, controlstore.ErrOutcomeUnknown) {
		t.Fatalf("cleanup with a delayed preparation = %v, want ErrOutcomeUnknown", err)
	}
	if got := fixture.runtime.count(); got != 0 {
		t.Fatalf("runtime retired %d times before the intent was stored", got)
	}
	fixture.takeover(t)
	successor := fixture.successor
	retried := request
	retried.Fence = successor.fence
	if err := successor.store.ReclaimSession(ctx, retried); err != nil {
		t.Fatalf("successor runs the cleanup: %v", err)
	}
	requireSessionReclaimed(t, fixture, request)
	requireAllFenced(t, replaced.half.deliverDelayed(), 1)
	if _, err := fixture.backend.GetSessionCleanupIntent(ctx, request.Namespace, request.SessionName); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("a delayed preparation recorded an intent after completion: %v", err)
	}
}
