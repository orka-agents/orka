package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/taskterminal"
)

type postTransitionPromptAttemptReadStore struct {
	store.DurableControlStore
	postReadErr      error
	returnNilRead    bool
	transitioned     bool
	postReadInjected bool
}

func (s *postTransitionPromptAttemptReadStore) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	if s.transitioned && !s.postReadInjected {
		s.postReadInjected = true
		if s.returnNilRead {
			return nil, nil
		}
		return nil, s.postReadErr
	}
	return s.DurableControlStore.GetPromptAttempt(ctx, id)
}

func (s *postTransitionPromptAttemptReadStore) TransitionPromptAttemptExecution(
	ctx context.Context,
	transition store.PromptAttemptExecutionTransition,
) (*store.PromptAttempt, error) {
	attempt, err := s.DurableControlStore.TransitionPromptAttemptExecution(ctx, transition)
	if err == nil && transition.NewState == store.PromptExecutionSubmittedUnknown {
		s.transitioned = true
	}
	return attempt, err
}

func TestPersistOutcomeUnknownHandlesPostTransitionAttemptReadFailure(t *testing.T) {
	for _, test := range []struct {
		name          string
		postReadErr   error
		returnNilRead bool
		wantErr       error
	}{
		{
			name:        "read error",
			postReadErr: errors.New("simulated post-transition PromptAttempt read failure"),
		},
		{
			name:          "nil attempt",
			returnNilRead: true,
			wantErr:       store.ErrNotFound,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "prompt-attempt-read.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck

			baseStore := sqlite.NewStore(db, "test")
			epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, baseStore, "prompt-attempt-post-transition-read")
			defer stopEpoch()
			fence, err := epochs.CurrentFence(ctx)
			if err != nil {
				t.Fatal(err)
			}
			attempt := createPromptAttemptInStateForSettlementTest(t, ctx, baseStore, fence, store.PromptExecutionSubmitting)
			failingStore := &postTransitionPromptAttemptReadStore{
				DurableControlStore: baseStore,
				postReadErr:         test.postReadErr,
				returnNilRead:       test.returnNilRead,
			}
			dispatcher := &ACPDispatcher{Store: failingStore}

			err = dispatcher.persistOutcomeUnknown(ctx, attempt.ID, fence, "RuntimeLost", "prompt outcome is unknown")
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("persistOutcomeUnknown() error = %v, want %v", err, test.wantErr)
				}
			} else if !errors.Is(err, test.postReadErr) {
				t.Fatalf("persistOutcomeUnknown() error = %v, want %v", err, test.postReadErr)
			}
			if !failingStore.postReadInjected {
				t.Fatal("post-transition GetPromptAttempt failure was not injected")
			}
		})
	}
}

type failingTerminalAttemptTransitionStore struct {
	store.DurableControlStore
	transitionState store.PromptExecutionState
	transitionErr   error
	enqueueCalls    int
}

func (s *failingTerminalAttemptTransitionStore) TransitionPromptAttemptExecution(
	ctx context.Context,
	transition store.PromptAttemptExecutionTransition,
) (*store.PromptAttempt, error) {
	if transition.NewState == s.transitionState {
		return nil, s.transitionErr
	}
	return s.DurableControlStore.TransitionPromptAttemptExecution(ctx, transition)
}

func (s *failingTerminalAttemptTransitionStore) EnqueueOutboxProjection(
	ctx context.Context,
	projection *store.OutboxProjection,
	fence store.ControllerEpochFence,
) (*store.OutboxProjection, error) {
	s.enqueueCalls++
	return s.DurableControlStore.EnqueueOutboxProjection(ctx, projection, fence)
}

func TestFinishNonSuccessStopsStandaloneProjectionWhenAttemptTerminalTransitionFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "terminal-transition-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	baseStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, baseStore, "terminal-transition-failure")
	defer stopEpoch()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	taskUID := types.UID("93939393-9393-9393-9393-939393939393")
	promptID := "prompt-" + string(taskUID) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "terminal-transition-failure", UID: taskUID},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "fail after durable settlement"},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State:           corev1alpha1.TaskExecutionStateRunning,
				Attempt:         1,
				PromptID:        promptID,
				RequestDigest:   testControlDigestForDispatcher("terminal-transition-failure"),
				ControllerEpoch: fence.Epoch,
			},
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	attempt := createPromptAttemptInStateForSettlementTest(t, ctx, baseStore, fence, store.PromptExecutionRunning)
	if attempt.Key.TaskUID != string(taskUID) || attempt.Key.PromptID != promptID {
		t.Fatalf("test attempt identity = %#v, want Task %s PromptID %s", attempt.Key, taskUID, promptID)
	}

	transitionErr := errors.New("simulated durable terminal transition failure")
	failingStore := &failingTerminalAttemptTransitionStore{
		DurableControlStore: baseStore,
		transitionState:     store.PromptExecutionFailed,
		transitionErr:       transitionErr,
	}
	dispatcher := &ACPDispatcher{Client: kubeClient, Store: failingStore, Epochs: epochs}

	err = dispatcher.finishNonSuccess(
		ctx,
		task.DeepCopy(),
		attempt.ID,
		fence,
		nil,
		harnessv2.Event{Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventFailed},
	)
	if !errors.Is(err, transitionErr) {
		t.Errorf("finishNonSuccess() error = %v, want %v", err, transitionErr)
	}
	if failingStore.enqueueCalls != 0 {
		t.Errorf("terminal Task projection enqueue calls = %d, want 0", failingStore.enqueueCalls)
	}

	persistedTask := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), persistedTask); err != nil {
		t.Fatal(err)
	}
	if persistedTask.Status.Phase != corev1alpha1.TaskPhaseRunning || persistedTask.Status.Execution == nil ||
		persistedTask.Status.Execution.State != corev1alpha1.TaskExecutionStateRunning {
		t.Errorf("Task was projected terminal after durable transition failure: %#v", persistedTask.Status)
	}
	persistedAttempt, err := baseStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedAttempt.ExecutionState != store.PromptExecutionRunning {
		t.Errorf("PromptAttempt state = %s, want %s", persistedAttempt.ExecutionState, store.PromptExecutionRunning)
	}
}

type successfulSessionFinalizationStore struct {
	store.SessionControlStore
	finalizeCalls int
	control       store.SessionControl
	turn          store.SessionTurn
}

func (s *successfulSessionFinalizationStore) FinalizeSessionTurn(
	_ context.Context,
	_ store.FinalizeSessionTurnRequest,
) (*store.SessionTurn, error) {
	s.finalizeCalls++
	turn := s.turn
	turn.State = store.SessionTurnFinalized
	return &turn, nil
}

func (s *successfulSessionFinalizationStore) GetSessionControl(
	_ context.Context,
	_, _ string,
) (*store.SessionControl, error) {
	control := s.control
	return &control, nil
}

func TestFinishNonSuccessStopsCancellationFinalizationWhenAttemptTerminalTransitionFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "cancel-transition-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	baseStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, baseStore, "cancel-transition-failure")
	defer stopEpoch()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	taskUID := types.UID("93939393-9393-9393-9393-939393939393")
	promptID := "prompt-" + string(taskUID) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cancel-transition-failure", UID: taskUID},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "cancel after durable settlement"},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State:           corev1alpha1.TaskExecutionStateRunning,
				Attempt:         1,
				PromptID:        promptID,
				RequestDigest:   testControlDigestForDispatcher("cancel-transition-failure"),
				ControllerEpoch: fence.Epoch,
			},
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	attempt := createPromptAttemptInStateForSettlementTest(t, ctx, baseStore, fence, store.PromptExecutionRunning)
	if attempt.Key.TaskUID != string(taskUID) || attempt.Key.PromptID != promptID {
		t.Fatalf("test attempt identity = %#v, want Task %s PromptID %s", attempt.Key, taskUID, promptID)
	}

	transitionErr := errors.New("simulated durable cancellation transition failure")
	failingStore := &failingTerminalAttemptTransitionStore{
		DurableControlStore: baseStore,
		transitionState:     store.PromptExecutionCancelled,
		transitionErr:       transitionErr,
	}
	session, finalizationStore := newOpenTaskSessionForSettlementTest(t, task, attempt)
	dispatcher := &ACPDispatcher{
		Client:   kubeClient,
		Store:    failingStore,
		Epochs:   epochs,
		Sessions: &ACPSessionContinuity{controls: finalizationStore},
	}

	err = dispatcher.finishNonSuccess(
		ctx,
		task.DeepCopy(),
		attempt.ID,
		fence,
		session,
		harnessv2.Event{Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventCancelled},
	)
	if !errors.Is(err, transitionErr) {
		t.Errorf("finishNonSuccess() error = %v, want %v", err, transitionErr)
	}
	if finalizationStore.finalizeCalls != 0 {
		t.Errorf("session finalization calls = %d, want 0", finalizationStore.finalizeCalls)
	}
	if session.finalized {
		t.Error("session was marked finalized after durable cancellation transition failure")
	}
	if failingStore.enqueueCalls != 0 {
		t.Errorf("terminal Task projection enqueue calls = %d, want 0", failingStore.enqueueCalls)
	}

	persistedTask := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), persistedTask); err != nil {
		t.Fatal(err)
	}
	if persistedTask.Status.Phase != corev1alpha1.TaskPhaseRunning || persistedTask.Status.Execution == nil ||
		persistedTask.Status.Execution.State != corev1alpha1.TaskExecutionStateRunning {
		t.Errorf("Task was projected terminal after durable cancellation transition failure: %#v", persistedTask.Status)
	}
	persistedAttempt, err := baseStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedAttempt.ExecutionState != store.PromptExecutionRunning {
		t.Errorf("PromptAttempt state = %s, want %s", persistedAttempt.ExecutionState, store.PromptExecutionRunning)
	}
}

func newOpenTaskSessionForSettlementTest(
	t *testing.T,
	task *corev1alpha1.Task,
	attempt *store.PromptAttempt,
) (*acpTaskSession, *successfulSessionFinalizationStore) {
	t.Helper()

	key := store.SessionTurnKey{
		SessionUID:      "acp-session-settlement",
		LeaseGeneration: 1,
		TaskUID:         string(task.UID),
		Attempt:         int64(task.Status.Execution.Attempt),
		PromptID:        task.Status.Execution.PromptID,
	}
	turnID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	control := store.SessionControl{
		Namespace:       task.Namespace,
		SessionName:     "settlement-session",
		SessionUID:      key.SessionUID,
		Availability:    store.SessionAvailable,
		LeaseGeneration: key.LeaseGeneration,
		Lease: &store.SessionMutationLease{
			Generation:    key.LeaseGeneration,
			TaskUID:       key.TaskUID,
			Attempt:       key.Attempt,
			PromptID:      key.PromptID,
			RequestDigest: testControlDigestForDispatcher("settlement-session-lease"),
		},
		Version: 1,
	}
	turn := store.SessionTurn{
		ID:              turnID,
		Key:             key,
		PromptAttemptID: attempt.ID,
		RequestDigest:   testControlDigestForDispatcher("settlement-session-turn"),
		UserPrompt:      task.Spec.Prompt,
		State:           store.SessionTurnOpen,
		Version:         1,
	}
	finalizationStore := &successfulSessionFinalizationStore{control: control, turn: turn}
	session := &acpTaskSession{
		Turn: &ACPSessionTurn{
			Lease: ACPSessionLease{Session: control, Key: key},
			Turn:  turn,
		},
	}
	return session, finalizationStore
}

func createPromptAttemptInStateForSettlementTest(
	t *testing.T,
	ctx context.Context,
	controlStore store.DurableControlStore,
	fence store.ControllerEpochFence,
	target store.PromptExecutionState,
) *store.PromptAttempt {
	t.Helper()

	const taskUID = "93939393-9393-9393-9393-939393939393"
	key := store.PromptAttemptKey{
		Namespace: "default",
		TaskUID:   taskUID,
		Attempt:   1,
		PromptID:  "prompt-" + taskUID + "-1",
	}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		Key:           key,
		RequestDigest: testControlDigestForDispatcher("prompt-attempt-settlement"),
	}), fence)
	if err != nil {
		t.Fatal(err)
	}
	if target == attempt.ExecutionState {
		return attempt
	}

	path := []store.PromptExecutionState{
		store.PromptExecutionReserved,
		store.PromptExecutionSessionStarting,
		store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting,
		store.PromptExecutionAccepted,
		store.PromptExecutionRunning,
	}
	for _, next := range path {
		operation := "prompt-attempt-settlement-" + strings.ToLower(string(next))
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID:              attempt.ID,
			Fence:           fence,
			ExpectedVersion: attempt.Version,
			ExpectedState:   attempt.ExecutionState,
			NewState:        next,
			OperationID:     operation,
			OperationDigest: testControlDigestForDispatcher(operation),
			UpdatedAt:       time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if next == target {
			return attempt
		}
	}
	t.Fatalf("unsupported target PromptAttempt state %s", target)
	return nil
}

// TestTerminalProjectionExecutionRoundTripsReclamationValidation proves the
// standalone failure, delivery-failure, and post-execution-cancellation
// projection constructors preserve the complete frozen execution identity:
// each classification is built through terminalProjectionExecution and must
// round-trip taskterminal.ValidateRestoredProjection against the Task it was
// built from, exactly as Kubernetes composite-store reclamation revalidates
// it before retiring the source PromptAttempt.
func TestTerminalProjectionExecutionRoundTripsReclamationValidation(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC))
	frozen := &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-standalone-terminal",
		RuntimePoolName: "codex-pool", RuntimePoolUID: "pool-uid", AgentRuntimeName: "codex", AgentRuntimeUID: "agent-uid",
		RuntimeInstanceID: "runtime-instance", RuntimeSessionUID: "runtime-session-uid", RuntimeSessionGeneration: 3,
		RuntimeSessionSupervisorBootID: "boot-id",
		RuntimeSessionProfileDigest:    acpSessionTestDigest("profile"),
		RuntimeSessionMCPDigest:        acpSessionTestDigest("mcp"),
		RuntimeSessionWorkspaceDigest:  acpSessionTestDigest("workspace"),
		RequestDigest:                  acpSessionTestDigest("request"),
		ControllerEpoch:                7, Message: "running", LastTransitionTime: &transition,
	}
	tests := []struct {
		name           string
		state          corev1alpha1.TaskExecutionState
		outcome        corev1alpha1.TaskExecutionOutcome
		reason         corev1alpha1.TaskExecutionReason
		message        string
		phase          corev1alpha1.TaskPhase
		executionState store.PromptExecutionState
		deliveryState  store.PromptDeliveryState
		delivery       corev1alpha1.TaskDeliveryStatus
	}{
		{
			// failTaskWithProjection: sessionless prompt failure after RuntimeSession binding.
			name: "prompt failure", state: corev1alpha1.TaskExecutionStateFailed,
			outcome: corev1alpha1.TaskExecutionOutcomeFailed, reason: "PromptFailed", message: "prompt failed",
			phase: corev1alpha1.TaskPhaseFailed, executionState: store.PromptExecutionFailed,
			deliveryState: store.PromptDeliveryNotRequested,
			delivery: corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			},
		},
		{
			// failTaskWithProjection: sessionless prompt cancellation after RuntimeSession binding.
			name: "prompt cancellation", state: corev1alpha1.TaskExecutionStateCancelled,
			outcome: corev1alpha1.TaskExecutionOutcomeCancelled, reason: "Cancelled", message: "prompt cancelled",
			phase: corev1alpha1.TaskPhaseCancelled, executionState: store.PromptExecutionCancelled,
			deliveryState: store.PromptDeliveryNotRequested,
			delivery: corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			},
		},
		{
			// failTaskForDelivery: execution succeeded, publication failed.
			name: "delivery failure", state: corev1alpha1.TaskExecutionStateSucceeded,
			outcome: corev1alpha1.TaskExecutionOutcomeSucceeded, reason: "", message: "",
			phase: corev1alpha1.TaskPhaseFailed, executionState: store.PromptExecutionSucceeded,
			deliveryState: store.PromptDeliveryConflict,
			delivery: corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
			},
		},
		{
			// cancelTaskAfterExecution: execution succeeded, publication cancelled before push.
			name: "post-execution cancellation", state: corev1alpha1.TaskExecutionStateSucceeded,
			outcome: corev1alpha1.TaskExecutionOutcomeSucceeded, reason: "", message: "",
			phase: corev1alpha1.TaskPhaseCancelled, executionState: store.PromptExecutionSucceeded,
			deliveryState: store.PromptDeliveryCancelledBeforePublish,
			delivery: corev1alpha1.TaskDeliveryStatus{
				State:   corev1alpha1.TaskDeliveryStateCancelledBeforePublish,
				Outcome: corev1alpha1.TaskDeliveryOutcomeCancelledBeforePublish,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			delivery := tt.delivery
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "orka-system", Name: "standalone-terminal", UID: types.UID("task-standalone-terminal")},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{
					Phase: tt.phase, Execution: frozen.DeepCopy(), Delivery: &delivery,
				},
			}
			execution := terminalProjectionExecution(task, tt.state, tt.outcome, tt.reason, tt.message)
			expected := *frozen.DeepCopy()
			expected.State = tt.state
			expected.Outcome = tt.outcome
			expected.Reason = tt.reason
			expected.Message = tt.message
			expected.LastTransitionTime = nil
			if !reflect.DeepEqual(execution, expected) {
				t.Fatalf("terminalProjectionExecution() = %#v, want %#v", execution, expected)
			}
			attempt := &store.PromptAttempt{
				ID: "prompt-attempt-standalone-terminal",
				Key: store.PromptAttemptKey{
					Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: frozen.PromptID,
				},
				RequestDigest: frozen.RequestDigest, RuntimeInstanceID: frozen.RuntimeInstanceID,
				ExecutionState: tt.executionState, DeliveryState: tt.deliveryState,
			}
			payload, err := json.Marshal(taskTerminalProjection{
				Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: execution.Attempt,
				Phase: tt.phase, Message: tt.message, Execution: execution, Delivery: task.Status.Delivery,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := taskterminal.ValidateRestoredProjection(payload, task, string(task.UID), attempt); err != nil {
				t.Fatalf("terminal projection failed reclamation validation: %v", err)
			}

			// The pre-fix sparse payload omitted the frozen runtime identity and
			// must keep failing closed at reclamation.
			sparse, err := json.Marshal(taskTerminalProjection{
				Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: frozen.Attempt,
				Phase: tt.phase, Message: tt.message, Delivery: task.Status.Delivery,
				Execution: corev1alpha1.TaskExecutionStatus{
					State: tt.state, Outcome: tt.outcome, Reason: tt.reason, Message: tt.message,
					Attempt: frozen.Attempt, PromptID: frozen.PromptID,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := taskterminal.ValidateRestoredProjection(sparse, task, string(task.UID), attempt); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("sparse terminal projection validation error = %v, want store.ErrConflict", err)
			}
		})
	}
}
