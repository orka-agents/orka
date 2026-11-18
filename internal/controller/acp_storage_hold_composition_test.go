package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
)

// These tests show how the Task record hold composes with the storage rules
// below it: an unknown write outcome is never retried blindly or taken for
// success, a replay answered from an operation receipt alone does not stand
// in for the record the hold checks, and an installation hold takes
// precedence over every record hold.

func requireNoTaskStorageHold(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) {
	t.Helper()
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired); condition != nil &&
		condition.Status == metav1.ConditionTrue {
		t.Fatalf("Task was held: %#v", condition)
	}
}

func requireAttemptState(t *testing.T, fixture *recoveryFixture, want store.PromptExecutionState) *store.PromptAttempt {
	t.Helper()
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != want {
		t.Fatalf("attempt state = %s, want %s", attempt.ExecutionState, want)
	}
	return attempt
}

// A receipt write whose outcome is unknown leaves the attempt Running. The
// dispatcher does not write it again, and recovery after a takeover records
// the outcome as unknown: it neither holds the Task, because the settling
// transition never acknowledged a receipt, nor reports success.
func TestUnknownReceiptOutcomeIsRecoveredAsUnknownWithoutAHold(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := configureRecoveryJournalIdentity(t, fixture)
	appendRecoveryOpenPrompt(t, fixture, task)
	receipts := &lostResponseReceiptStore{Store: fixture.controlStore, lose: 1}
	fixture.dispatcher.ResultStore = receipts

	err := fixture.dispatcher.transitionAttemptToSettlingWithResult(fixture.ctx, task, fixture.attemptID, fixture.fence, []byte(settledTestResult))
	if !errors.Is(err, store.ErrOutcomeUnknown) {
		t.Fatalf("settling with a lost receipt response = %v, want ErrOutcomeUnknown", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionRunning)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	requireNoTaskStorageHold(t, fixture, task)
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ResultRef != nil {
		t.Fatalf("a Task whose receipt outcome was unknown published a result: %#v", updated.Status.ResultRef)
	}
	if receipts.saves != 1 {
		t.Fatalf("receipt writes = %d, want the one whose outcome was unknown", receipts.saves)
	}
}

// lostResponseResultStore commits a Task result and then reports its outcome
// unknown, the first lose times.
type lostResponseResultStore struct {
	*sqlite.Store
	lose  int
	saves int
}

func (s *lostResponseResultStore) SaveResult(ctx context.Context, namespace, taskName string, data []byte) error {
	s.saves++
	if err := s.Store.SaveResult(ctx, namespace, taskName, data); err != nil {
		return err
	}
	if s.lose > 0 {
		s.lose--
		return fmt.Errorf("%w: result response lost", store.ErrOutcomeUnknown)
	}
	return nil
}

// When recovery repairs the result from the receipt and that write's outcome
// is unknown, the pass ends without success or a hold. The next pass runs the
// write again as a new operation, which replaces the result with the same
// bytes, and settles the Task.
func TestReceiptRepairWithUnknownResultOutcomeSettlesOnTheNextPass(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, _ := settlingTaskWithReceipt(t, fixture)
	results := &lostResponseResultStore{Store: fixture.controlStore, lose: 1}
	fixture.dispatcher.ResultStore = results

	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrOutcomeUnknown) {
		t.Fatalf("recovery with a lost result response = %v, want ErrOutcomeUnknown", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSettling)
	requireNoTaskStorageHold(t, fixture, task)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery on the next pass: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSucceeded)
	result, err := fixture.controlStore.GetResult(fixture.ctx, task.Namespace, task.Name)
	if err != nil || string(result) != settledTestResult {
		t.Fatalf("repaired result = %q, %v", result, err)
	}
	if results.saves != 2 {
		t.Fatalf("result writes = %d, want the unknown one and the next pass", results.saves)
	}
}

// A receipt write replayed from its operation receipt reports success
// without reading the record again. Recovery checks the record itself, so a
// receipt lost after it was acknowledged still holds the Task.
func TestReplayedReceiptWriteDoesNotMaskALostReceipt(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, receipt, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)

	info, err := fixture.controlStore.Describe(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	envelope := storagetest.NewWriteEnvelope(t, info.DatasetID, fixture.fence, "SavePromptResultReceipt", *receipt)
	write := storagev1.WithWriteEnvelope(fixture.ctx, envelope)
	if err := fixture.controlStore.SavePromptResultReceipt(write, *receipt); err != nil {
		t.Fatalf("receipt write: %v", err)
	}
	loseResultReceipt(t, fixture, task)
	if err := fixture.controlStore.SavePromptResultReceipt(write, *receipt); err != nil {
		t.Fatalf("replayed receipt write = %v, want its original success", err)
	}
	if _, err := fixture.controlStore.GetPromptResultReceipt(fixture.ctx, fixture.attemptID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a replay wrote the receipt again: %v", err)
	}

	requireTaskHeld(t, fixture, task, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	})
}

// While the installation is held, recovery acts on no Task: it neither
// places nor releases a record hold, even one the Task's annotation already
// acknowledges. Once the installation hold clears, the acknowledged record
// hold is released and the outcome recorded as unknown.
func TestInstallationHoldPrecedesTaskRecordHolds(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
	acknowledgeTaskStorageHold(t, fixture, task, hold.id())

	fixture.dispatcher.Epochs.setHold(store.StorageRecoveryHold{
		ID: "installation-hold", Reason: store.StorageRecoveryWatermarkRegressed, Message: "test hold",
	})
	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrStorageRecoveryRequired) {
		t.Fatalf("recovery while the installation is held = %v, want the installation hold", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSettling)
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired); condition != nil {
		t.Fatalf("recovery changed the Task's record hold while the installation was held: %#v", condition)
	}

	fixture.dispatcher.Epochs.clearHold()
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery after the installation hold cleared: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryAcknowledgedReason)
}

// holdingReceiptStore raises an installation hold while recovery reads a
// result receipt, after the recovery pass started.
type holdingReceiptStore struct {
	*sqlite.Store
	epochs *ControllerEpochManager
}

func (s *holdingReceiptStore) GetPromptResultReceipt(ctx context.Context, attemptID string) (*store.PromptResultReceipt, error) {
	s.epochs.setHold(store.StorageRecoveryHold{
		ID: "installation-hold", Reason: store.StorageRecoveryWatermarkRegressed, Message: "test hold",
	})
	return s.Store.GetPromptResultReceipt(ctx, attemptID)
}

// An installation hold raised while a recovery pass is under way also keeps
// that pass from placing or releasing a Task record hold: the condition write
// is admitted only while storage is not held, also under the epoch mutation
// interlock of a control store that has one.
func TestInstallationHoldRaisedDuringRecoveryWritesNoTaskRecordHold(t *testing.T) {
	for _, test := range []struct {
		name         string
		acknowledged bool
		interlock    bool
	}{
		{name: "placing the record hold"},
		{name: "releasing the acknowledged record hold", acknowledged: true},
		{name: "placing the record hold under the interlock", interlock: true},
		{name: "releasing the acknowledged record hold under the interlock", acknowledged: true, interlock: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
			defer fixture.close(t)
			task, _, digest := settlingTaskWithReceipt(t, fixture)
			loseResultReceipt(t, fixture, task)
			if test.acknowledged {
				acknowledgeTaskStorageHold(t, fixture, task, taskStorageRecordHold{
					reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
					key: fixture.attemptID, expected: digest,
				}.id())
			}
			var interlock *taskStorageHoldInterlockStore
			if test.interlock {
				interlock = &taskStorageHoldInterlockStore{DurableControlStore: fixture.dispatcher.Store}
				fixture.dispatcher.Store = interlock
			}
			fixture.dispatcher.ResultStore = &holdingReceiptStore{Store: fixture.controlStore, epochs: fixture.dispatcher.Epochs}

			err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
			if _, ok := errors.AsType[*store.StorageHoldError](err); !ok {
				t.Fatalf("recovery after the installation hold was raised = %v, want the installation hold", err)
			}
			if interlock != nil && interlock.calls == 0 {
				t.Fatal("the condition write did not use the epoch mutation interlock")
			}
			requireAttemptState(t, fixture, store.PromptExecutionSettling)
			updated := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
				t.Fatal(err)
			}
			if condition := apiMeta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired); condition != nil {
				t.Fatalf("recovery changed the Task's record hold after the installation was held: %#v", condition)
			}
		})
	}
}
