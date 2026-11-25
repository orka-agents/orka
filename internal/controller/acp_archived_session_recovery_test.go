package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestACPDispatcherRestartPreservesArchivedSessionEvidence(t *testing.T) {
	fixture, tasks, cleanup := newArchivedSessionRecoveryFixture(t)
	before := captureArchivedSessionEvidence(t, fixture, cleanup, tasks)
	for _, epoch := range []int64{2, 3} {
		dispatcher := archivedSessionRecoveryDispatcher(fixture, cleanup, epoch)
		runArchivedSessionRecovery(t, dispatcher, fixture.ctx)
		for _, task := range tasks {
			current := &corev1alpha1.Task{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Execution.ControllerEpoch != epoch {
				t.Fatalf("archived Task %s epoch = %d, want %d", current.Name, current.Status.Execution.ControllerEpoch, epoch)
			}
		}
		after := captureArchivedSessionEvidence(t, fixture, cleanup, tasks)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("restart changed archived projection, attempt, result, events, or Task status beyond its epoch")
		}
	}
	if fixture.createCalls.Load() != 1 || fixture.deleteCalls.Load() != 1 {
		t.Fatalf("restart replayed runtime work: creates=%d deletes=%d", fixture.createCalls.Load(), fixture.deleteCalls.Load())
	}
}

func TestACPDispatcherRestartIsolatesAndRetriesBlockedArchives(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "legacy missing archive", err: store.ErrNotFound},
		{name: "corrupt archive", err: store.ConflictErrorf("archive digest mismatch")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, tasks, cleanup := newArchivedSessionRecoveryFixture(t)
			before := captureArchivedSessionEvidence(t, fixture, cleanup, tasks)
			attemptID, err := promptAttemptIDFromTask(tasks[0])
			if err != nil {
				t.Fatal(err)
			}
			broken := &archivedSessionRecoveryStore{
				DurableControlStore: cleanup, receipts: cleanup, attemptID: attemptID, err: test.err,
			}
			dispatcher := archivedSessionRecoveryDispatcher(fixture, broken, 2)
			runArchivedSessionRecovery(t, dispatcher, fixture.ctx)
			for i, task := range tasks {
				current := &corev1alpha1.Task{}
				if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
					t.Fatal(err)
				}
				wantEpoch := int64(i + 1)
				if current.Status.Execution.ControllerEpoch != wantEpoch {
					t.Fatalf("Task %s epoch = %d, want %d", current.Name, current.Status.Execution.ControllerEpoch, wantEpoch)
				}
			}
			if broken.calls.Load() < 2 {
				t.Fatalf("blocked archive reads = %d, want startup plus periodic retry", broken.calls.Load())
			}
			if !reflect.DeepEqual(before, captureArchivedSessionEvidence(t, fixture, cleanup, tasks)) {
				t.Fatal("isolated recovery changed durable evidence or terminal Task status")
			}
			// Model a transient read failure clearing without restarting the
			// dispatcher. The ordinary scan must complete the blocked epoch.
			broken.err = nil
			var listed corev1alpha1.TaskList
			if err := fixture.client.List(fixture.ctx, &listed); err != nil {
				t.Fatal(err)
			}
			if err := dispatcher.scheduleACPDeliveryRecoveries(fixture.ctx, listed.Items); err != nil {
				t.Fatal(err)
			}
			dispatcher.staleRecoveryMu.Lock()
			dispatcher.staleRecoveryMu.Unlock() //nolint:staticcheck // SA2001: acquisition waits for the scheduled retry to finish.
			current := &corev1alpha1.Task{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(tasks[0]), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Execution.ControllerEpoch != 2 {
				t.Fatal("periodic retry did not recover after the read failure cleared")
			}
		})
	}
}

func TestACPDispatcherBlockedArchiveDoesNotStarveNewWork(t *testing.T) {
	fixture, tasks, cleanup := newArchivedSessionRecoveryFixture(t)
	blocked := tasks[0].DeepCopy()
	blocked.Status.Execution.ControllerEpoch = 0
	if err := fixture.client.Status().Update(fixture.ctx, blocked); err != nil {
		t.Fatal(err)
	}
	attemptID, err := promptAttemptIDFromTask(blocked)
	if err != nil {
		t.Fatal(err)
	}
	broken := &archivedSessionRecoveryStore{
		DurableControlStore: cleanup, receipts: cleanup, attemptID: attemptID, err: store.ErrNotFound,
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	broken.readBarrier, broken.readStarted = release, entered
	queued := fixture.queueTask(t, "independent-after-deletion", "independent-after-deletion-uid", "independent prompt", nil)
	dispatcher := archivedSessionRecoveryDispatcher(fixture, broken, 1)
	dispatcher.Epochs = fixture.epochs
	dispatcher.Sessions = fixture.dispatcher.Sessions
	ctx, cancel := context.WithCancel(fixture.ctx)
	done := make(chan error, 1)
	go func() { done <- dispatcher.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop dispatcher: %v", err)
		}
	}()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	select {
	case <-entered:
	case <-deadline:
		t.Fatal("periodic recovery did not reach the deliberately blocked read")
	}
	for {
		current := &corev1alpha1.Task{}
		if err := fixture.client.Get(ctx, client.ObjectKeyFromObject(queued), current); err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase == corev1alpha1.TaskPhaseSucceeded && runtimeSessionCleanupCompleteForUID(current, current.UID) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("independent Task did not finish while an archive was missing: %#v", current.Status.Execution)
		case <-tick.C:
		}
	}
	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(ctx, client.ObjectKeyFromObject(blocked), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Execution.ControllerEpoch != 0 || broken.calls.Load() < 2 {
		t.Fatal("independent work bypassed the blocked Task's recovery barrier or stopped retries")
	}
}

func TestACPDispatcherUnrecoveredEpochBlocksBothAdmissionChecks(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, client.ObjectKey{Namespace: defaultNS, Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateReserved
	if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	if keep, err := fixture.dispatcher.settleQueuedTaskBeforeAdmission(fixture.ctx, task); err != nil || keep {
		t.Fatalf("pre-admission accepted unrecovered epoch: keep=%v err=%v", keep, err)
	}
	if reserved, _, err := fixture.dispatcher.reserveTask(fixture.ctx, task); err != nil || reserved != nil {
		t.Fatalf("reservation accepted unrecovered epoch: task=%v err=%v", reserved, err)
	}
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if keep, err := fixture.dispatcher.settleQueuedTaskBeforeAdmission(fixture.ctx, task); err != nil || !keep {
		t.Fatalf("recovered Task remained outside admission: keep=%v err=%v", keep, err)
	}
}

func TestACPDispatcherStartupStillRejectsGlobalListFailure(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	watchClient, ok := fixture.client.(client.WithWatch)
	if !ok {
		t.Fatal("fixture client lacks watches")
	}
	want := errors.New("injected global Task list failure")
	dispatcher := archivedSessionRecoveryDispatcher(fixture, fixture.controlStore, 2)
	dispatcher.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return want },
	})
	if err := dispatcher.Start(fixture.ctx); !errors.Is(err, want) {
		t.Fatalf("structural startup failure = %v, want original list error", err)
	}
}

// A dispatcher that starts while the installation is held for storage
// recovery waits for the release instead of failing the manager, which would
// restart the controller into the same hold. It dispatches nothing while
// held, and dispatches once the hold is released.
func TestACPDispatcherStartWaitsForAnInstallationHold(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	queued := fixture.queueTask(t, "queued-during-hold", "queued-during-hold-uid", "prompt queued during the hold", nil)
	dispatcher := archivedSessionRecoveryDispatcher(fixture, fixture.controlStore, 1)
	dispatcher.Epochs = fixture.epochs
	dispatcher.Sessions = fixture.dispatcher.Sessions
	fixture.epochs.setHold(store.StorageRecoveryHold{
		ID: "hold-a", Reason: store.StorageRecoveryUnsafeSwitch, Message: "the dataset switched unsafely", RaisedAt: time.Now().UTC(),
	})
	t.Cleanup(fixture.epochs.clearHold)
	ctx, cancel := context.WithCancel(fixture.ctx)
	var startErr error
	returned := make(chan struct{})
	go func() {
		startErr = dispatcher.Start(ctx)
		close(returned)
	}()
	defer func() {
		cancel()
		<-returned
	}()
	select {
	case <-returned:
		t.Fatalf("dispatcher start under an installation hold returned %v, want it to wait for the release", startErr)
	case <-time.After(20 * dispatcher.Interval):
	}
	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(ctx, client.ObjectKeyFromObject(queued), current); err != nil {
		t.Fatal(err)
	}
	if calls := fixture.createCalls.Load(); calls != 0 || current.Status.Phase != queued.Status.Phase ||
		current.Status.Execution.State != queued.Status.Execution.State {
		t.Fatalf("the dispatcher acted on a Task while the installation was held: %d runtime sessions created, status %#v", calls, current.Status)
	}

	fixture.epochs.clearHold()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := fixture.client.Get(ctx, client.ObjectKeyFromObject(queued), current); err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase == corev1alpha1.TaskPhaseSucceeded && runtimeSessionCleanupCompleteForUID(current, current.UID) {
			break
		}
		select {
		case <-returned:
			t.Fatalf("dispatcher stopped after the release: %v", startErr)
		case <-deadline:
			t.Fatalf("queued Task did not finish after the hold was released: %#v", current.Status)
		case <-tick.C:
		}
	}
	cancel()
	<-returned
	if startErr != nil {
		t.Fatalf("stop dispatcher: %v", startErr)
	}
}

// A hold raised while the dispatcher runs stops dispatch without logging a
// failed scan on every tick, and dispatch resumes once it is released.
func TestACPDispatcherWaitsQuietlyForAHoldRaisedWhileItRuns(t *testing.T) {
	// Two Tasks are dispatched, which needs more time under the race
	// detector than the single-dispatch fixture default.
	fixture := newExternalACPDispatchFixtureWithOptions(t, "external-v2", testAgentRuntimeMCPPolicy(),
		externalACPDispatchFixtureOptions{contextTimeout: time.Minute})
	watchClient, ok := fixture.client.(client.WithWatch)
	if !ok {
		t.Fatal("fixture client lacks watches")
	}
	// A Task queued before the hold reaches a dispatcher scan only after
	// the hold is raised, as a scan that starts just after the hold does.
	// Each scan lists the Tasks once, so heldScans counts the scans that
	// started under the hold.
	const queuedName = "queued-before-hold"
	var hideQueued atomic.Bool
	var heldScans atomic.Int32
	hideQueued.Store(true)
	dispatcher := archivedSessionRecoveryDispatcher(fixture, fixture.controlStore, 1)
	dispatcher.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			tasks, ok := list.(*corev1alpha1.TaskList)
			switch {
			case !ok:
			case hideQueued.Load():
				tasks.Items = slices.DeleteFunc(tasks.Items, func(task corev1alpha1.Task) bool { return task.Name == queuedName })
			default:
				heldScans.Add(1)
			}
			return nil
		},
	})
	dispatcher.Epochs = fixture.epochs
	dispatcher.Sessions = fixture.dispatcher.Sessions
	var (
		logMu       sync.Mutex
		failedScans []string
		holdLines   []string
	)
	logger := funcr.New(func(prefix, args string) {
		logMu.Lock()
		defer logMu.Unlock()
		switch {
		case strings.Contains(args, "ACP dispatcher scan failed"):
			failedScans = append(failedScans, args)
		case strings.Contains(args, "Storage is held for recovery"):
			holdLines = append(holdLines, args)
		}
	}, funcr.Options{})
	ctx, cancel := context.WithCancel(logf.IntoContext(fixture.ctx, logger))
	var startErr error
	returned := make(chan struct{})
	go func() {
		startErr = dispatcher.Start(ctx)
		close(returned)
	}()
	defer func() {
		cancel()
		<-returned
	}()
	waitForTask := func(task *corev1alpha1.Task, what string) {
		t.Helper()
		current := &corev1alpha1.Task{}
		deadline := time.After(10 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if err := fixture.client.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase == corev1alpha1.TaskPhaseSucceeded && runtimeSessionCleanupCompleteForUID(current, current.UID) {
				return
			}
			select {
			case <-returned:
				t.Fatalf("dispatcher stopped %s: %v", what, startErr)
			case <-deadline:
				t.Fatalf("queued Task did not finish %s: %#v", what, current.Status)
			case <-tick.C:
			}
		}
	}
	waitForTask(fixture.queueTask(t, "dispatched-before-hold", "dispatched-before-hold-uid", "prompt dispatched before the hold", nil), "before the hold")
	created := fixture.createCalls.Load()

	queued := fixture.queueTask(t, queuedName, "queued-before-hold-uid", "prompt queued before the hold", nil)
	fixture.epochs.setHold(store.StorageRecoveryHold{
		ID: "hold-b", Reason: store.StorageRecoveryUnsafeSwitch, Message: "the dataset switched unsafely", RaisedAt: time.Now().UTC(),
	})
	t.Cleanup(fixture.epochs.clearHold)
	hideQueued.Store(false)
	// Five scans started under the hold means at least four ended at it.
	held := func() []string {
		logMu.Lock()
		defer logMu.Unlock()
		return slices.Clone(holdLines)
	}
	deadline := time.Now().Add(10 * time.Second)
	for heldScans.Load() < 5 || len(held()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d scans started under the hold and %d hold lines were logged within 10s", heldScans.Load(), len(held()))
		}
		time.Sleep(dispatcher.Interval)
	}
	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(ctx, client.ObjectKeyFromObject(queued), current); err != nil {
		t.Fatal(err)
	}
	if calls := fixture.createCalls.Load(); calls != created || current.Status.Phase != queued.Status.Phase ||
		current.Status.Execution.State != queued.Status.Execution.State {
		t.Fatalf("the dispatcher acted on a Task while the installation was held: %d runtime sessions created, status %#v", calls-created, current.Status)
	}
	logMu.Lock()
	failed := slices.Clone(failedScans)
	logMu.Unlock()
	if len(failed) != 0 {
		t.Fatalf("the dispatcher logged %d failed scans under the hold, want none: %q", len(failed), failed)
	}
	if lines := held(); len(lines) != 1 || !strings.Contains(lines[0], `"hold"="hold-b"`) {
		t.Fatalf("hold log lines = %q, want one naming hold-b", lines)
	}

	fixture.epochs.clearHold()
	waitForTask(queued, "after the hold was released")
	cancel()
	<-returned
	if startErr != nil {
		t.Fatalf("stop dispatcher: %v", startErr)
	}
}

func TestArchivedSessionRecoveryRejectsInvalidProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Task, *store.PromptAttempt, *store.SessionTurnCleanupReceipt)
	}{
		{name: "namespace", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.Namespace = "another"
		}},
		{name: "Session name", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.SessionName = "another"
		}},
		{name: "Session UID", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.Key.SessionUID = "another"
		}},
		{name: "lease generation", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.Key.LeaseGeneration++
		}},
		{name: "Task UID", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.Key.TaskUID = "another"
		}},
		{name: "prompt attempt", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.PromptAttemptID = "another"
		}},
		{name: "delivery proof", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.DeliveryDigest = ""
		}},
		{name: "DeadLetter", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.ProjectionState, r.DeliveredAt, r.DeliveryDigest = store.OutboxProjectionDeadLetter, nil, ""
		}},
		{name: "payload digest", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.Payload = []byte(`{}`)
		}},
		{name: "malformed terminal payload", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, r *store.SessionTurnCleanupReceipt) {
			r.Payload = []byte(`{"phase":"Succeeded"}`)
			r.PayloadDigest = store.CanonicalBytesDigest(r.Payload)
			r.ProjectionDigest = r.PayloadDigest
		}},
		{name: "Task phase", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *store.SessionTurnCleanupReceipt) {
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
		}},
		{name: "Task outcome", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *store.SessionTurnCleanupReceipt) {
			task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
		}},
		{name: "attempt nonterminal", mutate: func(_ *corev1alpha1.Task, attempt *store.PromptAttempt, _ *store.SessionTurnCleanupReceipt) {
			attempt.ExecutionState = store.PromptExecutionRunning
		}},
		{name: "runtime receipt absent", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *store.SessionTurnCleanupReceipt) {
			task.Status.Execution.RuntimeSessionCleanupDigest = ""
		}},
		{name: "runtime instance changed", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *store.SessionTurnCleanupReceipt) {
			task.Status.Execution.RuntimeInstanceID = "another"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task, control := deletedSessionTaskFixture(t)
			if err := validateArchivedSessionTask(task, control.attempt, control.receipt); err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
			test.mutate(task, control.attempt, control.receipt)
			if err := validateArchivedSessionTask(task, control.attempt, control.receipt); err == nil {
				t.Fatal("invalid archived proof authorized epoch recovery")
			}
		})
	}
}

type archivedSessionRecoveryStore struct {
	store.DurableControlStore
	receipts    store.SessionTurnCleanupReceiptStore
	attemptID   string
	err         error
	calls       atomic.Int32
	readBarrier <-chan struct{}
	readStarted chan<- struct{}
}

func (s *archivedSessionRecoveryStore) GetSessionTurnCleanupReceipt(ctx context.Context, namespace, sessionName, attemptID string) (*store.SessionTurnCleanupReceipt, error) {
	if attemptID == s.attemptID {
		calls := s.calls.Add(1)
		if calls > 1 && s.readBarrier != nil {
			select {
			case s.readStarted <- struct{}{}:
			default:
			}
			select {
			case <-s.readBarrier:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if s.err != nil {
			return nil, s.err
		}
	}
	return s.receipts.GetSessionTurnCleanupReceipt(ctx, namespace, sessionName, attemptID)
}

func newArchivedSessionRecoveryFixture(t *testing.T) (*externalACPDispatchFixture, []*corev1alpha1.Task, *storekube.Store) {
	t.Helper()
	fixture, tasks := newContinuedSessionCleanupFixture(t)
	cleanup := sessionRuntimeCleanupStore(t, fixture, fixture.dispatcher.CleanupSessionRuntime)
	manager := NewSessionManager(fixture.persistence)
	manager.SetACPSessionCleanup(cleanup, fixture.epochs)
	if err := manager.DeleteSession(fixture.ctx, defaultNS, "cleanup-conversation"); err != nil {
		t.Fatal(err)
	}
	assertSessionRuntimeCleanupCompleted(t, fixture, cleanup, tasks)
	for i, task := range tasks {
		current := &corev1alpha1.Task{}
		if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		tasks[i] = current
	}
	return fixture, tasks, cleanup
}

func archivedSessionRecoveryDispatcher(fixture *externalACPDispatchFixture, control store.DurableControlStore, epoch int64) *ACPDispatcher {
	epochs := NewControllerEpochManager(nil, "archive-recovery")
	epochs.current = &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: epoch, HolderID: "archive-recovery"}
	close(epochs.ready)
	return &ACPDispatcher{
		Client: fixture.client, APIReader: fixture.client, Store: control, ResultStore: fixture.persistence,
		EventStore: fixture.persistence, PlanStore: fixture.persistence, Snapshots: fixture.persistence, Epochs: epochs,
		Interval: 10 * time.Millisecond, MaxConcurrent: 1,
	}
}

func runArchivedSessionRecovery(t *testing.T, dispatcher *ACPDispatcher, parent context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 100*time.Millisecond)
	defer cancel()
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("dispatcher restart: %v", err)
	}
}

func captureArchivedSessionEvidence(t *testing.T, fixture *externalACPDispatchFixture, receipts store.SessionTurnCleanupReceiptStore, tasks []*corev1alpha1.Task) map[string][]byte {
	t.Helper()
	evidence := make(map[string][]byte)
	for _, task := range tasks {
		current := &corev1alpha1.Task{}
		if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		current.Status.Execution.ControllerEpoch = 0
		attemptID, err := promptAttemptIDFromTask(current)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, attemptID)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := receipts.GetSessionTurnCleanupReceipt(fixture.ctx, task.Namespace, task.Spec.SessionRef.Name, attemptID)
		if err != nil {
			t.Fatal(err)
		}
		for _, read := range []func() error{
			func() error { _, err := fixture.controlStore.GetSessionTurn(fixture.ctx, receipt.TurnID); return err },
			func() error {
				_, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, receipt.ProjectionID)
				return err
			},
		} {
			if err := read(); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("deleted ordinary Session evidence was recreated: %v", err)
			}
		}
		result, err := fixture.persistence.GetResult(fixture.ctx, task.Namespace, task.Name)
		if err != nil {
			t.Fatal(err)
		}
		events, err := fixture.persistence.ListExecutionEvents(fixture.ctx, store.ExecutionEventFilter{
			Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		for kind, value := range map[string]any{"status": current.Status, "attempt": attempt, "receipt": receipt, "events": events} {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			evidence[fmt.Sprintf("%s/%s", task.Name, kind)] = encoded
		}
		evidence[task.Name+"/result"] = result
	}
	return evidence
}
