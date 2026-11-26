package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const promptResultReceiptRecord = "PromptResultReceipt"

type taskStorageRecordHold struct {
	reason   string
	record   string
	key      string
	expected string
}

func (h taskStorageRecordHold) id() string {
	sum := sha256.Sum256([]byte(h.reason + "\x00" + h.record + "\x00" + h.key + "\x00" + h.expected))
	return "record-" + hex.EncodeToString(sum[:8])
}

func (h taskStorageRecordHold) message() string {
	what := "is missing"
	if h.reason == corev1alpha1.StorageRecoveryConflictingRecordReason {
		what = "differs from the Kubernetes record"
	}
	expected := "the Kubernetes record no longer holds its digest"
	if h.expected != "" {
		expected = "expected digest " + h.expected
	}
	return fmt.Sprintf("hold %s: store record %s %q %s (%s); the prompt is not run again and success is not reported. "+
		"After recovering storage, set the %s annotation to %s to continue, which records the outcome as unknown if the record is still missing",
		h.id(), h.record, h.key, what, expected, corev1alpha1.StorageRecoveryAcknowledgedAnnotation, h.id())
}

type taskStorageRecordObservation struct {
	fence   store.ControllerEpochFence
	attempt store.PromptAttempt
}

type taskStorageRecordHoldError struct {
	hold taskStorageRecordHold
}

func (e *taskStorageRecordHoldError) Error() string {
	return fmt.Sprintf("%v: %s", store.ErrStorageRecoveryRequired, e.hold.message())
}

func (e *taskStorageRecordHoldError) Is(target error) bool {
	return target == store.ErrStorageRecoveryRequired || target == store.ErrNotReady
}

func (d *ACPDispatcher) settleTaskStorageRecordHold(
	ctx context.Context, task *corev1alpha1.Task, observed taskStorageRecordObservation, hold taskStorageRecordHold,
) error {
	if strings.TrimSpace(task.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation]) == hold.id() {
		_, err := d.setTaskStorageRecordCondition(ctx, task, observed, metav1.Condition{
			Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionFalse,
			Reason:  corev1alpha1.StorageRecoveryAcknowledgedReason,
			Message: fmt.Sprintf("hold %s was acknowledged; recovery continued without the store record", hold.id()),
		})
		return err
	}
	changed, err := d.setTaskStorageRecordCondition(ctx, task, observed, metav1.Condition{
		Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionTrue,
		Reason: hold.reason, Message: hold.message(),
	})
	if err != nil {
		return err
	}
	if changed {
		logf.FromContext(ctx).Error(store.ErrStorageRecoveryRequired, "ACP Task is held for storage recovery",
			"namespace", task.Namespace, "task", task.Name, "hold", hold.id(), "reason", hold.reason,
			"record", hold.record, "key", hold.key)
	}
	return &taskStorageRecordHoldError{hold: hold}
}

func (d *ACPDispatcher) clearTaskStorageRecordHold(ctx context.Context, task *corev1alpha1.Task, observed taskStorageRecordObservation) error {
	if !meta.IsStatusConditionTrue(task.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) {
		return nil
	}
	_, err := d.setTaskStorageRecordCondition(ctx, task, observed, metav1.Condition{
		Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionFalse,
		Reason:  corev1alpha1.StorageRecoveryRecordFoundReason,
		Message: "the store record the Task was held for was found",
	})
	return err
}

func (d *ACPDispatcher) releaseSettledTaskStorageHold(ctx context.Context, latest *corev1alpha1.Task) (bool, error) {
	if !meta.IsStatusConditionTrue(latest.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) {
		return false, nil
	}
	attemptID, err := promptAttemptIDFromTaskUID(latest, acpTaskControlUID(latest))
	if err != nil {
		return false, err
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, attemptID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch attempt.ExecutionState {
	case store.PromptExecutionSucceeded:
	case store.PromptExecutionOutcomeUnknown:
		found, err := d.settledResultReceiptFound(ctx, latest, attempt)
		if err != nil || !found {
			return false, err
		}
	default:
		return false, nil
	}
	meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionFalse,
		Reason: corev1alpha1.StorageRecoveryRecordFoundReason, ObservedGeneration: latest.Generation,
		Message: "the store record the Task was held for was found",
	})
	return true, nil
}

func (d *ACPDispatcher) settledResultReceiptFound(ctx context.Context, task *corev1alpha1.Task, attempt *store.PromptAttempt) (bool, error) {
	receipts, ok := d.ResultStore.(store.PromptResultReceiptStore)
	if !ok {
		return false, nil
	}
	receipt, err := receipts.GetPromptResultReceipt(ctx, attempt.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	version, ok := acpSettlingOperationVersion(receipt.OperationID)
	if !ok || version >= attempt.Version {
		return false, nil
	}
	digest, err := acpSettlingTransitionDigest(attempt.ID, version, receipt.Data)
	if err != nil {
		return false, err
	}
	return receipt.Namespace == task.Namespace && receipt.TaskName == task.Name &&
		receipt.OperationID == acpSettlingOperationID(version) && receipt.OperationDigest == digest, nil
}

func (d *ACPDispatcher) setTaskStorageRecordCondition(
	ctx context.Context, task *corev1alpha1.Task, observed taskStorageRecordObservation, condition metav1.Condition,
) (bool, error) {
	if current := meta.FindStatusCondition(task.Status.Conditions, condition.Type); current != nil &&
		current.Status == condition.Status && current.Reason == condition.Reason && current.Message == condition.Message {
		return false, nil
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return d.withTaskStorageRecordAuthority(ctx, observed.fence, func(ctx context.Context) error {
			latest := &corev1alpha1.Task{}
			if err := d.Client.Get(ctx, key, latest); err != nil {
				return err
			}
			if latest.UID != task.UID {
				return fmt.Errorf("%w: Task %s/%s was replaced", store.ErrConflict, task.Namespace, task.Name)
			}
			if err := d.requireTaskStorageRecordObservation(ctx, latest, observed); err != nil {
				return err
			}
			base := latest.DeepCopy()
			condition.LastTransitionTime = metav1.Now()
			condition.ObservedGeneration = latest.Generation
			if !meta.SetStatusCondition(&latest.Status.Conditions, condition) {
				task.Status.Conditions = latest.Status.Conditions
				changed = false
				return nil
			}
			if err := d.Client.Status().Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
			task.Status.Conditions = latest.Status.Conditions
			changed = true
			return nil
		})
	})
	return changed, err
}

func (d *ACPDispatcher) withTaskStorageRecordAuthority(
	ctx context.Context, fence store.ControllerEpochFence, write func(context.Context) error,
) error {
	admitted := func(ctx context.Context) error {
		return d.Epochs.AdmitWork(ctx, write)
	}
	if guard, ok := d.Store.(store.ControllerEpochMutationStore); ok {
		return guard.WithControllerEpochMutation(ctx, fence, admitted)
	}
	current, err := d.Store.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		return err
	}
	if current.Epoch != fence.Epoch || current.HolderID != fence.HolderID {
		return store.ConflictErrorf("controller epoch fence %s/%d/%s does not match current %d/%s",
			fence.Name, fence.Epoch, fence.HolderID, current.Epoch, current.HolderID)
	}
	return admitted(ctx)
}

func (d *ACPDispatcher) requireTaskStorageRecordObservation(
	ctx context.Context, latest *corev1alpha1.Task, observed taskStorageRecordObservation,
) error {
	if latest.Status.Execution != nil && latest.Status.Execution.ControllerEpoch > observed.fence.Epoch {
		return store.ConflictErrorf("Task %s/%s was recovered under controller epoch %d after epoch %d read its store record",
			latest.Namespace, latest.Name, latest.Status.Execution.ControllerEpoch, observed.fence.Epoch)
	}
	attempt, err := d.Store.GetPromptAttempt(ctx, observed.attempt.ID)
	if err != nil {
		return err
	}
	if attempt.Version != observed.attempt.Version || attempt.ExecutionState != observed.attempt.ExecutionState ||
		attempt.DeliveryState != observed.attempt.DeliveryState || attempt.LastOperationID != observed.attempt.LastOperationID ||
		attempt.LastOperationDigest != observed.attempt.LastOperationDigest {
		return store.ConflictErrorf("prompt attempt %s changed after recovery read its store record", observed.attempt.ID)
	}
	return nil
}
