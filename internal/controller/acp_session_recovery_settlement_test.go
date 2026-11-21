package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// A recovery pass that reaches a Session Task after its live settlement
// finished must keep that settlement: the finalized SessionTurn's projection
// is the durable record, as a standalone projection is for a Task without a
// Session. Rewriting the Task would report a successful turn as one whose
// live settlement was interrupted.
func TestACPRecoveryKeepsALiveSessionSettlement(t *testing.T) {
	for _, test := range []struct {
		name string
		// projected is the delivery state the live settlement projected.
		projected store.PromptDeliveryState
		// phase is the Task's phase when recovery reaches it.
		phase corev1alpha1.TaskPhase
		kept  bool
	}{
		{name: "matching", projected: store.PromptDeliveryReadValidated, phase: corev1alpha1.TaskPhaseSucceeded, kept: true},
		{name: "projection-differs", projected: store.PromptDeliveryNoChange, phase: corev1alpha1.TaskPhaseSucceeded, kept: false},
		// The projection has not reached the Task yet: recovery settles it.
		{name: "task-not-settled", projected: store.PromptDeliveryReadValidated, phase: corev1alpha1.TaskPhaseRunning, kept: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			controls, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "recovery-settlement.db"))
			defer closeStore()
			continuity := newACPSessionTestContinuity(t, controls, ACPBootstrapLimits{})
			control := ensureACPSessionForTest(t, continuity, fence, "recovery-settlement")
			turn, attempt := openACPSessionTurnForTest(t, continuity, controls, fence, control,
				"recovery-settlement-task", "recovery-settlement-prompt", "inspect pwd")
			attempt = completeACPAttemptExecutionForTest(t, controls, fence, attempt, false)
			for _, state := range []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryReadValidated} {
				var err error
				attempt, err = controls.TransitionPromptAttemptDelivery(ctx, store.PromptAttemptDeliveryTransition{
					ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
					NewState: state, OperationID: "recovery-settlement-" + string(state),
					OperationDigest: acpSessionTestDigest(string(state)), UpdatedAt: attempt.UpdatedAt.Add(time.Second),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			settledAt := metav1.NewTime(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC))
			delivery := corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryState(attempt.DeliveryState), Outcome: corev1alpha1.TaskDeliveryOutcome(attempt.DeliveryState),
				LastTransitionTime: &settledAt,
			}
			// The Task as the live settlement left it.
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "recovery-settlement", UID: types.UID(attempt.Key.TaskUID)},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "inspect pwd",
					SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName, Create: true, Append: true}},
				Status: corev1alpha1.TaskStatus{Phase: test.phase, Message: "ACP task completed", Attempts: 1,
					CompletionTime: &settledAt,
					Execution: &corev1alpha1.TaskExecutionStatus{
						State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
						Attempt: 1, PromptID: attempt.Key.PromptID, RequestDigest: attempt.RequestDigest,
						RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration,
						LastTransitionTime: &settledAt,
					}, Delivery: delivery.DeepCopy()},
			}
			if err := controls.SaveResult(ctx, task.Namespace, task.Name, []byte("assistant result")); err != nil {
				t.Fatal(err)
			}
			kube := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
			dispatcher := ACPDispatcher{Client: kube, Store: controls, ResultStore: controls, Sessions: continuity}
			projected := delivery
			projected.State = corev1alpha1.TaskDeliveryState(test.projected)
			projected.Outcome = corev1alpha1.TaskDeliveryOutcome(test.projected)
			session := &acpTaskSession{Turn: turn, Binding: ACPRuntimeSessionBinding{SessionUID: control.SessionUID}}
			if err := dispatcher.finalizeTaskSessionResult(ctx, task, fence, session, "assistant result", "",
				corev1alpha1.TaskPhaseSucceeded, projected); err != nil {
				t.Fatal(err)
			}

			kept, err := dispatcher.validateExistingStandaloneTaskProjection(ctx, task, attempt)
			if err != nil {
				t.Fatal(err)
			}
			if kept != test.kept {
				t.Fatalf("finalized SessionTurn projection taken as the settlement = %v, want %v", kept, test.kept)
			}
			if err := dispatcher.patchRecoveredTerminalExecution(ctx, task, attempt, fence.Epoch+1); err != nil {
				t.Fatal(err)
			}
			latest := &corev1alpha1.Task{}
			if err := kube.Get(ctx, clientObjectKey(task), latest); err != nil {
				t.Fatal(err)
			}
			if !test.kept {
				if latest.Status.Message == "ACP task completed" || latest.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
					t.Fatalf("recovery left the Task %s with %q instead of settling it from the attempt", latest.Status.Phase, latest.Status.Message)
				}
				return
			}
			if latest.Status.Message != "ACP task completed" {
				t.Fatalf("recovery rewrote the live settlement's message to %q", latest.Status.Message)
			}
			if !latest.Status.Execution.LastTransitionTime.Equal(&settledAt) || !latest.Status.Delivery.LastTransitionTime.Equal(&settledAt) {
				t.Fatal("recovery moved the live settlement's transition times")
			}
		})
	}
}
