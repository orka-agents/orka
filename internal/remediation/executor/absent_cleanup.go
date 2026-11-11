package executor

import (
	"context"
	"errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	pvkube "github.com/orka-agents/orka/internal/patchverification/kube"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (b *Backend) requirePreparationAbsent(ctx context.Context, receipt contract.ExecutionReceipt) error {
	task := &corev1alpha1.Task{}
	err := b.reader.Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.TaskName}, task)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	return contract.ErrConflict
}

func (b *Backend) cancelAbsentPreparation(ctx context.Context, receipt contract.ExecutionReceipt) error {
	if err := b.ensureCancellationFence(ctx, receipt); err != nil {
		return err
	}
	if err := b.requirePreparationAbsent(ctx, receipt); err != nil {
		return err
	}
	requestID := pvkube.PreparedSubmissionID(receipt.TaskUID, receipt.InputDigest)
	submission, err := b.validation.GetSubmission(ctx, receipt.Namespace, requestID)
	if err != nil && !errors.Is(err, pv.ErrRunNotFound) {
		return safeError(contract.ErrUnavailable, err)
	}
	if submission != nil {
		deps := submission.Manifest.Environment.Dependencies
		if submission.SubmittedBy != receipt.TaskUID || deps[pv.PreparationTaskKey] != receipt.TaskName ||
			deps[pv.PreparationUIDKey] != receipt.TaskUID || deps[pv.PreparationDigestKey] != receipt.InputDigest {
			return contract.ErrConflict
		}
		if submission.State != pv.SubmissionTerminal {
			if err := b.validation.Cancel(ctx, receipt.Namespace, requestID); err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
		}
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: receipt.TaskName, Namespace: receipt.Namespace,
		UID: types.UID(receipt.TaskUID), Annotations: map[string]string{pv.PreparationInputAnnotation: receipt.InputDigest}}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: receipt.TaskName, Namespace: receipt.Namespace}}
	var jobUID types.UID
	for {
		if err := b.requirePreparationAbsent(ctx, receipt); err != nil {
			return err
		}
		if submission != nil {
			if err := b.validation.Advance(ctx, receipt.Namespace, requestID); err != nil {
				return safeError(contract.ErrUnavailable, err)
			}
		}
		childrenDone, err := b.validation.VerifyPreparedCleanup(ctx, receipt.Namespace, receipt.TaskName, receipt.TaskUID, receipt.InputDigest)
		if err != nil {
			return absentCleanupError(err)
		}
		computeDone, err := b.cleanupPreparationJobState(ctx, task, job, &jobUID, true)
		if err != nil {
			return err
		}
		if childrenDone && computeDone {
			if err := b.deletePreparationInputs(ctx, task); err != nil {
				return err
			}
			if err := b.requirePreparationAbsent(ctx, receipt); err != nil {
				return err
			}
			late := &batchv1.Job{}
			if err := b.reader.Get(ctx, client.ObjectKeyFromObject(job), late); !apierrors.IsNotFound(err) {
				if err != nil {
					return safeError(contract.ErrUnavailable, err)
				}
				return contract.ErrConflict
			}
			// A second authoritative observation catches a late or replaced
			// Job before returning settlement to the coordinator.
			computeDone, err = b.cleanupPreparationJobState(ctx, task, job, &jobUID, true)
			if err != nil {
				return err
			}
			if computeDone {
				childrenDone, err = b.validation.VerifyPreparedCleanup(ctx, receipt.Namespace, receipt.TaskName, receipt.TaskUID, receipt.InputDigest)
				if err != nil {
					return absentCleanupError(err)
				}
				if childrenDone {
					return b.requirePreparationAbsent(ctx, receipt)
				}
			}
		}

		if err := b.pause(ctx); err != nil {
			return err
		}
	}
}

func absentCleanupError(err error) error {
	if errors.Is(err, pv.ErrBinding) {
		return safeError(contract.ErrConflict, err)
	}
	return safeError(contract.ErrUnavailable, err)
}
