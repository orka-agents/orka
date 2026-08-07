package controller

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestHarnessV1CleanupOnlySettlesWithoutExecutionCalls(t *testing.T) {
	fixture := newHarnessV1DispatcherPreparedFixture(t)
	fixture.dispatcher.Epochs = readyHarnessV1TestEpochManager(fixture.durable, fixture.fence)
	recorder := &recordingHarnessV1RecoveryClient{}
	fixture.dispatcher.clientFactory = func(string, string, *http.Client) (harnessV1ProtocolClient, error) {
		return recorder, nil
	}

	current := &corev1alpha1.Task{}
	key := types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}
	if err := fixture.dispatcher.Client.Get(fixture.ctx, key, current); err != nil {
		t.Fatal(err)
	}
	current.Status.AgentExecutionBinding.Mode = corev1alpha1.AgentExecutionBindingModeCleanupOnly
	current.Status.AgentExecutionBinding.Provenance = corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly
	current.Status.AgentExecutionBinding.MigrationInventoryID = "cleanup-test"
	current.Status.AgentExecutionBinding.BindingDigest = testControlDigestForDispatcher("synthetic-v1-cleanup-binding")
	if err := fixture.dispatcher.Client.Status().Update(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}

	if err := fixture.dispatcher.dispatchOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	settled, err := fixture.durable.GetHarnessV1Attempt(fixture.ctx, harnessV1AttemptKey(fixture.attempt))
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != store.HarnessV1AttemptRejected || settled.TerminalReason != harnessV1ReasonLegacyCleanupOnly {
		t.Fatalf("cleanup-only attempt = %#v, want terminal Rejected", settled)
	}
	if recorder.startCalls != 0 || recorder.streamCalls != 0 || recorder.statusCalls != 0 {
		t.Fatalf("cleanup-only protocol calls: start=%d stream=%d status=%d", recorder.startCalls, recorder.streamCalls, recorder.statusCalls)
	}
	if err := fixture.dispatcher.Client.Get(fixture.ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.HarnessRuntime == nil ||
		current.Status.HarnessRuntime.State != corev1alpha1.TaskExecutionStateFailed {
		t.Fatalf("cleanup-only Task projection = %#v", current.Status)
	}
}

func TestACPCleanupOnlySettlesWithoutCapacityAdmission(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       store.PromptExecutionState
		wantState   store.PromptExecutionState
		wantTask    corev1alpha1.TaskExecutionState
		wantOutcome corev1alpha1.TaskExecutionOutcome
	}{
		{name: "queued", state: store.PromptExecutionQueued, wantState: store.PromptExecutionFailed,
			wantTask: corev1alpha1.TaskExecutionStateFailed, wantOutcome: corev1alpha1.TaskExecutionOutcomeFailed},
		{name: "running", state: store.PromptExecutionRunning, wantState: store.PromptExecutionOutcomeUnknown,
			wantTask: corev1alpha1.TaskExecutionStateOutcomeUnknown, wantOutcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "cleanup.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			controlStore := sqlite.NewStore(db, "cleanup-test")
			epochs, stopEpochs := startACPRecoveryEpochManager(t, ctx, controlStore, "cleanup-controller")
			defer stopEpochs()
			fence, err := epochs.CurrentFence(ctx)
			if err != nil {
				t.Fatal(err)
			}

			uid := types.UID("cleanup-" + test.name + "-uid")
			promptID := "prompt-" + string(uid) + "-1"
			requestDigest := testControlDigestForDispatcher("cleanup-" + test.name + "-request")
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cleanup-" + test.name, UID: uid},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
					AgentExecutionBinding: testACPCleanupBinding(),
					Execution: &corev1alpha1.TaskExecutionStatus{
						State: taskExecutionStateForPromptAttempt(test.state), Attempt: 1, PromptID: promptID,
						RuntimePoolName: "cleanup-pool", RuntimePoolUID: "cleanup-pool-uid",
						RequestDigest: requestDigest, ControllerEpoch: fence.Epoch,
					},
					Delivery: &corev1alpha1.TaskDeliveryStatus{
						State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
					},
				},
			}
			pool := &corev1alpha1.RuntimePool{
				ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "cleanup-pool", UID: "cleanup-pool-uid"},
				Status: corev1alpha1.RuntimePoolStatus{Capacity: corev1alpha1.RuntimePoolCapacityStatus{
					Reservations: []corev1alpha1.RuntimePoolCapacityReservationStatus{{
						PoolUID: string("cleanup-pool-uid"), TaskUID: string(uid), Attempt: 1,
						ControllerEpoch: fence.Epoch, RuntimeInstanceID: "runtime-instance",
						PromptSlots: 1, ReservedAt: metav1.Now(), ExpiresAt: metav1.NewTime(time.Now().Add(time.Minute)),
					}},
				}},
			}
			attemptKey := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(uid), Attempt: 1, PromptID: promptID}
			attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
				Key: attemptKey, RequestDigest: requestDigest,
			}), fence)
			if err != nil {
				t.Fatal(err)
			}
			if test.state == store.PromptExecutionRunning {
				attempt = transitionCleanupACPAttemptToRunning(t, ctx, controlStore, fence, attempt)
			}
			attemptObject := &corev1alpha1.PromptAttempt{
				ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "cleanup-attempt-" + test.name},
				Spec: corev1alpha1.PromptAttemptSpec{
					ID: attempt.ID, TaskUID: string(uid), Attempt: 1, PromptID: promptID,
					RequestDigest: requestDigest, BindingDigest: attempt.BindingDigest, SnapshotDigest: attempt.SnapshotDigest,
				},
			}

			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).
				WithObjects(task, pool, attemptObject).Build()
			dispatcher := &ACPDispatcher{
				Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore,
				Epochs: epochs, AdmissionGate: NewACPAdmissionGate(), IdlePoolTTL: time.Hour,
				active: make(map[types.UID]struct{}), sem: make(chan struct{}, 1),
			}
			if err := dispatcher.dispatchOnce(ctx); err != nil {
				t.Fatal(err)
			}

			settled, err := controlStore.GetPromptAttempt(ctx, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if settled.ExecutionState != test.wantState {
				t.Fatalf("cleanup attempt state = %s, want %s", settled.ExecutionState, test.wantState)
			}
			currentTask := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), currentTask); err != nil {
				t.Fatal(err)
			}
			if currentTask.Status.Execution == nil || currentTask.Status.Execution.State != test.wantTask ||
				currentTask.Status.Execution.Outcome != test.wantOutcome || currentTask.Status.Execution.Reason != acpLegacyCleanupReason {
				t.Fatalf("cleanup Task status = %#v", currentTask.Status.Execution)
			}
			currentPool := &corev1alpha1.RuntimePool{}
			if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
				t.Fatal(err)
			}
			if len(currentPool.Status.Capacity.Reservations) != 0 {
				t.Fatalf("cleanup-only Task retained %d capacity reservations", len(currentPool.Status.Capacity.Reservations))
			}
		})
	}
}

func testACPCleanupBinding() *corev1alpha1.AgentExecutionBinding {
	return &corev1alpha1.AgentExecutionBinding{
		SchemaVersion: 1, Mode: corev1alpha1.AgentExecutionBindingModeCleanupOnly,
		ContractVersion:      corev1alpha1.AgentRuntimeContractHarnessV2,
		Provenance:           corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly,
		Backend:              corev1alpha1.AgentExecutionBackendRuntimePool,
		MigrationInventoryID: "cleanup-test",
		BindingDigest:        testControlDigestForDispatcher("synthetic-v2-cleanup-binding"),
		Snapshot:             corev1alpha1.AgentExecutionSnapshotRef{Digest: testControlDigestForDispatcher("synthetic-v2-cleanup-snapshot")},
	}
}

func transitionCleanupACPAttemptToRunning(
	t *testing.T,
	ctx context.Context,
	controlStore *sqlite.Store,
	fence store.ControllerEpochFence,
	attempt *store.PromptAttempt,
) *store.PromptAttempt {
	t.Helper()
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
	} {
		operation := "cleanup-test-" + string(state)
		transition := store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: state, OperationID: operation, OperationDigest: testControlDigestForDispatcher(operation), UpdatedAt: time.Now().UTC(),
		}
		if state == store.PromptExecutionSessionStarting {
			transition.RuntimeInstanceID = "runtime-instance"
		}
		var err error
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, transition)
		if err != nil {
			t.Fatal(err)
		}
	}
	return attempt
}
