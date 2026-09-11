package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

var errScanRunLiveIdentityChanged = fmt.Errorf("%w: repository scan identity changed during ingestion", store.ErrConflict)

func (r *RepositoryScanReconciler) validateScanRunIngestionIdentity(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	if !security.ScanRunMatchesRepositoryScan(run, scan) || run.CancellationVersion != 0 {
		return store.ErrConflict
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if reader == nil {
		return fmt.Errorf("live reader is required to validate scan ingestion")
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		if apierrors.IsNotFound(err) {
			return errors.Join(errScanRunLiveIdentityChanged, err)
		}
		return err
	}
	if !security.ScanRunMatchesRepositoryScan(run, current) {
		return errScanRunLiveIdentityChanged
	}
	return nil
}

func (r *RepositoryScanReconciler) cancelScanRunAfterIdentityConflict(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, err error) error {
	if errors.Is(err, errScanRunLiveIdentityChanged) {
		// This runs after the ingestion transaction has rolled back, so the
		// cancellation survives without preserving any stale result writes.
		return errors.Join(err, security.CancelScanRun(ctx, r.SecurityStore, r.Client, r.APIReader, scan, run,
			"repository scan identity changed during ingestion"))
	}
	return err
}

func scanTaskIngestionIdentity(scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) store.ScanTaskIdentity {
	return store.ScanTaskIdentity{
		Namespace: scan.Namespace, RepositoryScan: scan.Name, ScanRunID: scanTaskRunID(task),
		TaskName: task.Name, TaskUID: string(task.UID), Stage: taskSecurityStage(task),
		SliceID: strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID]),
	}
}

func terminalScanRunPhase(phase string) bool {
	return phase == scanRunPhaseSucceeded || phase == scanRunPhaseFailed
}

func (r *RepositoryScanReconciler) applyScanTaskIngestion(
	ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun,
	apply func(*RepositoryScanReconciler, *store.ScanRun, *store.ScanTaskIngestion) error,
) error {
	ingestion := &store.ScanTaskIngestion{ScanTaskIdentity: scanTaskIngestionIdentity(scan, task), Completed: true}
	validate := func(current *store.ScanRun) error {
		return r.validateScanRunIngestionIdentity(ctx, scan, current)
	}
	applied, err := r.SecurityStore.ApplyScanTaskIngestion(ctx, ingestion, validate, func(tx store.SecurityStore, current *store.ScanRun) error {
		transactional := *r
		transactional.SecurityStore = tx
		if current.PolicyDigest == "" {
			current.PolicyDigest = run.PolicyDigest
		}
		if err := apply(&transactional, current, ingestion); err != nil {
			return err
		}
		if current.ErrorMessage != "" {
			current.Phase = scanRunPhaseFailed
			current.Summary = current.ErrorMessage
			if current.CompletedAt == nil && task.Status.CompletionTime != nil {
				completed := task.Status.CompletionTime.Time
				current.CompletedAt = &completed
			}
		}
		return nil
	})
	if err != nil {
		return r.cancelScanRunAfterIdentityConflict(ctx, scan, run, err)
	}
	current, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, run.ID)
	if err != nil {
		return err
	}
	*run = *current
	log.FromContext(ctx).V(1).Info("Reconciled scan Task ingestion",
		"task", task.Name, "taskUID", task.UID, "recordedRun", ingestion.ScanRunID,
		"selectedRun", run.ID, "stage", ingestion.Stage, "slice", ingestion.SliceID,
		"applied", applied, "reviewed", run.ReviewedSliceCount,
		"accepted", run.AcceptedFindings, "dropped", run.DroppedFindings)
	if applied {
		if err := r.finishScanTaskIngestion(ctx, scan, ingestion); err != nil {
			return err
		}
	}
	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
}

// SQLite results commit before Kubernetes validation Tasks or diagnostic
// artifacts are published. A pending receipt retries only that follow-up work.
func (r *RepositoryScanReconciler) finishScanTaskIngestion(ctx context.Context, scan *corev1alpha1.RepositoryScan, ingestion *store.ScanTaskIngestion) error {
	if ingestion.Completed {
		return nil
	}
	if ingestion.DroppedFindingsJSON != "" && r.ArtifactStore != nil {
		if err := r.ArtifactStore.SaveArtifact(ctx, ingestion.Namespace, ingestion.TaskName,
			security.ArtifactDroppedFindings, "application/json", []byte(ingestion.DroppedFindingsJSON)); err != nil {
			return err
		}
	}
	findings := make([]*store.Finding, 0, len(ingestion.FindingIDs))
	for _, id := range ingestion.FindingIDs {
		finding, err := r.SecurityStore.GetFinding(ctx, ingestion.Namespace, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		// A later scan may already have observed this canonical finding.
		if finding.RepositoryScan == ingestion.RepositoryScan && finding.ScanRunID == ingestion.ScanRunID {
			finding.ScanTaskName = ingestion.TaskName
			findings = append(findings, finding)
		}
	}
	if err := r.enqueueAutoValidationTasks(ctx, scan, findings); err != nil {
		return err
	}
	return r.SecurityStore.CompleteScanTaskIngestion(ctx, ingestion.ScanTaskIdentity)
}
