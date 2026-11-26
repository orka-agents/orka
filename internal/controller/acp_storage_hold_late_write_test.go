package controller

import (
	"context"
	"errors"
	"sync"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func interceptLateHold(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task, hold taskStorageRecordHold) {
	t.Helper()
	var once sync.Once
	lateHold := func(ctx context.Context) error {
		var err error
		once.Do(func() {
			latest := &corev1alpha1.Task{}
			if err = fixture.kubeClient.Get(ctx, clientObjectKey(task), latest); err != nil {
				return
			}
			apiMeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionTrue,
				Reason: hold.reason, Message: hold.message(),
			})
			err = fixture.kubeClient.Status().Update(ctx, latest)
		})
		return err
	}
	fixture.dispatcher.Client = interceptor.NewClient(fixture.kubeClient.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); isTask {
				if err := lateHold(ctx); err != nil {
					return err
				}
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); isTask {
				if err := lateHold(ctx); err != nil {
					return err
				}
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	})
}

func TestTerminalRecoveryClearsALateHoldFromAReplacedController(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	interceptLateHold(t, fixture, task, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	})

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionSucceeded)
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution == nil || updated.Status.Execution.ControllerEpoch != fixture.fence.Epoch {
		t.Fatalf("settled Task execution = %#v, want controller epoch %d", updated.Status.Execution, fixture.fence.Epoch)
	}
	if apiMeta.IsStatusConditionTrue(updated.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) {
		t.Fatalf("the settled Task kept a late hold: %#v", updated.Status.Conditions)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryRecordFoundReason)
}

func settlingTaskInWorkspaceValidationWithoutCompletion(t *testing.T, fixture *recoveryFixture) *corev1alpha1.Task {
	t.Helper()
	task, _, _ := settlingTaskWithReceipt(t, fixture)
	if err := fixture.controlStore.SaveResult(fixture.ctx, task.Namespace, task.Name, []byte(settledTestResult)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.dispatcher.transitionDelivery(
		fixture.ctx, fixture.attemptID, fixture.fence, store.PromptDeliveryNotRequested, store.PromptDeliveryValidating, "validate-workspace", "",
	); err != nil {
		t.Fatal(err)
	}
	return task
}

func recoverUntilTaskEpoch(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	updated := &corev1alpha1.Task{}
	for range 4 {
		if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
			t.Fatalf("recovery: %v", err)
		}
		if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
			t.Fatal(err)
		}
		if updated.Status.Execution != nil && updated.Status.Execution.ControllerEpoch == fixture.fence.Epoch {
			return updated
		}
	}
	t.Fatalf("Task execution = %#v, want controller epoch %d", updated.Status.Execution, fixture.fence.Epoch)
	return nil
}

func TestTerminalRecoveryClearsALateHoldAfterRecordingAnUnknownOutcome(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := settlingTaskInWorkspaceValidationWithoutCompletion(t, fixture)
	interceptLateHold(t, fixture, task, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord, key: fixture.attemptID,
	})

	updated := recoverUntilTaskEpoch(t, fixture, task)
	requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	if updated.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown {
		t.Fatalf("Task execution state = %s, want %s", updated.Status.Execution.State, corev1alpha1.TaskExecutionStateOutcomeUnknown)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryRecordFoundReason)
}

func TestTerminalEpochWriteKeepsALateHoldWhoseReceiptIsMissing(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := settlingTaskInWorkspaceValidationWithoutCompletion(t, fixture)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord, key: fixture.attemptID,
	}
	interceptLateHold(t, fixture, task, hold)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	loseResultReceipt(t, fixture, task)

	recoverUntilTaskEpoch(t, fixture, task)
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, hold.reason)
}

func TestTerminalEpochWriteReleasesALateHoldOnlyWhileStorageIsNotHeld(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := settlingTaskInWorkspaceValidationWithoutCompletion(t, fixture)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord, key: fixture.attemptID,
	}
	interceptLateHold(t, fixture, task, hold)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	requireAttemptState(t, fixture, store.PromptExecutionOutcomeUnknown)
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, hold.reason)

	fixture.dispatcher.ResultStore = &holdingReceiptStore{Store: fixture.controlStore, epochs: fixture.dispatcher.Epochs}
	err := fixture.dispatcher.patchRecoveredTerminalEpoch(fixture.ctx, task, fixture.fence.Epoch)
	if _, ok := errors.AsType[*store.StorageHoldError](err); !ok {
		t.Fatalf("terminal epoch write after the installation hold was raised = %v, want the installation hold", err)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, hold.reason)
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Execution != nil && updated.Status.Execution.ControllerEpoch == fixture.fence.Epoch {
		t.Fatalf("Task recorded controller epoch %d while the installation was held", fixture.fence.Epoch)
	}

	fixture.dispatcher.ResultStore = fixture.controlStore
	fixture.dispatcher.Epochs.clearHold()
	recoverUntilTaskEpoch(t, fixture, task)
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionFalse, corev1alpha1.StorageRecoveryRecordFoundReason)
}

func TestTerminalEpochWriteKeepsTheHoldOfASettlingAttempt(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	loseResultReceipt(t, fixture, task)
	requireTaskHeld(t, fixture, task, taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	})

	if err := fixture.dispatcher.patchRecoveredTerminalEpoch(fixture.ctx, task, fixture.fence.Epoch); err != nil {
		t.Fatal(err)
	}
	requireTaskStorageCondition(t, fixture, task, metav1.ConditionTrue, corev1alpha1.StorageRecoveryMissingRecordReason)
}
