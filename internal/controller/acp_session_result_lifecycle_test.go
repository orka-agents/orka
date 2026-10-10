package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPSessionResultProjectionPreservesTerminalLifecycleMessage(t *testing.T) {
	const assistantResult = "fixture-assistant-result-must-not-become-status"
	for _, test := range []struct {
		name           string
		phase          corev1alpha1.TaskPhase
		deliveryPath   []store.PromptDeliveryState
		projectionText string
		directText     string
	}{
		{
			name: "succeeded", phase: corev1alpha1.TaskPhaseSucceeded,
			deliveryPath:   []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryReadValidated},
			projectionText: "ACP task completed", directText: "ACP task completed",
		},
		{
			name: "delivery-failed", phase: corev1alpha1.TaskPhaseFailed,
			deliveryPath:   []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryReadOnlyWorkspaceModified},
			projectionText: "ACP delivery failed", directText: "read-only workspace was modified",
		},
		{
			name: "publication-cancelled", phase: corev1alpha1.TaskPhaseCancelled,
			deliveryPath: []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryPreparing,
				store.PromptDeliveryPrepared, store.PromptDeliveryCancelledBeforePublish},
			projectionText: "publication cancelled before push", directText: "publication cancelled before push",
		},
	} {
		for _, recovered := range []bool{false, true} {
			producer := "live"
			if recovered {
				producer = "recovery"
			}
			for _, outboxFirst := range []bool{true, false} {
				order := "outbox-first"
				if !outboxFirst {
					order = "direct-status-first"
				}
				t.Run(test.name+"/"+producer+"/"+order, func(t *testing.T) {
					ctx := context.Background()
					controls, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "result-lifecycle.db"))
					defer closeStore()
					continuity := newACPSessionTestContinuity(t, controls, ACPBootstrapLimits{})
					control := ensureACPSessionForTest(t, continuity, fence, "result-lifecycle")
					turn, attempt := openACPSessionTurnForTest(t, continuity, controls, fence, control,
						"result-lifecycle-task", "result-lifecycle-prompt", "inspect pwd")
					attempt = completeACPAttemptExecutionForTest(t, controls, fence, attempt, false)
					for _, state := range test.deliveryPath {
						var err error
						attempt, err = controls.TransitionPromptAttemptDelivery(ctx, store.PromptAttemptDeliveryTransition{
							ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
							NewState: state, OperationID: "result-lifecycle-" + string(state),
							OperationDigest: acpSessionTestDigest(string(state)), UpdatedAt: attempt.UpdatedAt.Add(time.Second),
						})
						if err != nil {
							t.Fatal(err)
						}
					}
					delivery := corev1alpha1.TaskDeliveryStatus{
						State: corev1alpha1.TaskDeliveryState(attempt.DeliveryState), Outcome: corev1alpha1.TaskDeliveryOutcome(attempt.DeliveryState),
					}
					if test.phase != corev1alpha1.TaskPhaseSucceeded {
						delivery.Message = test.directText
					}
					// A fixed historical completionTime models an already-authoritative Task
					// status from an interrupted prior settlement pass. Neither status writer
					// in this matrix (queued outbox projection delivery, or the direct
					// terminal delivery helper) may replace it with a fresh value.
					historicalCompletion := metav1.NewTime(time.Date(2024, 9, 17, 8, 0, 0, 0, time.UTC))
					task := &corev1alpha1.Task{
						ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "result-lifecycle", UID: types.UID(attempt.Key.TaskUID)},
						Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "inspect pwd",
							SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName, Create: true, Append: true}},
						Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Message: "runtime admission will be retried", Attempts: 1,
							CompletionTime: &historicalCompletion,
							Execution: &corev1alpha1.TaskExecutionStatus{
								State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
								Attempt: 1, PromptID: attempt.Key.PromptID, RequestDigest: attempt.RequestDigest,
								RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration,
							}, Delivery: delivery.DeepCopy()},
					}
					publicationID := ""
					if test.phase == corev1alpha1.TaskPhaseCancelled {
						task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
							Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/orka/source.git",
							PushBranch: "restore",
						}
						task.Status.Execution.RuntimeInstanceID = "result-lifecycle-runtime"
						cleanup, err := taskScopedRuntimeSessionCleanupDigest(task.UID, 1,
							task.Status.Execution.RuntimeInstanceID, control.SessionUID, turn.Lease.Key.LeaseGeneration)
						if err != nil {
							t.Fatal(err)
						}
						task.Status.Execution.RuntimeSessionCleanupDigest = cleanup
						publication := createACPRecoveryPublication(t,
							&recoveryFixture{ctx: ctx, controlStore: controls, fence: fence}, task, store.PublicationPrepared)
						publication = transitionACPPublicationForTest(t, controls, fence, publication,
							store.PublicationCancelledBeforePublish, "result-lifecycle-cancel-publication", time.Now().UTC(),
							nil, nil, nil, test.directText)
						publicationID = publication.ID
					}
					if err := controls.SaveResult(ctx, task.Namespace, task.Name, []byte(assistantResult)); err != nil {
						t.Fatal(err)
					}
					kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
					dispatcher := ACPDispatcher{Client: kube, Store: controls, ResultStore: controls, Sessions: continuity}
					if recovered {
						if err := dispatcher.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
							t.Fatal(err)
						}
					} else {
						session := &acpTaskSession{Turn: turn, Binding: ACPRuntimeSessionBinding{SessionUID: control.SessionUID}}
						if err := dispatcher.finalizeTaskSessionResult(ctx, task, fence, session, assistantResult, publicationID, test.phase, delivery); err != nil {
							t.Fatal(err)
						}
					}
					finalized, err := controls.GetSessionTurn(ctx, turn.Turn.ID)
					if err != nil || finalized.State != store.SessionTurnFinalized {
						t.Fatalf("SessionTurn finalization failed: %v", err)
					}
					projection, err := controls.GetOutboxProjection(ctx, finalized.ProjectionID)
					if err != nil {
						t.Fatal(err)
					}
					var payload taskTerminalProjection
					if err := json.Unmarshal(projection.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if payload.Message != test.projectionText {
						t.Errorf("durable projection message = %q, want %q", payload.Message, test.projectionText)
					}
					if payload.Execution.State != corev1alpha1.TaskExecutionStateSucceeded || payload.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded || payload.Execution.Message != "" {
						t.Fatal("Session result projection changed successful execution classification or included execution text")
					}
					// The recovery sweep must replay the immutable projection, including
					// its message, when it finds an already finalized SessionTurn.
					if err := dispatcher.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
						t.Fatal(err)
					}
					replayed, err := controls.GetOutboxProjection(ctx, projection.ID)
					if err != nil || !bytes.Equal(replayed.Payload, projection.Payload) || replayed.PayloadDigest != projection.PayloadDigest {
						t.Fatalf("recovery changed the durable projection: %v", err)
					}
					projector := ACPOutboxProjector{Client: kube}
					project := func() error { _, err := projector.deliver(ctx, *projection); return err }
					patchDirect := func() error {
						switch test.phase {
						case corev1alpha1.TaskPhaseSucceeded:
							return dispatcher.completeSuccessWithDelivery(ctx, task, delivery, test.directText)
						case corev1alpha1.TaskPhaseFailed:
							return dispatcher.failTaskForDelivery(ctx, task, delivery, test.directText)
						default:
							return dispatcher.cancelTaskAfterExecution(ctx, task, delivery, test.directText)
						}
					}
					writes := []acpSessionResultLifecycleWrite{
						{apply: project, message: test.projectionText},
						{apply: patchDirect, message: test.directText},
					}
					if !outboxFirst {
						writes[0], writes[1] = writes[1], writes[0]
					}
					reconciler := TaskReconciler{Client: kube, ExecutionEventStore: controls}
					assertACPSessionResultLifecycleWrites(t, &reconciler, task, test.phase, payload, writes, assistantResult, historicalCompletion)
				})
			}
		}
	}
}

type acpSessionResultLifecycleWrite struct {
	apply   func() error
	message string
}

func assertACPSessionResultLifecycleWrites(
	t *testing.T,
	reconciler *TaskReconciler,
	task *corev1alpha1.Task,
	phase corev1alpha1.TaskPhase,
	payload taskTerminalProjection,
	writes []acpSessionResultLifecycleWrite,
	assistantResult string,
	wantCompletionTime metav1.Time,
) {
	t.Helper()
	ctx := context.Background()
	filter := store.ExecutionEventFilter{Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask,
		StreamID: task.Name, EventTypes: []string{executionEventTypeForTaskPhase(phase)}, Limit: 10}
	var firstSeq int64
	for index, write := range writes {
		if err := write.apply(); err != nil {
			t.Fatal(err)
		}
		latest := &corev1alpha1.Task{}
		if err := reconciler.Get(ctx, clientObjectKey(task), latest); err != nil {
			t.Fatal(err)
		}
		if latest.Status.Phase != phase || latest.Status.Message != write.message {
			t.Errorf("write %d: phase/message = %q/%q, want %q/%q", index, latest.Status.Phase, latest.Status.Message, phase, write.message)
		}
		if latest.Status.CompletionTime == nil || !latest.Status.CompletionTime.Time.Equal(wantCompletionTime.Time) {
			t.Errorf("write %d: completionTime = %v, want preserved historical value %v", index, latest.Status.CompletionTime, wantCompletionTime)
		}
		if !reconciler.recordTerminalTaskLifecycleEventIfMissing(ctx, latest) {
			t.Fatal("record terminal lifecycle event")
		}
		rows, err := reconciler.ExecutionEventStore.ListExecutionEvents(ctx, filter)
		if err != nil || len(rows) != 1 {
			t.Fatalf("terminal event count=%d error=%v", len(rows), err)
		}
		if rows[0].Summary != writes[0].message {
			t.Errorf("write %d: durable summary = %q, want %q", index, rows[0].Summary, writes[0].message)
		}
		if index == 0 {
			firstSeq = rows[0].Seq
		} else if rows[0].Seq != firstSeq {
			t.Error("terminal lifecycle event identity changed after the second status writer")
		}
		published, err := json.Marshal([]any{latest.Status, payload, rows})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(published), assistantResult) {
			t.Fatal("assistant result escaped into Task status, projection, or lifecycle event")
		}
	}
}
