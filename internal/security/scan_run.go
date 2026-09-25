package security

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	RepositoryScanRunFinalizer = "orka.ai/security-scan-runs"
	repositoryScanKind         = "RepositoryScan"
)

var ErrScanRunCancellationPending = fmt.Errorf("%w: scan run cancellation is pending", store.ErrConflict)

// NewScanRunID does not depend on second-resolution Task names. The full ID
// fits in a Kubernetes label and remains stable for every stage of the run.
func NewScanRunID() string {
	return "scan_" + strings.ToLower(rand.Text())
}

// ScanStageTaskNameForRun gives a stage the same name on every reconciliation
// while keeping separate runs distinct, including runs started in one second.
func ScanStageTaskNameForRun(repositoryScanName, mode, stage, scope, runID string) string {
	parts := []string{sanitizeName(repositoryScanName), sanitizeName(mode), sanitizeName(stage)}
	if scope != "" {
		parts = append(parts, sanitizeName(scope))
	}
	return boundedTaskName(append(parts, sanitizeName(runID))...)
}

// ScanRunMatchesRepositoryScan never adopts an unbound historical run into a
// live RepositoryScan, whose Kubernetes UID and generation are nonzero.
func ScanRunMatchesRepositoryScan(run *store.ScanRun, scan *corev1alpha1.RepositoryScan) bool {
	return run != nil && scan != nil && scan.DeletionTimestamp.IsZero() &&
		run.Namespace == scan.Namespace && run.RepositoryScan == scan.Name &&
		run.RepositoryScanUID == string(scan.UID) && run.RepositoryScanGeneration == scan.Generation
}

func activeScanRunPhase(phase string) bool {
	return phase == "pending" || phase == "running"
}

// ValidateScanStageRun checks live parent identity and durable run ownership.
func ValidateScanStageRun(ctx context.Context, s store.SecurityStore, reader client.Reader, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	if s == nil {
		return fmt.Errorf("security store is required to validate scan stage admission")
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		return err
	}
	if !ScanRunMatchesRepositoryScan(run, current) {
		return fmt.Errorf("%w: repository scan changed before stage admission", store.ErrConflict)
	}
	latest, _, err := s.ListScanRuns(ctx, scan.Namespace, scan.Name, 1, "")
	if err != nil {
		return err
	}
	if len(latest) != 1 || latest[0].ID != run.ID || !ScanRunMatchesRepositoryScan(&latest[0], current) ||
		!activeScanRunPhase(latest[0].Phase) || latest[0].CancellationVersion != 0 {
		return fmt.Errorf("%w: scan run no longer admits stage Tasks", store.ErrConflict)
	}
	return nil
}

// CreateInitialScanTask keeps a failed admission reserved until cleanup confirms
// that no Task remains, including when Create returns an ambiguous response.
func CreateInitialScanTask(ctx context.Context, s store.SecurityStore, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, task *corev1alpha1.Task) error {
	if s == nil {
		return fmt.Errorf("security store is required to admit an initial scan Task")
	}
	if reader == nil {
		reader = c
	}
	if err := ValidateScanStageRun(ctx, s, reader, scan, run); err != nil {
		return errors.Join(err, CancelScanRun(ctx, s, c, reader, scan, run, "initial scan Task admission lost run ownership"))
	}
	if err := c.Create(ctx, task); err != nil {
		return errors.Join(err, CancelScanRun(ctx, s, c, reader, scan, run, "scan task creation failed"))
	}
	if err := ValidateScanStageRun(ctx, s, reader, scan, run); err != nil {
		return errors.Join(err, CancelScanRun(ctx, s, c, reader, scan, run, "initial scan Task admission lost run ownership"))
	}
	return nil
}

// EnsureRepositoryScanRunFinalizer installs deletion cleanup before reserving a
// run. The live reader prevents stale cache entries from retiring newer runs;
// the optimistic patch cannot attach the finalizer to a recreated or edited object.
func EnsureRepositoryScanRunFinalizer(ctx context.Context, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan) error {
	if reader == nil {
		reader = c
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.RepositoryScan{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
			return err
		}
		if current.UID != scan.UID || current.Generation != scan.Generation || !current.DeletionTimestamp.IsZero() {
			return fmt.Errorf("%w: repository scan changed before run admission", store.ErrConflict)
		}
		if controllerutil.ContainsFinalizer(current, RepositoryScanRunFinalizer) {
			return nil
		}
		before := current.DeepCopy()
		controllerutil.AddFinalizer(current, RepositoryScanRunFinalizer)
		return c.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// RetireStaleScanRuns releases reservations from an earlier object or spec.
// It also reports a stale status binding even if that run is already terminal.
// Legacy rows remain unbound; no identity is inferred from the current object.
func RetireStaleScanRuns(ctx context.Context, s store.SecurityStore, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan) (bool, error) {
	runs, err := s.ListActiveScanRuns(ctx, scan.Namespace, scan.Name)
	if err != nil {
		return false, err
	}
	pending, err := s.ListScanRunsPendingCancellation(ctx, scan.Namespace, scan.Name)
	if err != nil {
		return false, err
	}
	// Prefer the pending snapshots, including their cleanup version, when a
	// request appeared after the active-reservation read.
	runs = append(pending, runs...)
	// The newest admission also fences stale generations after it completes.
	latest, _, err := s.ListScanRuns(ctx, scan.Namespace, scan.Name, 1, "")
	if err != nil {
		return false, err
	}
	runs = append(runs, latest...)
	staleStatus := false
	if scan.Status.LastScanID != "" {
		bound, err := s.GetScanRun(ctx, scan.Namespace, scan.Status.LastScanID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if err == nil && bound.RepositoryScan == scan.Name {
			runs = append(runs, *bound)
		} else {
			staleStatus = true
		}
	}

	// The parent may have changed while the run snapshots were collected.
	// Include late Tasks, then validate the caller before any cancellation.
	cleanupPending, err := resumeScanPipelineCleanup(ctx, s, c, reader, scan)
	if err != nil {
		return false, err
	}

	seen := make(map[string]bool, len(runs))
	for i := range runs {
		run := &runs[i]
		if seen[run.ID] {
			continue
		}
		seen[run.ID] = true
		if scan.DeletionTimestamp.IsZero() && run.RepositoryScanUID == string(scan.UID) && run.RepositoryScanGeneration > scan.Generation {
			return false, fmt.Errorf("%w: a newer repository scan generation has already admitted a run", store.ErrConflict)
		}
		// A deleting owner can only release its own reservations. A newer
		// incarnation may already have appeared by the time cleanup retries.
		if !scan.DeletionTimestamp.IsZero() && run.RepositoryScanUID != "" && run.RepositoryScanUID != string(scan.UID) {
			continue
		}
		if run.CancellationPending {
			if err := finishScanRunCancellation(ctx, s, c, reader, scan, run); err != nil {
				return false, err
			}
		}
		if ScanRunMatchesRepositoryScan(run, scan) && run.CancellationVersion == 0 {
			continue
		}
		staleStatus = staleStatus || run.ID == scan.Status.LastScanID
		if run.CancellationVersion != 0 {
			cleanupPending = cleanupPending || run.CancellationPending
			continue
		}
		if !activeScanRunPhase(run.Phase) {
			continue
		}
		if err := CancelScanRun(ctx, s, c, reader, scan, run, "repository scan identity changed or was deleted; start a new scan"); err != nil {
			return false, err
		}
		cleanupPending = true
	}
	if cleanupPending {
		return false, ErrScanRunCancellationPending
	}
	return staleStatus, nil
}

// Owned Task events requeue the parent, including after a controller restart.
// Use their run IDs to find stale or cancelled history without paging through all runs.
// Tasks without a matching run must finish deletion before new admission.
func resumeScanPipelineCleanup(ctx context.Context, s store.SecurityStore, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan) (bool, error) {
	if reader == nil {
		reader = c
	}
	var tasks corev1alpha1.TaskList
	if err := reader.List(ctx, &tasks, client.InNamespace(scan.Namespace), client.MatchingLabels{
		labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
	}); err != nil {
		return false, err
	}
	snapshots := make(map[string]*store.ScanRun)
	late := make(map[string]*store.ScanRun)
	var orphans []*corev1alpha1.Task
	cleanupPending := false
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !activeScanPipelineTask(task) {
			continue
		}
		owner := metav1.GetControllerOf(task)
		if owner == nil || owner.Name != scan.Name || owner.Kind != repositoryScanKind {
			continue
		}
		runID := task.Labels[labels.LabelSecurityScanID]
		run, known := snapshots[runID]
		if runID != "" && !known {
			var err error
			run, err = s.GetScanRun(ctx, scan.Namespace, runID)
			if errors.Is(err, store.ErrNotFound) {
				run = nil
			} else if err != nil {
				return false, err
			}
			snapshots[runID] = run
		}
		if !scanRunOwnsTask(run, scan, task) {
			// Missing history cannot authorize cleanup for another owner or
			// justify reconstructing a run from Task labels.
			if owner.UID == scan.UID {
				orphans = append(orphans, task)
			}
			continue
		}
		if !scan.DeletionTimestamp.IsZero() && owner.UID != scan.UID {
			continue
		}
		if scan.DeletionTimestamp.IsZero() && run.RepositoryScanUID == string(scan.UID) && run.RepositoryScanGeneration > scan.Generation {
			return false, fmt.Errorf("%w: a newer repository scan generation has already admitted a run", store.ErrConflict)
		}
		if run.CancellationPending {
			// This read can see cancellation requested after the initial
			// pending-run snapshot. Admission must still wait for cleanup.
			cleanupPending = true
			continue
		}
		// A failed review can make a run terminal while sibling Tasks remain
		// active. They still need cancellation when the parent identity changes.
		staleTerminal := !activeScanRunPhase(run.Phase) && !ScanRunMatchesRepositoryScan(run, scan)
		if run.CancellationVersion != 0 || staleTerminal {
			late[run.ID] = run
		}
	}
	if err := validateScanRunRetirementIdentity(ctx, c, reader, scan); err != nil {
		return false, err
	}
	for _, run := range late {
		if err := CancelScanRun(ctx, s, c, reader, scan, run, "stale or cancelled scan run still owns an active pipeline Task"); err != nil {
			return false, err
		}
	}
	for _, task := range orphans {
		if err := deleteScanPipelineTask(ctx, c, task); err != nil {
			return false, err
		}
	}
	return cleanupPending || len(late) > 0 || len(orphans) > 0, nil
}

func validateScanRunRetirementIdentity(ctx context.Context, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan) error {
	if reader == nil {
		reader = c
	}
	current := &corev1alpha1.RepositoryScan{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
		return err
	}
	if current.UID != scan.UID || current.Generation != scan.Generation ||
		current.DeletionTimestamp.IsZero() != scan.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: repository scan changed before run retirement", store.ErrConflict)
	}
	return nil
}

// DeleteScanRunPipelineTasks requests cancellation through Task deletion before
// releasing a run reservation. Terminal Tasks and other owners are preserved.
// The result reports whether this live read observed any nonterminal owned Tasks.
func DeleteScanRunPipelineTasks(ctx context.Context, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) (bool, error) {
	if reader == nil {
		reader = c
	}
	var tasks corev1alpha1.TaskList
	if err := reader.List(ctx, &tasks, client.InNamespace(run.Namespace), client.MatchingLabels{
		labels.LabelSecurityTarget: labels.SelectorValue(run.RepositoryScan),
		labels.LabelSecurityScanID: run.ID,
	}); err != nil {
		return false, err
	}
	observedActive := false
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !scanRunOwnsTask(run, scan, task) || !activeScanPipelineTask(task) {
			continue
		}
		observedActive = true
		if err := deleteScanPipelineTask(ctx, c, task); err != nil {
			return true, err
		}
	}
	return observedActive, nil
}

func scanRunOwnsTask(run *store.ScanRun, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) bool {
	if run == nil || run.Namespace != task.Namespace || run.ID != task.Labels[labels.LabelSecurityScanID] {
		return false
	}
	ownerUID := types.UID(run.RepositoryScanUID)
	if ownerUID == "" {
		ownerUID = scan.UID
	}
	owner := metav1.GetControllerOf(task)
	return owner != nil && owner.UID == ownerUID && owner.Name == run.RepositoryScan && owner.Kind == repositoryScanKind
}

func deleteScanPipelineTask(ctx context.Context, c client.Client, task *corev1alpha1.Task) error {
	if !task.DeletionTimestamp.IsZero() {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, task, client.Preconditions{UID: &task.UID, ResourceVersion: &task.ResourceVersion}))
}

func activeScanPipelineTask(task *corev1alpha1.Task) bool {
	switch task.Labels[labels.LabelSecurityStage] {
	case StageThreatModel, StageMapper, StageReview:
	default:
		return false
	}
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return false
	}
	return true
}

// RollbackScanRunAdmission cancels work whose admission lost status ownership.
// Failed cancellation keeps the reservation and durable cleanup intent.
func RollbackScanRunAdmission(ctx context.Context, s store.SecurityStore, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	return CancelScanRun(ctx, s, c, reader, scan, run, "scan admission lost repository scan status ownership")
}

// CancelScanRun records intent before requesting Task deletion so reconciliation
// can retry failures even when the run still matches the current configuration.
func CancelScanRun(ctx context.Context, s store.SecurityStore, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, reason string) error {
	if err := s.RequestScanRunCancellation(ctx, run, reason); err != nil {
		return err
	}
	// A creator can outlive its preflight and crash after Create succeeds. Keep
	// intent pending until a later cleanup pass checks the live Task collection.
	_, err := DeleteScanRunPipelineTasks(ctx, c, reader, scan, run)
	return err
}

func finishScanRunCancellation(ctx context.Context, s store.SecurityStore, c client.Client, reader client.Reader, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	observedActive, err := DeleteScanRunPipelineTasks(ctx, c, reader, scan, run)
	if err != nil {
		return err
	}
	if observedActive {
		return nil
	}
	return s.CompleteScanRunCancellation(ctx, run)
}

// CurrentRepositoryScanTasks excludes foreign owners and pipeline Tasks whose
// persisted run belongs to a previous RepositoryScan incarnation or generation.
func CurrentRepositoryScanTasks(ctx context.Context, s store.SecurityStore, scan *corev1alpha1.RepositoryScan, tasks []corev1alpha1.Task) ([]corev1alpha1.Task, error) {
	current := make([]corev1alpha1.Task, 0, len(tasks))
	if s == nil {
		return current, nil
	}
	runs, _, err := s.ListScanRuns(ctx, scan.Namespace, scan.Name, 1, "")
	if err != nil {
		return nil, err
	}
	var latest *store.ScanRun
	if len(runs) > 0 {
		latest = &runs[0]
	}
	for i := range tasks {
		task := &tasks[i]
		owner := metav1.GetControllerOf(task)
		if owner == nil || owner.UID != scan.UID || owner.Name != scan.Name ||
			owner.Kind != repositoryScanKind || task.Namespace != scan.Namespace {
			continue
		}
		stage := task.Labels[labels.LabelSecurityStage]
		if stage == "" && task.Labels[labels.LabelSecurityFindingID] != "" {
			stage = task.Labels[labels.LabelSecurityMode]
		}
		switch stage {
		case StageThreatModel, StageMapper, StageReview:
			runID := task.Labels[labels.LabelSecurityScanID]
			if !ScanRunMatchesRepositoryScan(latest, scan) || latest.ID != runID || latest.CancellationVersion != 0 {
				continue
			}
		case StageValidation, StagePatch:
			// These follow-up Tasks have their own finding/publication checks.
		default:
			continue
		}
		current = append(current, *task)
	}
	return current, nil
}
