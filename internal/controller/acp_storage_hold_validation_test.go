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

// settlingTaskInWorkspaceValidation settles the fixture's attempt as the live
// path does up to workspace validation: the receipt and the attempt's move to
// Settling, the result, the journaled completion, and the delivery moved to
// Validating, which records a later operation on the attempt.
func settlingTaskInWorkspaceValidation(t *testing.T, fixture *recoveryFixture) *corev1alpha1.Task {
	t.Helper()
	task, _, _ := settlingTaskWithReceipt(t, fixture)
	if err := fixture.controlStore.SaveResult(fixture.ctx, task.Namespace, task.Name, []byte(settledTestResult)); err != nil {
		t.Fatal(err)
	}
	appendRecoveryPromptTerminal(t, fixture, task, harnessv2.EventCompleted, false)
	if err := fixture.dispatcher.transitionDelivery(
		fixture.ctx, fixture.attemptID, fixture.fence, store.PromptDeliveryNotRequested, store.PromptDeliveryValidating, "validate-workspace", "",
	); err != nil {
		t.Fatal(err)
	}
	attempt := requireAttemptState(t, fixture, store.PromptExecutionSettling)
	if attempt.LastOperationID == acpSettlingOperationID(attempt.Version-1) {
		t.Fatal("workspace validation did not record a later operation on the attempt")
	}
	return task
}

// Workspace validation records a later operation on the settling attempt, so
// the attempt no longer names its settling operation. The receipt is still
// required: when it is lost, even a result and a journaled completion that
// survived do not settle the Task.
func TestACPDispatcherHoldsTaskWhoseReceiptIsLostDuringWorkspaceValidation(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := settlingTaskInWorkspaceValidation(t, fixture)
	if err := fixture.controlStore.DeleteResult(fixture.ctx, task.Namespace, task.Name); err != nil {
		t.Fatal(err)
	}
	if err := fixture.controlStore.SaveResult(fixture.ctx, task.Namespace, task.Name, []byte(settledTestResult)); err != nil {
		t.Fatal(err)
	}
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord, key: fixture.attemptID,
	}

	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery error = %v, want a storage recovery hold", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSettling)
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, hold.reason)
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ResultRef != nil {
		t.Fatalf("a Task whose receipt was lost published a result: %#v", updated.Status.ResultRef)
	}
	if condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired); condition == nil ||
		!strings.Contains(condition.Message, hold.id()) {
		t.Fatalf("held Task condition = %#v, want hold %s", condition, hold.id())
	}
}

// With its receipt, an attempt in workspace validation recovers as it did
// before record holds: the receipt checks against its own settling operation.
func TestACPDispatcherRecoversWorkspaceValidationWithItsReceipt(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := settlingTaskInWorkspaceValidation(t, fixture)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSucceeded)
	requireNoTaskStorageHold(t, fixture, task)
}

// A receipt whose own settling operation does not match its data conflicts,
// also once the attempt records a later operation.
func TestACPDispatcherHoldsWorkspaceValidationWithAConflictingReceipt(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := settlingTaskInWorkspaceValidation(t, fixture)
	receipt, err := fixture.controlStore.GetPromptResultReceipt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.controlStore.DeleteResult(fixture.ctx, task.Namespace, task.Name); err != nil {
		t.Fatal(err)
	}
	conflicting := *receipt
	conflicting.Data = []byte("a result the attempt never settled with")
	if err := fixture.controlStore.SavePromptResultReceipt(fixture.ctx, conflicting); err != nil {
		t.Fatal(err)
	}

	err = fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery error = %v, want a storage recovery hold", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSettling)
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, corev1alpha1.StorageRecoveryConflictingRecordReason)
}

// A restore that changed the Task's UID, with the prompt's completion
// journaled and its receipt lost, holds the restored Task too; acknowledging
// the hold records the outcome as unknown.
func TestACPDispatcherHoldsRestoredJournaledCompletionWhoseReceiptIsMissing(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	appendRecoveryPromptTerminal(t, fixture, task, harnessv2.EventCompleted, false)
	loseResultReceipt(t, fixture, task)
	restoreACPRecoveryFixtureTask(t, fixture)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
	requireTaskHeld(t, fixture, task, hold)

	acknowledgeTaskStorageHold(t, fixture, task, hold.id())
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after acknowledgement: %v", err)
	}
	attempt := requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	if attempt.TerminalReason != acpRestoreIdentityChangedReason {
		t.Fatalf("acknowledged restored attempt reason = %q, want %q", attempt.TerminalReason, acpRestoreIdentityChangedReason)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryAcknowledgedReason)
}
