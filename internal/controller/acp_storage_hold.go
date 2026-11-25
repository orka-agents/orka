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
	expected := "the Kubernetes record no longer holds its digest"
	if h.expected != "" {
		expected = "expected digest " + h.expected
	}
	return fmt.Sprintf("hold %s: store record %s %q %s (%s); the prompt is not run again and success is not reported. "+
		"After recovering storage, set the %s annotation to %s to continue, which records the outcome as unknown if the record is still missing",
		h.id(), h.record, h.key, what, expected, corev1alpha1.StorageRecoveryAcknowledgedAnnotation, h.id())
}

// taskStorageRecordObservation is what recovery read when it judged a Task's
// store record: the controller epoch fence it recovers under and the
// PromptAttempt the record belongs to. The Task's storage condition is written
// only while both still stand, so neither a replaced controller nor a pass
// that read the attempt before it moved on can leave a hold on a Task that
// was settled without it.
type taskStorageRecordObservation struct {
	fence   store.ControllerEpochFence
	attempt store.PromptAttempt
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
// acknowledgement annotation names the hold, ignoring surrounding whitespace
// as the ControllerEpoch's acknowledgement does, it records the release and
// returns nil, and recovery continues as it did before holds existed, which
// never runs the prompt again or reports success. Otherwise it records the
// StorageRecoveryRequired condition and returns a taskStorageRecordHoldError.
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

// clearTaskStorageRecordHold records that the record a Task was held for was
// found again, so the Task no longer reports a hold once recovery continues.
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

// releaseSettledTaskStorageHold marks latest's hold as released, and reports
// whether it did, when its attempt was settled from the store record:
// recovery recorded success, or recorded the outcome as unknown while the
// store holds a receipt matching the attempt's own settling operation, as for
// an attempt in workspace validation whose receipt recovery found but whose
// completion the journal no longer showed. For an unknown outcome, a hold
// whose receipt is gone is kept. Recovery holds a Task only while the
// attempt is Settling, and an acknowledged hold is already False, so such a
// hold is a late condition write from a replaced controller that the API
// server applied after the successor read the Task. The caller writes latest
// with its resourceVersion, through Epochs.AdmitWork when the hold was
// released: a late write that lands earlier is cleared there, and one that
// lands later fails its own resourceVersion precondition.
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

// settledResultReceiptFound reports whether the store holds attempt's result
// receipt for task, matching the settling operation the receipt names, as
// recovery checks it once the attempt records a later operation.
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

// setTaskStorageRecordCondition writes condition to the exact Task
// incarnation, and to task, unless the Task already carries it. Each try
// runs under the controller epoch and first checks that observed still
// stands. It reports whether it changed the condition.
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

// withTaskStorageRecordAuthority runs write only while fence is the current
// controller epoch and storage is not held. A control store with the epoch
// mutation interlock checks the fence and keeps a successor from taking over
// until write returns; otherwise the authoritative epoch record is read
// before write. Within that, write runs through Epochs.AdmitWork, as Job
// creation and success status writes do, so an installation hold raised
// after the recovery pass started, or a dataset whose checks have not passed,
// keeps it from changing the Task's record hold too.
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

// requireTaskStorageRecordObservation checks that no later controller epoch
// recovered the Task and that its PromptAttempt is still the one recovery
// read, so the condition reflects the current evidence.
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
