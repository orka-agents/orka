package controller

import (
	"context"
	"sync"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// A hold condition that a replaced controller sent before it lost its epoch,
// and that the API server applies only after the successor has settled the
// attempt from its restored receipt, does not stay on the settled Task: the
// successor's terminal epoch write clears it.
func TestTerminalRecoveryClearsALateHoldFromAReplacedController(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task, _, digest := settlingTaskWithReceipt(t, fixture)
	hold := taskStorageRecordHold{
		reason: corev1alpha1.StorageRecoveryMissingRecordReason, record: promptResultReceiptRecord,
		key: fixture.attemptID, expected: digest,
	}
	var once sync.Once
	lateHold := func(ctx context.Context) error {
		var err error
		once.Do(func() {
			// The replaced controller's PATCH carried the resourceVersion the
			// successor read, so it lands only before the successor's first
			// write to the Task.
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

// A hold on a Task whose attempt is still Settling is current, and the
// terminal epoch write leaves it in place.
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
