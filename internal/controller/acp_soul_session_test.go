package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestACPConfigurationMismatchFailsDurablyInsteadOfCapacityRetry(t *testing.T) {
	ctx := context.Background()
	control, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "soul.db"))
	defer closeStore()
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "mismatch", UID: "mismatch-uid", Generation: 1}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, SessionRef: &corev1alpha1.SessionReference{Name: "existing"}}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateReserved, Attempt: 1, PromptID: "prompt-mismatch-uid-1", RequestDigest: testControlDigestForDispatcher("request"), ControllerEpoch: fence.Epoch}}}
	attempt, err := control.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{Key: store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}, RequestDigest: task.Status.Execution.RequestDigest}), fence)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = control.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState, NewState: store.PromptExecutionReserved, OperationID: "reserve-soul", OperationDigest: testControlDigestForDispatcher("reserve"), UpdatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	epoch, err := control.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	d := &ACPDispatcher{Client: c, Store: control, Epochs: &ControllerEpochManager{ready: ready, current: epoch}}
	if err := d.handleSessionPreparationError(ctx, task, attempt.ID, fence, errors.New("temporary lease contention")); err != nil {
		t.Fatal(err)
	}
	pending, err := control.GetPromptAttempt(ctx, attempt.ID)
	if err != nil || pending.ExecutionState != store.PromptExecutionReserved {
		t.Fatal("transient contention consumed the attempt")
	}

	attempt, err = control.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
		ID: pending.ID, Fence: fence, ExpectedVersion: pending.Version, ExpectedState: pending.ExecutionState,
		NewState: store.PromptExecutionSessionStarting, OperationID: "candidate-session", OperationDigest: testControlDigestForDispatcher("candidate-session"),
		RuntimeInstanceID: "runtime", SessionUID: "candidate", SessionLeaseGeneration: 1, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Store = failedSoulRollbackStore{DurableControlStore: control}
	cause := d.requeueSessionLeaseFailure(ctx, task, attempt.ID, fence, store.ErrSessionConfigurationMismatch)
	if !errors.Is(cause, errSessionAdmissionRollback) {
		t.Fatal("rollback failure lost its classification")
	}
	if err := d.handleSessionPreparationError(ctx, task, attempt.ID, fence, cause); err != nil {
		t.Fatal(err)
	}
	pending, err = control.GetPromptAttempt(ctx, attempt.ID)
	if err != nil || pending.ExecutionState != store.PromptExecutionSessionStarting || pending.SessionUID != "candidate" {
		t.Fatal("failed rollback was terminalized or lost its repair authority")
	}
	d.Store = control
	cause = d.requeueSessionLeaseFailure(ctx, task, attempt.ID, fence, store.ErrSessionConfigurationMismatch)
	if !errors.Is(cause, store.ErrSessionConfigurationMismatch) || errors.Is(cause, errSessionAdmissionRollback) {
		t.Fatal("successful rollback lost the permanent mismatch")
	}
	pending, err = control.GetPromptAttempt(ctx, attempt.ID)
	if err != nil || pending.ExecutionState != store.PromptExecutionReserved || pending.SessionUID != "" {
		t.Fatal("successful rollback retained candidate Session authority")
	}
	if err := d.handleSessionPreparationError(ctx, task, attempt.ID, fence, fmt.Errorf("lease acquisition: %w", cause)); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Task{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || updated.Status.Execution.Reason != "SessionConfigurationMismatch" {
		t.Fatal("permanent mismatch did not become an actionable Task failure")
	}
	failed, err := control.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ExecutionState != store.PromptExecutionFailed || failed.TerminalReason != "SessionConfigurationMismatch" {
		t.Fatal("failure was not durably recorded")
	}
}

type failedSoulRollbackStore struct{ store.DurableControlStore }

func (s failedSoulRollbackStore) RecoverPromptAttemptPreSubmission(context.Context, store.PromptAttemptPreSubmissionRecovery) (*store.PromptAttempt, error) {
	return nil, errors.New("temporary rollback storage failure")
}
