package controller

import (
	"context"
	"errors"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *RepositoryScanReconciler) reconcileScanRunIdentity(ctx context.Context, scan *corev1alpha1.RepositoryScan) (bool, error) {
	if r.SecurityStore == nil {
		if controllerutil.ContainsFinalizer(scan, security.RepositoryScanRunFinalizer) {
			return false, fmt.Errorf("security store is required to release scan runs")
		}
		return false, nil
	}
	if scan.DeletionTimestamp.IsZero() {
		if err := security.EnsureRepositoryScanRunFinalizer(ctx, r.Client, r.APIReader, scan); err != nil {
			return false, err
		}
	}
	staleStatus, err := security.RetireStaleScanRuns(ctx, r.SecurityStore, r.Client, r.APIReader, scan)
	if errors.Is(err, security.ErrScanRunCancellationPending) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !scan.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(scan, security.RepositoryScanRunFinalizer) {
			before := scan.DeepCopy()
			controllerutil.RemoveFinalizer(scan, security.RepositoryScanRunFinalizer)
			if err := r.Patch(ctx, scan, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if staleStatus {
		return true, r.updateStatusWithRetry(ctx, scan, func(current *corev1alpha1.RepositoryScan) {
			current.Status.Phase = repositoryScanPhasePending
			current.Status.LastScanID = ""
			current.Status.LastScanTaskName = ""
			current.Status.LastProcessedCommit = ""
			current.Status.LastObservedHeadSHA = ""
			meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: repositoryScanPhaseReady, Status: metav1.ConditionFalse, Reason: repositoryScanPhasePending,
				Message:            "Repository scan changed; waiting for a new scan run",
				ObservedGeneration: current.Generation, LastTransitionTime: metav1.Now(),
			})
		})
	}
	return false, nil
}

func (r *RepositoryScanReconciler) listCurrentScanTasks(ctx context.Context, scan *corev1alpha1.RepositoryScan, runID string) (*corev1alpha1.TaskList, error) {
	selector := map[string]string{labels.LabelSecurityTarget: labels.SelectorValue(scan.Name)}
	if runID != "" {
		selector[labels.LabelSecurityScanID] = runID
	}
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks, client.InNamespace(scan.Namespace), client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	var err error
	tasks.Items, err = security.CurrentRepositoryScanTasks(ctx, r.SecurityStore, scan, tasks.Items)
	return &tasks, err
}

func (r *RepositoryScanReconciler) createOrValidateScanStageTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, task *corev1alpha1.Task) error {
	if err := r.validateScanStageRun(ctx, scan, run); err != nil {
		return err
	}
	if err := r.Create(ctx, task); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		existing := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(task), existing); err != nil {
			return err
		}
		if !matchingRepositoryScanTask(existing, task) {
			return fmt.Errorf("%w: existing security stage task does not match the admitted run", store.ErrConflict)
		}
	}
	// Retirement may have listed Tasks between the preflight and Create. A new
	// cancellation request fences that cleanup and makes this late Task retryable.
	if err := r.validateScanStageRun(ctx, scan, run); err != nil {
		return errors.Join(err, security.CancelScanRun(ctx, r.SecurityStore, r.Client, r.APIReader, scan, run,
			"scan stage creation lost repository scan run ownership"))
	}
	return nil
}

func (r *RepositoryScanReconciler) validateScanStageRun(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	return security.ValidateScanStageRun(ctx, r.SecurityStore, reader, scan, run)
}
