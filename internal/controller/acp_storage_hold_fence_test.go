package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// heldTaskObservation prepares a Task whose result receipt the store lost and
// returns what a recovery pass under fence reads before it holds the Task.
func heldTaskObservation(
	t *testing.T, fixture *recoveryFixture, fence store.ControllerEpochFence,
) (*corev1alpha1.Task, taskStorageRecordObservation, taskStorageRecordHold) {
	t.Helper()
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	return task, taskStorageRecordObservation{fence: fence, attempt: *attempt}, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
}

// requireHoldRefused checks that writing hold for observed is refused as a
// conflict, not reported as a hold, and leaves the Task without one.
func requireHoldRefused(
	t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task,
	observed taskStorageRecordObservation, hold taskStorageRecordHold,
) {
	t.Helper()
	err := fixture.dispatcher.settleTaskStorageRecordHold(fixture.ctx, task, observed, hold)
	if !errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("hold write error = %v, want a conflict and no hold", err)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if apiMeta.IsStatusConditionTrue(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) {
		t.Fatalf("Task was held from a stale observation: %#v", updated.Status.Conditions)
	}
}

// A pass that read the attempt before a successor settled it does not hold
// the settled Task.
func TestTaskStorageHoldIsNotWrittenAfterTheAttemptMovedOn(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, observed, hold := heldTaskObservation(t, fixture, fixture.fence)

	if _, err := fixture.controlStore.TransitionPromptAttemptExecution(fixture.ctx, store.PromptAttemptExecutionTransition{
		ID: fixture.attemptID, Fence: fixture.fence, ExpectedVersion: observed.attempt.Version,
		ExpectedState: store.PromptExecutionSettling, NewState: store.PromptExecutionSucceeded,
		OperationID: acpSucceededOperation + "-successor", OperationDigest: testControlDigestForDispatcher("successor-settled"),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	settled := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), settled); err != nil {
		t.Fatal(err)
	}
	settled.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	settled.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
	if err := fixture.kubeClient.Status().Update(fixture.ctx, settled); err != nil {
		t.Fatal(err)
	}

	requireHoldRefused(t, fixture, task, observed, hold)
}

// A controller whose epoch was taken over does not hold the Task, even though
// the Task and its attempt are as it read them.
func TestTaskStorageHoldIsNotWrittenByAReplacedController(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, observed, hold := heldTaskObservation(t, fixture, fixture.fence)
	replaced := store.ControllerEpochFence{
		Name: fixture.fence.Name, Epoch: task.Status.Execution.ControllerEpoch, HolderID: "controller-old",
	}
	if replaced.Epoch >= fixture.fence.Epoch {
		t.Fatalf("replaced epoch %d is not older than the current epoch %d", replaced.Epoch, fixture.fence.Epoch)
	}
	observed.fence = replaced

	requireHoldRefused(t, fixture, task, observed, hold)
}

// A Task that a later controller epoch already recovered is not held from an
// earlier epoch's reading.
func TestTaskStorageHoldIsNotWrittenAfterALaterEpochRecoveredTheTask(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, observed, hold := heldTaskObservation(t, fixture, fixture.fence)
	recovered := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), recovered); err != nil {
		t.Fatal(err)
	}
	recovered.Status.Execution.ControllerEpoch = fixture.fence.Epoch + 1
	if err := fixture.kubeClient.Status().Update(fixture.ctx, recovered); err != nil {
		t.Fatal(err)
	}

	requireHoldRefused(t, fixture, task, observed, hold)
}

// taskStorageHoldInterlockStore stands in for a control store with the
// controller epoch mutation interlock, which refuses a fence that a takeover
// replaced.
type taskStorageHoldInterlockStore struct {
	store.DurableControlStore
	takenOver bool
	calls     int
}

func (s *taskStorageHoldInterlockStore) WithControllerEpochMutation(
	ctx context.Context, _ store.ControllerEpochFence, write func(context.Context) error,
) error {
	s.calls++
	if s.takenOver {
		return store.ConflictErrorf("controller epoch was taken over")
	}
	return write(ctx)
}

// With a control store that has the epoch mutation interlock, the hold is
// written under it, so a takeover the interlock reports refuses the write.
func TestTaskStorageHoldIsWrittenUnderTheEpochMutationInterlock(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, observed, hold := heldTaskObservation(t, fixture, fixture.fence)
	interlock := &taskStorageHoldInterlockStore{DurableControlStore: fixture.dispatcher.Store, takenOver: true}
	fixture.dispatcher.Store = interlock

	requireHoldRefused(t, fixture, task, observed, hold)
	if interlock.calls == 0 {
		t.Fatal("the hold write did not use the epoch mutation interlock")
	}

	interlock.takenOver = false
	if err := fixture.dispatcher.settleTaskStorageRecordHold(fixture.ctx, task, observed, hold); !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("hold write error = %v, want the hold", err)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, corev1alpha1.StorageRecoveryMissingRecordReason)
}
