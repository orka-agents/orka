package controller

import (
	"errors"
	"strings"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// settledTestResult is the result the hold tests settle with.
const settledTestResult = "the settled result"

// settlingTaskWithReceipt moves the fixture's attempt to Settling through the
// settling transition, which saves the result receipt first, and returns the
// Task, the receipt and the attempt's settling digest.
func settlingTaskWithReceipt(t *testing.T, fixture *recoveryFixture) (*corev1alpha1.Task, *store.PromptResultReceipt, string) {
	t.Helper()
	task := configureRecoveryJournalIdentity(t, fixture)
	appendRecoveryOpenPrompt(t, fixture, task)
	if err := fixture.dispatcher.transitionAttemptToSettlingWithResult(
		fixture.ctx, task, fixture.attemptID, fixture.fence, []byte(settledTestResult),
	); err != nil {
		t.Fatal(err)
	}
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateSettling
	if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.controlStore.GetPromptResultReceipt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	return task, receipt, attempt.LastOperationDigest
}

// loseResultReceipt removes the attempt's result receipt and result, as a
// store that lost acknowledged writes would.
func loseResultReceipt(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) {
	t.Helper()
	if err := fixture.controlStore.DeleteResult(fixture.ctx, task.Namespace, task.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlStore.GetPromptResultReceipt(fixture.ctx, fixture.attemptID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("result receipt after loss: %v", err)
	}
}

// requireTaskHeld checks that recovery held the Task: the attempt stays
// Settling, the Task reports neither success nor an unknown outcome, and its
// StorageRecoveryRequired condition names the hold.
func requireTaskHeld(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task, hold taskStorageRecordHold) {
	t.Helper()
	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery error = %v, want a storage recovery hold", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionSettling {
		t.Fatalf("held attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionSettling)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.State != corev1alpha1.TaskExecutionStateSettling {
		t.Fatalf("held Task execution = %#v, want Settling", updated.Status.Execution)
	}
	if updated.Status.ResultRef != nil {
		t.Fatalf("held Task published a result: %#v", updated.Status.ResultRef)
	}
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != hold.reason ||
		!strings.Contains(condition.Message, hold.id()) || !strings.Contains(condition.Message, fixture.attemptID) {
		t.Fatalf("held Task condition = %#v, want %s naming hold %s", condition, hold.reason, hold.id())
	}
	if _, err := fixture.controlStore.GetResult(fixture.ctx, task.Namespace, task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("held Task result read = %v, want none", err)
	}
}

func acknowledgeTaskStorageHold(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task, id string) {
	t.Helper()
	latest := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), latest); err != nil {
		t.Fatal(err)
	}
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = id
	if err := fixture.kubeClient.Update(fixture.ctx, latest); err != nil {
		t.Fatal(err)
	}
}

func requireTaskStorageCondition(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task, status metav1.ConditionStatus, reason string) {
	t.Helper()
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Fatalf("Task storage condition = %#v, want %s/%s", condition, status, reason)
	}
}

// A settling attempt whose result receipt the store lost holds the Task: the
// prompt is not run again, the outcome is not guessed and success is not
// reported, until the operator acknowledges the exact hold, after which the
// outcome is recorded as unknown.
func TestACPDispatcherHoldsSettlingTaskWhoseResultReceiptIsMissing(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}

	requireTaskHeld(t, fixture, task, hold)
	requireTaskHeld(t, fixture, task, hold) // Recovery runs again and stays held.

	acknowledgeTaskStorageHold(t, fixture, task, "record-0000000000000000")
	requireTaskHeld(t, fixture, task, hold) // Another hold's ID releases nothing.

	acknowledgeTaskStorageHold(t, fixture, task, hold.id())
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after acknowledgement: %v", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown {
		t.Fatalf("acknowledged attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionOutcomeUnknown)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown {
		t.Fatalf("acknowledged Task execution = %#v, want OutcomeUnknown", updated.Status.Execution)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryAcknowledgedReason)
}

// A receipt found again after the store was recovered releases the hold, and
// recovery settles the Task from it as it would have without the loss.
func TestACPDispatcherReleasesTaskHoldWhenResultReceiptIsRestored(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, receipt, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	requireTaskHeld(t, fixture, task, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	})

	if err := fixture.controlStore.SavePromptResultReceipt(fixture.ctx, *receipt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after restore: %v", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionSucceeded {
		t.Fatalf("restored attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionSucceeded)
	}
	result, err := fixture.controlStore.GetResult(fixture.ctx, task.Namespace, task.Name)
	if err != nil || string(result) != settledTestResult {
		t.Fatalf("restored result = %q, %v", result, err)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryRecordFoundReason)
}

// A receipt that differs from what the attempt settled with is a conflicting
// record, and recovery holds the Task instead of choosing either result.
func TestACPDispatcherHoldsSettlingTaskWhoseResultReceiptConflicts(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, receipt, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	other := []byte("a result the attempt never settled with")
	otherDigest, err := acpSettlingTransitionDigest(fixture.attemptID, attempt.Version-1, other)
	if err != nil {
		t.Fatal(err)
	}
	conflicting := *receipt
	conflicting.Data, conflicting.OperationDigest = other, otherDigest
	if err := fixture.controlStore.SavePromptResultReceipt(fixture.ctx, conflicting); err != nil {
		t.Fatal(err)
	}

	requireTaskHeld(t, fixture, task, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryConflictingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	})
}

// A journaled completion does not stand in for a lost receipt: the result the
// receipt protected may be gone too, or belong to something else.
func TestACPDispatcherHoldsJournaledCompletionWhoseResultReceiptIsMissing(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	appendRecoveryPromptTerminal(t, fixture, task, harnessv2.EventCompleted, false)
	loseResultReceipt(t, fixture, task)
	if err := fixture.controlStore.SaveResult(fixture.ctx, task.Namespace, task.Name, []byte("a result from elsewhere")); err != nil {
		t.Fatal(err)
	}
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery error = %v, want a storage recovery hold", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionSettling {
		t.Fatalf("held attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionSettling)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, hold.reason)

	acknowledgeTaskStorageHold(t, fixture, task, hold.id())
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after acknowledgement: %v", err)
	}
	attempt, err = fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown {
		t.Fatalf("acknowledged attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionOutcomeUnknown)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ResultRef != nil {
		t.Fatalf("acknowledged Task published the unverified result: %#v", updated.Status.ResultRef)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryAcknowledgedReason)
}

// A restore that changed the Task's UID recovers the source attempt through
// another path, which holds the restored Task the same way.
func TestACPDispatcherHoldsRestoredTaskWhoseResultReceiptIsMissing(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	_, restoredUID := restoreACPRecoveryFixtureTask(t, fixture)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
	requireTaskHeld(t, fixture, task, hold)

	acknowledgeTaskStorageHold(t, fixture, task, hold.id())
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after acknowledgement: %v", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown || attempt.TerminalReason != acpRestoreIdentityChangedReason {
		t.Fatalf("acknowledged restored attempt = %s/%s, want OutcomeUnknown/%s",
			attempt.ExecutionState, attempt.TerminalReason, acpRestoreIdentityChangedReason)
	}
	restored := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), restored); err != nil {
		t.Fatal(err)
	}
	if restored.UID != restoredUID || restored.Status.Execution == nil ||
		restored.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeOutcomeUnknown || restored.Status.ResultRef != nil {
		t.Fatalf("acknowledged restored Task status = %#v", restored.Status)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryAcknowledgedReason)
}

// An attempt that reached Settling without the settling transition never
// had a result receipt, so its absence holds nothing and recovery records the
// outcome as unknown, as before.
func TestACPDispatcherDoesNotHoldSettlingAttemptWithoutReceiptTransition(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionSettling)
	defer fixture.close(t)
	task := configureRecoveryJournalIdentity(t, fixture)
	appendRecoveryOpenPrompt(t, fixture, task)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown {
		t.Fatalf("attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionOutcomeUnknown)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired); condition != nil {
		t.Fatalf("Task without a receipt transition was held: %#v", condition)
	}
}

// Hold IDs are derived from the evidence: the same evidence gives the same
// ID, so an acknowledgement keeps applying, and other evidence another one.
func TestTaskStorageRecordHoldIDIsStableAndSpecific(t *testing.T) {
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: "attempt-1", expected: "sha256:" + strings.Repeat("a", 64),
	}
	same := hold
	if hold.id() != same.id() || !strings.HasPrefix(hold.id(), "record-") {
		t.Fatalf("hold ID = %q, same evidence %q", hold.id(), same.id())
	}
	for _, other := range []taskStorageRecordHold{
		{reason: corev1alpha1.StorageRecoveryConflictingRecordReason, record: hold.record, key: hold.key, expected: hold.expected},
		{reason: hold.reason, record: hold.record, key: "attempt-2", expected: hold.expected},
		{reason: hold.reason, record: hold.record, key: hold.key, expected: "sha256:" + strings.Repeat("b", 64)},
	} {
		if other.id() == hold.id() {
			t.Fatalf("holds %#v and %#v share ID %s", hold, other, hold.id())
		}
	}
	if err := (&taskStorageRecordHoldError{hold: hold}); !errors.Is(err, store.ErrStorageRecoveryRequired) || !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("hold error %v matches neither the hold nor ErrNotReady", err)
	}
}
