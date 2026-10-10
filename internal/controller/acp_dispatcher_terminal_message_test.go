package controller

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestACPDispatcherSessionTerminalMessagePrecedesLifecycleEvent(t *testing.T) {
	const runtimeDetail = "fixture-runtime-diagnostic-must-stay-in-journal"
	for _, test := range []struct {
		name      string
		terminal  harnessv2.Event
		cancel    harnessv2.CancelReason
		wantState corev1alpha1.TaskExecutionState
		wantPhase corev1alpha1.TaskPhase
		wantMsg   string
	}{
		{
			name: "failed", terminal: harnessv2.Event{Type: harnessv2.EventFailed,
				Failed: &harnessv2.FailedEvent{StopReason: harnessv2.ACPStopReasonRefusal, Code: "provider_failure", Message: runtimeDetail}},
			wantState: corev1alpha1.TaskExecutionStateFailed, wantPhase: corev1alpha1.TaskPhaseFailed, wantMsg: "prompt failed",
		},
		{
			name: "cancelled", terminal: harnessv2.Event{Type: harnessv2.EventCancelled,
				Cancelled: &harnessv2.CancelledEvent{StopReason: harnessv2.ACPStopReasonCancelled, Reason: runtimeDetail}},
			wantState: corev1alpha1.TaskExecutionStateCancelled, wantPhase: corev1alpha1.TaskPhaseCancelled, wantMsg: "prompt cancelled",
		},
		{
			name: "timeout", terminal: harnessv2.Event{Type: harnessv2.EventCancelled,
				Cancelled: &harnessv2.CancelledEvent{StopReason: harnessv2.ACPStopReasonCancelled, Reason: runtimeDetail}},
			cancel: harnessv2.CancelReasonTaskTimeout, wantState: corev1alpha1.TaskExecutionStateCancelled,
			wantPhase: corev1alpha1.TaskPhaseCancelled, wantMsg: acpTaskTimeoutCancellationSettledMessage,
		},
	} {
		for _, initialMessage := range []string{"", "runtime admission will be retried"} {
			initialName := "empty-message"
			if initialMessage != "" {
				initialName = "stale-message"
			}
			t.Run(test.name+"/"+initialName, func(t *testing.T) {
				ctx := context.Background()
				controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "failure-lifecycle.db"))
				defer closeStore()
				continuity := newACPSessionTestContinuity(t, controlStore, ACPBootstrapLimits{})
				control := ensureACPSessionForTest(t, continuity, fence, "failure-lifecycle")
				const taskUID, promptID = "failure-lifecycle-task", "failure-lifecycle-prompt"
				turn, attempt := openACPSessionTurnForTest(t, continuity, controlStore, fence, control, taskUID, promptID, "inspect pwd")
				for _, next := range []store.PromptExecutionState{
					store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
					store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
				} {
					operation := "failure-lifecycle-" + string(next)
					var err error
					attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
						ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
						NewState: next, OperationID: operation, OperationDigest: testControlDigestForDispatcher(operation),
						UpdatedAt: attempt.UpdatedAt.Add(time.Second),
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				task := &corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "failure-lifecycle", UID: types.UID(taskUID)},
					Spec: corev1alpha1.TaskSpec{
						Type: corev1alpha1.TaskTypeAgent, Prompt: "inspect pwd",
						SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName, Create: true, Append: true},
					},
					Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Message: initialMessage, Attempts: 1,
						Execution: &corev1alpha1.TaskExecutionStatus{
							State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: promptID,
							RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration,
							RequestDigest: attempt.RequestDigest,
						}},
				}
				kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
				dispatcher := ACPDispatcher{Client: kube, Store: controlStore, Sessions: continuity}
				session := &acpTaskSession{Turn: turn, Binding: ACPRuntimeSessionBinding{SessionUID: control.SessionUID}}
				if err := dispatcher.finishNonSuccessWithCancellationReason(ctx, task, attempt.ID, fence, session, test.terminal, test.cancel); err != nil {
					t.Fatal(err)
				}
				latest := &corev1alpha1.Task{}
				if err := kube.Get(ctx, clientObjectKey(task), latest); err != nil {
					t.Fatal(err)
				}
				if latest.Status.Phase != test.wantPhase || latest.Status.Execution.State != test.wantState || latest.Status.Execution.Message != test.wantMsg {
					t.Fatal("terminal execution classification differs from the safe controller-owned outcome")
				}
				if latest.Status.Message != test.wantMsg {
					t.Errorf("message before projection = %q, want %q", latest.Status.Message, test.wantMsg)
				}
				// The Task controller can observe the terminal phase before the
				// Session outbox runs. That event is immutable after emission.
				reconciler := TaskReconciler{Client: kube, ExecutionEventStore: controlStore}
				if !reconciler.recordTerminalTaskLifecycleEventIfMissing(ctx, latest) {
					t.Fatal("record Task terminal lifecycle before outbox delivery")
				}
				filter := store.ExecutionEventFilter{Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask,
					StreamID: task.Name, EventTypes: []string{executionEventTypeForTaskPhase(test.wantPhase)}, Limit: 10}
				before, err := controlStore.ListExecutionEvents(ctx, filter)
				if err != nil || len(before) != 1 {
					t.Fatalf("terminal event count=%d err=%v", len(before), err)
				}
				if before[0].Summary != test.wantMsg {
					t.Errorf("durable summary before projection = %q, want %q", before[0].Summary, test.wantMsg)
				}
				finalizedTurn, payload := deliverTerminalMessageProjectionForTest(t, kube, controlStore, turn.Turn.ID, test.wantMsg)
				if err := kube.Get(ctx, clientObjectKey(task), latest); err != nil {
					t.Fatal(err)
				}
				if latest.Status.Message != test.wantMsg {
					t.Errorf("message after projection = %q, want %q", latest.Status.Message, test.wantMsg)
				}
				if !reconciler.recordTerminalTaskLifecycleEventIfMissing(ctx, latest) {
					t.Fatal("repeat Task terminal lifecycle after outbox delivery")
				}
				after, err := controlStore.ListExecutionEvents(ctx, filter)
				if err != nil || len(after) != 1 || after[0].Seq != before[0].Seq {
					t.Fatalf("terminal event identity changed after projection: count=%d err=%v", len(after), err)
				}
				if after[0].Summary != test.wantMsg {
					t.Errorf("durable summary after projection = %q, want %q", after[0].Summary, test.wantMsg)
				}
				published, err := json.Marshal([]any{latest.Status, payload, finalizedTurn.TerminalContent, after})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(published), runtimeDetail) {
					t.Fatal("runtime diagnostic escaped the redacted journal into status or lifecycle data")
				}
				if after[0].Severity != events.ExecutionEventSeverityError && test.wantPhase == corev1alpha1.TaskPhaseFailed {
					t.Error("failed terminal event lost error severity")
				}
			})
		}
	}
}

func deliverTerminalMessageProjectionForTest(t *testing.T, kube client.Client, controlStore *sqlite.Store, turnID, wantMsg string) (*store.SessionTurn, taskTerminalProjection) {
	t.Helper()
	ctx := context.Background()
	finalizedTurn, err := controlStore.GetSessionTurn(ctx, turnID)
	if err != nil || finalizedTurn.State != store.SessionTurnFinalized {
		t.Fatalf("SessionTurn was not finalized: %v", err)
	}
	projection, err := controlStore.GetOutboxProjection(ctx, finalizedTurn.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message != wantMsg || payload.Execution.Message != wantMsg {
		t.Error("Session projection does not retain the safe terminal message")
	}
	projector := ACPOutboxProjector{Client: kube}
	if _, err := projector.deliver(ctx, *projection); err != nil {
		t.Fatal(err)
	}
	return finalizedTurn, payload
}
