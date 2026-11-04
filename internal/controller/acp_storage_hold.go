package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// promptResultReceiptRecord names the store record a settling prompt attempt
// requires.
const promptResultReceiptRecord = "PromptResultReceipt"

// taskStorageRecordHold is a store record that a Task's Kubernetes records
// require but that is missing or differs from what they recorded. Recovery
// holds the Task instead of acting on it: it never runs the prompt again,
// never reports success and deletes nothing, until the record is found again
// or an operator acknowledges the hold.
type taskStorageRecordHold struct {
	// reason is StorageRecoveryMissingRecordReason or
	// StorageRecoveryConflictingRecordReason.
	reason string
	// record and key name the store record.
	record string
	key    string
	// expected is the digest the Kubernetes records hold for it.
	expected string
}

// id identifies the hold. It is derived from the evidence, so it stays the
// same each time recovery finds the same record missing, and an operator's
// acknowledgement keeps applying to it.
func (h taskStorageRecordHold) id() string {
	sum := sha256.Sum256([]byte(h.reason + "\x00" + h.record + "\x00" + h.key + "\x00" + h.expected))
	return "record-" + hex.EncodeToString(sum[:8])
}

func (h taskStorageRecordHold) message() string {
	what := "is missing"
	if h.reason == corev1alpha1.StorageRecoveryConflictingRecordReason {
		what = "differs from the Kubernetes record"
	}
	return fmt.Sprintf("hold %s: store record %s %q %s (expected digest %s); the prompt is not run again and success is not reported. "+
		"After recovering storage, set the %s annotation to %s to continue, which records the outcome as unknown if the record is still missing",
		h.id(), h.record, h.key, what, h.expected, corev1alpha1.StorageRecoveryAcknowledgedAnnotation, h.id())
}

// taskStorageRecordHoldError reports that recovery held a Task. It matches
// store.ErrStorageRecoveryRequired and store.ErrNotReady, so recovery treats
// it as work that cannot proceed yet rather than as a failure to classify.
type taskStorageRecordHoldError struct {
	hold taskStorageRecordHold
}

func (e *taskStorageRecordHoldError) Error() string {
	return fmt.Sprintf("%v: %s", store.ErrStorageRecoveryRequired, e.hold.message())
}

func (e *taskStorageRecordHoldError) Is(target error) bool {
	return target == store.ErrStorageRecoveryRequired || target == store.ErrNotReady
}

// settleTaskStorageRecordHold applies a detected hold. When the Task's
// acknowledgement annotation names the hold, it records the release and
// returns nil, and recovery continues as it did before holds existed, which
// never runs the prompt again or reports success. Otherwise it records the
// StorageRecoveryRequired condition and returns a taskStorageRecordHoldError.
func (d *ACPDispatcher) settleTaskStorageRecordHold(ctx context.Context, task *corev1alpha1.Task, hold taskStorageRecordHold) error {
	if task.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] == hold.id() {
		return d.setTaskStorageRecordCondition(ctx, task, metav1.Condition{
			Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionFalse,
			Reason:  corev1alpha1.StorageRecoveryAcknowledgedReason,
			Message: fmt.Sprintf("hold %s was acknowledged; recovery continued without the store record", hold.id()),
		})
	}
	if err := d.setTaskStorageRecordCondition(ctx, task, metav1.Condition{
		Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionTrue,
		Reason: hold.reason, Message: hold.message(),
	}); err != nil {
		return err
	}
	logf.FromContext(ctx).Error(store.ErrStorageRecoveryRequired, "ACP Task is held for storage recovery",
		"namespace", task.Namespace, "task", task.Name, "hold", hold.id(), "reason", hold.reason,
		"record", hold.record, "key", hold.key)
	return &taskStorageRecordHoldError{hold: hold}
}

// clearTaskStorageRecordHold records that the record a Task was held for was
// found again, so the Task no longer reports a hold once recovery continues.
func (d *ACPDispatcher) clearTaskStorageRecordHold(ctx context.Context, task *corev1alpha1.Task) error {
	if !meta.IsStatusConditionTrue(task.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) {
		return nil
	}
	return d.setTaskStorageRecordCondition(ctx, task, metav1.Condition{
		Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionFalse,
		Reason:  corev1alpha1.StorageRecoveryRecordFoundReason,
		Message: "the store record the Task was held for was found",
	})
}

// setTaskStorageRecordCondition writes condition to the exact Task
// incarnation, and to task, unless the Task already carries it.
func (d *ACPDispatcher) setTaskStorageRecordCondition(ctx context.Context, task *corev1alpha1.Task, condition metav1.Condition) error {
	if current := meta.FindStatusCondition(task.Status.Conditions, condition.Type); current != nil &&
		current.Status == condition.Status && current.Reason == condition.Reason && current.Message == condition.Message {
		return nil
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return err
		}
		if latest.UID != task.UID {
			return fmt.Errorf("%w: Task %s/%s was replaced", store.ErrConflict, task.Namespace, task.Name)
		}
		base := latest.DeepCopy()
		condition.LastTransitionTime = metav1.Now()
		condition.ObservedGeneration = latest.Generation
		if !meta.SetStatusCondition(&latest.Status.Conditions, condition) {
			task.Status.Conditions = latest.Status.Conditions
			return nil
		}
		if err := d.Client.Status().Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		task.Status.Conditions = latest.Status.Conditions
		return nil
	})
}
