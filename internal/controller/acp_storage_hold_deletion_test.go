package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// heldDeletingTask holds the fixture's settling Task for its lost receipt,
// then marks the Task for deletion behind a finalizer, as a user deleting a
// held Task would. It returns the Task, the receipt and the hold.
func heldDeletingTask(t *testing.T, fixture *recoveryFixture) (*corev1alpha1.Task, *store.PromptResultReceipt, taskStorageRecordHold) {
	t.Helper()
	task, receipt, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
	requireTaskHeld(t, fixture, task, hold)

	latest := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), latest); err != nil {
		t.Fatal(err)
	}
	latest.Finalizers = append(latest.Finalizers, "orka.ai/storage-hold-test")
	if err := fixture.kubeClient.Update(fixture.ctx, latest); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Delete(fixture.ctx, latest); err != nil {
		t.Fatal(err)
	}
	deleting := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), deleting); err != nil {
		t.Fatal(err)
	}
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatal("the held Task is not marked for deletion")
	}
	return task, receipt, hold
}

// requireDeletingTaskCleanupProceeds checks that recovery handed the released
// deleting Task to the current epoch, whose terminal recovery owns its pending
// runtime cleanup, and that this recovery completes the cleanup the Task
// finalizer waits on.
func requireDeletingTaskCleanupProceeds(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) {
	t.Helper()
	released := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), released); err != nil {
		t.Fatal(err)
	}
	if released.Status.Execution == nil || released.Status.Execution.ControllerEpoch != fixture.fence.Epoch {
		t.Fatalf("released deleting Task execution = %#v, want controller epoch %d", released.Status.Execution, fixture.fence.Epoch)
	}
	if taskScopedRuntimeSessionCleanupComplete(released) {
		t.Fatal("the released deleting Task has no runtime cleanup left to prove")
	}
	if err := fixture.dispatcher.recoverStaleTask(fixture.ctx, released, fixture.fence); err != nil {
		t.Fatalf("current-epoch terminal recovery: %v", err)
	}
	cleaned := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), cleaned); err != nil {
		t.Fatal(err)
	}
	if !taskScopedRuntimeSessionCleanupComplete(cleaned) {
		t.Fatalf("released deleting Task runtime cleanup is still pending: %#v", cleaned.Status.Execution)
	}
}

// Acknowledging the hold of a Task already marked for deletion still records
// its outcome as unknown, so the finalizer's terminal attempt is reached.
func TestACPDispatcherReleasesAcknowledgedHoldOfADeletingTask(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, hold := heldDeletingTask(t, fixture)

	acknowledgeTaskStorageHold(t, fixture, task, hold.id())
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after acknowledgement: %v", err)
	}
	attempt := requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	if !store.IsTerminalPromptDeliveryState(attempt.DeliveryState) {
		t.Fatalf("acknowledged attempt delivery = %s, want terminal", attempt.DeliveryState)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown {
		t.Fatalf("acknowledged deleting Task execution = %#v, want OutcomeUnknown", updated.Status.Execution)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryAcknowledgedReason)
	requireDeletingTaskCleanupProceeds(t, fixture, task)
}

// A receipt restored after the held Task was marked for deletion releases the
// hold and settles the attempt from it, without running the prompt again.
func TestACPDispatcherReleasesRestoredHoldOfADeletingTask(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, receipt, _ := heldDeletingTask(t, fixture)

	if err := fixture.controlStore.SavePromptResultReceipt(fixture.ctx, *receipt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after restore: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSucceeded)
	result, err := fixture.controlStore.GetResult(fixture.ctx, task.Namespace, task.Name)
	if err != nil || string(result) != settledTestResult {
		t.Fatalf("restored result = %q, %v", result, err)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryRecordFoundReason)
	requireDeletingTaskCleanupProceeds(t, fixture, task)
}
