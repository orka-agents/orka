package controller

import (
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// A Task deleted while no controller was running keeps the previous epoch.
// The next leader must still cancel it before admission so its finalizer can
// complete and the RuntimePool stops counting it as queued demand.
func TestACPDispatcherCancelsPreSubmissionTaskDeletedDuringControllerOutage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   store.PromptExecutionState
		session bool
	}{
		{name: "Queued", state: store.PromptExecutionQueued},
		{name: "Reserved", state: store.PromptExecutionReserved},
		{name: "SessionStarting", state: store.PromptExecutionSessionStarting},
		{name: "QueuedSessionTask", state: store.PromptExecutionQueued, session: true},
		{name: "ReservedSessionTask", state: store.PromptExecutionReserved, session: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, tc.state)
			defer fixture.close(t)
			ctx := fixture.ctx
			key := types.NamespacedName{Namespace: "default", Name: "task"}
			deleteACPOutageTestTask(t, fixture, func(task *corev1alpha1.Task) {
				if tc.session {
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "outage-session"}
				}
			})
			wantQueued := int32(0)
			if tc.state == store.PromptExecutionQueued || tc.state == store.PromptExecutionReserved {
				wantQueued = 1
			}
			if queued := acpOutageTestQueuedTasks(t, fixture); queued != wantQueued {
				t.Fatalf("queuedTasks before recovery = %d, want %d for the deleting Task", queued, wantQueued)
			}

			recoverAndSettleACPOutageTestTask(t, fixture)

			task := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(ctx, key, task); err != nil {
				t.Fatal(err)
			}
			if task.Status.Execution.State != corev1alpha1.TaskExecutionStateCancelled ||
				task.Status.Execution.ControllerEpoch != fixture.fence.Epoch || task.Status.Phase != corev1alpha1.TaskPhaseCancelled {
				t.Fatalf("deleting Task after recovery: phase %s, state %s, epoch %d (current %d)",
					task.Status.Phase, task.Status.Execution.State, task.Status.Execution.ControllerEpoch, fixture.fence.Epoch)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != store.PromptExecutionCancelled {
				t.Fatalf("PromptAttempt state = %s, want Cancelled", attempt.ExecutionState)
			}
			if queued := acpOutageTestQueuedTasks(t, fixture); queued != 0 {
				t.Fatalf("queuedTasks after cancellation = %d, want 0", queued)
			}

			// A later stale scan and admission pass must leave the settled Task alone.
			recoverAndSettleACPOutageTestTask(t, fixture)
			again, err := fixture.controlStore.GetPromptAttempt(ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if again.ExecutionState != store.PromptExecutionCancelled || again.Version != attempt.Version {
				t.Fatalf("PromptAttempt after a second scan = %s version %d, want Cancelled version %d",
					again.ExecutionState, again.Version, attempt.Version)
			}

			// Once the terminal projection is delivered, the Task finalizer may finish.
			projector := &ACPOutboxProjector{
				Client: fixture.kubeClient, Store: fixture.controlStore, Epochs: fixture.dispatcher.Epochs, WorkerID: "outage-test",
			}
			if err := projector.projectOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err := fixture.kubeClient.Get(ctx, key, task); err != nil {
				t.Fatal(err)
			}
			reconciler := &TaskReconciler{Client: fixture.kubeClient, DurableControlStore: fixture.controlStore}
			if ready, err := reconciler.acpTaskDeletionReady(ctx, task); err != nil || !ready {
				t.Fatalf("Task deletion ready = %t, err %v; want the finalizer released", ready, err)
			}
		})
	}
}

// Recovery must not replay or reclassify a deleting Task, or discard runtime
// cleanup authority its finalizer still waits on. These Tasks keep the
// previous epoch and are left to their existing cleanup paths.
func TestACPDispatcherRecoveryLeavesDeletedTaskWithRuntimeState(t *testing.T) {
	const (
		runtimeInstanceID = "runtime-old"
		runtimeSessionUID = "runtime-session-old"
	)
	for _, tc := range []struct {
		name    string
		state   store.PromptExecutionState
		binding *attemptRuntimeBinding
		mutate  func(*corev1alpha1.Task)
	}{
		{name: "Accepted", state: store.PromptExecutionAccepted},
		{name: "Running", state: store.PromptExecutionRunning},
		{
			name: "PlannedWithRuntimeSessionBinding", state: store.PromptExecutionPlanned,
			binding: &attemptRuntimeBinding{RuntimeInstanceID: runtimeInstanceID, SessionUID: runtimeSessionUID, SessionGeneration: 1},
		},
		{
			name: "ReservedSessionTaskWithRuntimeSession", state: store.PromptExecutionReserved,
			mutate: func(task *corev1alpha1.Task) {
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "outage-session"}
				task.Status.Execution.RuntimeInstanceID = runtimeInstanceID
				task.Status.Execution.RuntimeSessionUID = runtimeSessionUID
				task.Status.Execution.RuntimeSessionGeneration = 1
			},
		},
		{
			name: "QueuedAttemptOnTerminalTask", state: store.PromptExecutionQueued,
			mutate: func(task *corev1alpha1.Task) {
				task.Status.Phase = corev1alpha1.TaskPhaseCancelled
				task.Status.Execution.State = corev1alpha1.TaskExecutionStateCancelled
				task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeCancelled
				task.Status.Execution.Reason = acpTaskTimeoutReason
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newACPRecoveryFixtureWithBinding(t, tc.state, tc.binding)
			defer fixture.close(t)
			ctx := fixture.ctx
			before := deleteACPOutageTestTask(t, fixture, tc.mutate)
			attemptBefore, err := fixture.controlStore.GetPromptAttempt(ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}

			if err := fixture.dispatcher.recoverStaleAttempts(ctx); err != nil {
				t.Fatal(err)
			}

			task := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(before), task); err != nil {
				t.Fatal(err)
			}
			if task.Status.Phase != before.Status.Phase || task.Status.Execution.State != before.Status.Execution.State ||
				task.Status.Execution.ControllerEpoch != before.Status.Execution.ControllerEpoch ||
				task.Status.Execution.RuntimeSessionUID != before.Status.Execution.RuntimeSessionUID {
				t.Fatalf("deleting Task after recovery: phase %s, state %s, epoch %d, runtime session %q; want it unchanged",
					task.Status.Phase, task.Status.Execution.State, task.Status.Execution.ControllerEpoch, task.Status.Execution.RuntimeSessionUID)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != attemptBefore.ExecutionState || attempt.Version != attemptBefore.Version ||
				attempt.SessionUID != attemptBefore.SessionUID || attempt.RuntimeInstanceID != attemptBefore.RuntimeInstanceID {
				t.Fatalf("PromptAttempt after recovery = %s version %d session %q, want %s version %d session %q",
					attempt.ExecutionState, attempt.Version, attempt.SessionUID,
					attemptBefore.ExecutionState, attemptBefore.Version, attemptBefore.SessionUID)
			}
		})
	}
}

// deleteACPOutageTestTask applies mutate to the fixture Task, adds the Task
// finalizer, and deletes it as a client would while no controller runs.
func deleteACPOutageTestTask(t *testing.T, fixture *recoveryFixture, mutate func(*corev1alpha1.Task)) *corev1alpha1.Task {
	t.Helper()
	ctx := fixture.ctx
	key := types.NamespacedName{Namespace: "default", Name: "task"}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(ctx, key, task); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(task)
	}
	status := task.Status.DeepCopy()
	controllerutil.AddFinalizer(task, labels.TaskFinalizer)
	if err := fixture.kubeClient.Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Status = *status
	if err := fixture.kubeClient.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Delete(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Get(ctx, key, task); err != nil {
		t.Fatal(err)
	}
	if task.DeletionTimestamp.IsZero() || task.Status.Execution.ControllerEpoch >= fixture.fence.Epoch {
		t.Fatalf("fixture Task is not a deleting old-epoch Task: deletion %v epoch %d", task.DeletionTimestamp, task.Status.Execution.ControllerEpoch)
	}
	return task
}

// recoverAndSettleACPOutageTestTask runs leader startup recovery, then the
// first dispatch scan, which settles deleting Tasks before admission.
func recoverAndSettleACPOutageTestTask(t *testing.T, fixture *recoveryFixture) {
	t.Helper()
	dispatcher := fixture.dispatcher
	if dispatcher.active == nil {
		dispatcher.active = make(map[types.UID]struct{})
	}
	if err := dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	err := dispatcher.dispatchOnce(fixture.ctx)
	// Wait for any stale recovery the scan scheduled, as Start does on exit.
	dispatcher.staleRecoveryMu.Lock()
	dispatcher.staleRecoveryMu.Unlock() //nolint:staticcheck // SA2001: acquisition waits for the recovery worker to finish.
	if err != nil {
		t.Fatalf("dispatch scan: %v", err)
	}
}

// acpOutageTestQueuedTasks runs the dispatcher's pool demand accounting over
// the fixture's current Tasks and returns the resulting queuedTasks.
func acpOutageTestQueuedTasks(t *testing.T, fixture *recoveryFixture) int32 {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool"},
		Spec:       corev1alpha1.RuntimePoolSpec{DesiredReplicas: 1},
	}
	pools := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RuntimePool{}).WithObjects(pool).Build()
	var tasks corev1alpha1.TaskList
	if err := fixture.kubeClient.List(fixture.ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	reaper := &ACPDispatcher{Client: pools, Epochs: fixture.dispatcher.Epochs, IdlePoolTTL: time.Hour}
	if err := reaper.reapIdlePools(fixture.ctx, tasks.Items); err != nil {
		t.Fatal(err)
	}
	if err := pools.Get(fixture.ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
		t.Fatal(err)
	}
	return pool.Status.Capacity.QueuedTasks
}
