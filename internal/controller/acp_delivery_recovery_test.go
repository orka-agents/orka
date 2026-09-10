package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPDispatcherRecoversPublicationConflictPhase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deleting bool
		stale    bool
	}{
		{name: "current epoch"},
		{name: "deleting current epoch", deleting: true},
		{name: "deleting after restart", deleting: true, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionSucceeded)
			defer fixture.close(t)
			task := preparePublicationConflictRecovery(t, fixture, tc.stale)
			if tc.deleting {
				task = markRestoredACPRecoveryTaskDeleting(t, fixture, func(*corev1alpha1.Task) {})
			}
			reconciler := &TaskReconciler{Client: fixture.kubeClient, DurableControlStore: fixture.controlStore}
			if ready, err := reconciler.acpTaskDeletionReady(fixture.ctx, task); err != nil || ready {
				t.Fatalf("missing projection deletion barrier: ready=%v err=%v", ready, err)
			}
			if tc.stale {
				if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				fixture.dispatcher.sem = make(chan struct{}, 1)
				fixture.dispatcher.active = make(map[types.UID]struct{})
				if err := fixture.dispatcher.scheduleACPDeliveryRecoveries(fixture.ctx, []corev1alpha1.Task{*task}); err != nil {
					t.Fatal(err)
				}
			}
			if err := wait.PollUntilContextTimeout(fixture.ctx, 10*time.Millisecond, time.Second, true, func(ctx context.Context) (bool, error) {
				if err := fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
					return false, err
				}
				return task.Status.Phase == corev1alpha1.TaskPhaseFailed, nil
			}); err != nil {
				t.Fatalf("publication conflict remained nonterminal: status=%#v err=%v", task.Status, err)
			}
			if task.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded ||
				task.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeDeliveryConflict ||
				!taskScopedRuntimeSessionCleanupComplete(task) {
				t.Fatalf("recovery changed terminal evidence: %#v", task.Status)
			}
			projection, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, standaloneTaskTerminalProjectionID(task, 1))
			if err != nil {
				t.Fatal(err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Phase != corev1alpha1.TaskPhaseFailed || payload.TaskUID != string(task.UID) ||
				payload.Execution.RuntimeSessionCleanupDigest != task.Status.Execution.RuntimeSessionCleanupDigest {
				t.Fatalf("recovered projection lost terminal identity: %#v", payload)
			}
			if ready, err := reconciler.acpTaskDeletionReady(fixture.ctx, task); err != nil || ready {
				t.Fatalf("undelivered projection deletion barrier: ready=%v err=%v", ready, err)
			}
			projector := &ACPOutboxProjector{Client: fixture.kubeClient, Store: fixture.controlStore, Epochs: fixture.dispatcher.Epochs, WorkerID: "conflict-recovery"}
			if err := projector.projectOnce(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			if err := fixture.kubeClient.Get(fixture.ctx, client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			if ready, err := reconciler.acpTaskDeletionReady(fixture.ctx, task); err != nil || !ready {
				t.Fatalf("settled conflict could not pass deletion barriers: ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestACPDispatcherDeletingRecoveryRequiresTerminalCleanupProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  store.PromptExecutionState
		mutate func(*corev1alpha1.Task)
	}{
		{name: "queued prompt", state: store.PromptExecutionQueued},
		{name: "running prompt", state: store.PromptExecutionRunning},
		{name: "missing cleanup", state: store.PromptExecutionSucceeded, mutate: func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionCleanupDigest = "" }},
		{name: "different runtime generation", state: store.PromptExecutionSucceeded, mutate: func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionGeneration++ }},
		{name: "session task", state: store.PromptExecutionSucceeded, mutate: func(task *corev1alpha1.Task) { task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, tc.state)
			defer fixture.close(t)
			if tc.state == store.PromptExecutionSucceeded {
				preparePublicationConflictRecovery(t, fixture, true)
			}
			task := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(task)
				if err := fixture.kubeClient.Update(fixture.ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			task = markRestoredACPRecoveryTaskDeleting(t, fixture, func(task *corev1alpha1.Task) {
				if tc.mutate != nil {
					tc.mutate(task)
				}
			})
			attemptBefore, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			attemptAfter, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attemptAfter.Version != attemptBefore.Version || attemptAfter.ExecutionState != attemptBefore.ExecutionState {
				t.Fatal("deleting attempt was replayed or changed without terminal cleanup proof")
			}
			if _, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, standaloneTaskTerminalProjectionID(task, 1)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("created projection without terminal cleanup proof: %v", err)
			}
		})
	}
}

func preparePublicationConflictRecovery(t *testing.T, fixture *recoveryFixture, stale bool) *corev1alpha1.Task {
	t.Helper()
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryConflict} {
		attempt, err = fixture.controlStore.TransitionPromptAttemptDelivery(fixture.ctx, store.PromptAttemptDeliveryTransition{
			ID: attempt.ID, Fence: fixture.fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
			NewState: state, OperationID: "conflict-" + string(state), OperationDigest: testControlDigestForDispatcher(string(state)), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/example/repo.git"}
	if err := fixture.kubeClient.Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeSucceeded
	task.Status.Execution.RuntimeInstanceID = acpRecoveryRuntimeInstanceID
	task.Status.Execution.RuntimeSessionUID = acpRecoveryRuntimeSessionUID
	task.Status.Execution.RuntimeSessionGeneration = 1
	task.Status.Execution.RuntimeSessionCleanupDigest, err = taskScopedRuntimeSessionCleanupDigest(task.UID, 1, acpRecoveryRuntimeInstanceID, acpRecoveryRuntimeSessionUID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		task.Status.Execution.ControllerEpoch = fixture.fence.Epoch
	}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
		Reason: "PublicationFailed", Message: "publication branch is already claimed by a different owner",
	}
	if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.controlStore.SaveResult(fixture.ctx, task.Namespace, task.Name, []byte("completed code change")); err != nil {
		t.Fatal(err)
	}
	return task
}
