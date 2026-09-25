package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	storetest "github.com/orka-agents/orka/internal/store/storetest"
)

func retainedSessionTaskForTerminalEvent(t *testing.T, phase corev1alpha1.TaskPhase) (*TaskReconciler, *corev1alpha1.Task) {
	t.Helper()
	now := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "retained-session-task", UID: types.UID("terminal-session-task-uid"),
			Finalizers: []string{labels.TaskFinalizer}, DeletionTimestamp: &now,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			SessionRef: &corev1alpha1.SessionReference{Name: "retained-session"},
			Workspace:  &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: phase, Message: "prompt cancellation settled",
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionState(phase), Outcome: corev1alpha1.TaskExecutionOutcome(phase),
				Attempt: 1, PromptID: "prompt-1", RuntimeInstanceID: "runtime-1",
				RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 1,
			},
		},
	}
	r := newUnitReconciler(newTestScheme(), task)
	r.DurableControlStore = r.ResultStore.(store.DurableControlStore)
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("retained result")); err != nil {
		t.Fatal(err)
	}
	return r, task
}

func TestDeletingSessionTaskPublishesTerminalEventBeforeCleanup(t *testing.T) {
	for _, phase := range []corev1alpha1.TaskPhase{corev1alpha1.TaskPhaseCancelled, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			r, task := retainedSessionTaskForTerminalEvent(t, phase)
			eventStore := storetest.NewFakeExecutionEventStore()
			r.ExecutionEventStore = eventStore
			ctx := context.Background()
			for _, kind := range []string{events.ExecutionEventTypeTaskCreated, "ModelRequestStarted", "ModelRequestFailed"} {
				if _, err := eventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
					Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name,
					TaskName: task.Name, Type: kind, Severity: events.ExecutionEventSeverityInfo,
				}); err != nil {
					t.Fatal(err)
				}
			}
			filter := store.ExecutionEventFilter{Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10}
			before, err := eventStore.ListExecutionEvents(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: clientObjectKey(task)})
				if err != nil || result.RequeueAfter != 2*time.Second {
					t.Fatalf("reconcile = %#v, %v, want cleanup deferred", result, err)
				}
			}
			after, err := eventStore.ListExecutionEvents(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) {
				t.Fatalf("terminal deleting Task events = %#v, want unchanged prefix and one terminal", after)
			}
			terminal := after[len(before)]
			if terminal.Type != executionEventTypeForTaskPhase(phase) || terminal.Summary != task.Status.Message || terminal.Seq <= before[len(before)-1].Seq {
				t.Fatalf("terminal event = %#v", terminal)
			}
			latest := &corev1alpha1.Task{}
			if err := r.Get(ctx, clientObjectKey(task), latest); err != nil || !controllerutil.ContainsFinalizer(latest, labels.TaskFinalizer) ||
				latest.Status.Execution.RuntimeSessionCleanupDigest != "" {
				t.Fatalf("Task authority changed while Session cleanup was pending: %#v, %v", latest, err)
			}
			if result, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err != nil || string(result) != "retained result" {
				t.Fatalf("result removed before Session retirement: %q, %v", result, err)
			}
		})
	}
}

func TestDeletingNonterminalSessionTaskDoesNotPublishTerminalEvent(t *testing.T) {
	r, task := retainedSessionTaskForTerminalEvent(t, corev1alpha1.TaskPhaseRunning)
	eventStore := storetest.NewFakeExecutionEventStore()
	r.ExecutionEventStore = eventStore
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clientObjectKey(task)})
	if err != nil || result.RequeueAfter != 2*time.Second {
		t.Fatalf("reconcile = %#v, %v", result, err)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10,
	})
	if err != nil || len(listed) != 0 {
		t.Fatalf("nonterminal Task acquired a terminal event: %#v, %v", listed, err)
	}
}

type terminalDeletionEventErrorStore struct {
	store.ExecutionEventStore
	failList    bool
	failAppend  bool
	listCalls   int
	appendCalls int
}

func (s *terminalDeletionEventErrorStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	s.listCalls++
	if s.failList {
		return nil, errors.New("injected event-list failure")
	}
	return s.ExecutionEventStore.ListExecutionEvents(ctx, filter)
}

func (s *terminalDeletionEventErrorStore) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	s.appendCalls++
	if s.failAppend {
		return nil, errors.New("injected event-append failure")
	}
	return s.ExecutionEventStore.AppendExecutionEvent(ctx, event)
}

func TestDeletingSessionTaskRetainsAuthorityOnTerminalEventFailure(t *testing.T) {
	for _, stage := range []string{"list", "append"} {
		t.Run(stage, func(t *testing.T) {
			r, task := retainedSessionTaskForTerminalEvent(t, corev1alpha1.TaskPhaseCancelled)
			storeValue := &terminalDeletionEventErrorStore{ExecutionEventStore: storetest.NewFakeExecutionEventStore(), failList: stage == "list", failAppend: stage == "append"}
			r.ExecutionEventStore = storeValue
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clientObjectKey(task)})
			if err != nil || result.RequeueAfter != time.Second || storeValue.listCalls != 1 ||
				(stage == "append" && storeValue.appendCalls != 1) {
				t.Fatalf("event failure reconciliation = %#v, %v; lists=%d appends=%d", result, err, storeValue.listCalls, storeValue.appendCalls)
			}
			latest := &corev1alpha1.Task{}
			if err := r.Get(context.Background(), clientObjectKey(task), latest); err != nil || !controllerutil.ContainsFinalizer(latest, labels.TaskFinalizer) {
				t.Fatalf("event failure removed Task cleanup authority: %v", err)
			}
		})
	}
}

func TestTerminalDeletingTaskStillErasesEventsAfterReady(t *testing.T) {
	now := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ready-terminal", UID: types.UID("ready-terminal-uid"),
			Finalizers: []string{labels.TaskFinalizer}, DeletionTimestamp: &now},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled, Message: "cancelled"},
	}
	r := newUnitReconciler(newTestScheme(), task)
	eventStore := storetest.NewFakeExecutionEventStore()
	r.ExecutionEventStore = eventStore
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: clientObjectKey(task)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), clientObjectKey(task), &corev1alpha1.Task{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ready Task remains after finalization: %v", err)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10,
	})
	if err != nil || len(listed) != 0 {
		t.Fatalf("event history leaked after Task deletion: %#v, %v", listed, err)
	}
}

func TestRecoveredTerminalSessionCanonicalMarkerMatchesProjection(t *testing.T) {
	tests := []struct {
		name           string
		state          store.PromptExecutionState
		terminalReason string
		outcomeMarker  string
		wantState      corev1alpha1.TaskExecutionState
		wantOutcome    corev1alpha1.TaskExecutionOutcome
		wantReason     corev1alpha1.TaskExecutionReason
		wantMessage    string
	}{
		{
			name: "explicit cancellation", state: store.PromptExecutionCancelled,
			terminalReason: "Cancelled", outcomeMarker: "prompt cancellation settled",
			wantState: corev1alpha1.TaskExecutionStateCancelled, wantOutcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			wantReason: "Cancelled", wantMessage: "prompt cancellation settled",
		},
		{
			name: "provider failure", state: store.PromptExecutionFailed,
			terminalReason: "PromptFailed", outcomeMarker: "provider response failed after acceptance",
			wantState: corev1alpha1.TaskExecutionStateFailed, wantOutcome: corev1alpha1.TaskExecutionOutcomeFailed,
			wantReason: "PromptFailed", wantMessage: "provider response failed after acceptance",
		},
		{
			name: "task timeout", state: store.PromptExecutionCancelled,
			terminalReason: string(acpTaskTimeoutReason), outcomeMarker: acpTaskTimeoutCancellationSettledMessage,
			wantState: corev1alpha1.TaskExecutionStateCancelled, wantOutcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			wantReason: acpTaskTimeoutReason, wantMessage: acpTaskTimeoutCancellationSettledMessage,
		},
		{
			name: "credential blocked", state: store.PromptExecutionFailed,
			terminalReason: acpCredentialBlockedOperation,
			wantState:      corev1alpha1.TaskExecutionStateFailed, wantOutcome: corev1alpha1.TaskExecutionOutcomeFailed,
			wantReason: acpCredentialBlockedExecutionReason, wantMessage: acpCredentialBlockedMessage,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "recovered-terminal.db"))
			defer closeStore()
			continuity := newACPSessionTestContinuity(t, controlStore, ACPBootstrapLimits{})
			control := ensureACPSessionForTest(t, continuity, fence, "recovered-terminal")
			const (
				taskUID    = "task-recovered-terminal"
				promptID   = "prompt-recovered-terminal"
				userPrompt = "recover this terminal turn"
			)
			turn, attempt := openACPSessionTurnForTest(
				t, continuity, controlStore, fence, control, taskUID, promptID, userPrompt,
			)
			for _, next := range []store.PromptExecutionState{
				store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
				store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning, test.state,
			} {
				operation := "recovered-terminal-" + string(next)
				transition := store.PromptAttemptExecutionTransition{
					ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
					NewState: next, OperationID: operation, OperationDigest: testControlDigestForDispatcher(operation),
					UpdatedAt: attempt.UpdatedAt.Add(time.Second),
				}
				if next == test.state {
					transition.TerminalReason = test.terminalReason
					transition.OutcomeMarker = test.outcomeMarker
				}
				var err error
				attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, transition)
				if err != nil {
					t.Fatalf("transition PromptAttempt to %s: %v", next, err)
				}
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "recovered-terminal", UID: types.UID(taskUID)},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAgent, Prompt: userPrompt,
					SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName},
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
					Execution: &corev1alpha1.TaskExecutionStatus{
						State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: promptID,
						RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration,
						RequestDigest: attempt.RequestDigest,
					},
				},
			}
			dispatcher := &ACPDispatcher{Store: controlStore, Sessions: continuity}
			if err := dispatcher.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
				t.Fatal(err)
			}
			finalizedTurn, err := controlStore.GetSessionTurn(ctx, turn.Turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := controlStore.GetOutboxProjection(ctx, finalizedTurn.ProjectionID)
			if err != nil {
				t.Fatal(err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Execution.State != test.wantState || payload.Execution.Outcome != test.wantOutcome ||
				payload.Execution.Reason != test.wantReason || payload.Execution.Message != test.wantMessage {
				t.Fatalf("recovered terminal projection execution = %#v", payload.Execution)
			}
			if payload.Message != test.wantMessage {
				t.Errorf("terminal Task message = %q, want %q", payload.Message, test.wantMessage)
			}
			transcript, err := controlStore.GetSession(ctx, control.Namespace, control.SessionName)
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveredTerminalMarker(t, finalizedTurn, transcript, string(test.wantState), test.wantMessage, userPrompt)
			// Re-enter recovery with a new owner object. Immutable finalized bytes must not be rewritten.
			dispatcher = &ACPDispatcher{Store: controlStore, Sessions: newACPSessionTestContinuity(t, controlStore, ACPBootstrapLimits{})}
			if err := dispatcher.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
				t.Fatal(err)
			}
			afterTurn, err := controlStore.GetSessionTurn(ctx, turn.Turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterProjection, err := controlStore.GetOutboxProjection(ctx, finalizedTurn.ProjectionID)
			if err != nil {
				t.Fatal(err)
			}
			afterTranscript, err := controlStore.GetSession(ctx, control.Namespace, control.SessionName)
			if err != nil {
				t.Fatal(err)
			}
			if afterTurn.TerminalContent != finalizedTurn.TerminalContent || afterTurn.FinalizationDigest != finalizedTurn.FinalizationDigest ||
				!bytes.Equal(afterProjection.Payload, projection.Payload) || afterProjection.PayloadDigest != projection.PayloadDigest ||
				!reflect.DeepEqual(afterTranscript.Messages, transcript.Messages) {
				t.Fatal("recovery changed immutable finalized Session/projection evidence")
			}
		})
	}
}

func assertRecoveredTerminalMarker(t *testing.T, finalizedTurn *store.SessionTurn, transcript *store.SessionRecord, wantKind, wantMessage, userPrompt string) {
	t.Helper()
	var marker struct {
		Kind                    string `json:"kind"`
		Reason                  string `json:"reason"`
		AssistantResultRecorded bool   `json:"assistantResultRecorded"`
	}
	if err := json.Unmarshal([]byte(finalizedTurn.TerminalContent), &marker); err != nil {
		t.Fatal(err)
	}
	if finalizedTurn.TerminalKind != store.SessionTurnOutcomeMarker || marker.Kind != wantKind ||
		marker.Reason != wantMessage || marker.AssistantResultRecorded {
		t.Errorf("canonical terminal marker = %#v, want exact terminal message and no assistant", marker)
	}
	if len(transcript.Messages) != 2 || transcript.Messages[0].Role != "user" || transcript.Messages[0].Content != userPrompt ||
		transcript.Messages[1].Role != "system" || transcript.Messages[1].Content != finalizedTurn.TerminalContent {
		t.Fatalf("canonical transcript does not contain exactly the original user and terminal system marker: %#v", transcript.Messages)
	}
}
