package executor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (b *Backend) Cancel(ctx context.Context, receipt contract.ExecutionReceipt) error {
	if b == nil || !b.config.Enabled {
		return contract.ErrDisabled
	}
	if receipt.Namespace != b.config.Namespace || receipt.TaskUID == "" || !digestPattern.MatchString(receipt.InputDigest) ||
		!strings.HasPrefix(receipt.TaskName, "rv-prep-") {
		return contract.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.CleanupTimeout)
	defer cancel()
	if err := b.lockEffects(ctx); err != nil {
		return err
	}
	defer func() { <-b.effectGate }()
	task, err := b.cancellationTask(ctx, receipt)
	if errors.Is(err, contract.ErrNotFound) {
		return b.cancelAbsentPreparation(ctx, receipt)
	}
	if err != nil {
		return err
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseCancelled && task.Status.CompletionTime != nil &&
		task.Annotations[preparationCleaned] == preparationMarkerSet &&
		!controllerutil.ContainsFinalizer(task, preparationFinalizer) {
		return nil
	}
	if err := b.cancelSubmission(ctx, receipt, task.OwnerReferences[0]); err != nil {
		return err
	}
	if err := b.cleanupPreparation(ctx, task); err != nil {
		return err
	}
	return b.setTaskPhase(ctx, task, corev1alpha1.TaskPhaseCancelled)
}

func (b *Backend) cancellationTask(ctx context.Context, receipt contract.ExecutionReceipt) (*corev1alpha1.Task, error) {
	task := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, contract.ErrNotFound
		}
		return nil, safeError(contract.ErrUnavailable, err)
	}
	if string(task.UID) != receipt.TaskUID || task.Annotations[pv.PreparationInputAnnotation] != receipt.InputDigest ||
		task.Annotations[pv.ValidationBackendKey] != preparationBackend || len(task.OwnerReferences) != 1 {
		return nil, contract.ErrConflict
	}
	owner := task.OwnerReferences[0]
	if owner.Kind != "Task" || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.UID == "" ||
		owner.Controller == nil || !*owner.Controller {
		return nil, contract.ErrConflict
	}
	parent := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: owner.Name}, parent); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, safeError(contract.ErrUnavailable, err)
		}
	} else if parent.UID != owner.UID {
		return nil, contract.ErrConflict
	}
	if err := b.ensureCancellationFence(ctx, receipt); err != nil {
		return nil, err
	}
	if task.Annotations[pv.PreparationCancelAnnotation] == "" {
		base := task.DeepCopy()
		task.Annotations[pv.PreparationCancelAnnotation] = preparationMarkerSet
		if err := b.client.Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, safeError(contract.ErrUnavailable, err)
		}
	}
	return task, nil
}

func (b *Backend) cancelSubmission(ctx context.Context, receipt contract.ExecutionReceipt, owner metav1.OwnerReference) error {
	requestID := pvkube.PreparedSubmissionID(receipt.TaskUID, receipt.InputDigest)
	submission, err := b.validation.GetSubmission(ctx, receipt.Namespace, requestID)
	if err != nil && !errors.Is(err, pv.ErrRunNotFound) {
		return safeError(contract.ErrUnavailable, err)
	}
	if submission != nil {
		deps := submission.Manifest.Environment.Dependencies
		if deps[pv.PreparationUIDKey] != receipt.TaskUID || deps[pv.PreparationTaskKey] != receipt.TaskName ||
			deps[pv.PreparationDigestKey] != receipt.InputDigest || deps[pv.PreparationParentTaskKey] != owner.Name ||
			deps[pv.PreparationParentUIDKey] != string(owner.UID) {
			return contract.ErrConflict
		}
		if submission.State != pv.SubmissionTerminal {
			if err := b.validation.Cancel(ctx, receipt.Namespace, requestID); err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
			for {
				if err := b.validation.Advance(ctx, receipt.Namespace, requestID); err != nil {
					return safeError(contract.ErrUnavailable, err)
				}
				submission, err = b.validation.GetSubmission(ctx, receipt.Namespace, requestID)
				if err != nil {
					return safeError(contract.ErrUnavailable, err)
				}
				if submission.State == pv.SubmissionTerminal {
					break
				}
				if err := b.pause(ctx); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *Backend) cleanupPreparation(ctx context.Context, task *corev1alpha1.Task) error {
	ctx, cancel := context.WithTimeout(ctx, b.config.CleanupTimeout)
	defer cancel()
	expected := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: task.Name, Namespace: task.Namespace}}
	jobUID := types.UID(task.Annotations[preparationJobUID])
	for {
		gone, err := b.cleanupPreparationJob(ctx, task, expected, &jobUID)
		if err != nil {
			return err
		}
		if gone {
			break
		}
		if err := b.pause(ctx); err != nil {
			return err
		}
	}
	if err := b.deletePreparationInputs(ctx, task); err != nil {
		return err
	}
	current := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if current.UID != task.UID || !reflect.DeepEqual(current.OwnerReferences, task.OwnerReferences) {
		return contract.ErrConflict
	}
	return nil
}

func (b *Backend) cleanupPreparationJob(ctx context.Context, task *corev1alpha1.Task, expected *batchv1.Job, jobUID *types.UID) (bool, error) {
	current := &corev1alpha1.Task{}
	if err := b.reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		return false, safeError(contract.ErrUnavailable, err)
	}
	if current.UID != task.UID || current.Annotations[pv.PreparationInputAnnotation] != task.Annotations[pv.PreparationInputAnnotation] {
		return false, contract.ErrConflict
	}
	return b.cleanupPreparationJobState(ctx, current, expected, jobUID,
		current.Annotations[pv.PreparationCancelAnnotation] == preparationMarkerSet)
}

func (b *Backend) cleanupPreparationJobState(ctx context.Context, task *corev1alpha1.Task,
	expected *batchv1.Job, jobUID *types.UID, cancelled bool) (bool, error) {
	job := &batchv1.Job{}
	gone := false
	err := b.reader.Get(ctx, client.ObjectKeyFromObject(expected), job)
	switch {
	case apierrors.IsNotFound(err):
		gone = true
	case err != nil:
		return false, safeError(contract.ErrUnavailable, err)
	default:
		// Configuration or admission drift must prevent evidence, not prevent
		// stopping the exact owned workload that the caller already accepted.
		if job.UID == "" || !metav1.IsControlledBy(job, task) ||
			(*jobUID != "" && job.UID != *jobUID) {
			return false, contract.ErrConflict
		}
		*jobUID = job.UID
		if job.DeletionTimestamp.IsZero() {
			if err := b.client.Delete(ctx, job, client.Preconditions{UID: jobUID},
				client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return false, safeError(contract.ErrUnavailable, err)
			}
		}
	}
	if *jobUID == "" && gone {
		if !cancelled {
			return false, contract.ErrConflict
		}
	}
	for _, labels := range []map[string]string{
		{"job-name": expected.Name},
		{pv.ValidationTaskPrefix + "preparation": string(task.UID)},
	} {
		pods := &corev1.PodList{}
		if err := b.reader.List(ctx, pods, client.InNamespace(task.Namespace),
			client.MatchingLabels(labels), client.Limit(1)); err != nil {
			return false, safeError(contract.ErrUnavailable, err)
		}
		if len(pods.Items) != 0 {
			return false, nil
		}
	}
	owners := map[string]bool{string(task.UID): true}
	if *jobUID != "" {
		owners[string(*jobUID)] = true
	}
	hasPods, err := pvkube.HasOwnedPods(ctx, b.reader, task.Namespace, owners, expected.Name)
	if err != nil {
		return false, safeError(contract.ErrUnavailable, err)
	}
	return gone && !hasPods, nil
}

func (b *Backend) deletePreparationInputs(ctx context.Context, task *corev1alpha1.Task) error {
	original, patched := pvkube.PreparedTaskNames(string(task.UID), task.Annotations[pv.PreparationInputAnnotation])
	names := []string{task.Name + "-input", original + "-intent", patched + "-intent"}
	for {
		remaining := false
		for _, name := range names {
			cm := &corev1.ConfigMap{}
			err := b.reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: name}, cm)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
			if cm.UID == "" || !metav1.IsControlledBy(cm, task) {
				return contract.ErrConflict
			}
			remaining = true
			if cm.DeletionTimestamp.IsZero() {
				if err := b.client.Delete(ctx, cm, client.Preconditions{UID: &cm.UID}); err != nil && !apierrors.IsNotFound(err) {
					return safeError(contract.ErrUnavailable, err)
				}
			}
		}
		if !remaining {
			return nil
		}
		if err := b.pause(ctx); err != nil {
			return err
		}
	}
}

func (b *Backend) pause(ctx context.Context) error {
	timer := time.NewTimer(b.config.ReconcileInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return safeError(contract.ErrUnavailable, ctx.Err())
	case <-timer.C:
		return nil
	}
}
