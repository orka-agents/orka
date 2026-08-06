/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	cron "github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/security"
	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/workers/common"
)

const (
	maxThreatModelFallbackBytes  = 1 << 20
	repositoryScanPhasePending   = "Pending"
	repositoryScanPhaseScanning  = "Scanning"
	repositoryScanPhaseReady     = "Ready"
	repositoryScanPhaseError     = "Error"
	repositoryScanPhaseSuspended = "Suspended"

	scanRunPhasePending   = "pending"
	scanRunPhaseRunning   = "running"
	scanRunPhaseSucceeded = "succeeded"
	scanRunPhaseFailed    = "failed"

	scanModeIncremental = "incremental"
	scanModeManual      = "manual"
	confidenceHigh      = "high"

	reviewSliceStatusPending   = "pending"
	reviewSliceStatusReviewed  = "reviewed"
	reviewSliceStatusFailed    = "failed"
	reviewSliceStatusSkipped   = "skipped"
	reviewSliceStatusCompleted = "completed"

	findingStateOpen                 = "open"
	findingStatePatchPending         = "patch_pending"
	findingStatePatchReady           = "patch_ready"
	findingValidationStatusPending   = "pending"
	findingValidationStatusValidated = "validated"
	findingValidationStatusFailed    = "failed"
	validationModeOff                = "off"
	validationModeFull               = "full"
	validationThresholdLow           = "low"

	scanSummaryRunning             = "scan is running"
	scanSummaryThreatModelPending  = "Threat model generated; deterministic mapper pending"
	repositorySecurityCreatedBy    = "repository-security"
	qualityConditionReasonDegraded = "QualityDegraded"
	assessmentOutcomeDeferred      = "deferred"
	securityTaskFailureCancelled   = "task_cancelled"
	securityTaskFailureFailed      = "task_failed"
	findingEvidenceKindFile        = "file"

	// Kubernetes rejects condition messages longer than 32 KiB. Scan summaries can
	// exceed that, so keep the full summary in storage and only publish a capped
	// status message on the CRD.
	repositoryScanConditionMessageLimit  = 32 * 1024
	repositoryScanConditionMessageSuffix = "\n...[truncated]"
)

var errScannerPolicyDigestChanged = errors.New("scanner policy digest changed during scan run")

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get

// RepositoryScanReconciler reconciles RepositoryScan resources.
type RepositoryScanReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	SecurityStore       store.SecurityStore
	IntegrityStore      store.SecurityIntegrityStore
	TargetReceiptStore  store.SecurityTargetReceiptStore
	RunTaskInputStore   store.SecurityRunTaskInputStore
	RunThreatModelStore store.SecurityRunThreatModelStore
	BundleStore         store.SecurityBundleStore
	ArtifactStore       store.ArtifactStore
	ResultStore         store.ResultStore
	IntegrityConfig     security.IntegrityConfig
}

func (r *RepositoryScanReconciler) analysisTaskAnnotations(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
) (map[string]string, error) {
	policy := security.EffectiveAnalysisIsolationPolicy(scan)
	recordOutcome := func(outcome string, err error) {
		if err != nil {
			outcome = string(store.IsolationStatusFailed)
		}
		metrics.RecordSecurityIsolationOutcome(policy, outcome)
	}
	if policy == "legacy" {
		annotations, outcome, err := security.AnalysisIsolationAnnotations(policy, nil)
		recordOutcome(outcome, err)
		return annotations, err
	}
	if r.Client == nil || scan == nil {
		err := fmt.Errorf("analysis capability resolution is unavailable")
		recordOutcome("", err)
		return nil, err
	}
	namespace := strings.TrimSpace(scan.Spec.AnalysisAgentRef.Namespace)
	if namespace == "" {
		namespace = scan.Namespace
	}
	agent := &corev1alpha1.Agent{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: scan.Spec.AnalysisAgentRef.Name}, agent); err != nil {
		err = fmt.Errorf("resolve analysis agent capability: %w", err)
		recordOutcome("", err)
		return nil, err
	}
	annotations, outcome, err := security.AnalysisIsolationAnnotations(policy, agent)
	recordOutcome(outcome, err)
	return annotations, err
}

func mergeSecurityTaskAnnotations(base map[string]string, overlays ...map[string]string) map[string]string {
	out := make(map[string]string)
	maps.Copy(out, base)
	for _, overlay := range overlays {
		maps.Copy(out, overlay)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func applyRunIsolationFromAnnotations(run *store.ScanRun, annotations map[string]string) {
	if run == nil {
		return
	}
	next := store.IsolationStatus("")
	switch strings.TrimSpace(annotations["orka.ai/security-isolation-status"]) {
	case security.IsolationStatusHardened:
		next = store.IsolationStatusHardened
	case security.IsolationStatusFallback:
		next = store.IsolationStatusFallback
	case security.IsolationStatusLegacy:
		next = store.IsolationStatusLegacy
	}
	if next == "" {
		return
	}
	rank := func(status store.IsolationStatus) int {
		switch status {
		case store.IsolationStatusFailed:
			return 4
		case store.IsolationStatusFallback, store.IsolationStatusLegacy:
			return 3
		case store.IsolationStatusHardened:
			return 1
		default:
			return 0
		}
	}
	if run.Quality.IsolationStatus == store.IsolationStatusUnverified || rank(next) > rank(run.Quality.IsolationStatus) {
		run.Quality.IsolationStatus = next
	}
}

func repositoryScanConditionMessage(message, fallback string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return fallback
	}
	if len(message) <= repositoryScanConditionMessageLimit {
		return message
	}

	maxPrefixBytes := repositoryScanConditionMessageLimit - len(repositoryScanConditionMessageSuffix)
	if maxPrefixBytes <= 0 {
		return repositoryScanConditionMessageSuffix
	}

	message = message[:maxPrefixBytes]
	for len(message) > 0 && !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	message = strings.TrimRight(message, " \t\r\n")
	return message + repositoryScanConditionMessageSuffix
}

func titleCaseMode(mode string) string {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return "Scan"
	}
	return strings.ToUpper(mode[:1]) + mode[1:]
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=repositoryscans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=repositoryscans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=repositoryscans/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives repository scan lifecycle, task creation, and task ingestion.
func (r *RepositoryScanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("repositoryscan")

	scan := &corev1alpha1.RepositoryScan{}
	if err := r.Get(ctx, req.NamespacedName, scan); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if scan.Status.Phase == "" {
		if err := r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
			s.Status.Phase = repositoryScanPhasePending
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Pending",
				Message:            "Waiting for the first scan run",
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.ingestOwnedTasks(ctx, scan); err != nil {
		logger.Error(err, "failed to ingest security tasks")
		return ctrl.Result{}, err
	}

	if err := r.IntegrityConfig.ValidateRepositoryScanSpec(scan.Spec); err != nil {
		if updateErr := r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
			s.Status.Phase = repositoryScanPhaseError
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "IntegrityPolicyUnavailable",
				Message:            repositoryScanConditionMessage(err.Error(), "repository security integrity policy is unavailable"),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
		}); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	if security.IsSuspended(scan) {
		if scan.Status.Phase != repositoryScanPhaseSuspended {
			if err := r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
				s.Status.Phase = repositoryScanPhaseSuspended
				meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					Reason:             "Suspended",
					Message:            "Scheduled scans are suspended",
					LastTransitionTime: metav1.Now(),
					ObservedGeneration: s.Generation,
				})
			}); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	active, err := r.hasActiveScanPipelineTask(ctx, scan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if active {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if scan.Status.LastScanID == "" {
		if err := r.createScanRun(ctx, scan, "initial", "", ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	progressed, err := r.progressLatestScanRun(ctx, scan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if progressed {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if scan.Spec.Schedule == "" {
		return ctrl.Result{}, nil
	}

	sched, err := cron.ParseStandard(scan.Spec.Schedule)
	if err != nil {
		if updateErr := r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
			s.Status.Phase = repositoryScanPhaseError
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "InvalidSchedule",
				Message:            repositoryScanConditionMessage(err.Error(), "invalid scan schedule"),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
		}); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	base := scan.CreationTimestamp.Time
	if scan.Status.LastSuccessfulScanAt != nil {
		base = scan.Status.LastSuccessfulScanAt.Time
	}
	nextRun := sched.Next(base)
	if time.Now().Before(nextRun) {
		return ctrl.Result{RequeueAfter: time.Until(nextRun)}, nil
	}

	if err := r.createScanRun(ctx, scan, scanModeIncremental, security.IncrementalBaselineCommit(scan), ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func taskSecurityStage(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	if stage := strings.TrimSpace(task.Labels[labels.LabelSecurityStage]); stage != "" {
		return stage
	}
	if task.Labels[labels.LabelSecurityFindingID] != "" {
		switch strings.TrimSpace(task.Labels[labels.LabelSecurityMode]) {
		case security.StagePatch:
			return security.StagePatch
		case security.StageValidation:
			return security.StageValidation
		}
	}
	return ""
}

func isScanPipelineStage(stage string) bool {
	switch stage {
	case security.StageThreatModel, security.StageMapper, security.StageReview:
		return true
	default:
		return false
	}
}

func isActiveTaskPhase(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing, corev1alpha1.TaskPhaseScheduled:
		return true
	default:
		return false
	}
}

func securityTaskFailureClass(task *corev1alpha1.Task) string {
	if task != nil && task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return securityTaskFailureCancelled
	}
	return securityTaskFailureFailed
}

func scanTaskRunID(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	if scanID := strings.TrimSpace(task.Labels[labels.LabelSecurityScanID]); scanID != "" {
		return scanID
	}
	return security.ScanRunID(task.Name)
}

func latestOwnedScanPipelineRunID(tasks []corev1alpha1.Task) string {
	var latest *corev1alpha1.Task
	for i := range tasks {
		task := &tasks[i]
		if !isScanPipelineStage(taskSecurityStage(task)) {
			continue
		}
		if latest == nil {
			latest = task
			continue
		}
		if task.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = task
			continue
		}
		if task.CreationTimestamp.Equal(&latest.CreationTimestamp) && task.Name > latest.Name {
			latest = task
		}
	}
	return scanTaskRunID(latest)
}

func (r *RepositoryScanReconciler) hasActiveScanPipelineTask(ctx context.Context, scan *corev1alpha1.RepositoryScan) (bool, error) {
	if r.Client == nil {
		return false, nil
	}
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{labels.LabelSecurityTarget: labels.SelectorValue(scan.Name)}),
	); err != nil {
		return false, err
	}

	tasks.Items = repositoryScanControlledTasks(scan, tasks.Items)
	for _, task := range tasks.Items {
		if !isScanPipelineStage(taskSecurityStage(&task)) {
			continue
		}
		if task.Status.Phase == "" || isActiveTaskPhase(task.Status.Phase) {
			return true, nil
		}
	}
	return false, nil
}

//nolint:unparam // headCommit is usually mapper-resolved but kept for explicit scan ranges.
func (r *RepositoryScanReconciler) createScanRun(ctx context.Context, scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit string) error {
	policy, err := security.LoadScannerPolicy(ctx, r.Client, scan.Namespace, scan.Spec)
	if err != nil {
		if terminalScannerPolicyLoadError(err) {
			if statusErr := r.updateRepositoryScanPolicyError(ctx, scan, err); statusErr != nil {
				return errors.Join(err, statusErr)
			}
		}
		return err
	}
	requestKey := security.RequestIdempotencyKey(scan, mode, baseCommit, headCommit, policy.Digest)
	legacyKey := security.ScanRunIdempotencyKey(scan.Namespace, scan.Name, mode, baseCommit, headCommit, scan.Spec.SubPath, policy.Digest)
	if duplicate, err := r.hasActiveScanRunWithIdempotencyKey(ctx, scan, requestKey, legacyKey); err != nil {
		return err
	} else if duplicate {
		return nil
	}
	if !r.IntegrityConfig.PinnedScanTargetsEnabled {
		legacyTaskName := security.ScanStageTaskName(scan.Name, mode, security.StageThreatModel, "")
		legacyTask := &corev1alpha1.Task{}
		legacyErr := r.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: legacyTaskName}, legacyTask)
		if legacyErr == nil && (legacyTask.Status.Phase == "" || isActiveTaskPhase(legacyTask.Status.Phase)) &&
			metav1.IsControlledBy(legacyTask, scan) &&
			legacyTask.Labels[labels.LabelSecurityTarget] == labels.SelectorValue(scan.Name) {
			// Legacy Tasks predate immutable run/task-input bindings, so their
			// current prompt, policy, generation, and execution metadata cannot be
			// reconstructed safely. Treat the active Task only as a blocker; never
			// attribute it to a newly synthesized ScanRun.
			return nil
		} else if legacyErr != nil && !apierrors.IsNotFound(legacyErr) {
			return legacyErr
		}
	}

	runUID, err := security.NewRunUID()
	if err != nil {
		return err
	}
	scanID := security.PublicScanRunID(runUID)
	firstStage := security.StageThreatModel
	if r.IntegrityConfig.PinnedScanTargetsEnabled {
		firstStage = security.StageMapper
	}
	taskName := security.ScanStageTaskNameForRun(scan.Name, mode, firstStage, "", runUID)
	quality := initialScanQuality(scan, r.IntegrityConfig.PinnedScanTargetsEnabled)
	run := &store.ScanRun{
		ID:                       scanID,
		RunUID:                   runUID,
		Namespace:                scan.Namespace,
		RepositoryScan:           scan.Name,
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		TaskName:                 taskName,
		Mode:                     mode,
		Phase:                    scanRunPhasePending,
		BaseCommit:               baseCommit,
		HeadCommit:               headCommit,
		ScannerPolicyVersion:     security.ScannerPolicyVersion,
		PolicyDigest:             policy.Digest,
		RequestIdempotencyKey:    requestKey,
		IdempotencyKey:           requestKey,
		Quality:                  quality,
		StartedAt:                time.Now().UTC(),
	}
	threatModelInput, err := r.captureRunThreatModelTaskInput(ctx, scan, run)
	if err != nil {
		return err
	}
	if err := r.securityRunTaskInputStore().CreateScanRunWithTaskInput(ctx, run, threatModelInput); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		run, err = r.loadConflictingActiveScanRun(ctx, scan, requestKey, mode, baseCommit, headCommit, policy.Digest)
		if err != nil {
			return err
		}
		if run == nil {
			return nil
		}
	}

	if r.IntegrityConfig.PinnedScanTargetsEnabled {
		err = r.createMapperTask(ctx, scan, run)
	} else {
		err = r.createThreatModelTask(ctx, scan, run)
	}
	if err != nil {
		// The ScanRun and Kubernetes Task live in different stores. A Task
		// admission error cannot prove that the deterministic Task was not
		// concurrently admitted, so keep the unique active run pending for an
		// idempotent retry instead of releasing it for a replacement run.
		return err
	}

	return r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
		s.Status.Phase = repositoryScanPhaseScanning
		s.Status.LastScanID = run.ID
		s.Status.LastScanTaskName = run.TaskName
		s.Status.Quality = nil
		if r.IntegrityConfig.QualityStateWritesEnabled {
			s.Status.Quality = repositoryScanQualityStatus(run, s)
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type: "QualityReady", Status: metav1.ConditionUnknown, Reason: "QualityPending",
				Message: "Scan quality is still being evaluated", LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
		} else {
			meta.RemoveStatusCondition(&s.Status.Conditions, "QualityReady")
		}
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "Scanning",
			Message:            fmt.Sprintf("%s scan is running", titleCaseMode(mode)),
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: s.Generation,
		})
	})
}

func (r *RepositoryScanReconciler) loadConflictingActiveScanRun(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	requestKey, mode, baseCommit, headCommit, policyDigest string,
) (*store.ScanRun, error) {
	cursor := ""
	unrelatedReservation := false
	for {
		runs, next, err := r.SecurityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 100, cursor)
		if err != nil {
			return nil, fmt.Errorf("load conflicting scan run: %w", err)
		}
		for i := range runs {
			run := &runs[i]
			if !scanRunReservesRepository(run) {
				continue
			}
			if run.RequestIdempotencyKey != requestKey && run.IdempotencyKey != requestKey {
				unrelatedReservation = true
				continue
			}
			if !activeScanRunPhase(run.Phase) {
				return nil, nil
			}
			failure := error(nil)
			switch {
			case run.RepositoryScan != scan.Name:
				failure = errors.New("conflicting scan run targets a different RepositoryScan")
			case run.RepositoryScanUID != string(scan.UID) || run.RepositoryScanGeneration != scan.Generation:
				failure = errors.New("conflicting scan run is not bound to the current RepositoryScan generation")
			case !security.ValidRunUID(run.RunUID):
				failure = errors.New("conflicting scan run has no valid run UID")
			case run.Mode != mode || run.BaseCommit != baseCommit ||
				!strings.EqualFold(strings.TrimSpace(run.HeadCommit), strings.TrimSpace(headCommit)) ||
				run.PolicyDigest != policyDigest:
				failure = errors.New("conflicting scan run does not match the requested scan inputs")
			}
			if failure != nil {
				now := time.Now().UTC()
				run.Phase = scanRunPhaseFailed
				run.CompletedAt = &now
				run.ErrorMessage = failure.Error()
				run.Summary = failure.Error()
				if updateErr := r.SecurityStore.UpdateScanRun(ctx, run); updateErr != nil {
					return nil, errors.Join(failure, updateErr)
				}
				return nil, failure
			}
			return run, nil
		}
		if next == "" {
			break
		}
		if next == cursor {
			return nil, errors.New("load conflicting scan run: pagination cursor did not advance")
		}
		cursor = next
	}
	if unrelatedReservation {
		return nil, nil
	}
	return nil, fmt.Errorf("%w: active repository reservation was not found", store.ErrConflict)
}

func initialScanQuality(scan *corev1alpha1.RepositoryScan, targetResolutionPending bool) store.ScanQuality {
	quality := store.LegacyScanQuality()
	quality.SchemaVersion = store.SecurityQualitySchemaVersion
	quality.InventoryCoverageStatus = store.CoverageStatusPending
	quality.CandidateCoverageStatus = store.CoverageStatusPending
	quality.CoverageStatus = store.CoverageStatusPending
	quality.ValidationExecution = store.QualityExecutionNotStarted
	quality.AttackPathExecution = store.QualityExecutionNotStarted
	quality.AnalysisAttestationLevel = store.AnalysisAttestationUnverified
	quality.IsolationStatus = store.IsolationStatusUnverified
	quality.BundleStatus = store.BundleStatusNotStarted
	// Durable admission/authorization receipts are intentionally not available
	// before the authority cutover. Keep this explicit rather than promoting an
	// authenticated request to verified by assertion; validated completion is
	// rejected while this state is the strongest available value.
	quality.AuthorizationStatus = store.AuthorizationStatusLegacyUnverified
	if targetResolutionPending {
		quality.TargetVerification = store.TargetVerificationPending
	} else {
		quality.TargetVerification = store.TargetVerificationUnverified
	}
	switch security.EffectiveValidationMode(scan) {
	case validationModeOff:
		quality.ValidationScope = store.ValidationScopeOff
	case validationModeFull:
		quality.ValidationScope = store.ValidationScopeAll
	default:
		quality.ValidationScope = store.ValidationScopeSampled
	}
	switch security.EffectiveAnalysisIsolationPolicy(scan) {
	case "require-hardened", "prefer-hardened":
		quality.IsolationStatus = store.IsolationStatusUnverified
	default:
		quality.IsolationStatus = store.IsolationStatusLegacy
	}
	return quality
}

func (r *RepositoryScanReconciler) securityRunTaskInputStore() store.SecurityRunTaskInputStore {
	if r.RunTaskInputStore != nil {
		return r.RunTaskInputStore
	}
	compatible, _ := r.SecurityStore.(store.SecurityRunTaskInputStore)
	return compatible
}

func (r *RepositoryScanReconciler) captureRunThreatModelTaskInput(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) (*store.SecurityRunTaskInput, error) {
	inputStore := r.securityRunTaskInputStore()
	if inputStore == nil {
		return nil, errors.New("security run task-input store is unavailable")
	}
	input := &store.SecurityRunTaskInput{
		RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan,
		ScanRunID: run.ID, Stage: security.StageThreatModel,
	}
	if r.SecurityStore != nil {
		model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
		switch {
		case err == nil && threatModelMatchesRepositoryScan(model, scan):
			input.SourceVersion = model.Version
			input.Content = model.Content
		case err == nil, errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, err
		}
	}
	input.Content = security.NormalizeTaskInputSnapshot(input.Content)
	return input, nil
}

func (r *RepositoryScanReconciler) loadRunThreatModelTaskInput(
	ctx context.Context,
	run *store.ScanRun,
) (*store.SecurityRunTaskInput, error) {
	inputStore := r.securityRunTaskInputStore()
	if inputStore == nil {
		return nil, errors.New("security run task-input store is unavailable")
	}
	input, err := inputStore.GetSecurityRunTaskInput(ctx, run.Namespace, run.RunUID, security.StageThreatModel)
	if err != nil {
		return nil, fmt.Errorf("load immutable threat-model task input: %w", err)
	}
	if input.RunUID != run.RunUID || input.Namespace != run.Namespace || input.RepositoryScan != run.RepositoryScan ||
		input.ScanRunID != run.ID || input.Stage != security.StageThreatModel {
		return nil, errors.New("immutable threat-model task input does not match scan run")
	}
	return input, nil
}

func (r *RepositoryScanReconciler) createThreatModelTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) error {
	if run == nil {
		return errors.New("scan run is required")
	}
	if err := security.ValidateRunRepositoryScanIdentity(run, scan); err != nil {
		return err
	}
	var threatModel string
	if security.ValidRunUID(run.RunUID) {
		input, err := r.loadRunThreatModelTaskInput(ctx, run)
		if err != nil {
			return err
		}
		threatModel = input.Content
	} else if r.SecurityStore != nil {
		// Legacy runs predate immutable task-input snapshots.
		model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
		if err == nil && threatModelMatchesRepositoryScan(model, scan) {
			threatModel = model.Content
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	policy, err := security.LoadScannerPolicy(ctx, r.Client, scan.Namespace, scan.Spec)
	if err != nil {
		return err
	}
	if err := ensureScanRunPolicyDigest(run, policy); err != nil {
		return err
	}
	analysisAnnotations, err := r.analysisTaskAnnotations(ctx, scan)
	if err != nil {
		return err
	}
	applyRunIsolationFromAnnotations(run, analysisAnnotations)
	taskName := security.ScanStageTaskNameForRun(scan.Name, run.Mode, security.StageThreatModel, "", run.RunUID)
	if !security.ValidRunUID(run.RunUID) {
		taskName = security.ScanStageTaskName(scan.Name, run.Mode, security.StageThreatModel, "")
	}
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:        taskName,
			Namespace:   scan.Namespace,
			Annotations: analysisAnnotations,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      repositorySecurityCreatedBy,
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: run.ID,
				labels.LabelSecurityMode:   run.Mode,
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt: security.BuildThreatModelPrompt(
				scan, run.Mode, run.BaseCommit, run.HeadCommit, threatModel, policy.PromptPolicy(),
			),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageThreatModel},
				{Name: security.EnvScanID, Value: run.ID},
				{Name: security.EnvScannerPolicyVersion, Value: security.ScannerPolicyVersion},
				{Name: security.EnvPolicyDigest, Value: policy.Digest},
				{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{Workspace: resolvedWorkspaceForRun(scan, run)},
		},
	}
	if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrValidateSecurityTask(ctx, scan, task); err != nil {
		return err
	}
	if run.TaskName == "" {
		run.TaskName = taskName
	}
	return r.SecurityStore.UpdateScanRun(ctx, run)
}

func resolvedWorkspaceForRun(scan *corev1alpha1.RepositoryScan, run *store.ScanRun) *corev1alpha1.WorkspaceConfig {
	workspace := &corev1alpha1.WorkspaceConfig{
		GitRepo:      scan.Spec.RepoURL,
		Branch:       security.EffectiveWorkspaceBranch(scan),
		Ref:          security.EffectiveRef(scan),
		GitSecretRef: scan.Spec.GitSecretRef,
		SubPath:      scan.Spec.SubPath,
		ForkRepo:     scan.Spec.ForkRepo,
		PRBaseBranch: scan.Spec.PRBaseBranch,
	}
	if run != nil && strings.TrimSpace(run.HeadCommit) != "" {
		workspace.Ref = strings.TrimSpace(run.HeadCommit)
	}
	return workspace
}

func (r *RepositoryScanReconciler) hasActiveScanRunWithIdempotencyKey(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	keys ...string,
) (bool, error) {
	if r.SecurityStore == nil || len(keys) == 0 {
		return false, nil
	}
	runs, _, err := r.SecurityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 100, "")
	if err != nil {
		return false, err
	}
	for i := range runs {
		run := &runs[i]
		matches := false
		for _, key := range keys {
			if strings.TrimSpace(key) != "" && (run.RequestIdempotencyKey == key || run.IdempotencyKey == key) {
				matches = true
				break
			}
		}
		if !matches || !activeScanRunPhase(run.Phase) {
			continue
		}
		hasActiveTask, err := r.scanRunHasActivePipelineTask(ctx, scan, run.ID)
		if err != nil {
			return false, err
		}
		if hasActiveTask {
			return true, nil
		}
		// Leave the active run in place. Returning false lets createScanRun hit
		// the active-request uniqueness fence, reload this exact run, and verify
		// or recreate its deterministic initial Task.
	}
	return false, nil
}

func (r *RepositoryScanReconciler) scanRunHasActivePipelineTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, runID string) (bool, error) {
	if r.Client == nil || strings.TrimSpace(runID) == "" {
		return false, nil
	}
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
			labels.LabelSecurityScanID: runID,
		}),
	); err != nil {
		return false, err
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !isScanPipelineStage(taskSecurityStage(task)) {
			continue
		}
		if task.Status.Phase == "" || isActiveTaskPhase(task.Status.Phase) {
			return true, nil
		}
	}
	return false, nil
}

func activeScanRunPhase(phase string) bool {
	return phase == scanRunPhasePending || phase == scanRunPhaseRunning
}

func scanRunReservesRepository(run *store.ScanRun) bool {
	return run != nil && (activeScanRunPhase(run.Phase) || run.Quality.BundleStatus == store.BundleStatusSealing)
}

func scanRunUsesPinnedTarget(run *store.ScanRun) bool {
	if run == nil || !security.ValidRunUID(run.RunUID) {
		return false
	}
	return run.Quality.TargetVerification == store.TargetVerificationPending ||
		run.Quality.TargetVerification == store.TargetVerificationVerified ||
		run.Quality.TargetVerification == store.TargetVerificationMismatch ||
		strings.TrimSpace(run.TargetReceiptID) != "" || strings.TrimSpace(run.ResolvedTargetKey) != ""
}

func ensureScanRunPolicyDigest(run *store.ScanRun, policy security.ScannerPolicy) error {
	if run == nil {
		return nil
	}
	if run.PolicyDigest == "" {
		run.PolicyDigest = policy.Digest
		return nil
	}
	if policy.Digest != "" && run.PolicyDigest != policy.Digest {
		return fmt.Errorf("%w: recorded %s current %s", errScannerPolicyDigestChanged, run.PolicyDigest, policy.Digest)
	}
	return nil
}

func (r *RepositoryScanReconciler) recordTerminalScanRunError(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, failure error) error {
	if err := r.markScanRunTerminalError(ctx, scan, run, failure); err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

func (r *RepositoryScanReconciler) updateRepositoryScanPolicyError(ctx context.Context, scan *corev1alpha1.RepositoryScan, failure error) error {
	if scan == nil || failure == nil {
		return nil
	}
	message := failure.Error()
	return r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
		s.Status.Phase = repositoryScanPhaseError
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ScanFailed",
			Message:            repositoryScanConditionMessage(message, "scanner policy could not be loaded"),
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: s.Generation,
		})
	})
}

func (r *RepositoryScanReconciler) markScanRunTerminalError(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, failure error) error {
	if r.SecurityStore == nil || run == nil || failure == nil {
		return nil
	}
	currentRun, err := r.SecurityStore.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		return err
	}
	if currentRun.Quality.BundleStatus == store.BundleStatusSealed {
		return nil
	}
	now := time.Now()
	message := failure.Error()
	currentRun.Phase = scanRunPhaseFailed
	currentRun.CompletedAt = &now
	currentRun.ErrorMessage = message
	currentRun.Summary = message
	if err := r.SecurityStore.UpdateScanRun(ctx, currentRun); err != nil {
		return err
	}
	*run = *currentRun
	if scan == nil {
		return nil
	}
	return r.refreshScanRunStatus(ctx, scan, currentRun, currentRun.ID, true)
}

func (r *RepositoryScanReconciler) createMapperTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	policy, err := security.LoadScannerPolicy(ctx, r.Client, scan.Namespace, scan.Spec)
	if err != nil {
		if run != nil && activeScanRunPhase(run.Phase) && terminalScannerPolicyLoadError(err) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	if err := ensureScanRunPolicyDigest(run, policy); err != nil {
		if errors.Is(err, errScannerPolicyDigestChanged) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	timeout := metav1.Duration{Duration: 30 * time.Minute}
	priority := int32(690)
	taskName := security.ScanStageTaskNameForRun(scan.Name, run.Mode, security.StageMapper, "", run.RunUID)
	if !security.ValidRunUID(run.RunUID) {
		taskName = security.ScanStageTaskName(scan.Name, run.Mode, security.StageMapper, "")
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      repositorySecurityCreatedBy,
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: run.ID,
				labels.LabelSecurityMode:   run.Mode,
				labels.LabelSecurityStage:  security.StageMapper,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Command:  []string{"--security-mapper"},
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageMapper},
				{Name: security.EnvScanID, Value: run.ID},
				{Name: security.EnvScannerPolicyVersion, Value: security.ScannerPolicyVersion},
				{Name: security.EnvPolicyDigest, Value: policy.Digest},
				{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
				{Name: security.EnvScanBaseCommit, Value: run.BaseCommit},
				{Name: security.EnvScanHeadCommit, Value: run.HeadCommit},
				{Name: security.EnvPinnedScanTargetsEnabled, Value: strconv.FormatBool(scanRunUsesPinnedTarget(run))},
			},
			Workspace: &corev1alpha1.WorkspaceConfig{
				GitRepo:      scan.Spec.RepoURL,
				Branch:       security.EffectiveWorkspaceBranch(scan),
				Ref:          security.EffectiveRef(scan),
				GitSecretRef: scan.Spec.GitSecretRef,
				SubPath:      scan.Spec.SubPath,
				ForkRepo:     scan.Spec.ForkRepo,
				PRBaseBranch: scan.Spec.PRBaseBranch,
			},
		},
	}
	if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrValidateSecurityTask(ctx, scan, task); err != nil {
		return err
	}
	return nil
}

type latestScanPipelineState struct {
	hasThreatModelTasks     bool
	hasSucceededThreatModel bool
	hasMapperTasks          bool
	hasSucceededMapper      bool
	hasReviewTasks          bool
	hasActiveTask           bool
}

func latestScanPipelineStateForRun(tasks []corev1alpha1.Task, scanID string) latestScanPipelineState {
	state := latestScanPipelineState{}
	for i := range tasks {
		task := &tasks[i]
		if scanTaskRunID(task) != scanID {
			continue
		}
		switch taskSecurityStage(task) {
		case security.StageThreatModel:
			state.hasThreatModelTasks = true
			state.hasSucceededThreatModel = task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || state.hasSucceededThreatModel
		case security.StageMapper:
			state.hasMapperTasks = true
			state.hasSucceededMapper = task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || state.hasSucceededMapper
		case security.StageReview:
			state.hasReviewTasks = true
		}
		if isActiveTaskPhase(task.Status.Phase) {
			state.hasActiveTask = true
		}
	}
	return state
}

func (r *RepositoryScanReconciler) pendingReviewSlices(ctx context.Context, scan *corev1alpha1.RepositoryScan, runID string) ([]store.ReviewSlice, error) {
	const pageSize = 1000
	var all []store.ReviewSlice
	cursor := ""
	for {
		reviewSlices, nextCursor, err := r.SecurityStore.ListReviewSlices(ctx, store.ReviewSliceFilter{
			Namespace:      scan.Namespace,
			RepositoryScan: scan.Name,
			Status:         reviewSliceStatusPending,
			LastScanRunID:  runID,
			Limit:          pageSize,
			Cursor:         cursor,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, reviewSlices...)
		if nextCursor == "" {
			return all, nil
		}
		cursor = nextCursor
	}
}

func reviewSliceMatchesChangedFiles(slice store.ReviewSlice, changedFiles map[string]struct{}) bool {
	for _, file := range slice.OwnedFiles {
		if _, ok := changedFiles[normalizeRepoPath(file.Path)]; ok {
			return true
		}
	}
	if slice.Confidence != confidenceHigh {
		return false
	}
	for _, file := range slice.ContextFiles {
		if _, ok := changedFiles[normalizeRepoPath(file.Path)]; ok {
			return true
		}
	}
	for _, test := range slice.Tests {
		if _, ok := changedFiles[normalizeRepoPath(test.Path)]; ok {
			return true
		}
	}
	return false
}

func normalizeRepoPath(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
}

func attachChangedMetadataToReviewSlice(slice *store.ReviewSlice, changedFiles []string, changedLineRanges []security.ChangedLineRange) {
	if slice == nil {
		return
	}
	slicePaths := reviewSlicePathSet(*slice)
	slice.ChangedFiles = reviewSliceChangedFiles(changedFiles, slicePaths)
	slice.ChangedLineRanges = reviewSliceChangedLineRanges(changedLineRanges, slicePaths)
}

func reviewSlicePathSet(slice store.ReviewSlice) map[string]struct{} {
	paths := map[string]struct{}{}
	for _, file := range slice.Entrypoints {
		if normalized := normalizeRepoPath(file.Path); security.SafeRepoPath(normalized) {
			paths[normalized] = struct{}{}
		}
	}
	for _, file := range slice.OwnedFiles {
		if normalized := normalizeRepoPath(file.Path); security.SafeRepoPath(normalized) {
			paths[normalized] = struct{}{}
		}
	}
	for _, file := range slice.ContextFiles {
		if normalized := normalizeRepoPath(file.Path); security.SafeRepoPath(normalized) {
			paths[normalized] = struct{}{}
		}
	}
	for _, test := range slice.Tests {
		if normalized := normalizeRepoPath(test.Path); security.SafeRepoPath(normalized) {
			paths[normalized] = struct{}{}
		}
	}
	return paths
}

func reviewSliceChangedFiles(changedFiles []string, slicePaths map[string]struct{}) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(changedFiles))
	for _, file := range changedFiles {
		file = normalizeRepoPath(file)
		if !security.SafeRepoPath(file) {
			continue
		}
		if _, ok := slicePaths[file]; !ok {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func reviewSliceChangedLineRanges(changedLineRanges []security.ChangedLineRange, slicePaths map[string]struct{}) []security.ChangedLineRange {
	out := make([]security.ChangedLineRange, 0, len(changedLineRanges))
	for _, lineRange := range changedLineRanges {
		lineRange.Path = normalizeRepoPath(lineRange.Path)
		if !security.SafeRepoPath(lineRange.Path) || lineRange.StartLine <= 0 || lineRange.EndLine < lineRange.StartLine {
			continue
		}
		if _, ok := slicePaths[lineRange.Path]; !ok {
			continue
		}
		out = append(out, lineRange)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].EndLine < out[j].EndLine
	})
	return out
}

func changedFileSet(files []string) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	for _, file := range files {
		file = normalizeRepoPath(file)
		if file == "" {
			continue
		}
		out[file] = struct{}{}
	}
	return out
}

func trustedFindingsRepository(scan *corev1alpha1.RepositoryScan, run *store.ScanRun) security.FindingsV2Repository {
	repo := security.FindingsV2Repository{
		RepoURL: strings.TrimSpace(scan.Spec.RepoURL),
		Branch:  trustedFindingsBranch(scan),
		SubPath: strings.Trim(strings.TrimSpace(scan.Spec.SubPath), "/"),
	}
	if run != nil {
		repo.BaseSHA = run.BaseCommit
		repo.HeadSHA = run.HeadCommit
	}
	return repo
}

func trustedFindingsBranch(scan *corev1alpha1.RepositoryScan) string {
	if branch := strings.TrimSpace(scan.Spec.Branch); branch != "" {
		return branch
	}
	if ref := security.EffectiveRef(scan); ref != "" {
		return "ref:" + ref
	}
	return security.EffectiveBranch(scan)
}

func (r *RepositoryScanReconciler) createReviewTasks(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun, threatModel string, reviewSlices []store.ReviewSlice) error {
	if err := security.ValidateRunRepositoryScanIdentity(run, scan); err != nil {
		return err
	}
	policy, err := security.LoadScannerPolicy(ctx, r.Client, scan.Namespace, scan.Spec)
	if err != nil {
		if run != nil && activeScanRunPhase(run.Phase) && terminalScannerPolicyLoadError(err) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	if err := ensureScanRunPolicyDigest(run, policy); err != nil {
		if errors.Is(err, errScannerPolicyDigestChanged) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)
	analysisAnnotations, err := r.analysisTaskAnnotations(ctx, scan)
	if err != nil {
		return err
	}
	applyRunIsolationFromAnnotations(run, analysisAnnotations)
	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	for _, reviewSlice := range reviewSlices {
		sliceJSON, err := json.Marshal(reviewSlice)
		if err != nil {
			return err
		}
		taskName := security.ScanStageTaskNameForRun(scan.Name, run.Mode, security.StageReview, reviewSlice.ID, run.RunUID)
		if !security.ValidRunUID(run.RunUID) {
			taskName = security.ScanStageTaskName(scan.Name, run.Mode, security.StageReview, reviewSlice.ID)
		}
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:        taskName,
				Namespace:   scan.Namespace,
				Annotations: analysisAnnotations,
				Labels: map[string]string{
					labels.LabelManaged:         "true",
					labels.LabelCreatedBy:       repositorySecurityCreatedBy,
					labels.LabelSecurityTarget:  labels.SelectorValue(scan.Name),
					labels.LabelSecurityScanID:  run.ID,
					labels.LabelSecurityMode:    run.Mode,
					labels.LabelSecurityStage:   security.StageReview,
					labels.LabelSecuritySliceID: reviewSlice.ID,
				},
			},
			Spec: corev1alpha1.TaskSpec{
				Type:     corev1alpha1.TaskTypeAgent,
				AgentRef: &scan.Spec.AnalysisAgentRef,
				Prompt:   security.BuildReviewPrompt(scan, run.Mode, run.BaseCommit, run.HeadCommit, threatModel, reviewSlice, policy.PromptPolicy()),
				Timeout:  &timeout,
				Priority: &priority,
				Env: []corev1.EnvVar{
					{Name: security.EnvReviewSliceJSON, Value: string(sliceJSON)},
					{Name: security.EnvRepositoryScanName, Value: scan.Name},
					{Name: security.EnvStage, Value: security.StageReview},
					{Name: security.EnvScanID, Value: run.ID},
					{Name: security.EnvScannerPolicyVersion, Value: security.ScannerPolicyVersion},
					{Name: security.EnvPolicyDigest, Value: policy.Digest},
					{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
					{Name: security.EnvSliceID, Value: reviewSlice.ID},
				},
				AgentRuntime: &corev1alpha1.AgentRuntimeSpec{Workspace: resolvedWorkspaceForRun(scan, run)},
			},
		}
		if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
			return err
		}
		if err := r.createOrValidateSecurityTask(ctx, scan, task); err != nil {
			return err
		}
	}
	return nil
}

func (r *RepositoryScanReconciler) progressLatestScanRun(ctx context.Context, scan *corev1alpha1.RepositoryScan) (bool, error) {
	if r.Client == nil || r.SecurityStore == nil {
		return false, nil
	}

	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
		}),
	); err != nil {
		return false, err
	}

	tasks.Items = repositoryScanControlledTasks(scan, tasks.Items)
	scanID := latestOwnedScanPipelineRunID(tasks.Items)
	if scanID == "" {
		scanID = strings.TrimSpace(scan.Status.LastScanID)
	}
	if scanID == "" {
		return false, nil
	}

	state := latestScanPipelineStateForRun(tasks.Items, scanID)
	if state.hasActiveTask {
		return false, nil
	}
	run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		return false, err
	}
	if run.Phase == scanRunPhaseFailed {
		return false, nil
	}

	if scanRunUsesPinnedTarget(run) {
		if !state.hasMapperTasks {
			if err := r.createMapperTask(ctx, scan, run); err != nil {
				return false, err
			}
			run.Phase = scanRunPhaseRunning
			run.Summary = "Resolving and inventorying the pinned repository target"
			if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
				return false, err
			}
			return true, nil
		}
		if !state.hasSucceededMapper {
			return false, nil
		}
		if strings.TrimSpace(run.HeadCommit) == "" || strings.TrimSpace(run.TargetReceiptID) == "" {
			return false, r.markScanRunTerminalError(ctx, scan, run, errors.New("mapper completed without a trusted target receipt"))
		}
		if !state.hasThreatModelTasks {
			if err := r.createThreatModelTask(ctx, scan, run); err != nil {
				return false, err
			}
			run.Phase = scanRunPhaseRunning
			run.Summary = "Pinned target verified; threat model started"
			if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
				return false, err
			}
			return true, nil
		}
		if !state.hasSucceededThreatModel {
			return false, nil
		}
		if state.hasReviewTasks {
			if run.Phase == scanRunPhaseSucceeded {
				return false, nil
			}
			return r.retryMissingReviewSliceTasks(ctx, scan, run, tasks.Items)
		}
		return r.progressScanRunAfterMapper(ctx, scan, run)
	}

	if run.Phase == scanRunPhaseSucceeded {
		return false, nil
	}
	if !state.hasSucceededThreatModel {
		return false, nil
	}
	if state.hasReviewTasks {
		return r.retryMissingReviewSliceTasks(ctx, scan, run, tasks.Items)
	}
	if !state.hasMapperTasks {
		if err := r.createMapperTask(ctx, scan, run); err != nil {
			return false, err
		}
		run.Phase = scanRunPhaseRunning
		run.Summary = "Threat model generated; deterministic mapper started"
		if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
			return false, err
		}
		return true, nil
	}
	if !state.hasSucceededMapper {
		return false, nil
	}
	return r.progressScanRunAfterMapper(ctx, scan, run)
}

func (r *RepositoryScanReconciler) retryMissingReviewSliceTasks(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	tasks []corev1alpha1.Task,
) (bool, error) {
	reviewSlices, err := r.pendingReviewSlices(ctx, scan, run.ID)
	if err != nil {
		return false, err
	}
	missing := make([]store.ReviewSlice, 0, len(reviewSlices))
	for _, reviewSlice := range reviewSlices {
		if reviewSliceTaskExists(tasks, run.ID, reviewSlice.ID) {
			continue
		}
		missing = append(missing, reviewSlice)
	}
	if len(missing) == 0 {
		return false, nil
	}

	threatModel, err := r.immutableRunThreatModel(ctx, run)
	if err != nil {
		return false, err
	}
	if err := r.createReviewTasks(ctx, scan, run, threatModel, missing); err != nil {
		return false, err
	}
	run.Summary = fmt.Sprintf("Threat model generated; retrying %d pending review slices", len(missing))
	run.Phase = scanRunPhaseRunning
	run.CompletedAt = nil
	run.ErrorMessage = ""
	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return false, err
	}
	return true, nil
}

func reviewSliceTaskExists(tasks []corev1alpha1.Task, runID, sliceID string) bool {
	for i := range tasks {
		task := &tasks[i]
		if scanTaskRunID(task) != runID || taskSecurityStage(task) != security.StageReview {
			continue
		}
		if strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID]) == sliceID {
			return true
		}
	}
	return false
}

func terminalScannerPolicyLoadError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr apierrors.APIStatus
	if errors.As(err, &statusErr) {
		switch statusErr.Status().Reason {
		case metav1.StatusReasonNotFound,
			metav1.StatusReasonInvalid,
			metav1.StatusReasonBadRequest,
			metav1.StatusReasonForbidden:
			return true
		default:
			return false
		}
	}
	message := err.Error()
	return containsAnyPolicyLoadError(message,
		"name is required",
		" is missing in ConfigMap ",
		"must be labeled or annotated",
		"policy exceeds ",
		"policy appears to contain a secret or token",
	)
}

func containsAnyPolicyLoadError(message string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

func (r *RepositoryScanReconciler) progressScanRunAfterMapper(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) (bool, error) {
	if strings.TrimSpace(run.ErrorMessage) != "" {
		if err := r.refreshScanRunStatus(ctx, scan, run, run.ID, true); err != nil {
			return false, err
		}
		return true, nil
	}

	threatModel, err := r.immutableRunThreatModel(ctx, run)
	if err != nil {
		return false, err
	}

	reviewSlices, err := r.pendingReviewSlices(ctx, scan, run.ID)
	if err != nil {
		return false, err
	}
	if len(reviewSlices) > 0 {
		if err := r.createReviewTasks(ctx, scan, run, threatModel, reviewSlices); err != nil {
			return false, err
		}
		run.Summary = fmt.Sprintf("Threat model generated; %d deterministic review slices started", len(reviewSlices))
		run.Phase = scanRunPhaseRunning
		run.CompletedAt = nil
		run.ErrorMessage = ""
		if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
			return false, err
		}
		return true, nil
	}

	if run.Mode == scanModeIncremental && run.SliceCount > 0 && run.SkippedSliceCount == run.SliceCount {
		now := time.Now()
		run.Phase = scanRunPhaseSucceeded
		run.CompletedAt = &now
		run.ErrorMessage = ""
		if needsNoopScanSummary(run.Summary) {
			run.Summary = "Threat model generated; no changed files matched deterministic review slices"
		}
		if err := r.finalizeNoopScanRunIntegrity(ctx, scan, run); err != nil {
			return false, err
		}
		if err := r.persistNoopScanRun(ctx, run); err != nil {
			return false, err
		}
		if err := r.updateNoopScanStatus(ctx, scan, run); err != nil {
			return false, err
		}
		return true, nil
	}

	now := time.Now()
	run.Phase = scanRunPhaseSucceeded
	run.CompletedAt = &now
	run.ErrorMessage = ""
	if needsNoopScanSummary(run.Summary) {
		run.Summary = "Threat model generated; no reviewable security slices found"
	}
	if err := r.finalizeNoopScanRunIntegrity(ctx, scan, run); err != nil {
		return false, err
	}
	if err := r.persistNoopScanRun(ctx, run); err != nil {
		return false, err
	}
	if err := r.updateNoopScanStatus(ctx, scan, run); err != nil {
		return false, err
	}

	return true, nil
}

func (r *RepositoryScanReconciler) persistNoopScanRun(ctx context.Context, run *store.ScanRun) error {
	if run == nil {
		return errors.New("scan run is required")
	}
	if run.Quality.BundleStatus != store.BundleStatusSealed {
		return r.SecurityStore.UpdateScanRun(ctx, run)
	}
	storedRun, err := r.SecurityStore.GetScanRun(ctx, run.Namespace, run.ID)
	if err != nil {
		return err
	}
	if storedRun.Quality.BundleStatus != store.BundleStatusSealed {
		return r.SecurityStore.UpdateScanRun(ctx, run)
	}
	*run = *storedRun
	return nil
}

func (r *RepositoryScanReconciler) finalizeNoopScanRunIntegrity(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) error {
	if r.IntegrityConfig.FindingObservationWrites {
		if err := r.finalizeRunOccurrences(ctx, scan, run); err != nil {
			return err
		}
	}
	return r.maybeSealRunBundle(ctx, scan, run)
}

func (r *RepositoryScanReconciler) updateNoopScanStatus(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	counts, err := r.SecurityStore.GetFindingCounts(ctx, scan.Namespace, scan.Name)
	if err != nil {
		return err
	}

	var threatModelVersion int64
	if model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err == nil && threatModelMatchesRepositoryScan(model, scan) {
		threatModelVersion = model.Version
	}

	return r.updateStatusWithRetryChecked(ctx, scan, func(s *corev1alpha1.RepositoryScan) (bool, error) {
		latestRunID, err := r.latestSecurityScanRunID(ctx, s.Namespace, s.Name)
		if err != nil {
			return false, err
		}
		currentRun, err := r.SecurityStore.GetScanRun(ctx, s.Namespace, run.ID)
		if err != nil {
			return false, err
		}
		if latestRunID != currentRun.ID || scanRunExplicitlyMismatchesRepositoryScan(currentRun, s) ||
			currentRun.Phase != scanRunPhaseSucceeded {
			return false, nil
		}
		run := currentRun
		runMatchesScan := scanRunMatchesRepositoryScan(run, s)
		qualityProjectionReady := r.IntegrityConfig.QualityStateWritesEnabled && runMatchesScan
		if qualityProjectionReady {
			s.Status.Quality = repositoryScanQualityStatus(run, s)
		} else if r.IntegrityConfig.QualityStateWritesEnabled {
			setRepositoryScanQualityUnbound(s)
		} else {
			s.Status.Quality = nil
			meta.RemoveStatusCondition(&s.Status.Conditions, "QualityReady")
		}
		s.Status.Phase = repositoryScanPhaseReady
		s.Status.LastScanID = run.ID
		s.Status.LastScanTaskName = run.TaskName
		s.Status.LastObservedHeadSHA = run.HeadCommit
		s.Status.LastProcessedCommit = run.HeadCommit
		if runMatchesScan && run.Quality.InventoryCoverageStatus == store.CoverageStatusComplete &&
			run.Quality.CandidateCoverageStatus == store.CoverageStatusComplete &&
			run.Quality.TargetVerification == store.TargetVerificationVerified {
			s.Status.LastCompleteCoverageCommit = run.HeadCommit
		}
		if runMatchesScan && run.Quality.BundleStatus == store.BundleStatusSealed {
			s.Status.LastBundleSealedCommit = run.HeadCommit
		}
		if runMatchesScan && scanRunAssuranceQualified(run) {
			s.Status.LastAssuranceQualifiedCommit = run.HeadCommit
		}
		s.Status.ThreatModelVersion = threatModelVersion
		s.Status.FindingCounts = corev1alpha1.FindingCountsStatus{
			Total:    counts.Total,
			Critical: counts.Critical,
			High:     counts.High,
			Medium:   counts.Medium,
			Low:      counts.Low,
		}
		if run.CompletedAt != nil {
			completedAt := &metav1.Time{Time: *run.CompletedAt}
			s.Status.LastScanAt = completedAt
			s.Status.LastSuccessfulScanAt = completedAt
		}
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "ScanSucceeded",
			Message:            repositoryScanConditionMessage(run.Summary, "scan completed successfully"),
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: s.Generation,
		})
		if qualityProjectionReady {
			qualityStatus := metav1.ConditionTrue
			reason := "QualityComplete"
			message := "Scan quality requirements are satisfied"
			if scanRunQualityDegraded(run) || (security.EffectiveCompletionPolicy(scan) == "validated" && !scanRunAssuranceQualified(run)) {
				qualityStatus = metav1.ConditionFalse
				reason = qualityConditionReasonDegraded
				message = "Discovery completed, but target, coverage, validation, isolation, authorization, or bundle quality is degraded"
			}
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type: "QualityReady", Status: qualityStatus, Reason: reason, Message: message,
				LastTransitionTime: metav1.Now(), ObservedGeneration: s.Generation,
			})
		}
		return true, nil
	})
}

func (r *RepositoryScanReconciler) latestSecurityScanRunID(ctx context.Context, namespace, repositoryScan string) (string, error) {
	if r.SecurityStore == nil {
		return "", nil
	}
	runs, _, err := r.SecurityStore.ListScanRuns(ctx, namespace, repositoryScan, 1, "")
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "", nil
	}
	return runs[0].ID, nil
}

func needsNoopScanSummary(summary string) bool {
	trimmed := strings.TrimSpace(summary)
	return trimmed == "" || trimmed == scanSummaryThreatModelPending
}

func (r *RepositoryScanReconciler) ingestOwnedTasks(ctx context.Context, scan *corev1alpha1.RepositoryScan) error {
	if r.SecurityStore == nil {
		return nil
	}

	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{labels.LabelSecurityTarget: labels.SelectorValue(scan.Name)}),
	); err != nil {
		return err
	}

	tasks.Items = repositoryScanControlledTasks(scan, tasks.Items)
	slices.SortFunc(tasks.Items, func(a, b corev1alpha1.Task) int {
		if cmp := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Name, b.Name)
	})

	latestScanRunID := ""

	for i := range tasks.Items {
		task := &tasks.Items[i]
		switch task.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		default:
			continue
		}

		stage := taskSecurityStage(task)
		if stage == security.StagePatch {
			if err := r.ingestPatchTask(ctx, scan, task); err != nil {
				return err
			}
			continue
		}
		if stage == security.StageValidation {
			if err := r.ingestValidationTask(ctx, scan, task); err != nil {
				return err
			}
			continue
		}

		if !isScanPipelineStage(stage) {
			continue
		}
		run, err := r.ingestReservedScanTask(ctx, scan, task)
		if err != nil {
			return err
		}
		if run != nil {
			latestScanRunID = run.ID
		}
	}

	if latestScanRunID != "" {
		run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, latestScanRunID)
		if err != nil {
			return err
		}
		return r.refreshScanRunStatus(ctx, scan, run, latestScanRunID, true)
	}

	return nil
}

func isTerminalScanTask(task corev1alpha1.Task) bool {
	if task.Labels[labels.LabelSecurityFindingID] != "" {
		return false
	}
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func latestTerminalScanTask(tasks []corev1alpha1.Task) *corev1alpha1.Task {
	var latest *corev1alpha1.Task
	for i := range tasks {
		task := &tasks[i]
		if !isTerminalScanTask(*task) {
			continue
		}
		if latest == nil {
			latest = task
			continue
		}
		if task.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = task
			continue
		}
		if task.CreationTimestamp.Equal(&latest.CreationTimestamp) && task.Name > latest.Name {
			latest = task
		}
	}
	return latest
}

func taskPhaseToSecurityPhase(phase corev1alpha1.TaskPhase) string {
	if phase == corev1alpha1.TaskPhaseSucceeded {
		return scanRunPhaseSucceeded
	}
	if phase == corev1alpha1.TaskPhaseFailed {
		return scanRunPhaseFailed
	}
	if phase == corev1alpha1.TaskPhaseRunning {
		return scanRunPhaseRunning
	}
	return scanRunPhasePending
}

type validationTaskArtifacts struct {
	artifact   security.ValidationArtifact
	rawSource  []byte
	rawJSON    string
	transcript string
}

func (r *RepositoryScanReconciler) persistThreatModelIfChanged(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	scanID string,
	scanStartedAt time.Time,
	content string,
) error {
	if r.SecurityStore == nil {
		return nil
	}

	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	latest, latestErr := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
	if latestErr != nil && !errors.Is(latestErr, store.ErrNotFound) {
		return latestErr
	}
	if latestErr == nil && !threatModelMatchesRepositoryScan(latest, scan) {
		latest = nil
		latestErr = store.ErrNotFound
	}
	if latestErr == nil {
		if strings.TrimSpace(latest.Content) == content {
			return nil
		}
		if latest.GeneratedByScan != scanID && !scanStartedAt.IsZero() && scanStartedAt.Before(latest.UpdatedAt) {
			return nil
		}
	}

	model := &store.ThreatModel{
		Namespace:                scan.Namespace,
		RepositoryScan:           scan.Name,
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		Content:                  content,
		Source:                   "generated",
		GeneratedByScan:          scanID,
	}
	if err := r.SecurityStore.SaveThreatModel(ctx, model); err != nil {
		return err
	}

	return nil
}

func threatModelLooksLikeToolTranscript(content string) bool {
	for _, marker := range []string{
		"<tool_call>",
		"</tool_call>",
		"<tool_name>",
		"</tool_name>",
		"<parameters>",
		"</parameters>",
		"<command>",
		"</command>",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

const (
	securityControllerIngestionVersion = "security-integrity-v1"
	gitObjectFormatSHA1                = "sha1"
	gitObjectFormatSHA256              = "sha256"
)

func securityDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func canonicalSecurityJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func securityReceiptID(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "rcpt_" + hex.EncodeToString(digest[:])
}

func (r *RepositoryScanReconciler) stageReceiptIDFor(
	_ context.Context,
	run *store.ScanRun,
	task *corev1alpha1.Task,
	artifactName string,
	raw []byte,
	disposition store.StageReceiptDisposition,
) string {
	if run == nil || task == nil {
		return ""
	}
	rawDigest := ""
	if len(raw) > 0 {
		rawDigest = securityDigest(raw)
	}
	return securityReceiptID(
		run.RunUID, string(task.UID), strconv.FormatInt(harnessWrapperOutputAttempt(task), 10),
		task.Status.JobName, strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]),
		strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]),
		taskSecurityStage(task), artifactName, rawDigest, string(disposition),
	)
}

func securityTargetReceiptID(runUID, targetDigest string) string {
	targetIDSum := sha256.Sum256([]byte(runUID + "\x00" + targetDigest))
	return "target_" + hex.EncodeToString(targetIDSum[:])
}

func stageReceiptExpectedTargetSHA(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	workspaceRef := ""
	if task.Spec.Workspace != nil {
		workspaceRef = task.Spec.Workspace.Ref
	} else if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
		workspaceRef = task.Spec.AgentRuntime.Workspace.Ref
	}
	objectID, _ := security.NormalizeFullGitObjectID(workspaceRef)
	return objectID
}

func stageReceiptTargetBinding(
	task *corev1alpha1.Task,
	run *store.ScanRun,
	artifactName string,
	normalized []byte,
) (expectedTargetSHA, observedTargetSHA, targetReceiptID string, err error) {
	if task == nil || run == nil {
		return "", "", "", nil
	}
	expectedTargetSHA = stageReceiptExpectedTargetSHA(task)
	switch taskSecurityStage(task) {
	case security.StageThreatModel:
		// Threat modeling runs before trusted mapper target resolution. Never
		// project a later run-level target receipt back onto this attempt.
		return expectedTargetSHA, "", "", nil
	case security.StageMapper:
		if artifactName != security.ArtifactSlices || len(normalized) == 0 {
			return expectedTargetSHA, "", "", nil
		}
		var artifact security.ReviewSlicesArtifact
		if err := json.Unmarshal(normalized, &artifact); err != nil {
			return "", "", "", fmt.Errorf("decode normalized mapper target binding: %w", err)
		}
		observed := strings.TrimSpace(artifact.HeadCommit)
		if artifact.TargetReceipt != nil {
			observed = strings.TrimSpace(artifact.TargetReceipt.HeadOID)
			targetBytes, err := json.Marshal(artifact.TargetReceipt)
			if err != nil {
				return "", "", "", fmt.Errorf("encode mapper target receipt binding: %w", err)
			}
			targetReceiptID = securityTargetReceiptID(run.RunUID, securityDigest(targetBytes))
		}
		if observed != "" {
			var ok bool
			observedTargetSHA, ok = security.NormalizeFullGitObjectID(observed)
			if !ok {
				return "", "", "", fmt.Errorf("mapper observed target %q is not a full Git object ID", observed)
			}
		}
		return expectedTargetSHA, observedTargetSHA, targetReceiptID, nil
	default:
		// Post-mapper tasks are created only after this run-level receipt is
		// fixed, and their workspace ref pins expectedTargetSHA independently.
		return expectedTargetSHA, "", run.TargetReceiptID, nil
	}
}

func (r *RepositoryScanReconciler) appendStageReceiptCreated(
	ctx context.Context,
	task *corev1alpha1.Task,
	run *store.ScanRun,
	artifactName string,
	raw []byte,
	normalized []byte,
	disposition store.StageReceiptDisposition,
	reasonCode string,
	reason string,
) (bool, error) {
	if r.IntegrityStore == nil || task == nil || run == nil || !security.ValidRunUID(run.RunUID) || task.UID == "" {
		return false, nil
	}
	startedAt := task.CreationTimestamp.Time
	if startedAt.IsZero() {
		startedAt = run.StartedAt
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	ingestedAt := startedAt
	var completedAt *time.Time
	if task.Status.CompletionTime != nil {
		completed := task.Status.CompletionTime.UTC()
		completedAt = &completed
		ingestedAt = completed
	}
	provenance := store.ExecutionProvenance{
		Kind: store.ExecutionProvenanceKubernetes,
		Kubernetes: &store.KubernetesExecutionProvenance{
			TaskName: task.Name,
			TaskUID:  string(task.UID),
			Attempt:  int64(task.Status.Attempts),
			JobName:  task.Status.JobName,
		},
	}
	if task.Annotations != nil {
		runtimeSessionID := strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation])
		turnID := strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])
		correlationID := strings.TrimSpace(task.Annotations["orka.ai/harness-wrapper-correlation-id"])
		if runtimeSessionID != "" && turnID != "" && correlationID != "" {
			provenance = store.ExecutionProvenance{
				Kind: store.ExecutionProvenanceHarness,
				Harness: &store.HarnessExecutionProvenance{
					RuntimeSessionID: runtimeSessionID,
					TurnID:           turnID,
					CorrelationID:    correlationID,
				},
			}
		}
	}
	sourceGeneration := int64(0)
	sourceSize := int64(len(raw))
	sourceDigest := ""
	if bound, ok := r.ArtifactStore.(store.BoundOutputStore); ok && artifactName != "" {
		if artifact, err := bound.GetBoundArtifact(ctx, task.Namespace, task.Name, artifactName, string(task.UID), harnessWrapperOutputAttempt(task)); err == nil {
			if len(raw) > 0 && (artifact.Provenance.ContentSHA256 != securityDigest(raw) || artifact.Provenance.ContentSize != int64(len(raw))) {
				return false, fmt.Errorf("security artifact %s changed after parsing; refusing mixed-generation receipt", artifactName)
			}
			sourceGeneration = artifact.Provenance.StagingGeneration
			sourceSize = artifact.Provenance.ContentSize
			sourceDigest = artifact.Provenance.ContentSHA256
			if provenance.Kubernetes != nil {
				provenance.Kubernetes.Attempt = artifact.Provenance.TaskAttempt
				provenance.Kubernetes.JobUID = artifact.Provenance.JobUID
				provenance.Kubernetes.PodUID = artifact.Provenance.PodUID
			}
		}
	}
	attestation := store.AnalysisAttestationDelivered
	if taskSecurityStage(task) == security.StageMapper {
		attestation = store.AnalysisAttestationToolObserved
	}
	expectedTargetSHA, observedTargetSHA, targetReceiptID, err := stageReceiptTargetBinding(task, run, artifactName, normalized)
	if err != nil {
		return false, err
	}
	rawDigest := sourceDigest
	if rawDigest == "" && len(raw) > 0 {
		rawDigest = securityDigest(raw)
	}
	normalizedDigest := ""
	if len(normalized) > 0 {
		normalizedDigest = securityDigest(normalized)
	}
	scopeKind, scopeID, err := securityTaskScope(task)
	if err != nil {
		return false, err
	}
	receipt := &store.StageReceipt{
		ID:                         r.stageReceiptIDFor(ctx, run, task, artifactName, raw, disposition),
		Namespace:                  run.Namespace,
		RepositoryScan:             run.RepositoryScan,
		ScanRunID:                  run.ID,
		RunUID:                     run.RunUID,
		Stage:                      taskSecurityStage(task),
		ScopeKind:                  scopeKind,
		ScopeID:                    scopeID,
		Provenance:                 provenance,
		ExpectedTargetSHA:          expectedTargetSHA,
		ObservedTargetSHA:          observedTargetSHA,
		TargetReceiptID:            targetReceiptID,
		AttestationLevel:           attestation,
		ScannerPolicyDigest:        run.PolicyDigest,
		SourceArtifactName:         artifactName,
		SourceArtifactMediaType:    securityArtifactMediaType(artifactName),
		SourceArtifactSize:         sourceSize,
		SourceArtifactGeneration:   sourceGeneration,
		SourceArtifactDigest:       rawDigest,
		ControllerIngestionVersion: securityControllerIngestionVersion,
		NormalizedOutputDigest:     normalizedDigest,
		Disposition:                disposition,
		ReasonCode:                 reasonCode,
		Reason:                     reason,
		StartedAt:                  startedAt,
		IngestedAt:                 ingestedAt,
		CompletedAt:                completedAt,
	}
	return r.IntegrityStore.AppendStageReceipt(ctx, receipt)
}

func (r *RepositoryScanReconciler) appendStageReceipt(
	ctx context.Context,
	task *corev1alpha1.Task,
	run *store.ScanRun,
	artifactName string,
	raw []byte,
	normalized []byte,
	disposition store.StageReceiptDisposition,
	reasonCode string,
	reason string,
) error {
	_, err := r.appendStageReceiptCreated(
		ctx, task, run, artifactName, raw, normalized, disposition, reasonCode, reason,
	)
	return err
}

func securityArtifactMediaType(name string) string {
	switch name {
	case security.ArtifactThreatModel:
		return "text/markdown"
	case security.ArtifactValidationText:
		return "text/plain"
	default:
		return "application/json"
	}
}

func taskSecurityBoundID(task *corev1alpha1.Task, labelKey, envName, kind string) (string, error) {
	if task == nil {
		return "", nil
	}
	labelValue := strings.TrimSpace(task.Labels[labelKey])
	var binding *corev1.EnvVar
	for i := range task.Spec.Env {
		if task.Spec.Env[i].Name != envName {
			continue
		}
		if binding != nil {
			return "", fmt.Errorf("security task has duplicate %s environment bindings", kind)
		}
		binding = &task.Spec.Env[i]
	}
	if binding == nil {
		return labelValue, nil
	}
	if binding.ValueFrom != nil {
		return "", fmt.Errorf("security task %s environment binding must be literal", kind)
	}
	fullID := strings.TrimSpace(binding.Value)
	if binding.Value != fullID {
		return "", fmt.Errorf("security task %s environment binding is not canonical", kind)
	}
	if labelValue != labels.SelectorValue(fullID) {
		return "", fmt.Errorf("security task %s label does not match its environment binding", kind)
	}
	return fullID, nil
}

func taskSecurityScanRunID(task *corev1alpha1.Task) (string, error) {
	return taskSecurityBoundID(task, labels.LabelSecurityScanID, security.EnvScanID, "scan run ID")
}

func taskSecurityFindingID(task *corev1alpha1.Task) (string, error) {
	return taskSecurityBoundID(task, labels.LabelSecurityFindingID, security.EnvFindingID, "finding ID")
}

func taskSecurityOccurrenceID(task *corev1alpha1.Task) (string, error) {
	return taskSecurityBoundID(task, labels.LabelSecurityOccurrenceID, security.EnvOccurrenceID, "occurrence ID")
}

func securityTaskScope(task *corev1alpha1.Task) (string, string, error) {
	occurrenceID, err := taskSecurityOccurrenceID(task)
	if err != nil {
		return "", "", err
	}
	if occurrenceID != "" {
		return "occurrence", occurrenceID, nil
	}
	if task != nil {
		if sliceID := strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID]); sliceID != "" {
			return "slice", sliceID, nil
		}
	}
	findingID, err := taskSecurityFindingID(task)
	if err != nil {
		return "", "", err
	}
	if findingID != "" {
		return "finding", findingID, nil
	}
	return "run", "", nil
}

func (r *RepositoryScanReconciler) saveControllerArtifact(
	ctx context.Context,
	task *corev1alpha1.Task,
	filename string,
	contentType string,
	data []byte,
) error {
	if r.ArtifactStore == nil || task == nil {
		return nil
	}
	mode := r.IntegrityConfig.WorkerOutputBindingMode
	securityTask := strings.TrimSpace(task.Labels[labels.LabelCreatedBy]) == repositorySecurityCreatedBy
	useBound := securityTask && mode != "" && mode != security.WorkerOutputBindingOff
	if bound, ok := r.ArtifactStore.(store.BoundOutputStore); useBound && ok && task.UID != "" {
		attempt := harnessWrapperOutputAttempt(task)
		binding := sha256.Sum256([]byte(strings.Join([]string{
			"controller-artifact-v1", string(task.UID), strconv.FormatInt(attempt, 10), filename,
		}, "\x00")))
		return bound.SaveBoundArtifact(ctx, &store.BoundArtifact{
			Namespace: task.Namespace, TaskName: task.Name, Filename: filename, ContentType: contentType, Data: data,
			Provenance: store.OutputProvenance{
				TaskUID: string(task.UID), TaskAttempt: attempt, ProducerKind: store.OutputProducerController,
				SubmissionNonceDigest: "sha256:" + hex.EncodeToString(binding[:]),
			},
		})
	}
	if securityTask && (mode == security.WorkerOutputBindingEnforce || r.IntegrityConfig.PinnedScanTargetsEnabled) {
		return fmt.Errorf("bound artifact storage is unavailable for repository-security task")
	}
	return r.ArtifactStore.SaveArtifact(ctx, task.Namespace, task.Name, filename, contentType, data)
}

func (r *RepositoryScanReconciler) taskArtifacts(ctx context.Context, task *corev1alpha1.Task) ([]store.ArtifactMetadata, error) {
	if r.ArtifactStore == nil || task == nil {
		return nil, store.ErrNotFound
	}
	mode := r.IntegrityConfig.WorkerOutputBindingMode
	securityTask := strings.TrimSpace(task.Labels[labels.LabelCreatedBy]) == repositorySecurityCreatedBy
	strictRead := mode == security.WorkerOutputBindingEnforce || r.IntegrityConfig.PinnedScanTargetsEnabled
	if securityTask && (mode != "" && mode != security.WorkerOutputBindingOff || strictRead) {
		if bound, ok := r.ArtifactStore.(store.BoundOutputStore); ok {
			items, err := bound.ListBoundArtifacts(ctx, task.Namespace, task.Name, string(task.UID), harnessWrapperOutputAttempt(task))
			if err == nil && (len(items) > 0 || strictRead) {
				return items, nil
			}
			if strictRead {
				return nil, err
			}
		} else if strictRead {
			return nil, fmt.Errorf("bound artifact storage is unavailable")
		}
	}
	return r.ArtifactStore.ListArtifacts(ctx, task.Namespace, task.Name)
}

func (r *RepositoryScanReconciler) taskArtifact(
	ctx context.Context,
	task *corev1alpha1.Task,
	filename string,
) ([]byte, error) {
	if r.ArtifactStore == nil || task == nil {
		return nil, store.ErrNotFound
	}
	mode := r.IntegrityConfig.WorkerOutputBindingMode
	securityTask := strings.TrimSpace(task.Labels[labels.LabelCreatedBy]) == repositorySecurityCreatedBy
	strictRead := mode == security.WorkerOutputBindingEnforce || r.IntegrityConfig.PinnedScanTargetsEnabled
	if securityTask && (mode != "" && mode != security.WorkerOutputBindingOff || strictRead) {
		if bound, ok := r.ArtifactStore.(store.BoundOutputStore); ok {
			artifact, err := bound.GetBoundArtifact(ctx, task.Namespace, task.Name, filename, string(task.UID), harnessWrapperOutputAttempt(task))
			if err == nil {
				return artifact.Data, nil
			}
			if strictRead {
				metrics.RecordSecurityOutputWrite("artifact_read", string(mode), "denied", "provenance_mismatch")
				return nil, err
			}
		} else if strictRead {
			return nil, fmt.Errorf("bound artifact storage is unavailable")
		}
	}
	data, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, filename)
	if securityTask && mode == security.WorkerOutputBindingAudit && err == nil {
		metrics.RecordSecurityOutputWrite("artifact_read", string(mode), "accepted_legacy_unverified", "provenance_mismatch")
	}
	return data, err
}

func (r *RepositoryScanReconciler) taskResult(ctx context.Context, task *corev1alpha1.Task) ([]byte, error) {
	if r.ResultStore == nil || task == nil {
		return nil, store.ErrNotFound
	}
	mode := r.IntegrityConfig.WorkerOutputBindingMode
	securityTask := strings.TrimSpace(task.Labels[labels.LabelCreatedBy]) == repositorySecurityCreatedBy
	strictRead := mode == security.WorkerOutputBindingEnforce || r.IntegrityConfig.PinnedScanTargetsEnabled
	if securityTask && (mode != "" && mode != security.WorkerOutputBindingOff || strictRead) {
		if bound, ok := r.ResultStore.(store.BoundOutputStore); ok {
			result, err := bound.GetBoundResult(ctx, task.Namespace, task.Name, string(task.UID), harnessWrapperOutputAttempt(task))
			if err == nil {
				return result.Data, nil
			}
			if strictRead {
				metrics.RecordSecurityOutputWrite("result_read", string(mode), "denied", "provenance_mismatch")
				return nil, err
			}
		} else if strictRead {
			return nil, fmt.Errorf("bound result storage is unavailable")
		}
	}
	data, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
	if securityTask && mode == security.WorkerOutputBindingAudit && err == nil {
		metrics.RecordSecurityOutputWrite("result_read", string(mode), "accepted_legacy_unverified", "provenance_mismatch")
	}
	return data, err
}

func (r *RepositoryScanReconciler) getArtifactWithRetry(ctx context.Context, task *corev1alpha1.Task, filename string) ([]byte, error) {
	var lastErr error
	for range 5 {
		data, err := r.taskArtifact(ctx, task, filename)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, lastErr
}

func (r *RepositoryScanReconciler) loadThreatModelArtifact(
	ctx context.Context,
	task *corev1alpha1.Task,
) (string, []byte, string, error) {
	if r.ArtifactStore == nil {
		return "", nil, "", nil
	}

	threatModelData, err := r.getArtifactWithRetry(ctx, task, security.ArtifactThreatModel)
	switch {
	case err == nil:
		content := strings.TrimSpace(string(threatModelData))
		if content == "" {
			return "", threatModelData, fmt.Sprintf("%s is empty", security.ArtifactThreatModel), nil
		}
		if threatModelLooksLikeToolTranscript(content) {
			return "", threatModelData, fmt.Sprintf("%s looks like tool transcript, not markdown", security.ArtifactThreatModel), nil
		}
		return content, threatModelData, "", nil
	case errors.Is(err, store.ErrNotFound):
		content, ok, resultErr := r.threatModelFromTaskResult(ctx, task)
		if resultErr != nil {
			return "", nil, "", resultErr
		}
		if ok {
			return content, []byte(content), "", nil
		}
		return "", nil, fmt.Sprintf("%s is missing", security.ArtifactThreatModel), nil
	default:
		return "", nil, "", err
	}
}

func (r *RepositoryScanReconciler) threatModelFromTaskResult(ctx context.Context, task *corev1alpha1.Task) (string, bool, error) {
	if r.ResultStore == nil || task == nil {
		return "", false, nil
	}
	data, err := r.taskResult(ctx, task)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(data) > maxThreatModelFallbackBytes {
		return "", false, nil
	}
	content := strings.TrimSpace(string(data))
	if !strings.HasPrefix(content, "#") || threatModelLooksLikeToolTranscript(content) {
		return "", false, nil
	}
	return content, true, nil
}

func (r *RepositoryScanReconciler) loadDiscoveryFindingsV2Artifact(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*security.FindingsV2Artifact, *security.ReviewContextManifest, []byte, []byte, string, error) {
	if r.ArtifactStore == nil {
		return nil, nil, nil, nil, "", nil
	}

	findingsData, err := r.taskArtifact(ctx, task, security.ArtifactFindingsV2)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(findingsData))) == 0 {
			return nil, nil, findingsData, nil, fmt.Sprintf("%s is empty", security.ArtifactFindingsV2), nil
		}
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, nil, nil, "", nil
	default:
		return nil, nil, nil, nil, "", err
	}

	findings, err := security.ParseFindingsV2Artifact(findingsData)
	if err != nil {
		return nil, nil, findingsData, nil, fmt.Sprintf("%s is invalid: %v", security.ArtifactFindingsV2, err), nil
	}
	trustedSliceID := strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID])
	artifactSliceID := strings.TrimSpace(findings.Scan.SliceID)
	if trustedSliceID == "" {
		return nil, nil, findingsData, nil, "v2 findings require trusted security slice task label", nil
	}
	if artifactSliceID == "" {
		return nil, nil, findingsData, nil, "v2 findings artifact missing scan.sliceId", nil
	}
	if artifactSliceID != trustedSliceID {
		return nil, nil, findingsData, nil, fmt.Sprintf("v2 findings scan.sliceId %q does not match task slice %q", artifactSliceID, trustedSliceID), nil
	}
	contextName := security.ReviewContextArtifactName(trustedSliceID)
	contextData, err := r.taskArtifact(ctx, task, contextName)
	switch {
	case err == nil:
		manifest, err := security.ParseReviewContextManifest(contextData)
		if err != nil {
			return nil, nil, findingsData, contextData, fmt.Sprintf("%s is invalid: %v", contextName, err), nil
		}
		if strings.TrimSpace(manifest.SliceID) != trustedSliceID {
			return nil, nil, findingsData, contextData, fmt.Sprintf("%s sliceId %q does not match task slice %q", contextName, manifest.SliceID, trustedSliceID), nil
		}
		return findings, manifest, findingsData, contextData, "", nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, findingsData, nil, fmt.Sprintf("%s is missing", contextName), nil
	default:
		return nil, nil, findingsData, nil, "", err
	}
}

func (r *RepositoryScanReconciler) loadReviewSlicesArtifact(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*security.ReviewSlicesArtifact, []byte, string, error) {
	if r.ArtifactStore == nil {
		return nil, nil, "", nil
	}

	data, err := r.taskArtifact(ctx, task, security.ArtifactSlices)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, data, fmt.Sprintf("%s is empty", security.ArtifactSlices), nil
		}
		artifact, err := security.ParseReviewSlicesArtifact(data)
		if err != nil {
			return nil, data, fmt.Sprintf("%s is invalid: %v", security.ArtifactSlices, err), nil
		}
		return artifact, data, "", nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, fmt.Sprintf("%s is missing", security.ArtifactSlices), nil
	default:
		return nil, nil, "", err
	}
}

func validationTaskRunBinding(task *corev1alpha1.Task) (required bool, supported bool) {
	if task == nil || task.Annotations == nil {
		return false, true
	}
	version := strings.TrimSpace(task.Annotations[security.AnnotationValidationBindingVersion])
	if version == "" {
		return false, true
	}
	return version == security.ValidationBindingVersion, version == security.ValidationBindingVersion
}

func validationRepositorySubPath(scan *corev1alpha1.RepositoryScan) (string, error) {
	if scan == nil {
		return "", nil
	}
	raw := strings.TrimSpace(strings.ReplaceAll(scan.Spec.SubPath, "\\", "/"))
	raw = strings.Trim(raw, "/")
	if raw == "" || raw == "." {
		return "", nil
	}
	if cleaned := path.Clean(raw); cleaned != raw || !security.SafeRepoPath(raw) {
		return "", fmt.Errorf("RepositoryScan subPath %q is not a canonical repository path", scan.Spec.SubPath)
	}
	return raw, nil
}

func canonicalScopedValidationPath(scan *corev1alpha1.RepositoryScan, scopedPath string) (string, error) {
	if strings.Contains(scopedPath, "\\") {
		return "", errors.New("validation evidence path must not contain backslashes")
	}
	if cleaned := path.Clean(scopedPath); cleaned != scopedPath || cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("validation evidence path %q is not a canonical repository path", scopedPath)
	}
	subPath, err := validationRepositorySubPath(scan)
	if err != nil {
		return "", err
	}
	if subPath == "" {
		return scopedPath, nil
	}
	return subPath + "/" + scopedPath, nil
}

func canonicalValidationEvidencePath(
	scan *corev1alpha1.RepositoryScan,
	finding *store.Finding,
	evidencePath string,
) (string, error) {
	if finding == nil {
		return "", errors.New("discovery finding is unavailable")
	}
	matches := map[string]struct{}{}
	consider := func(candidate string) error {
		if candidate == "" {
			return nil
		}
		canonical, err := canonicalScopedValidationPath(scan, candidate)
		if err != nil {
			return err
		}
		if evidencePath == candidate || evidencePath == canonical {
			matches[canonical] = struct{}{}
		}
		return nil
	}
	if err := consider(finding.FilePath); err != nil {
		return "", err
	}
	for _, ref := range finding.Evidence {
		if ref.Kind != findingEvidenceKindFile {
			continue
		}
		if err := consider(ref.Path); err != nil {
			return "", err
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("file evidence path %q was not accepted for the discovery occurrence", evidencePath)
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("file evidence path %q is ambiguous under RepositoryScan subPath", evidencePath)
	}
	for canonical := range matches {
		return canonical, nil
	}
	return "", fmt.Errorf("file evidence path %q could not be canonicalized", evidencePath)
}

func validationEvidencePathKey(ref store.FindingEvidenceRef) string {
	return strings.Join([]string{ref.Path, strconv.Itoa(ref.StartLine), strconv.Itoa(ref.EndLine)}, "\x00")
}

func (r *RepositoryScanReconciler) loadValidationTaskArtifacts(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
	finding *store.Finding,
	run *store.ScanRun,
) (*validationTaskArtifacts, string, error) {
	if r.ArtifactStore == nil {
		return nil, "", nil
	}

	data, err := r.taskArtifact(ctx, task, security.ArtifactValidation)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, fmt.Sprintf("%s is empty", security.ArtifactValidation), nil
		}
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Sprintf("%s is missing", security.ArtifactValidation), nil
	default:
		return nil, "", err
	}

	trustedFindingID, err := taskSecurityFindingID(task)
	if err != nil {
		return nil, "", err
	}
	trustedOccurrenceID, err := taskSecurityOccurrenceID(task)
	if err != nil {
		return nil, "", err
	}
	trustedScanRunID := strings.TrimSpace(task.Labels[labels.LabelSecurityScanID])
	if finding == nil || trustedFindingID == "" || trustedFindingID != finding.ID {
		return nil, "validation task finding binding is missing or stale", nil
	}
	requireRunBinding, supportedBinding := validationTaskRunBinding(task)
	if !supportedBinding {
		return nil, "validation task binding version is unsupported", nil
	}
	if requireRunBinding && finding.ScanRunID != "" && trustedScanRunID != finding.ScanRunID {
		return nil, fmt.Sprintf("validation task scan run %q does not match current finding scan run %q", trustedScanRunID, finding.ScanRunID), nil
	}
	if requireRunBinding && finding.CurrentOccurrenceID != "" && trustedOccurrenceID != finding.CurrentOccurrenceID {
		return nil, fmt.Sprintf("validation task occurrence %q does not match current finding occurrence %q", trustedOccurrenceID, finding.CurrentOccurrenceID), nil
	}
	canonicalPaths := map[string]string{}
	parsed, err := security.ParseValidationArtifact(data, security.ValidationArtifactParseOptions{
		ExpectedFindingID:    trustedFindingID,
		ExpectedScanRunID:    trustedScanRunID,
		ExpectedOccurrenceID: trustedOccurrenceID,
		TrustedTaskName:      task.Name,
		RequireRunBinding:    requireRunBinding,
		ValidateFileEvidence: func(ref store.FindingEvidenceRef) error {
			canonical, err := canonicalValidationEvidencePath(scan, finding, ref.Path)
			if err != nil {
				return err
			}
			canonicalRef := ref
			canonicalRef.Path = canonical
			if scanRunUsesPinnedTarget(run) {
				if err := r.validateCanonicalFileEvidenceAgainstTarget(ctx, run, canonicalRef); err != nil {
					return err
				}
			}
			canonicalPaths[validationEvidencePathKey(ref)] = canonical
			return nil
		},
		ResolveArtifact: func(name string) (string, int64, error) {
			artifactData, err := r.taskArtifact(ctx, task, name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return "", 0, fmt.Errorf("artifact evidence %q does not exist under the validation task", name)
				}
				return "", 0, err
			}
			digest := sha256.Sum256(artifactData)
			return hex.EncodeToString(digest[:]), int64(len(artifactData)), nil
		},
	})
	if err != nil {
		return nil, fmt.Sprintf("%s is invalid: %v", security.ArtifactValidation, err), nil
	}
	for i := range parsed.Artifact.Evidence {
		ref := &parsed.Artifact.Evidence[i]
		if ref.Kind != findingEvidenceKindFile {
			continue
		}
		canonical := canonicalPaths[validationEvidencePathKey(*ref)]
		if canonical == "" {
			return nil, fmt.Sprintf("%s normalized file evidence path %q is unbound", security.ArtifactValidation, ref.Path), nil
		}
		ref.Path = canonical
	}
	normalizedJSON, err := json.Marshal(parsed.Artifact)
	if err != nil {
		return nil, fmt.Sprintf("%s normalized payload cannot be encoded: %v", security.ArtifactValidation, err), nil
	}
	canonicalJSON, err := canonicalSecurityJSON(normalizedJSON)
	if err != nil {
		return nil, fmt.Sprintf("%s normalized payload is invalid: %v", security.ArtifactValidation, err), nil
	}
	result := &validationTaskArtifacts{
		artifact:  parsed.Artifact,
		rawSource: append([]byte(nil), data...),
		rawJSON:   string(canonicalJSON),
	}
	if transcript, err := r.taskArtifact(ctx, task, security.ArtifactValidationText); err == nil {
		result.transcript = strings.TrimSpace(string(transcript))
	}
	return result, "", nil
}

func (r *RepositoryScanReconciler) loadValidationTargetReceipt(
	ctx context.Context,
	run *store.ScanRun,
) (*security.MapperTargetReceipt, error) {
	if r.TargetReceiptStore == nil || run == nil || strings.TrimSpace(run.TargetReceiptID) == "" {
		return nil, errors.New("trusted target receipt is unavailable")
	}
	stored, err := r.TargetReceiptStore.GetSecurityTargetReceipt(ctx, run.Namespace, run.TargetReceiptID)
	if err != nil {
		return nil, fmt.Errorf("load trusted target receipt: %w", err)
	}
	var receipt security.MapperTargetReceipt
	if err := json.Unmarshal(stored.ReceiptJSON, &receipt); err != nil {
		return nil, fmt.Errorf("decode trusted target receipt: %w", err)
	}
	return &receipt, nil
}

func validateCanonicalFileEvidenceAgainstReceipt(
	receipt *security.MapperTargetReceipt,
	ref store.FindingEvidenceRef,
) error {
	if receipt == nil {
		return errors.New("trusted target receipt is unavailable")
	}
	for _, entry := range receipt.TreeIndex {
		if entry.Path != ref.Path {
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") ||
			entry.Disposition != security.MapperTreeDispositionRegular {
			return fmt.Errorf("file evidence %q is not a receipted regular blob", ref.Path)
		}
		if entry.LineCount <= 0 {
			return fmt.Errorf("file evidence %q has no trusted line-count metadata", ref.Path)
		}
		if ref.StartLine <= 0 || ref.EndLine < ref.StartLine || ref.EndLine > entry.LineCount {
			return fmt.Errorf("file evidence %q line range %d-%d exceeds trusted line count %d",
				ref.Path, ref.StartLine, ref.EndLine, entry.LineCount)
		}
		return nil
	}
	if receipt.TreeIndexTruncated {
		return fmt.Errorf("file evidence %q is outside the bounded trusted tree index", ref.Path)
	}
	return fmt.Errorf("file evidence %q does not exist in the pinned target", ref.Path)
}

func (r *RepositoryScanReconciler) validateCanonicalFileEvidenceAgainstTarget(
	ctx context.Context,
	run *store.ScanRun,
	ref store.FindingEvidenceRef,
) error {
	receipt, err := r.loadValidationTargetReceipt(ctx, run)
	if err != nil {
		return err
	}
	return validateCanonicalFileEvidenceAgainstReceipt(receipt, ref)
}

func validationEvidenceRepoRootCandidates(scan *corev1alpha1.RepositoryScan, evidencePath string) ([]string, error) {
	if strings.Contains(evidencePath, "\\") {
		return nil, errors.New("validation evidence path must not contain backslashes")
	}
	if cleaned := path.Clean(evidencePath); cleaned != evidencePath || cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return nil, fmt.Errorf("validation evidence path %q is not a canonical repository path", evidencePath)
	}
	subPath, err := validationRepositorySubPath(scan)
	if err != nil {
		return nil, err
	}
	candidates := []string{evidencePath}
	if subPath != "" {
		scoped := subPath + "/" + evidencePath
		if scoped != evidencePath {
			candidates = append(candidates, scoped)
		}
	}
	return candidates, nil
}

func (r *RepositoryScanReconciler) validateFileEvidenceAgainstTarget(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	ref store.FindingEvidenceRef,
) error {
	receipt, err := r.loadValidationTargetReceipt(ctx, run)
	if err != nil {
		return err
	}
	candidates, err := validationEvidenceRepoRootCandidates(scan, ref.Path)
	if err != nil {
		return err
	}
	matches := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		for _, entry := range receipt.TreeIndex {
			if entry.Path == candidate {
				matches = append(matches, candidate)
				break
			}
		}
	}
	if len(matches) > 1 {
		return fmt.Errorf("file evidence path %q is ambiguous under RepositoryScan subPath", ref.Path)
	}
	if len(matches) == 0 {
		if receipt.TreeIndexTruncated {
			return fmt.Errorf("file evidence %q is outside the bounded trusted tree index", ref.Path)
		}
		return fmt.Errorf("file evidence %q does not exist in the pinned target", ref.Path)
	}
	if len(candidates) > 1 && receipt.TreeIndexTruncated {
		return fmt.Errorf("file evidence path %q cannot be disambiguated against a truncated trusted tree index", ref.Path)
	}
	canonical := ref
	canonical.Path = matches[0]
	return validateCanonicalFileEvidenceAgainstReceipt(receipt, canonical)
}

func scanRunHasImmutableRepositoryBinding(run *store.ScanRun, scan *corev1alpha1.RepositoryScan) bool {
	return run != nil && scan != nil &&
		security.ValidRunUID(run.RunUID) &&
		run.Namespace == scan.Namespace && run.RepositoryScan == scan.Name &&
		run.RepositoryScanUID != "" && run.RepositoryScanUID == string(scan.UID) &&
		run.RepositoryScanGeneration > 0 && run.RepositoryScanGeneration == scan.Generation
}

func (r *RepositoryScanReconciler) getReservedScanRun(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
) (*store.ScanRun, error) {
	if r.SecurityStore == nil || scan == nil || task == nil {
		return nil, nil
	}
	scanID := strings.TrimSpace(task.Labels[labels.LabelSecurityScanID])
	if scanID == "" {
		return nil, nil
	}

	run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, scanID)
	if errors.Is(err, store.ErrNotFound) {
		// Legacy Tasks were created before the controller atomically reserved a
		// full-width run identity. Their inputs and RepositoryScan incarnation
		// cannot be reconstructed safely, so terminal ingestion must not fabricate
		// a ScanRun from mutable Task metadata.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !scanRunHasImmutableRepositoryBinding(run, scan) {
		return nil, nil
	}
	return run, nil
}

func conciseTaskMessage(message, fallback string) string {
	for line := range strings.SplitSeq(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 512 {
			line = line[:512]
		}
		return line
	}
	return fallback
}

func (r *RepositoryScanReconciler) pipelineTaskSummary(ctx context.Context, task *corev1alpha1.Task, fallback string) string {
	if task.Status.Message != "" {
		return conciseTaskMessage(task.Status.Message, fallback)
	}
	if r.ResultStore != nil && task.Status.ResultRef != nil && task.Status.ResultRef.Available {
		if result, err := r.taskResult(ctx, task); err == nil {
			return conciseTaskMessage(string(result), fallback)
		}
	}
	return fallback
}

func pipelineTaskDisplayName(task *corev1alpha1.Task) string {
	stage := taskSecurityStage(task)
	if stage == security.StageReview {
		if sliceID := strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID]); sliceID != "" {
			return fmt.Sprintf("review:%s", sliceID)
		}
	}
	return stage
}

type scanRunProgress struct {
	hasActive           bool
	hasThreatModelReady bool
	hasMapper           bool
	hasMapperReady      bool
	hasReview           bool
	reviewCount         int
	reviewSucceeded     int
	failedStages        []string
	failureMessage      string
	latestCompletion    *time.Time
}

func recordScanProgressFailure(progress *scanRunProgress, task *corev1alpha1.Task, message string) {
	progress.failedStages = append(progress.failedStages, pipelineTaskDisplayName(task))
	if progress.failureMessage == "" {
		progress.failureMessage = message
	}
}

func (r *RepositoryScanReconciler) collectScanRunProgress(
	ctx context.Context,
	tasks []corev1alpha1.Task,
) scanRunProgress {
	progress := scanRunProgress{}
	for i := range tasks {
		task := &tasks[i]
		stage := taskSecurityStage(task)
		if !isScanPipelineStage(stage) {
			continue
		}
		if isActiveTaskPhase(task.Status.Phase) {
			progress.hasActive = true
		}
		if task.Status.CompletionTime != nil {
			completed := task.Status.CompletionTime.Time
			if progress.latestCompletion == nil || completed.After(*progress.latestCompletion) {
				progress.latestCompletion = &completed
			}
		}
		switch stage {
		case security.StageThreatModel:
			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
				progress.hasThreatModelReady = true
			}
			if task.Status.Phase == corev1alpha1.TaskPhaseFailed {
				recordScanProgressFailure(&progress, task, r.pipelineTaskSummary(ctx, task, "threat model stage failed"))
			}
		case security.StageMapper:
			progress.hasMapper = true
			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
				progress.hasMapperReady = true
			}
			if task.Status.Phase == corev1alpha1.TaskPhaseFailed {
				recordScanProgressFailure(&progress, task, r.pipelineTaskSummary(ctx, task, "mapper stage failed"))
			}
		case security.StageReview:
			progress.hasReview = true
			progress.reviewCount++
			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
				progress.reviewSucceeded++
			}
			if task.Status.Phase == corev1alpha1.TaskPhaseFailed {
				recordScanProgressFailure(&progress, task, r.pipelineTaskSummary(ctx, task, "review stage failed"))
			}
		}
	}
	return progress
}

func applyScanRunProgress(run *store.ScanRun, progress scanRunProgress) {
	if run.ErrorMessage != "" {
		run.Phase = scanRunPhaseFailed
		run.Summary = run.ErrorMessage
		return
	}
	if progress.hasActive {
		run.Phase = scanRunPhaseRunning
		run.CompletedAt = nil
		if run.Summary == "" {
			if progress.hasThreatModelReady {
				run.Summary = scanSummaryThreatModelPending
			} else {
				run.Summary = scanSummaryRunning
			}
		}
		return
	}

	if progress.failureMessage != "" || run.ErrorMessage != "" || len(progress.failedStages) > 0 {
		run.Phase = scanRunPhaseFailed
		if progress.latestCompletion != nil {
			run.CompletedAt = progress.latestCompletion
		}
		if progress.failureMessage != "" {
			run.ErrorMessage = progress.failureMessage
		} else if run.ErrorMessage == "" {
			run.ErrorMessage = fmt.Sprintf(
				"scan failed in stages: %s",
				strings.Join(progress.failedStages, ", "),
			)
		}
		run.Summary = run.ErrorMessage
		return
	}

	if progress.hasThreatModelReady && !progress.hasReview {
		if progress.hasMapper && !progress.hasMapperReady {
			run.Phase = scanRunPhaseRunning
			run.CompletedAt = nil
			run.ErrorMessage = ""
			run.Summary = "Threat model generated; deterministic mapper pending"
			return
		}
		run.Phase = scanRunPhaseRunning
		run.CompletedAt = nil
		run.ErrorMessage = ""
		if !progress.hasMapper {
			run.Summary = scanSummaryThreatModelPending
		}
		return
	}

	if progress.hasThreatModelReady && progress.hasReview {
		if progress.reviewSucceeded < progress.reviewCount {
			run.Phase = scanRunPhaseRunning
			run.CompletedAt = nil
			run.ErrorMessage = ""
			run.Summary = fmt.Sprintf(
				"Threat model generated and %d/%d review slices completed successfully",
				progress.reviewSucceeded,
				progress.reviewCount,
			)
			return
		}
		run.Phase = scanRunPhaseSucceeded
		run.ErrorMessage = ""
		if progress.latestCompletion != nil {
			run.CompletedAt = progress.latestCompletion
		}
		run.Summary = fmt.Sprintf(
			"Threat model generated and %d/%d review slices completed successfully",
			progress.reviewSucceeded,
			progress.reviewCount,
		)
		return
	}

	run.Phase = scanRunPhaseSucceeded
	run.ErrorMessage = ""
	if progress.latestCompletion != nil {
		run.CompletedAt = progress.latestCompletion
	}
	run.Summary = "Threat model generated successfully"
}

//nolint:gocyclo // scan integrity reducers intentionally keep ordered fail-closed branches
func (r *RepositoryScanReconciler) refreshScanRunStatus(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	scanID string,
	updateStatus bool,
) error {
	if r.Client == nil {
		if run.ErrorMessage != "" {
			run.Phase = scanRunPhaseFailed
			run.Summary = run.ErrorMessage
		} else if run.Phase == "" {
			run.Phase = scanRunPhaseRunning
		}
		if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
			return err
		}
		return nil
	}

	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
			labels.LabelSecurityScanID: scanID,
		}),
	); err != nil {
		return err
	}
	slices.SortFunc(tasks.Items, func(a, b corev1alpha1.Task) int {
		if cmp := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Name, b.Name)
	})

	progress := r.collectScanRunProgress(ctx, tasks.Items)
	applyScanRunProgress(run, progress)
	if scanRunUsesPinnedTarget(run) &&
		progress.hasMapperReady && !progress.hasThreatModelReady && run.ErrorMessage == "" {
		run.Phase = scanRunPhaseRunning
		run.CompletedAt = nil
		run.Summary = "Pinned target verified; threat model pending"
	}
	if err := r.keepScanRunningForPendingReviewSlices(ctx, scan, run, progress); err != nil {
		return err
	}
	if run.Phase == scanRunPhaseSucceeded && r.IntegrityConfig.FindingObservationWrites {
		if err := r.finalizeRunOccurrences(ctx, scan, run); err != nil {
			return err
		}
	}
	if err := r.maybeSealRunBundle(ctx, scan, run); err != nil {
		return err
	}

	updateRun := true
	if run.Quality.BundleStatus == store.BundleStatusSealed {
		if storedRun, getErr := r.SecurityStore.GetScanRun(ctx, run.Namespace, run.ID); getErr == nil &&
			storedRun.Quality.BundleStatus == store.BundleStatusSealed {
			updateRun = false
		} else if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
	}
	if updateRun {
		if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
			return err
		}
	}
	if !updateStatus {
		return nil
	}
	counts, err := r.SecurityStore.GetFindingCounts(ctx, scan.Namespace, scan.Name)
	if err != nil {
		return err
	}

	var threatModelVersion int64
	if model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err == nil && threatModelMatchesRepositoryScan(model, scan) {
		threatModelVersion = model.Version
	}

	return r.updateStatusWithRetryChecked(ctx, scan, func(s *corev1alpha1.RepositoryScan) (bool, error) {
		latestRunID, err := r.latestSecurityScanRunID(ctx, s.Namespace, s.Name)
		if err != nil {
			return false, err
		}
		currentRun, err := r.SecurityStore.GetScanRun(ctx, s.Namespace, run.ID)
		if err != nil {
			return false, err
		}
		if latestRunID != currentRun.ID || scanRunExplicitlyMismatchesRepositoryScan(currentRun, s) {
			return false, nil
		}
		run := currentRun
		runMatchesScan := scanRunMatchesRepositoryScan(run, s)
		qualityProjectionReady := r.IntegrityConfig.QualityStateWritesEnabled && runMatchesScan
		if qualityProjectionReady {
			s.Status.Quality = repositoryScanQualityStatus(run, s)
		} else if r.IntegrityConfig.QualityStateWritesEnabled {
			setRepositoryScanQualityUnbound(s)
		} else {
			s.Status.Quality = nil
			meta.RemoveStatusCondition(&s.Status.Conditions, "QualityReady")
		}
		s.Status.LastScanID = run.ID
		s.Status.LastScanTaskName = run.TaskName
		s.Status.LastObservedHeadSHA = run.HeadCommit
		s.Status.ThreatModelVersion = threatModelVersion
		s.Status.FindingCounts = corev1alpha1.FindingCountsStatus{
			Total:    counts.Total,
			Critical: counts.Critical,
			High:     counts.High,
			Medium:   counts.Medium,
			Low:      counts.Low,
		}

		switch run.Phase {
		case scanRunPhaseRunning, scanRunPhasePending:
			s.Status.Phase = repositoryScanPhaseScanning
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Scanning",
				Message:            repositoryScanConditionMessage(run.Summary, scanSummaryRunning),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
			if qualityProjectionReady {
				meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
					Type:               "QualityReady",
					Status:             metav1.ConditionUnknown,
					Reason:             "QualityPending",
					Message:            "Scan quality is still being evaluated",
					LastTransitionTime: metav1.Now(),
					ObservedGeneration: s.Generation,
				})
			}
		case scanRunPhaseSucceeded:
			s.Status.Phase = repositoryScanPhaseReady
			s.Status.LastProcessedCommit = run.HeadCommit
			if runMatchesScan && run.Quality.InventoryCoverageStatus == store.CoverageStatusComplete &&
				run.Quality.CandidateCoverageStatus == store.CoverageStatusComplete &&
				run.Quality.TargetVerification == store.TargetVerificationVerified {
				s.Status.LastCompleteCoverageCommit = run.HeadCommit
			}
			if runMatchesScan && run.Quality.BundleStatus == store.BundleStatusSealed {
				s.Status.LastBundleSealedCommit = run.HeadCommit
			}
			if runMatchesScan && scanRunAssuranceQualified(run) {
				s.Status.LastAssuranceQualifiedCommit = run.HeadCommit
			}
			if run.CompletedAt != nil {
				t := &metav1.Time{Time: *run.CompletedAt}
				s.Status.LastScanAt = t
				s.Status.LastSuccessfulScanAt = t
			}
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "ScanSucceeded",
				Message:            repositoryScanConditionMessage(run.Summary, "scan completed successfully"),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
			if qualityProjectionReady {
				qualityStatus := metav1.ConditionTrue
				reason := "QualityComplete"
				message := "Scan quality requirements are satisfied"
				if scanRunQualityDegraded(run) || (security.EffectiveCompletionPolicy(scan) == "validated" && !scanRunAssuranceQualified(run)) {
					qualityStatus = metav1.ConditionFalse
					reason = qualityConditionReasonDegraded
					message = "Discovery completed, but target, coverage, validation, isolation, authorization, or bundle quality is degraded"
				}
				meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
					Type: "QualityReady", Status: qualityStatus, Reason: reason, Message: message,
					LastTransitionTime: metav1.Now(), ObservedGeneration: s.Generation,
				})
			}
		default:
			s.Status.Phase = repositoryScanPhaseError
			if run.CompletedAt != nil {
				s.Status.LastScanAt = &metav1.Time{Time: *run.CompletedAt}
			}
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "ScanFailed",
				Message:            repositoryScanConditionMessage(run.Summary, "scan failed"),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
			if qualityProjectionReady {
				meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
					Type:               "QualityReady",
					Status:             metav1.ConditionFalse,
					Reason:             "ScanFailed",
					Message:            "Scan failed before quality requirements could be satisfied",
					LastTransitionTime: metav1.Now(),
					ObservedGeneration: s.Generation,
				})
			}
		}
		return true, nil
	})
}

func repositoryScanQualityStatus(run *store.ScanRun, scan *corev1alpha1.RepositoryScan) *corev1alpha1.RepositoryScanQualityStatus {
	if run == nil || scan == nil {
		return nil
	}
	return &corev1alpha1.RepositoryScanQualityStatus{
		SchemaVersion:             int32(run.Quality.SchemaVersion),
		ObservedRepositoryScanUID: run.RepositoryScanUID,
		ObservedGeneration:        run.RepositoryScanGeneration,
		InventoryCoverageStatus:   string(run.Quality.InventoryCoverageStatus),
		CandidateCoverageStatus:   string(run.Quality.CandidateCoverageStatus),
		CoverageStatus:            string(run.Quality.CoverageStatus),
		ValidationScope:           string(run.Quality.ValidationScope),
		ValidationExecution:       string(run.Quality.ValidationExecution),
		AttackPathExecution:       string(run.Quality.AttackPathExecution),
		AnalysisAttestationLevel:  string(run.Quality.AnalysisAttestationLevel),
		TargetVerification:        string(run.Quality.TargetVerification),
		BundleStatus:              string(run.Quality.BundleStatus),
		AuthorizationStatus:       string(run.Quality.AuthorizationStatus),
		IsolationStatus:           string(run.Quality.IsolationStatus),
		ReasonCodes:               append([]string(nil), run.Quality.ReasonCodes...),
	}
}

func scanRunMatchesRepositoryScan(run *store.ScanRun, scan *corev1alpha1.RepositoryScan) bool {
	return run != nil && scan != nil && run.RepositoryScanUID != "" && run.RepositoryScanGeneration > 0 &&
		run.RepositoryScanUID == string(scan.UID) && run.RepositoryScanGeneration == scan.Generation
}

func scanRunExplicitlyMismatchesRepositoryScan(run *store.ScanRun, scan *corev1alpha1.RepositoryScan) bool {
	if run == nil || scan == nil {
		return true
	}
	return (run.RepositoryScanUID != "" && run.RepositoryScanUID != string(scan.UID)) ||
		(run.RepositoryScanGeneration > 0 && run.RepositoryScanGeneration != scan.Generation)
}

func setRepositoryScanQualityUnbound(scan *corev1alpha1.RepositoryScan) {
	if scan == nil {
		return
	}
	scan.Status.Quality = nil
	meta.SetStatusCondition(&scan.Status.Conditions, metav1.Condition{
		Type:               "QualityReady",
		Status:             metav1.ConditionUnknown,
		Reason:             "QualityUnavailable",
		Message:            "Legacy scan run has no complete RepositoryScan UID and generation binding; discovery status is available but quality is not attributable",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: scan.Generation,
	})
}

func validateRequestedCommitTarget(requestedRef, objectFormat, resolvedHead string) error {
	requestedOID, isOID := security.NormalizeFullGitObjectID(requestedRef)
	if !isOID {
		return nil
	}
	expectedWidth := 0
	switch strings.ToLower(strings.TrimSpace(objectFormat)) {
	case gitObjectFormatSHA1:
		expectedWidth = 40
	case gitObjectFormatSHA256:
		expectedWidth = 64
	}
	resolvedHead = strings.ToLower(strings.TrimSpace(resolvedHead))
	if expectedWidth == 0 || len(requestedOID) != expectedWidth {
		return fmt.Errorf("requested commit object %q is incompatible with repository object format %q", requestedOID, objectFormat)
	}
	if requestedOID != resolvedHead {
		return fmt.Errorf("resolved head %q does not match requested commit object %q", resolvedHead, requestedOID)
	}
	return nil
}

func scanRunQualityDegraded(run *store.ScanRun) bool {
	if run == nil {
		return true
	}
	quality := run.Quality
	if quality.InventoryCoverageStatus != store.CoverageStatusComplete ||
		run.Quality.CandidateCoverageStatus != store.CoverageStatusComplete ||
		run.Quality.CoverageStatus != store.CoverageStatusComplete ||
		run.Quality.TargetVerification != store.TargetVerificationVerified ||
		(run.Quality.AuthorizationStatus != store.AuthorizationStatusVerified && run.Quality.AuthorizationStatus != store.AuthorizationStatusAdmitted) ||
		run.Quality.IsolationStatus != store.IsolationStatusHardened ||
		(run.Quality.AnalysisAttestationLevel != store.AnalysisAttestationToolObserved &&
			run.Quality.AnalysisAttestationLevel != store.AnalysisAttestationBrokered) ||
		run.Quality.BundleStatus == store.BundleStatusRetryableFailed || run.Quality.BundleStatus == store.BundleStatusFailed {
		return true
	}
	switch quality.ValidationScope {
	case store.ValidationScopeOff:
		return quality.ValidationExecution != store.QualityExecutionComplete ||
			(quality.AttackPathExecution != store.QualityExecutionComplete && quality.AttackPathExecution != store.QualityExecutionDeferred)
	case store.ValidationScopeSampled, store.ValidationScopeAll:
		return quality.ValidationExecution != store.QualityExecutionComplete || quality.AttackPathExecution != store.QualityExecutionComplete
	default:
		return true
	}
}

func scanRunAssuranceQualified(run *store.ScanRun) bool {
	if run == nil || run.Quality.ValidationScope != store.ValidationScopeAll {
		return false
	}
	return run.Quality.InventoryCoverageStatus == store.CoverageStatusComplete &&
		run.Quality.CandidateCoverageStatus == store.CoverageStatusComplete &&
		run.Quality.CoverageStatus == store.CoverageStatusComplete &&
		run.Quality.TargetVerification == store.TargetVerificationVerified &&
		run.Quality.ValidationExecution == store.QualityExecutionComplete &&
		run.Quality.AttackPathExecution == store.QualityExecutionComplete &&
		(run.Quality.AnalysisAttestationLevel == store.AnalysisAttestationToolObserved ||
			run.Quality.AnalysisAttestationLevel == store.AnalysisAttestationBrokered) &&
		run.Quality.BundleStatus == store.BundleStatusSealed &&
		run.Quality.IsolationStatus == store.IsolationStatusHardened &&
		(run.Quality.AuthorizationStatus == store.AuthorizationStatusVerified || run.Quality.AuthorizationStatus == store.AuthorizationStatusAdmitted)
}

func (r *RepositoryScanReconciler) keepScanRunningForPendingReviewSlices(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	progress scanRunProgress,
) error {
	if r.SecurityStore == nil || run == nil || run.Phase != scanRunPhaseSucceeded {
		return nil
	}
	if !progress.hasReview {
		return nil
	}
	reviewSlices, err := r.pendingReviewSlices(ctx, scan, run.ID)
	if err != nil {
		return err
	}
	if len(reviewSlices) == 0 {
		return nil
	}
	run.Phase = scanRunPhaseRunning
	run.CompletedAt = nil
	run.ErrorMessage = ""
	run.Summary = fmt.Sprintf("Threat model generated; %d review slices remain pending", len(reviewSlices))
	return nil
}

func (r *RepositoryScanReconciler) shouldAutoValidateFinding(scan *corev1alpha1.RepositoryScan, finding *store.Finding, createdForRun int) bool {
	if finding == nil {
		return false
	}
	minSeverity := security.EffectiveValidationMinSeverity(scan)
	minConfidence := security.EffectiveValidationMinConfidence(scan)
	mode := security.EffectiveValidationMode(scan)
	if mode == validationModeFull {
		return true
	}
	severityOK := security.SeverityMeetsMinimum(finding.Severity, minSeverity)
	confidenceOK := security.ConfidenceMeetsMinimum(finding.Confidence, minConfidence)
	switch mode {
	case validationModeOff:
		return false
	default:
		limit := int(security.EffectiveValidationMaxFindingsPerRun(scan))
		if limit <= 0 || createdForRun >= limit {
			return false
		}
		return severityOK || confidenceOK
	}
}

func (r *RepositoryScanReconciler) hasActiveValidationTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	finding *store.Finding,
) (bool, error) {
	if r.Client == nil || finding == nil {
		return false, nil
	}
	findingID := strings.TrimSpace(finding.ID)
	scanRunID := strings.TrimSpace(finding.ScanRunID)
	occurrenceID := strings.TrimSpace(finding.CurrentOccurrenceID)

	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
			labels.LabelSecurityFindingID: labels.SelectorValue(findingID),
			labels.LabelSecurityStage:     security.StageValidation,
		}),
	); err != nil {
		return false, err
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !repositoryScanControlsTask(scan, task) || !isActiveTaskPhase(task.Status.Phase) {
			continue
		}
		taskFindingID, err := taskSecurityFindingID(task)
		if err != nil {
			return false, err
		}
		taskScanRunID, err := taskSecurityScanRunID(task)
		if err != nil {
			return false, err
		}
		taskOccurrenceID, err := taskSecurityOccurrenceID(task)
		if err != nil {
			return false, err
		}
		if taskFindingID == findingID && taskScanRunID == scanRunID && taskOccurrenceID == occurrenceID {
			return true, nil
		}
	}
	return false, nil
}

func (r *RepositoryScanReconciler) createValidationTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, finding *store.Finding) error {
	if r.Client == nil {
		return nil
	}
	var run *store.ScanRun
	if r.SecurityStore != nil && strings.TrimSpace(finding.ScanRunID) != "" {
		var err error
		run, err = r.SecurityStore.GetScanRun(ctx, scan.Namespace, finding.ScanRunID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if run != nil {
		if err := security.ValidateRunRepositoryScanIdentity(run, scan); err != nil {
			return err
		}
	}
	policy, err := security.LoadScannerPolicy(ctx, r.Client, scan.Namespace, scan.Spec)
	if err != nil {
		if run != nil && activeScanRunPhase(run.Phase) && terminalScannerPolicyLoadError(err) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	if run != nil && activeScanRunPhase(run.Phase) {
		if err := ensureScanRunPolicyDigest(run, policy); err != nil {
			if errors.Is(err, errScannerPolicyDigestChanged) {
				return r.recordTerminalScanRunError(ctx, scan, run, err)
			}
			return err
		}
	}
	timeout := metav1.Duration{Duration: 90 * time.Minute}
	priority := int32(725)
	var taskName string
	validationScopeID := finding.CurrentOccurrenceID
	if validationScopeID == "" {
		validationScopeID = finding.ID
	}
	if run != nil && security.ValidRunUID(run.RunUID) {
		taskName = security.ScanStageTaskNameForRun(scan.Name, "validation", security.StageValidation, validationScopeID, run.RunUID)
	} else {
		taskName = security.ScanStageTaskName(scan.Name, "validation", security.StageValidation, finding.ID)
	}
	analysisAnnotations, err := r.analysisTaskAnnotations(ctx, scan)
	if err != nil {
		return err
	}
	applyRunIsolationFromAnnotations(run, analysisAnnotations)
	if run != nil {
		if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Annotations: mergeSecurityTaskAnnotations(analysisAnnotations, map[string]string{
				security.AnnotationValidationBindingVersion: security.ValidationBindingVersion,
			}),
			Labels: map[string]string{
				labels.LabelManaged:              "true",
				labels.LabelCreatedBy:            repositorySecurityCreatedBy,
				labels.LabelSecurityTarget:       labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:       finding.ScanRunID,
				labels.LabelSecurityMode:         security.StageValidation,
				labels.LabelSecurityStage:        security.StageValidation,
				labels.LabelSecurityFindingID:    labels.SelectorValue(finding.ID),
				labels.LabelSecurityOccurrenceID: labels.SelectorValue(finding.CurrentOccurrenceID),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildValidationPrompt(scan, finding, policy.PromptPolicy()),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageValidation},
				{Name: security.EnvScanID, Value: finding.ScanRunID},
				{Name: security.EnvPolicyDigest, Value: policy.Digest},
				{Name: security.EnvPolicyProvenance, Value: security.PolicyProvenanceEnv(policy)},
				{Name: security.EnvFindingID, Value: finding.ID},
				{Name: security.EnvOccurrenceID, Value: finding.CurrentOccurrenceID},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{Workspace: resolvedWorkspaceForRun(scan, run)},
		},
	}
	if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrValidateSecurityTask(ctx, scan, task); err != nil {
		return err
	}

	finding.ValidationStatus = findingValidationStatusPending
	return r.SecurityStore.UpsertFinding(ctx, finding)
}

func (r *RepositoryScanReconciler) ensureActiveScanRunPolicyCurrent(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	if run == nil || !activeScanRunPhase(run.Phase) {
		return nil
	}
	policy, err := security.LoadScannerPolicy(ctx, r.Client, scan.Namespace, scan.Spec)
	if err != nil {
		if terminalScannerPolicyLoadError(err) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	if err := ensureScanRunPolicyDigest(run, policy); err != nil {
		if errors.Is(err, errScannerPolicyDigestChanged) {
			return r.recordTerminalScanRunError(ctx, scan, run, err)
		}
		return err
	}
	return nil
}

func mergeEvidenceRefs(existing []store.FindingEvidenceRef, refs ...store.FindingEvidenceRef) []store.FindingEvidenceRef {
	merged := append([]store.FindingEvidenceRef{}, existing...)
	seen := map[string]struct{}{}
	for _, ref := range merged {
		key := evidenceRefKey(ref)
		seen[key] = struct{}{}
	}
	for _, ref := range refs {
		key := evidenceRefKey(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, ref)
	}
	return merged
}

func evidenceRefKey(ref store.FindingEvidenceRef) string {
	return strings.Join([]string{
		ref.Kind,
		ref.TaskName,
		ref.Name,
		ref.Label,
		ref.Path,
		fmt.Sprint(ref.StartLine),
		fmt.Sprint(ref.EndLine),
		ref.Symbol,
		ref.Quote,
	}, "|")
}

func (r *RepositoryScanReconciler) mergeExistingFinding(ctx context.Context, scan *corev1alpha1.RepositoryScan, finding *store.Finding) error {
	existing, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, finding.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if existing.State != "" && existing.State != findingStateOpen {
		finding.State = existing.State
	}
	if existing.PatchProposalID != "" {
		finding.PatchProposalID = existing.PatchProposalID
	}
	finding.PRNumber = existing.PRNumber
	finding.PRURL = existing.PRURL
	finding.CreatedAt = existing.CreatedAt
	if existing.ValidationStatus == findingValidationStatusValidated ||
		existing.ValidationStatus == findingValidationStatusPending {
		finding.ValidationStatus = existing.ValidationStatus
	}
	if len(existing.Evidence) > 0 {
		finding.Evidence = mergeEvidenceRefs(existing.Evidence, finding.Evidence...)
	}
	if existing.ValidationJSON != "" {
		finding.ValidationJSON = existing.ValidationJSON
	}
	return nil
}

func (r *RepositoryScanReconciler) persistDroppedFindingDiagnostics(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
	run *store.ScanRun,
	diagnostics []security.DroppedFindingDiagnostic,
) error {
	if len(diagnostics) == 0 {
		return nil
	}
	sliceID := strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID])
	for _, diagnostic := range diagnostics {
		dropped := &store.DroppedFinding{
			ID:             "drop_" + security.FindingID(strings.Join([]string{run.ID, task.Name, sliceID, fmt.Sprint(diagnostic.Index), diagnostic.Reason}, "|")),
			Namespace:      scan.Namespace,
			RepositoryScan: scan.Name,
			ScanRunID:      run.ID,
			TaskName:       task.Name,
			SliceID:        sliceID,
			Reason:         diagnostic.Reason,
			Layer:          diagnostic.Layer,
			SampleJSON:     security.DroppedFindingSampleJSON(diagnostic),
		}
		if err := r.SecurityStore.CreateDroppedFinding(ctx, dropped); err != nil {
			return err
		}
	}
	if r.ArtifactStore != nil {
		artifact := security.DroppedFindingArtifact{
			SchemaVersion: 1,
			Dropped:       diagnostics,
		}
		data, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			return err
		}
		if err := r.saveControllerArtifact(ctx, task, security.ArtifactDroppedFindings, "application/json", data); err != nil {
			return err
		}
	}
	return nil
}

func (r *RepositoryScanReconciler) validationTaskCountForScanRun(ctx context.Context, scan *corev1alpha1.RepositoryScan, scanRunID string) (int, error) {
	if r.Client == nil || strings.TrimSpace(scanRunID) == "" {
		return 0, nil
	}
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
			labels.LabelSecurityStage:  security.StageValidation,
			labels.LabelSecurityScanID: scanRunID,
		}),
	); err != nil {
		return 0, err
	}
	return len(tasks.Items), nil
}

func (r *RepositoryScanReconciler) enqueueAutoValidationTasks(ctx context.Context, scan *corev1alpha1.RepositoryScan, findings []*store.Finding) error {
	createdByRun := map[string]int{}
	for _, finding := range findings {
		if finding == nil {
			continue
		}
		created, ok := createdByRun[finding.ScanRunID]
		if !ok {
			existing, err := r.validationTaskCountForScanRun(ctx, scan, finding.ScanRunID)
			if err != nil {
				return err
			}
			created = existing
		}
		if !r.shouldAutoValidateFinding(scan, finding, created) {
			createdByRun[finding.ScanRunID] = created
			continue
		}
		if finding.ValidationStatus == findingValidationStatusValidated ||
			finding.ValidationStatus == findingValidationStatusPending {
			continue
		}
		active, err := r.hasActiveValidationTask(ctx, scan, finding)
		if err != nil {
			return err
		}
		if active {
			continue
		}
		if err := r.createValidationTask(ctx, scan, finding); err != nil {
			return err
		}
		created++
		createdByRun[finding.ScanRunID] = created
	}
	return nil
}

func clearRunError(run *store.ScanRun) {
	if run == nil {
		return
	}
	if run.Summary == run.ErrorMessage {
		run.Summary = ""
	}
	run.ErrorMessage = ""
}

func clearThreatModelRunError(run *store.ScanRun) {
	if run == nil || run.ErrorMessage == "" {
		return
	}
	if strings.Contains(run.ErrorMessage, security.ArtifactThreatModel) ||
		strings.Contains(run.ErrorMessage, "threat model stage failed") {
		clearRunError(run)
	}
}

func clearReviewRunError(run *store.ScanRun, sliceID string) {
	if run == nil || run.ErrorMessage == "" {
		return
	}
	if sliceID != "" {
		if strings.Contains(run.ErrorMessage, fmt.Sprintf("slice %s:", sliceID)) {
			clearRunError(run)
		}
		if strings.Contains(run.ErrorMessage, "slice ") {
			return
		}
	}
	if strings.Contains(run.ErrorMessage, security.ArtifactFindingsV2) ||
		strings.Contains(run.ErrorMessage, "review stage failed") {
		clearRunError(run)
	}
}

func (r *RepositoryScanReconciler) ingestThreatModelTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun) error {
	run.TaskName = task.Name
	if mode := task.Labels[labels.LabelSecurityMode]; mode != "" {
		run.Mode = mode
	}

	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		threatModel, rawThreatModel, validationProblem, err := r.loadThreatModelArtifact(ctx, task)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			run.ErrorMessage = "required scan artifacts were missing or invalid: " + validationProblem
			if err := r.appendStageReceipt(ctx, task, run, security.ArtifactThreatModel, nil, nil,
				store.StageReceiptRejected, "artifact_invalid", validationProblem); err != nil {
				return err
			}
		} else {
			content := []byte(strings.TrimSpace(threatModel))
			if err := r.appendStageReceipt(ctx, task, run, security.ArtifactThreatModel, rawThreatModel, content,
				store.StageReceiptAccepted, "", ""); err != nil {
				return err
			}
			if err := r.persistThreatModelIfChanged(ctx, scan, run.ID, run.StartedAt, threatModel); err != nil {
				return err
			}
			if r.RunThreatModelStore != nil && security.ValidRunUID(run.RunUID) {
				if _, err := r.RunThreatModelStore.SaveSecurityRunThreatModel(ctx, &store.SecurityRunThreatModel{
					RunUID: run.RunUID, Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID,
					Version: 1, Content: threatModel, ContentDigest: securityDigest(content),
					SourceReceiptID: r.stageReceiptIDFor(ctx, run, task, security.ArtifactThreatModel, rawThreatModel, store.StageReceiptAccepted),
				}); err != nil {
					return err
				}
			}
			// Threat-model generation is model analysis over delivered context; it
			// does not independently observe the checked-out Git target.
			run.Quality.AnalysisAttestationLevel = store.AnalysisAttestationDelivered
			clearThreatModelRunError(run)
			run.Summary = scanSummaryThreatModelPending
		}
	} else {
		run.ErrorMessage = r.pipelineTaskSummary(ctx, task, "threat model stage failed")
		if err := r.appendStageReceipt(ctx, task, run, security.ArtifactThreatModel, nil, nil,
			store.StageReceiptRejected, securityTaskFailureClass(task), run.ErrorMessage); err != nil {
			return err
		}
	}

	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
}

//nolint:gocyclo // scan integrity reducers intentionally keep ordered fail-closed branches
func (r *RepositoryScanReconciler) ingestReviewTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun) error {
	if run.Phase == scanRunPhaseFailed {
		return nil
	}
	sliceID := strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID])
	reviewSlice, staleReviewTask, err := r.reviewSliceForTaskRun(ctx, scan, sliceID, run.ID)
	if err != nil {
		return err
	}
	if staleReviewTask {
		return nil
	}
	if reviewSlice != nil && reviewSlice.Status == reviewSliceStatusReviewed {
		return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
	}

	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		run.ErrorMessage = r.pipelineTaskSummary(ctx, task, "review stage failed")
		if err := r.appendStageReceipt(ctx, task, run, security.ArtifactFindingsV2, nil, nil,
			store.StageReceiptRejected, securityTaskFailureClass(task), run.ErrorMessage); err != nil {
			return err
		}
		if sliceID != "" {
			if err := r.SecurityStore.UpdateReviewSliceStatus(ctx, scan.Namespace, scan.Name, sliceID, run.ID, reviewSliceStatusFailed); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
	}

	findingsV2, manifest, rawFindings, rawContext, validationProblem, err := r.loadDiscoveryFindingsV2Artifact(ctx, task)
	if err != nil {
		return err
	}
	if findingsV2 == nil && validationProblem == "" {
		validationProblem = fmt.Sprintf("%s is missing", security.ArtifactFindingsV2)
	}
	if validationProblem != "" {
		if err := r.appendStageReceipt(ctx, task, run, security.ArtifactFindingsV2, rawFindings, nil,
			store.StageReceiptRejected, "artifact_invalid", validationProblem); err != nil {
			return err
		}
		if sliceID != "" {
			run.ErrorMessage = fmt.Sprintf("slice %s: %s", sliceID, validationProblem)
			if err := r.SecurityStore.UpdateReviewSliceStatus(ctx, scan.Namespace, scan.Name, sliceID, run.ID, reviewSliceStatusFailed); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		} else {
			run.ErrorMessage = validationProblem
		}
		return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
	}

	if scanRunUsesPinnedTarget(run) {
		artifactHead := strings.ToLower(strings.TrimSpace(findingsV2.Repository.HeadSHA))
		expectedHead := strings.ToLower(strings.TrimSpace(run.HeadCommit))
		if artifactHead == "" || artifactHead != expectedHead {
			problem := fmt.Sprintf("findings artifact headSHA %q does not match pinned target %q", artifactHead, expectedHead)
			run.ErrorMessage = problem
			if err := r.appendStageReceipt(ctx, task, run, security.ArtifactFindingsV2, rawFindings, nil,
				store.StageReceiptRejected, "target_mismatch", problem); err != nil {
				return err
			}
			return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
		}
	}
	normalizedFindings, err := json.Marshal(findingsV2)
	if err != nil {
		return err
	}
	if err := r.appendStageReceipt(ctx, task, run, security.ArtifactFindingsV2, rawFindings, normalizedFindings,
		store.StageReceiptAccepted, "", ""); err != nil {
		return err
	}
	normalizedContext, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := r.appendStageReceipt(ctx, task, run, security.ReviewContextArtifactName(sliceID), rawContext, normalizedContext,
		store.StageReceiptAccepted, "", ""); err != nil {
		return err
	}
	for _, included := range manifest.IncludedFiles {
		if included.Truncated || !included.Readable || included.IncludedBytes < included.Bytes {
			run.Quality.InventoryCoverageStatus = store.CoverageStatusPartial
			run.Quality.CoverageStatus = store.CoverageStatusPartial
			run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "review_context_partial")
			break
		}
	}
	if len(manifest.OmittedFiles) > 0 {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusPartial
		run.Quality.CoverageStatus = store.CoverageStatusPartial
		run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "review_context_omitted")
	}

	// Run-level attestation reflects the weakest repository-dependent analysis
	// stage. The mapper observes the Git target directly, but the model review
	// currently has only controller-delivered context, so preserving the mapper's
	// stronger value here would overstate end-to-end analysis assurance.
	run.Quality.AnalysisAttestationLevel = store.AnalysisAttestationDelivered
	clearReviewRunError(run, sliceID)
	trustedRepo := trustedFindingsRepository(scan, run)
	partition := security.ValidateFindingsV2(*findingsV2, *manifest, security.FindingValidationOptions{
		Namespace:            scan.Namespace,
		RepositoryScan:       scan.Name,
		ScanRunID:            run.ID,
		TaskName:             task.Name,
		TrustedRepository:    trustedRepo,
		UseTrustedRepository: true,
	})
	if err := r.ensureActiveScanRunPolicyCurrent(ctx, scan, run); err != nil {
		return err
	}
	filterResult := security.FilterFindings(partition.Accepted, security.FindingFilterOptions{
		RepositoryScan: scan.Name,
		ScanRunID:      run.ID,
		TaskName:       task.Name,
		SliceID:        sliceID,
	})
	partition.Accepted = filterResult.Kept
	partition.Dropped = append(partition.Dropped, filterResult.Dropped...)
	var capDrops []security.DroppedFindingDiagnostic
	partition.Accepted, capDrops = capAcceptedFindingsForRun(scan, run, partition.Accepted)
	partition.Dropped = append(partition.Dropped, capDrops...)
	if err := r.persistDroppedFindingDiagnostics(ctx, scan, task, run, partition.Dropped); err != nil {
		return err
	}
	run.AcceptedFindings += len(partition.Accepted)
	run.DroppedFindings += len(partition.Dropped)
	run.ReviewedSliceCount++
	if findingsV2.Scan.Summary != "" {
		run.Summary = findingsV2.Scan.Summary
	} else if sliceID != "" {
		run.Summary = fmt.Sprintf("Reviewed slice %s", sliceID)
	}
	useImmutableObservations := r.IntegrityConfig.FindingObservationWrites && r.IntegrityStore != nil &&
		task.UID != "" && security.ValidRunUID(run.RunUID)
	if useImmutableObservations {
		if err := r.persistFindingObservations(ctx, scan, task, run, rawFindings, partition.Accepted, partition.Dropped); err != nil {
			return err
		}
	}
	upserted := make([]*store.Finding, 0, len(partition.Accepted))
	if !useImmutableObservations {
		for _, finding := range partition.Accepted {
			if err := r.mergeExistingFinding(ctx, scan, finding); err != nil {
				return err
			}
			if err := r.SecurityStore.UpsertFinding(ctx, finding); err != nil {
				return err
			}
			upserted = append(upserted, finding)
		}
	}
	if !useImmutableObservations {
		if err := r.enqueueAutoValidationTasks(ctx, scan, upserted); err != nil {
			return err
		}
	}
	if sliceID != "" {
		if err := r.SecurityStore.UpdateReviewSliceStatus(ctx, scan.Namespace, scan.Name, sliceID, run.ID, reviewSliceStatusReviewed); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
}

func (r *RepositoryScanReconciler) persistFindingObservations(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
	run *store.ScanRun,
	sourceArtifact []byte,
	accepted []*store.Finding,
	dropped []security.DroppedFindingDiagnostic,
) error {
	if !r.IntegrityConfig.FindingObservationWrites || r.IntegrityStore == nil || task == nil || task.UID == "" ||
		run == nil || !security.ValidRunUID(run.RunUID) {
		return nil
	}
	sourceDigest := securityDigest(sourceArtifact)
	sourceGeneration := int64(0)
	if bound, ok := r.ArtifactStore.(store.BoundOutputStore); ok {
		if artifact, err := bound.GetBoundArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2, string(task.UID), harnessWrapperOutputAttempt(task)); err == nil {
			if artifact.Provenance.ContentSHA256 != sourceDigest || artifact.Provenance.ContentSize != int64(len(sourceArtifact)) {
				return fmt.Errorf("security artifact %s changed after parsing; refusing mixed-generation observations", security.ArtifactFindingsV2)
			}
			sourceGeneration = artifact.Provenance.StagingGeneration
			sourceDigest = artifact.Provenance.ContentSHA256
		}
	}
	stageReceiptID := r.stageReceiptIDFor(
		ctx, run, task, security.ArtifactFindingsV2, sourceArtifact, store.StageReceiptAccepted,
	)
	for ordinal, finding := range accepted {
		if finding == nil {
			continue
		}
		identity := security.DeriveSemanticIdentity(scan.Spec.RepoURL, finding)
		if finding.SemanticFingerprint == "" {
			finding.SemanticFingerprint = identity.SemanticFingerprint
			finding.IdentityQuality = identity.Quality
			finding.IdentityAlgorithmVersion = identity.AlgorithmVersion
		}
		payload, err := json.Marshal(finding)
		if err != nil {
			return err
		}
		payloadDigest := securityDigest(payload)
		observation := &store.FindingObservation{
			ID:                       security.ObservationID(run.RunUID, string(task.UID), sourceGeneration, payloadDigest, ordinal),
			Namespace:                run.Namespace,
			RepositoryScan:           run.RepositoryScan,
			ScanRunID:                run.ID,
			RunUID:                   run.RunUID,
			StageReceiptID:           stageReceiptID,
			TargetReceiptID:          run.TargetReceiptID,
			SliceID:                  strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID]),
			CandidateKey:             finding.ID,
			ProducerFindingID:        finding.ID,
			SourceArtifactName:       security.ArtifactFindingsV2,
			SourceArtifactGeneration: sourceGeneration,
			SourceArtifactDigest:     sourceDigest,
			PolicyDigest:             run.PolicyDigest,
			Ordinal:                  ordinal,
			Disposition:              store.FindingObservationAccepted,
			RuleID:                   identity.RuleID,
			IdentityAnchor:           identity.Anchor,
			IdentityInstance:         identity.Instance,
			IdentityQuality:          finding.IdentityQuality,
			IdentityAlgorithmVersion: finding.IdentityAlgorithmVersion,
			SemanticFingerprint:      finding.SemanticFingerprint,
			LegacyFingerprint:        finding.Fingerprint,
			NormalizedPayload:        payload,
			PayloadDigest:            payloadDigest,
		}
		if _, err := r.IntegrityStore.AcceptFindingObservation(ctx, observation); err != nil {
			return err
		}
	}
	for offset, diagnostic := range dropped {
		ordinal := len(accepted) + offset
		payload, err := json.Marshal(diagnostic)
		if err != nil {
			return err
		}
		payloadDigest := securityDigest(payload)
		observation := &store.FindingObservation{
			ID:                       security.ObservationID(run.RunUID, string(task.UID), sourceGeneration, payloadDigest, ordinal),
			Namespace:                run.Namespace,
			RepositoryScan:           run.RepositoryScan,
			ScanRunID:                run.ID,
			RunUID:                   run.RunUID,
			StageReceiptID:           stageReceiptID,
			TargetReceiptID:          run.TargetReceiptID,
			SliceID:                  strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID]),
			CandidateKey:             fmt.Sprintf("dropped-%d", ordinal),
			SourceArtifactName:       security.ArtifactFindingsV2,
			SourceArtifactGeneration: sourceGeneration,
			SourceArtifactDigest:     sourceDigest,
			PolicyDigest:             run.PolicyDigest,
			Ordinal:                  ordinal,
			Disposition:              store.FindingObservationRejected,
			ReasonCode:               droppedFindingObservationReasonCode(diagnostic.Layer),
			Reason:                   diagnostic.Reason,
			NormalizedPayload:        payload,
			PayloadDigest:            payloadDigest,
		}
		if _, err := r.IntegrityStore.AcceptFindingObservation(ctx, observation); err != nil {
			return err
		}
	}
	return nil
}

func droppedFindingObservationReasonCode(layer string) string {
	switch strings.ToLower(strings.TrimSpace(layer)) {
	case "cap":
		return "candidate_cap"
	case "filter":
		return "policy_rejected"
	case "validation":
		return "schema_rejected"
	default:
		return "candidate_rejected"
	}
}

func applyObservationCoverageDegradation(run *store.ScanRun, observations []store.FindingObservation) bool {
	if run == nil {
		return false
	}
	degraded := false
	for _, observation := range observations {
		if observation.Disposition == store.FindingObservationAccepted {
			continue
		}
		degraded = true
		reason := strings.TrimSpace(observation.ReasonCode)
		if reason == "" {
			switch observation.Disposition {
			case store.FindingObservationDeferred:
				reason = "candidate_deferred"
			default:
				reason = "candidate_rejected"
			}
		}
		run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, reason)
	}
	return degraded
}

func findingObservationOccurrenceGroupKey(observation store.FindingObservation) string {
	key := strings.TrimSpace(observation.SemanticFingerprint)
	if key == "" {
		return ""
	}
	if observation.IdentityQuality != store.IdentityQualityCanonical {
		key += "\x00" + strings.TrimSpace(observation.PayloadDigest)
	}
	return key
}

func (r *RepositoryScanReconciler) listRunFindingObservations(
	ctx context.Context,
	run *store.ScanRun,
) ([]store.FindingObservation, error) {
	if r.IntegrityStore == nil || run == nil {
		return nil, nil
	}
	observations := make([]store.FindingObservation, 0)
	cursor := ""
	for {
		items, next, err := r.IntegrityStore.ListFindingObservations(ctx, store.FindingObservationFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		observations = append(observations, items...)
		if next == "" {
			return observations, nil
		}
		cursor = next
	}
}

//nolint:gocyclo // scan integrity reducers intentionally keep ordered fail-closed branches
func (r *RepositoryScanReconciler) finalizeRunOccurrences(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) error {
	if r.IntegrityStore == nil || run == nil || !security.ValidRunUID(run.RunUID) {
		return nil
	}
	allObservations, err := r.listRunFindingObservations(ctx, run)
	if err != nil {
		return err
	}
	observations := make([]store.FindingObservation, 0, len(allObservations))
	for _, observation := range allObservations {
		if observation.Disposition == store.FindingObservationAccepted {
			observations = append(observations, observation)
		}
	}
	hasUnclosedCandidates := applyObservationCoverageDegradation(run, allObservations)
	groups := map[string][]store.FindingObservation{}
	for _, observation := range observations {
		groupKey := findingObservationOccurrenceGroupKey(observation)
		if groupKey == "" {
			continue
		}
		// Producer proposals are not authoritative reconciliation keys. Only
		// byte-identical normalized candidates may collapse before a controller
		// policy promotes their identity to canonical.
		groups[groupKey] = append(groups[groupKey], observation)
	}
	groupKeys := make([]string, 0, len(groups))
	for groupKey := range groups {
		groupKeys = append(groupKeys, groupKey)
	}
	sort.Strings(groupKeys)
	finalized := make([]*store.Finding, 0, len(groupKeys))
	for _, groupKey := range groupKeys {
		group := groups[groupKey]
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		semanticFingerprint := group[0].SemanticFingerprint
		occurrenceID := security.OccurrenceID(run.RunUID, groupKey)
		if existing, err := r.IntegrityStore.GetFindingOccurrence(ctx, run.Namespace, occurrenceID); err == nil {
			var finding store.Finding
			if err := json.Unmarshal(existing.DiscoveryPayload, &finding); err != nil {
				return fmt.Errorf("decode immutable finding %q for existing occurrence %q: %w", existing.PublicFindingID, occurrenceID, err)
			}
			finding.ID = existing.PublicFindingID
			finding.Namespace = existing.Namespace
			finding.RepositoryScan = existing.RepositoryScan
			finding.ScanRunID = existing.ScanRunID
			finding.CurrentOccurrenceID = existing.ID
			finding.SemanticFingerprint = existing.SemanticFingerprint
			finding.IdentityQuality = existing.IdentityQuality
			finding.IdentityAlgorithmVersion = existing.IdentityAlgorithmVersion
			finding.LegacyFingerprint = existing.LegacyFingerprint
			finalized = append(finalized, &finding)
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var projection *store.Finding
		links := make([]store.FindingOccurrenceObservation, 0, len(group))
		for index, observation := range group {
			var finding store.Finding
			if err := json.Unmarshal(observation.NormalizedPayload, &finding); err != nil {
				return fmt.Errorf("decode finding observation %s: %w", observation.ID, err)
			}
			if projection == nil || finding.ID < projection.ID {
				candidate := finding
				projection = &candidate
			}
			relationship := store.FindingObservationRelationshipAbsorbed
			if index == 0 {
				relationship = store.FindingObservationRelationshipContributor
			}
			links = append(links, store.FindingOccurrenceObservation{
				ObservationID: observation.ID,
				Relationship:  relationship,
				Ordinal:       index,
			})
		}
		if projection == nil {
			continue
		}
		if group[0].IdentityQuality != store.IdentityQualityCanonical {
			projection.ID = security.ProvisionalFindingID(run.RunUID, groupKey)
			projection.Fingerprint = security.ProvisionalFindingFingerprint(run.RunUID, groupKey)
		}
		if group[0].IdentityQuality == store.IdentityQualityCanonical {
			alias, err := r.IntegrityStore.GetFindingAlias(ctx, run.Namespace, run.RepositoryScan, semanticFingerprint)
			if err == nil {
				existing, getErr := r.SecurityStore.GetFinding(ctx, run.Namespace, alias.PublicFindingID)
				if getErr != nil {
					return getErr
				}
				projection.ID = existing.ID
				projection.Fingerprint = existing.Fingerprint
				projection.CreatedAt = existing.CreatedAt
				projection.DecisionVersion = existing.DecisionVersion
				projection.State = findingStateOpen
				projection.ValidationStatus = "unvalidated"
				projection.ValidationJSON = ""
				projection.PatchProposalID = ""
				projection.PRNumber = nil
				projection.PRURL = ""
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		for _, observation := range group {
			var finding store.Finding
			if err := json.Unmarshal(observation.NormalizedPayload, &finding); err != nil {
				return err
			}
			projection.Evidence = mergeEvidenceRefs(projection.Evidence, finding.Evidence...)
		}
		projection.CurrentOccurrenceID = occurrenceID
		projection.SemanticFingerprint = semanticFingerprint
		projection.IdentityQuality = group[0].IdentityQuality
		projection.IdentityAlgorithmVersion = group[0].IdentityAlgorithmVersion
		projection.LegacyFingerprint = group[0].LegacyFingerprint
		projection.HistoryStatus = store.FindingHistoryCanonical
		projection.ScanRunID = run.ID
		projection.CommitSHA = run.HeadCommit
		discoveryPayload, err := json.Marshal(projection)
		if err != nil {
			return err
		}
		occurrence := store.FindingOccurrence{
			ID:                       occurrenceID,
			Namespace:                run.Namespace,
			RepositoryScan:           run.RepositoryScan,
			ScanRunID:                run.ID,
			RunUID:                   run.RunUID,
			PublicFindingID:          projection.ID,
			SemanticFindingID:        security.SemanticFindingID(groupKey),
			SemanticFingerprint:      semanticFingerprint,
			IdentityQuality:          group[0].IdentityQuality,
			IdentityAlgorithmVersion: group[0].IdentityAlgorithmVersion,
			LegacyFingerprint:        group[0].LegacyFingerprint,
			RuleID:                   group[0].RuleID,
			IdentityAnchor:           group[0].IdentityAnchor,
			IdentityInstance:         group[0].IdentityInstance,
			TargetReceiptID:          run.TargetReceiptID,
			TargetSHA:                run.HeadCommit,
			DiscoveryPayload:         discoveryPayload,
			PayloadDigest:            securityDigest(discoveryPayload),
		}
		_, err = r.IntegrityStore.FinalizeFindingOccurrence(ctx, &store.FindingOccurrenceFinalization{
			Occurrence:       occurrence,
			ObservationLinks: links,
			Projection:       *projection,
		})
		if err != nil {
			return err
		}
		storedFinding, err := r.SecurityStore.GetFinding(ctx, run.Namespace, projection.ID)
		if err != nil {
			return err
		}
		finalized = append(finalized, storedFinding)
	}
	if run.Quality.InventoryCoverageStatus == store.CoverageStatusPending {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusComplete
	}
	if security.EffectiveValidationMode(scan) == validationModeOff {
		if err := r.recordDisabledAssessments(ctx, run, groups); err != nil {
			return err
		}
		run.Quality.ValidationExecution = store.QualityExecutionComplete
		run.Quality.AttackPathExecution = store.QualityExecutionDeferred
		if hasUnclosedCandidates {
			run.Quality.CandidateCoverageStatus = store.CoverageStatusPartial
		} else {
			run.Quality.CandidateCoverageStatus = store.CoverageStatusComplete
		}
		if run.Quality.InventoryCoverageStatus == store.CoverageStatusComplete && !hasUnclosedCandidates {
			run.Quality.CoverageStatus = store.CoverageStatusComplete
		} else if hasUnclosedCandidates {
			run.Quality.CoverageStatus = store.CoverageStatusPartial
		}
		return nil
	}
	if len(finalized) == 0 {
		run.Quality.ValidationExecution = store.QualityExecutionComplete
		run.Quality.AttackPathExecution = store.QualityExecutionComplete
		if hasUnclosedCandidates {
			run.Quality.CandidateCoverageStatus = store.CoverageStatusPartial
		} else {
			run.Quality.CandidateCoverageStatus = store.CoverageStatusComplete
		}
		if run.Quality.InventoryCoverageStatus == store.CoverageStatusComplete && !hasUnclosedCandidates {
			run.Quality.CoverageStatus = store.CoverageStatusComplete
		} else if hasUnclosedCandidates {
			run.Quality.CoverageStatus = store.CoverageStatusPartial
		}
		return nil
	}
	run.Quality.ValidationExecution = store.QualityExecutionPending
	run.Quality.AttackPathExecution = store.QualityExecutionPending
	run.Quality.CandidateCoverageStatus = store.CoverageStatusPending
	if run.Quality.InventoryCoverageStatus != store.CoverageStatusPartial {
		run.Quality.CoverageStatus = store.CoverageStatusPending
	}
	if err := r.scheduleFinalizedValidation(ctx, scan, run, finalized); err != nil {
		return err
	}
	return r.recomputeRunAssessmentQuality(ctx, run)
}

func (r *RepositoryScanReconciler) scheduleFinalizedValidation(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	findings []*store.Finding,
) error {
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	created, err := r.validationTaskCountForScanRun(ctx, scan, run.ID)
	if err != nil {
		return err
	}
	for _, finding := range findings {
		if finding == nil {
			continue
		}
		if r.IntegrityStore != nil && finding.CurrentOccurrenceID != "" {
			assessments, _, err := r.IntegrityStore.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
				Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, OccurrenceID: finding.CurrentOccurrenceID,
				Kind: store.FindingAssessmentValidation, Limit: 1,
			})
			if err != nil {
				return err
			}
			if len(assessments) > 0 {
				continue
			}
		}
		if r.shouldAutoValidateFinding(scan, finding, created) {
			active, err := r.hasActiveValidationTask(ctx, scan, finding)
			if err != nil {
				return err
			}
			if !active && finding.ValidationStatus != findingValidationStatusValidated && finding.ValidationStatus != findingValidationStatusPending {
				if err := r.createValidationTask(ctx, scan, finding); err != nil {
					return err
				}
				created++
			}
			continue
		}
		if err := r.recordOccurrenceDeferrals(ctx, run, finding.CurrentOccurrenceID, "validation_not_selected",
			"assessment deferred because the finding was outside the sampled validation selection"); err != nil {
			return err
		}
	}
	return nil
}

func (r *RepositoryScanReconciler) recordDisabledAssessments(
	ctx context.Context,
	run *store.ScanRun,
	groups map[string][]store.FindingObservation,
) error {
	for fingerprint, group := range groups {
		if len(group) == 0 {
			continue
		}
		occurrenceID := security.OccurrenceID(run.RunUID, fingerprint)
		occurrence, err := r.IntegrityStore.GetFindingOccurrence(ctx, run.Namespace, occurrenceID)
		if err != nil {
			return err
		}
		_ = occurrence
		if err := r.recordOccurrenceDeferralsWithReceipt(ctx, run, occurrenceID, group[0].StageReceiptID,
			"validation_disabled", "assessment deferred because validation mode is off"); err != nil {
			return err
		}
	}
	return nil
}

func (r *RepositoryScanReconciler) recordOccurrenceDeferrals(
	ctx context.Context,
	run *store.ScanRun,
	occurrenceID string,
	failureClass string,
	summary string,
) error {
	occurrence, err := r.IntegrityStore.GetFindingOccurrence(ctx, run.Namespace, occurrenceID)
	if err != nil {
		return err
	}
	if len(occurrence.ObservationLinks) == 0 {
		return errors.New("occurrence has no observation receipt")
	}
	observation, err := r.IntegrityStore.GetFindingObservation(ctx, run.Namespace, occurrence.ObservationLinks[0].ObservationID)
	if err != nil {
		return err
	}
	return r.recordOccurrenceDeferralsWithReceipt(ctx, run, occurrenceID, observation.StageReceiptID, failureClass, summary)
}

func (r *RepositoryScanReconciler) recordOccurrenceDeferralsWithReceipt(
	ctx context.Context,
	run *store.ScanRun,
	occurrenceID string,
	stageReceiptID string,
	failureClass string,
	summary string,
) error {
	occurrence, err := r.IntegrityStore.GetFindingOccurrence(ctx, run.Namespace, occurrenceID)
	if err != nil {
		return err
	}
	for _, kind := range []store.FindingAssessmentKind{store.FindingAssessmentValidation, store.FindingAssessmentAttackPath} {
		idDigest := sha256.Sum256([]byte(strings.Join([]string{"assessment-deferred-v1", occurrenceID, string(kind), failureClass}, "\x00")))
		assessment := &store.FindingAssessment{
			ID:              "assessment_" + hex.EncodeToString(idDigest[:]),
			Namespace:       run.Namespace,
			RepositoryScan:  run.RepositoryScan,
			ScanRunID:       run.ID,
			RunUID:          run.RunUID,
			OccurrenceID:    occurrenceID,
			PublicFindingID: occurrence.PublicFindingID,
			Kind:            kind,
			StageReceiptID:  stageReceiptID,
			TargetReceiptID: run.TargetReceiptID,
			TargetSHA:       run.HeadCommit,
			Method:          "policy",
			Outcome:         assessmentOutcomeDeferred,
			FailureClass:    failureClass,
			Summary:         summary,
		}
		if _, err := r.IntegrityStore.RecordFindingAssessment(ctx, assessment); err != nil {
			return err
		}
	}
	return nil
}

func validationAssessmentHasAssuranceGap(assessment *store.FindingAssessment, scope store.ValidationScope) bool {
	if assessment == nil {
		return true
	}
	if assessment.Outcome == "skipped" {
		return true
	}
	if assessment.Outcome != assessmentOutcomeDeferred {
		return false
	}
	switch assessment.FailureClass {
	case "validation_disabled":
		return scope != store.ValidationScopeOff
	case "validation_not_selected":
		return scope != store.ValidationScopeSampled
	default:
		return true
	}
}

//nolint:gocyclo // scan integrity reducers intentionally keep ordered fail-closed branches
func (r *RepositoryScanReconciler) recomputeRunAssessmentQuality(ctx context.Context, run *store.ScanRun) error {
	if r.IntegrityStore == nil || run == nil {
		return nil
	}
	observations, err := r.listRunFindingObservations(ctx, run)
	if err != nil {
		return err
	}
	hasUnclosedCandidates := applyObservationCoverageDegradation(run, observations)
	occurrences := make([]store.FindingOccurrence, 0)
	cursor := ""
	for {
		items, next, err := r.IntegrityStore.ListFindingOccurrences(ctx, store.FindingOccurrenceFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return err
		}
		occurrences = append(occurrences, items...)
		if next == "" {
			break
		}
		cursor = next
	}
	missing := false
	validationInfrastructureFailure := false
	validationAssuranceGap := false
	attackFailure := false
	allAttackDeferred := len(occurrences) > 0
	for _, occurrence := range occurrences {
		assessments, _, err := r.IntegrityStore.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, OccurrenceID: occurrence.ID, Limit: 100,
		})
		if err != nil {
			return err
		}
		var validation, attack *store.FindingAssessment
		for i := range assessments {
			switch assessments[i].Kind {
			case store.FindingAssessmentValidation:
				validation = &assessments[i]
			case store.FindingAssessmentAttackPath:
				attack = &assessments[i]
			}
		}
		if validation == nil || attack == nil {
			missing = true
			continue
		}
		if validation.FailureClass == "artifact_invalid" || validation.FailureClass == securityTaskFailureFailed ||
			validation.FailureClass == securityTaskFailureCancelled {
			validationInfrastructureFailure = true
		}
		if validationAssessmentHasAssuranceGap(validation, run.Quality.ValidationScope) {
			validationAssuranceGap = true
		}
		if attack.FailureClass == "attack_path_not_provided" || attack.FailureClass == "artifact_invalid" ||
			attack.FailureClass == securityTaskFailureFailed || attack.FailureClass == securityTaskFailureCancelled {
			attackFailure = true
		}
		if attack.Outcome != assessmentOutcomeDeferred {
			allAttackDeferred = false
		}
	}
	if missing {
		run.Quality.ValidationExecution = store.QualityExecutionPending
		run.Quality.AttackPathExecution = store.QualityExecutionPending
		run.Quality.CandidateCoverageStatus = store.CoverageStatusPending
	} else {
		if validationInfrastructureFailure || validationAssuranceGap {
			run.Quality.ValidationExecution = store.QualityExecutionPartial
		} else {
			run.Quality.ValidationExecution = store.QualityExecutionComplete
		}
		switch {
		case attackFailure:
			run.Quality.AttackPathExecution = store.QualityExecutionPartial
		case allAttackDeferred:
			run.Quality.AttackPathExecution = store.QualityExecutionDeferred
		default:
			run.Quality.AttackPathExecution = store.QualityExecutionComplete
		}
		if validationInfrastructureFailure || validationAssuranceGap || attackFailure || hasUnclosedCandidates {
			run.Quality.CandidateCoverageStatus = store.CoverageStatusPartial
		} else {
			run.Quality.CandidateCoverageStatus = store.CoverageStatusComplete
		}
	}
	switch {
	case run.Quality.InventoryCoverageStatus == store.CoverageStatusFailed:
		run.Quality.CoverageStatus = store.CoverageStatusFailed
	case run.Quality.InventoryCoverageStatus == store.CoverageStatusPartial || run.Quality.CandidateCoverageStatus == store.CoverageStatusPartial:
		run.Quality.CoverageStatus = store.CoverageStatusPartial
	case run.Quality.InventoryCoverageStatus == store.CoverageStatusComplete && run.Quality.CandidateCoverageStatus == store.CoverageStatusComplete:
		run.Quality.CoverageStatus = store.CoverageStatusComplete
	default:
		run.Quality.CoverageStatus = store.CoverageStatusPending
	}
	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return err
	}
	return r.maybeSealRunBundle(ctx, nil, run)
}

func validationTaskBlocksBundleSealing(
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	task *corev1alpha1.Task,
) bool {
	if scan == nil || run == nil || task == nil || task.Namespace != run.Namespace ||
		!repositoryScanControlsTask(scan, task) {
		return false
	}
	if strings.TrimSpace(task.Labels[labels.LabelSecurityTarget]) != labels.SelectorValue(run.RepositoryScan) ||
		strings.TrimSpace(task.Labels[labels.LabelSecurityScanID]) != run.ID ||
		strings.TrimSpace(task.Labels[labels.LabelSecurityStage]) != security.StageValidation {
		return false
	}
	return task.Status.Phase == "" || isActiveTaskPhase(task.Status.Phase)
}

//nolint:gocyclo // Bundle sealing verifies every immutable prerequisite and replay boundary in fail-closed order.
func (r *RepositoryScanReconciler) maybeSealRunBundle(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) error {
	mode := r.IntegrityConfig.BundleSealingMode
	if mode == "" || mode == security.BundleSealingOff || r.BundleStore == nil || r.TargetReceiptStore == nil ||
		r.IntegrityStore == nil || run == nil || run.Phase != scanRunPhaseSucceeded ||
		run.Quality.BundleStatus == store.BundleStatusFailed {
		return nil
	}
	if run.Quality.CandidateCoverageStatus == store.CoverageStatusPending ||
		run.Quality.InventoryCoverageStatus == store.CoverageStatusPending ||
		strings.TrimSpace(run.TargetReceiptID) == "" {
		return nil
	}
	if existing, err := r.BundleStore.GetSecurityScanBundle(ctx, run.Namespace, run.ID); err == nil {
		if verifyErr := verifySecurityScanBundleForRun(existing, run); verifyErr != nil {
			run.Quality.BundleStatus = store.BundleStatusFailed
			run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "bundle_corrupt")
			_ = r.SecurityStore.UpdateScanRun(ctx, run)
			if mode == security.BundleSealingEnforce {
				return verifyErr
			}
			return nil
		}
		alreadySealed := run.Quality.BundleStatus == store.BundleStatusSealed
		run.Quality.BundleStatus = store.BundleStatusSealed
		if run.CompletedAt == nil {
			completed := existing.SealedAt
			run.CompletedAt = &completed
		}
		if alreadySealed {
			return nil
		}
		return r.SecurityStore.UpdateScanRun(ctx, run)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if scan == nil && r.Client != nil {
		scan = &corev1alpha1.RepositoryScan{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.RepositoryScan}, scan); err != nil {
			return err
		}
	}
	if !scanRunMatchesRepositoryScan(run, scan) {
		return errors.New("bundle construction requires the immutable RepositoryScan specification snapshot for the run generation")
	}
	if r.Client != nil {
		var validationTasks corev1alpha1.TaskList
		if err := r.List(ctx, &validationTasks,
			client.InNamespace(run.Namespace),
			client.MatchingLabels(map[string]string{
				labels.LabelSecurityTarget: labels.SelectorValue(run.RepositoryScan),
				labels.LabelSecurityScanID: run.ID,
				labels.LabelSecurityStage:  security.StageValidation,
			}),
		); err != nil {
			return err
		}
		for i := range validationTasks.Items {
			if validationTaskBlocksBundleSealing(scan, run, &validationTasks.Items[i]) {
				return nil
			}
		}
	}
	started := time.Now()
	run.Quality.BundleStatus = store.BundleStatusSealing
	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return err
	}
	sealed, err := r.buildRunBundle(ctx, scan, run)
	if err != nil {
		run.Quality.BundleStatus = store.BundleStatusRetryableFailed
		run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "bundle_seal_failed")
		_ = r.SecurityStore.UpdateScanRun(ctx, run)
		metrics.RecordSecurityBundleSealing(string(mode), "failed", time.Since(started).Seconds())
		if mode == security.BundleSealingEnforce {
			return err
		}
		return nil
	}
	if sealed {
		run.Quality.BundleStatus = store.BundleStatusSealed
		metrics.RecordSecurityBundleSealing(string(mode), "sealed", time.Since(started).Seconds())
	} else {
		run.Quality.BundleStatus = store.BundleStatusSealed
		metrics.RecordSecurityBundleSealing(string(mode), "idempotent", time.Since(started).Seconds())
	}
	return r.SecurityStore.UpdateScanRun(ctx, run)
}

func verifySecurityScanBundleForRun(sealed *store.SecurityScanBundle, run *store.ScanRun) error {
	if sealed == nil || run == nil {
		return errors.New("security bundle and scan run are required")
	}
	var evidence []securitybundle.EvidenceBlob
	if err := json.Unmarshal(sealed.EvidenceJSON, &evidence); err != nil {
		return fmt.Errorf("decode sealed bundle evidence: %w", err)
	}
	if err := securitybundle.Verify(&securitybundle.Bundle{
		ManifestJSON: sealed.ManifestJSON, FindingsJSON: sealed.FindingsJSON, CoverageJSON: sealed.CoverageJSON,
		Evidence: evidence, Roots: securitybundle.RootDigests{
			ContentDigest: sealed.ContentDigest, RunReceiptDigest: sealed.RunReceiptDigest,
		},
	}, securitybundle.DefaultLimits()); err != nil {
		return fmt.Errorf("verify sealed security bundle: %w", err)
	}
	if sealed.Namespace != run.Namespace || sealed.RepositoryScan != run.RepositoryScan ||
		sealed.RepositoryScanUID != run.RepositoryScanUID || sealed.RepositoryScanGeneration != run.RepositoryScanGeneration ||
		sealed.ScanRunID != run.ID || sealed.RunUID != run.RunUID {
		return errors.New("sealed security bundle does not match scan run binding")
	}
	return nil
}

//nolint:gocyclo // integrity flow keeps ordered fail-closed validation branches
func (r *RepositoryScanReconciler) buildRunBundle(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
) (bool, error) {
	target, err := r.TargetReceiptStore.GetSecurityTargetReceipt(ctx, run.Namespace, run.TargetReceiptID)
	if err != nil {
		return false, err
	}
	var targetReceipt security.MapperTargetReceipt
	if err := json.Unmarshal(target.ReceiptJSON, &targetReceipt); err != nil {
		return false, err
	}
	var mapperArtifact security.ReviewSlicesArtifact
	if len(target.InventoryJSON) == 0 || json.Unmarshal(target.InventoryJSON, &mapperArtifact) != nil {
		return false, errors.New("immutable mapper inventory is unavailable")
	}
	if r.RunThreatModelStore == nil {
		return false, errors.New("immutable run threat-model store is unavailable")
	}
	threatModel, err := r.RunThreatModelStore.GetSecurityRunThreatModel(ctx, run.Namespace, run.RunUID)
	if err != nil {
		return false, err
	}
	occurrences, err := listAllRunOccurrences(ctx, r.IntegrityStore, run)
	if err != nil {
		return false, err
	}
	observations, err := listAllRunObservations(ctx, r.IntegrityStore, run)
	if err != nil {
		return false, err
	}
	receipts, err := listAllRunStageReceipts(ctx, r.IntegrityStore, run)
	if err != nil {
		return false, err
	}

	bundleFindings := make([]securitybundle.Finding, 0, len(occurrences))
	occurrenceIDs := make([]string, 0, len(occurrences))
	assessmentIDs := make([]string, 0)
	assessmentByOccurrence := map[string][]string{}
	for _, occurrence := range occurrences {
		assessments, _, err := r.IntegrityStore.ListFindingAssessments(ctx, store.FindingAssessmentFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, OccurrenceID: occurrence.ID, Limit: 1000,
		})
		if err != nil {
			return false, err
		}
		for _, assessment := range assessments {
			assessmentIDs = append(assessmentIDs, assessment.ID)
			assessmentByOccurrence[occurrence.ID] = append(assessmentByOccurrence[occurrence.ID], assessment.ID)
		}
	}
	for _, occurrence := range occurrences {
		var finding store.Finding
		if err := json.Unmarshal(occurrence.DiscoveryPayload, &finding); err != nil {
			return false, err
		}
		locations := make([]securitybundle.FindingLocation, 0)
		for _, ref := range finding.Evidence {
			if ref.Kind != findingEvidenceKindFile {
				continue
			}
			var symbol *string
			if strings.TrimSpace(ref.Symbol) != "" {
				value := strings.TrimSpace(ref.Symbol)
				symbol = &value
			}
			locations = append(locations, securitybundle.FindingLocation{
				Path: ref.Path, StartLine: ref.StartLine, EndLine: ref.EndLine, Symbol: symbol,
			})
		}
		var legacy *string
		if occurrence.LegacyFingerprint != "" {
			value := occurrence.LegacyFingerprint
			legacy = &value
		}
		var instance *string
		if occurrence.IdentityInstance != "" {
			value := occurrence.IdentityInstance
			instance = &value
		}
		summary := finding.Summary
		bundleFindings = append(bundleFindings, securitybundle.Finding{
			SemanticFingerprint: occurrence.SemanticFingerprint,
			OccurrenceID:        occurrence.ID, LegacyFingerprint: legacy, RuleID: occurrence.RuleID,
			IdentityAnchor: occurrence.IdentityAnchor, IdentityInstance: instance,
			Title: finding.Title, Summary: &summary, Severity: finding.Severity, Confidence: finding.Confidence,
			Locations: locations, Evidence: []securitybundle.EvidenceReference{}, AssessmentIDs: assessmentByOccurrence[occurrence.ID],
		})
		occurrenceIDs = append(occurrenceIDs, occurrence.ID)
	}
	inventory := bundleInventoryCoverage(mapperArtifact, receipts)
	occurrenceByObservation := map[string]string{}
	for _, occurrence := range occurrences {
		for _, link := range occurrence.ObservationLinks {
			occurrenceByObservation[link.ObservationID] = occurrence.ID
		}
	}
	candidates := make([]securitybundle.CandidateCoverageEntry, 0, len(observations))
	for _, observation := range observations {
		var occurrenceID *string
		if id := occurrenceByObservation[observation.ID]; id != "" {
			value := id
			occurrenceID = &value
		}
		var reason *string
		if observation.Reason != "" {
			value := observation.Reason
			reason = &value
		}
		candidates = append(candidates, securitybundle.CandidateCoverageEntry{
			CandidateID: observation.ID, Disposition: string(observation.Disposition), OccurrenceID: occurrenceID,
			Reason: reason, ReceiptIDs: []string{observation.StageReceiptID},
		})
	}
	stages := make([]securitybundle.StageCoverageEntry, 0, len(receipts))
	stageReceiptIDs := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		var scopeID *string
		if receipt.ScopeID != "" {
			value := receipt.ScopeID
			scopeID = &value
		}
		stages = append(stages, securitybundle.StageCoverageEntry{
			Stage: receipt.Stage, ScopeID: scopeID, Disposition: string(receipt.Disposition), ReceiptID: receipt.ID,
		})
		stageReceiptIDs = append(stageReceiptIDs, receipt.ID)
	}
	publicRunID := run.ID
	var subPath *string
	if strings.Trim(strings.TrimSpace(scan.Spec.SubPath), "/") != "" {
		value := strings.Trim(strings.TrimSpace(scan.Spec.SubPath), "/")
		subPath = &value
	}
	original := strings.TrimSpace(scan.Spec.Ref)
	if original == "" {
		original = strings.TrimSpace(scan.Spec.Branch)
	}
	var originalRef *string
	if original != "" {
		originalRef = &original
	}
	policyVersion := run.ScannerPolicyVersion
	mapperVersion := fmt.Sprintf("mapper-schema-%d", mapperArtifact.SchemaVersion)
	agentNamespace := strings.TrimSpace(scan.Spec.AnalysisAgentRef.Namespace)
	if agentNamespace == "" {
		agentNamespace = scan.Namespace
	}
	authorizationMetadata := map[string]string{
		security.BundleMetadataAuthorizationBranch:         scan.Spec.Branch,
		security.BundleMetadataAuthorizationRef:            scan.Spec.Ref,
		security.BundleMetadataAuthorizationAgentName:      scan.Spec.AnalysisAgentRef.Name,
		security.BundleMetadataAuthorizationAgentNamespace: agentNamespace,
	}
	sealedAt := time.Now().UTC()
	input := securitybundle.Input{
		Manifest: securitybundle.ManifestInput{
			SchemaVersion: securitybundle.SchemaVersion,
			Repository: securitybundle.RepositoryIdentity{
				Provider: scan.Spec.Provider, RepositoryID: target.TargetID, RepoURL: scan.Spec.RepoURL, SubPath: subPath,
			},
			Target: securitybundle.TargetSnapshot{
				CommitSHA: run.HeadCommit, TreeDigest: target.TreeDigest, TargetID: target.TargetID,
				OriginalRef: originalRef, ReceiptID: target.ID, ReceiptDigest: target.PayloadDigest,
			},
			ThreatModel: securitybundle.ThreatModelInput{
				Version: strconv.FormatInt(threatModel.Version, 10), Content: threatModel.Content, Scope: subPath,
				Assumptions: []string{}, Limitations: []string{},
			},
			Quality: securitybundle.QualitySummary{
				InventoryCoverage: string(run.Quality.InventoryCoverageStatus), CandidateCoverage: string(run.Quality.CandidateCoverageStatus),
				Coverage: string(run.Quality.CoverageStatus), ValidationScope: string(run.Quality.ValidationScope),
				ValidationExecution: string(run.Quality.ValidationExecution), AttackPathExecution: string(run.Quality.AttackPathExecution),
				AnalysisAttestation: string(run.Quality.AnalysisAttestationLevel),
				TargetVerification:  string(run.Quality.TargetVerification), Authorization: string(run.Quality.AuthorizationStatus),
				Isolation: string(run.Quality.IsolationStatus),
			},
			Versions: securitybundle.ComponentVersions{
				Schema: "security-bundle-v1", Controller: securityControllerIngestionVersion,
				Mapper: &mapperVersion, Policy: &policyVersion, Additional: map[string]string{},
			},
			OccurrenceIDs: occurrenceIDs, AssessmentIDs: assessmentIDs, StageReceiptIDs: stageReceiptIDs,
			EvidenceReceiptIDs: []string{}, Metadata: authorizationMetadata,
			Run: securitybundle.RunEnvelope{
				RunUID: run.RunUID, PublicRunID: &publicRunID, Namespace: run.Namespace,
				RepositoryScanName: run.RepositoryScan, RepositoryScanUID: run.RepositoryScanUID,
				RepositoryScanGeneration: run.RepositoryScanGeneration, StartedAt: run.StartedAt,
				CompletedAt: run.CompletedAt, SealedAt: sealedAt,
			},
		},
		Findings: securitybundle.FindingsInput{SchemaVersion: securitybundle.SchemaVersion, Findings: bundleFindings, Metadata: map[string]string{}},
		Coverage: securitybundle.CoverageInput{
			SchemaVersion: securitybundle.SchemaVersion, InventoryStatus: string(run.Quality.InventoryCoverageStatus),
			CandidateStatus: string(run.Quality.CandidateCoverageStatus), CoverageStatus: string(run.Quality.CoverageStatus),
			Inventory: inventory, Candidates: candidates, Stages: stages, Metadata: map[string]string{},
		},
		Evidence: []securitybundle.EvidenceBlobInput{
			{Name: "receipts/target.json", MediaType: "application/json; charset=utf-8", Data: target.ReceiptJSON},
			{Name: "receipts/inventory.json", MediaType: "application/json; charset=utf-8", Data: target.InventoryJSON},
		},
	}
	built, err := securitybundle.Build(input, securitybundle.DefaultLimits())
	if err != nil {
		return false, err
	}
	if err := securitybundle.Verify(built, securitybundle.DefaultLimits()); err != nil {
		return false, err
	}
	evidence := built.Evidence
	if evidence == nil {
		evidence = []securitybundle.EvidenceBlob{}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return false, err
	}
	idDigest := sha256.Sum256([]byte("security-bundle-v1\x00" + run.RunUID))
	return r.BundleStore.SealSecurityScanBundle(ctx, &store.SecurityScanBundle{
		ID: "bundle_" + hex.EncodeToString(idDigest[:]), Namespace: run.Namespace, RepositoryScan: run.RepositoryScan,
		RepositoryScanUID: run.RepositoryScanUID, RepositoryScanGeneration: run.RepositoryScanGeneration,
		ScanRunID: run.ID, RunUID: run.RunUID, Version: securitybundle.SchemaVersion,
		ManifestJSON: built.ManifestJSON, FindingsJSON: built.FindingsJSON, CoverageJSON: built.CoverageJSON,
		EvidenceJSON: evidenceJSON, ContentDigest: built.Roots.ContentDigest, RunReceiptDigest: built.Roots.RunReceiptDigest, SealedAt: sealedAt,
	})
}

func bundleInventoryCoverage(artifact security.ReviewSlicesArtifact, receipts []store.StageReceipt) []securitybundle.InventoryCoverageEntry {
	sliceIDs := map[string][]string{}
	for _, reviewSlice := range artifact.Slices {
		for _, file := range reviewSlice.OwnedFiles {
			sliceIDs[file.Path] = append(sliceIDs[file.Path], reviewSlice.ID)
		}
	}
	receiptIDsBySlice := map[string][]string{}
	for _, receipt := range receipts {
		if receipt.Stage != security.StageReview || receipt.ScopeID == "" || receipt.Disposition != store.StageReceiptAccepted ||
			!strings.HasPrefix(receipt.SourceArtifactName, "security-review-context-") {
			continue
		}
		receiptIDsBySlice[receipt.ScopeID] = append(receiptIDsBySlice[receipt.ScopeID], receipt.ID)
	}
	for sliceID := range receiptIDsBySlice {
		sort.Strings(receiptIDsBySlice[sliceID])
	}
	entries := make([]securitybundle.InventoryCoverageEntry, 0, len(artifact.DiscoveredFiles)+len(artifact.ReviewableFiles))
	seen := map[string]struct{}{}
	for _, entry := range append(append([]security.MapperFileInventoryEntry{}, artifact.DiscoveredFiles...), artifact.ReviewableFiles...) {
		key := entry.Path + "\x00" + entry.Disposition + "\x00" + entry.Reason
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		var reason *string
		if entry.Reason != "" {
			value := entry.Reason
			reason = &value
		}
		receiptIDs := make([]string, 0)
		for _, sliceID := range sliceIDs[entry.Path] {
			receiptIDs = append(receiptIDs, receiptIDsBySlice[sliceID]...)
		}
		sort.Strings(receiptIDs)
		receiptIDs = slices.Compact(receiptIDs)
		entries = append(entries, securitybundle.InventoryCoverageEntry{
			Path: entry.Path, Classification: entry.Disposition, Reason: reason,
			SliceIDs: append([]string(nil), sliceIDs[entry.Path]...), ReceiptIDs: receiptIDs,
		})
	}
	return entries
}

func listAllRunOccurrences(ctx context.Context, integrity store.SecurityIntegrityStore, run *store.ScanRun) ([]store.FindingOccurrence, error) {
	items := make([]store.FindingOccurrence, 0)
	cursor := ""
	for {
		page, next, err := integrity.ListFindingOccurrences(ctx, store.FindingOccurrenceFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if next == "" {
			return items, nil
		}
		cursor = next
	}
}

func listAllRunObservations(ctx context.Context, integrity store.SecurityIntegrityStore, run *store.ScanRun) ([]store.FindingObservation, error) {
	items := make([]store.FindingObservation, 0)
	cursor := ""
	for {
		page, next, err := integrity.ListFindingObservations(ctx, store.FindingObservationFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if next == "" {
			return items, nil
		}
		cursor = next
	}
}

func listAllRunStageReceipts(ctx context.Context, integrity store.SecurityIntegrityStore, run *store.ScanRun) ([]store.StageReceipt, error) {
	items := make([]store.StageReceipt, 0)
	cursor := ""
	for {
		page, next, err := integrity.ListStageReceipts(ctx, store.StageReceiptFilter{
			Namespace: run.Namespace, RepositoryScan: run.RepositoryScan, ScanRunID: run.ID, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if next == "" {
			return items, nil
		}
		cursor = next
	}
}

func capAcceptedFindingsForRun(scan *corev1alpha1.RepositoryScan, run *store.ScanRun, accepted []*store.Finding) ([]*store.Finding, []security.DroppedFindingDiagnostic) {
	if len(accepted) == 0 {
		return accepted, nil
	}
	limit := int(security.EffectiveMaxFindingsPerRun(scan))
	remaining := limit - run.AcceptedFindings
	if remaining >= len(accepted) {
		return accepted, nil
	}
	if remaining < 0 {
		remaining = 0
	}

	dropped := make([]security.DroppedFindingDiagnostic, 0, len(accepted)-remaining)
	for i, finding := range accepted[remaining:] {
		dropped = append(dropped, cappedFindingDiagnostic(remaining+i, finding, limit))
	}
	return accepted[:remaining], dropped
}

func cappedFindingDiagnostic(index int, finding *store.Finding, limit int) security.DroppedFindingDiagnostic {
	sample := map[string]string{}
	if finding != nil {
		if strings.TrimSpace(finding.Title) != "" {
			sample["title"] = finding.Title
		}
		if strings.TrimSpace(finding.Category) != "" {
			sample["category"] = finding.Category
		}
		if strings.TrimSpace(finding.Severity) != "" {
			sample["severity"] = finding.Severity
		}
	}
	return security.DroppedFindingDiagnostic{
		Index:  index,
		Reason: fmt.Sprintf("maxFindingsPerRun limit %d reached", limit),
		Sample: sample,
		Layer:  "cap",
	}
}

func (r *RepositoryScanReconciler) reviewSliceForTaskRun(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	sliceID string,
	runID string,
) (*store.ReviewSlice, bool, error) {
	if strings.TrimSpace(sliceID) == "" {
		return nil, false, nil
	}
	reviewSlice, err := r.SecurityStore.GetReviewSlice(ctx, scan.Namespace, scan.Name, sliceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if reviewSlice.LastScanRunID != "" && reviewSlice.LastScanRunID != runID {
		return reviewSlice, true, nil
	}
	return reviewSlice, false, nil
}

//nolint:gocyclo // scan integrity reducers intentionally keep ordered fail-closed branches
func (r *RepositoryScanReconciler) ingestMapperTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun) error {
	if run.Phase == scanRunPhaseFailed {
		return nil
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		artifact, rawArtifact, validationProblem, err := r.loadReviewSlicesArtifact(ctx, task)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			run.ErrorMessage = "mapper stage failed: " + validationProblem
		} else if artifact != nil {
			normalizedArtifact, marshalErr := json.Marshal(artifact)
			if marshalErr != nil {
				return marshalErr
			}
			if scanRunUsesPinnedTarget(run) {
				if artifact.SchemaVersion != security.SchemaVersionReviewSlicesV2 || artifact.TargetReceipt == nil {
					run.ErrorMessage = "mapper stage failed: pinned target mode requires mapper schema v2 target receipt"
					run.Quality.TargetVerification = store.TargetVerificationMismatch
					metrics.RecordSecurityTargetVerification("missing_receipt")
					return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
				}
				resolvedHead := strings.ToLower(strings.TrimSpace(artifact.TargetReceipt.HeadOID))
				if err := validateRequestedCommitTarget(scan.Spec.Ref, artifact.TargetReceipt.ObjectFormat, resolvedHead); err != nil {
					run.ErrorMessage = "mapper stage failed: " + err.Error()
					run.Quality.TargetVerification = store.TargetVerificationMismatch
					metrics.RecordSecurityTargetVerification("requested_oid_mismatch")
					return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
				}
				if expected := strings.ToLower(strings.TrimSpace(run.HeadCommit)); expected != "" && expected != resolvedHead {
					run.ErrorMessage = fmt.Sprintf("mapper stage failed: resolved head %q does not match requested head %q", resolvedHead, expected)
					run.Quality.TargetVerification = store.TargetVerificationMismatch
					metrics.RecordSecurityTargetVerification("mismatch")
					return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
				}
				targetBytes, marshalErr := json.Marshal(artifact.TargetReceipt)
				if marshalErr != nil {
					return marshalErr
				}
				targetDigest := securityDigest(targetBytes)
				run.HeadCommit = resolvedHead
				run.TargetReceiptID = securityTargetReceiptID(run.RunUID, targetDigest)
				if r.TargetReceiptStore != nil {
					if _, err := r.TargetReceiptStore.SaveSecurityTargetReceipt(ctx, &store.SecurityTargetReceipt{
						ID:              run.TargetReceiptID,
						Namespace:       run.Namespace,
						RepositoryScan:  run.RepositoryScan,
						ScanRunID:       run.ID,
						RunUID:          run.RunUID,
						TargetID:        security.RepositoryTargetID(scan),
						HeadSHA:         resolvedHead,
						ObjectFormat:    artifact.TargetReceipt.ObjectFormat,
						SnapshotDigest:  artifact.TargetReceipt.SnapshotDigest,
						TreeDigest:      artifact.TargetReceipt.TreeDigest,
						ReceiptJSON:     targetBytes,
						InventoryJSON:   normalizedArtifact,
						InventoryDigest: securityDigest(normalizedArtifact),
						PayloadDigest:   targetDigest,
					}); err != nil {
						return err
					}
				}
				run.ResolvedTargetKey = security.ResolvedTargetKey(
					security.RepositoryTargetID(scan), run.BaseCommit, run.HeadCommit, scan.Spec.SubPath, run.PolicyDigest,
				)
				run.Quality.TargetVerification = store.TargetVerificationVerified
				run.Quality.AnalysisAttestationLevel = store.AnalysisAttestationToolObserved
				if artifact.TargetReceipt.TreeIndexTruncated {
					run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "tree_index_truncated")
				}
			} else if artifact.SchemaVersion == security.SchemaVersionReviewSlices {
				run.Quality.TargetVerification = store.TargetVerificationUnverified
			}
			updateMapperQuality(run, artifact)
			receiptCreated, err := r.appendStageReceiptCreated(
				ctx, task, run, security.ArtifactSlices, rawArtifact, normalizedArtifact,
				store.StageReceiptAccepted, "", "",
			)
			if err != nil {
				return err
			}
			if receiptCreated {
				recordSecurityInventoryMetrics(artifact)
				if scanRunUsesPinnedTarget(run) {
					metrics.RecordSecurityTargetVerification("verified")
				}
			}
			changedFiles := changedFileSet(artifact.ChangedFiles)
			incrementalSelection := run.Mode == scanModeIncremental && artifact.ChangedFilesComputed
			annotateChangedMetadata := artifact.ChangedFilesComputed && (run.Mode == scanModeIncremental || run.Mode == scanModeManual)
			skippedSlices := 0
			for i := range artifact.Slices {
				slice := artifact.Slices[i]
				slice.Namespace = scan.Namespace
				slice.RepositoryScan = scan.Name
				slice.LastScanRunID = run.ID
				if annotateChangedMetadata {
					attachChangedMetadataToReviewSlice(&slice, artifact.ChangedFiles, artifact.ChangedLineRanges)
				}
				if incrementalSelection {
					if reviewSliceMatchesChangedFiles(slice, changedFiles) {
						slice.Status = reviewSliceStatusPending
					} else {
						slice.Status = reviewSliceStatusSkipped
						skippedSlices++
					}
				}
				if err := r.preserveCurrentRunReviewSliceTerminalState(ctx, scan, &slice); err != nil {
					return err
				}
				if err := r.SecurityStore.UpsertReviewSlice(ctx, &slice); err != nil {
					return err
				}
			}
			clearRunError(run)
			if artifact.BaseCommit != "" {
				run.BaseCommit = artifact.BaseCommit
			}
			if artifact.HeadCommit != "" {
				run.HeadCommit = artifact.HeadCommit
			}
			if run.PolicyDigest == "" {
				run.PolicyDigest = security.ScannerPolicyDigest(security.ScannerPolicy{})
			}
			if run.IdempotencyKey == "" {
				run.IdempotencyKey = security.ScanRunIdempotencyKey(scan.Namespace, scan.Name, run.Mode, run.BaseCommit, run.HeadCommit, scan.Spec.SubPath, run.PolicyDigest)
			}
			run.SliceCount = len(artifact.Slices)
			run.SkippedSliceCount = skippedSlices
			switch {
			case incrementalSelection && skippedSlices == len(artifact.Slices):
				run.Summary = fmt.Sprintf("Threat model generated; no review slices matched %d changed files", len(artifact.ChangedFiles))
			case incrementalSelection:
				run.Summary = fmt.Sprintf(
					"Threat model generated; deterministic mapper selected %d/%d review slices from %d changed files",
					len(artifact.Slices)-skippedSlices,
					len(artifact.Slices),
					len(artifact.ChangedFiles),
				)
			case run.Mode == scanModeIncremental && artifact.ChangedFilesError != "":
				run.Summary = fmt.Sprintf("Threat model generated; deterministic mapper produced %d review slices after changed-file selection failed", len(artifact.Slices))
			default:
				run.Summary = fmt.Sprintf("Threat model generated; deterministic mapper produced %d review slices", len(artifact.Slices))
			}
		}
	} else {
		run.ErrorMessage = r.pipelineTaskSummary(ctx, task, "mapper stage failed")
		if err := r.appendStageReceipt(ctx, task, run, security.ArtifactSlices, nil, nil,
			store.StageReceiptRejected, securityTaskFailureClass(task), run.ErrorMessage); err != nil {
			return err
		}
	}

	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
}

func appendQualityReason(reasons []string, reason string) []string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return reasons
	}
	if slices.Contains(reasons, reason) {
		return reasons
	}
	if len(reasons) >= 16 {
		return reasons
	}
	return append(reasons, reason)
}

type securityInventoryMetricRecord struct {
	path        string
	disposition string
	reason      string
}

// recordSecurityInventoryMetrics counts each canonical inventory record once.
// Paths participate only in local de-duplication and never become metric labels.
func recordSecurityInventoryMetrics(artifact *security.ReviewSlicesArtifact) {
	if artifact == nil || artifact.SchemaVersion != security.SchemaVersionReviewSlicesV2 {
		return
	}
	seen := make(map[securityInventoryMetricRecord]struct{}, len(artifact.DiscoveredFiles)+len(artifact.ReviewableFiles)+len(artifact.OmittedFiles))
	for _, entries := range [][]security.MapperFileInventoryEntry{
		artifact.DiscoveredFiles,
		artifact.ReviewableFiles,
		artifact.OmittedFiles,
	} {
		for _, entry := range entries {
			record := securityInventoryMetricRecord{
				path:        entry.Path,
				disposition: entry.Disposition,
				reason:      entry.Reason,
			}
			if _, ok := seen[record]; ok {
				continue
			}
			seen[record] = struct{}{}
			metrics.RecordSecurityInventoryEntries(entry.Disposition, entry.Reason, 1)
		}
	}
	if summary := artifact.InventorySummary; summary != nil {
		truncated := summary.TruncatedEntries
		if summary.OmissionRecords != nil {
			truncated += summary.OmissionRecords.TruncatedRecords
		}
		if truncated > 0 {
			reason := summary.Reason
			if strings.TrimSpace(reason) == "" {
				reason = security.MapperCoverageReasonInventoryEntryLimit
			}
			metrics.RecordSecurityInventoryEntries("truncated", reason, truncated)
		}
	}
}

func updateMapperQuality(run *store.ScanRun, artifact *security.ReviewSlicesArtifact) {
	if run == nil || artifact == nil {
		return
	}
	if artifact.SchemaVersion != security.SchemaVersionReviewSlicesV2 {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusUnknown
		run.Quality.CoverageStatus = store.CoverageStatusUnknown
		run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "mapper_schema_v1")
		return
	}
	mapperPartial := artifact.CoverageStatus == security.MapperCoveragePartial ||
		(artifact.InventorySummary != nil && artifact.InventorySummary.Truncated)
	if mapperPartial {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusPartial
		run.Quality.CoverageStatus = store.CoverageStatusPartial
		if len(artifact.CoverageReasonCodes) == 0 {
			run.Quality.ReasonCodes = appendQualityReason(
				run.Quality.ReasonCodes,
				security.MapperCoverageReasonInventoryEntryLimit,
			)
		} else {
			for _, reason := range artifact.CoverageReasonCodes {
				run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, reason)
			}
		}
	} else if artifact.CoverageStatus != security.MapperCoverageAccountable {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusUnknown
		run.Quality.CoverageStatus = store.CoverageStatusUnknown
		run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "mapper_coverage_unknown")
		return
	}
	assigned := 0
	omitted := 0
	for _, entry := range artifact.ReviewableFiles {
		switch entry.Disposition {
		case security.MapperDispositionAssigned:
			assigned++
		case security.MapperDispositionOmitted:
			omitted++
		}
	}
	if omitted > 0 {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusPartial
		run.Quality.CoverageStatus = store.CoverageStatusPartial
		run.Quality.ReasonCodes = appendQualityReason(run.Quality.ReasonCodes, "eligible_paths_omitted")
		mapperPartial = true
	}
	if mapperPartial {
		return
	}
	if assigned == 0 {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusComplete
	} else {
		run.Quality.InventoryCoverageStatus = store.CoverageStatusPending
	}
	if run.Quality.CandidateCoverageStatus == store.CoverageStatusUnknown {
		run.Quality.CandidateCoverageStatus = store.CoverageStatusPending
	}
	run.Quality.CoverageStatus = store.CoverageStatusPending
}

func (r *RepositoryScanReconciler) preserveCurrentRunReviewSliceTerminalState(ctx context.Context, scan *corev1alpha1.RepositoryScan, slice *store.ReviewSlice) error {
	if slice == nil || strings.TrimSpace(slice.ID) == "" || strings.TrimSpace(slice.LastScanRunID) == "" {
		return nil
	}
	existing, err := r.SecurityStore.GetReviewSlice(ctx, scan.Namespace, scan.Name, slice.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.LastScanRunID != slice.LastScanRunID || !terminalReviewSliceStatus(existing.Status) {
		return nil
	}
	slice.Status = existing.Status
	slice.LastReviewedAt = existing.LastReviewedAt
	return nil
}

func terminalReviewSliceStatus(status string) bool {
	switch status {
	case reviewSliceStatusReviewed, reviewSliceStatusFailed, reviewSliceStatusCompleted:
		return true
	default:
		return false
	}
}

func (r *RepositoryScanReconciler) ingestScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) error {
	_, err := r.ingestReservedScanTask(ctx, scan, task)
	return err
}

func (r *RepositoryScanReconciler) ingestReservedScanTask(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
) (*store.ScanRun, error) {
	run, err := r.getReservedScanRun(ctx, scan, task)
	if err != nil || run == nil {
		return nil, err
	}
	if run.Quality.BundleStatus == store.BundleStatusSealing || run.Quality.BundleStatus == store.BundleStatusSealed {
		return run, nil
	}
	switch taskSecurityStage(task) {
	case security.StageThreatModel:
		return run, r.ingestThreatModelTask(ctx, scan, task, run)
	case security.StageMapper:
		return run, r.ingestMapperTask(ctx, scan, task, run)
	case security.StageReview:
		return run, r.ingestReviewTask(ctx, scan, task, run)
	default:
		return run, nil
	}
}

//nolint:gocyclo // integrity flow keeps ordered fail-closed validation branches
func (r *RepositoryScanReconciler) ingestValidationTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) error {
	findingID, err := taskSecurityFindingID(task)
	if err != nil {
		return err
	}
	if findingID == "" {
		return nil
	}
	taskOccurrenceID, err := taskSecurityOccurrenceID(task)
	if err != nil {
		return err
	}
	requireRunBinding, supportedBinding := validationTaskRunBinding(task)
	if !supportedBinding {
		return nil
	}

	finding, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, findingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	taskRunID := strings.TrimSpace(task.Labels[labels.LabelSecurityScanID])
	if requireRunBinding && taskRunID == "" {
		return nil
	}
	sourceRunID := finding.ScanRunID
	if taskRunID != "" {
		sourceRunID = taskRunID
	}
	var run *store.ScanRun
	if sourceRunID != "" {
		run, err = r.SecurityStore.GetScanRun(ctx, scan.Namespace, sourceRunID)
		if errors.Is(err, store.ErrNotFound) && requireRunBinding {
			return nil
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if requireRunBinding {
		if !scanRunHasImmutableRepositoryBinding(run, scan) {
			return nil
		}
	} else if run != nil && scanRunExplicitlyMismatchesRepositoryScan(run, scan) {
		return nil
	}
	if run != nil && (run.Quality.BundleStatus == store.BundleStatusSealing || run.Quality.BundleStatus == store.BundleStatusSealed) {
		// Supplemental bundle revisions are not implemented. A validation result
		// that arrives after the frozen bundle boundary must not mutate immutable
		// assessment history or the run quality projected by that bundle.
		return nil
	}
	assessmentFinding := finding
	projectCurrentOccurrence := taskOccurrenceID == "" && finding.CurrentOccurrenceID == "" &&
		(taskRunID == "" || taskRunID == finding.ScanRunID)
	if taskOccurrenceID != "" && r.IntegrityStore != nil {
		occurrence, occurrenceErr := r.IntegrityStore.GetFindingOccurrence(ctx, scan.Namespace, taskOccurrenceID)
		if occurrenceErr != nil {
			if errors.Is(occurrenceErr, store.ErrNotFound) {
				return nil
			}
			return occurrenceErr
		}
		if occurrence.PublicFindingID != findingID || (taskRunID != "" && occurrence.ScanRunID != taskRunID) {
			return nil
		}
		var immutableFinding store.Finding
		if err := json.Unmarshal(occurrence.DiscoveryPayload, &immutableFinding); err != nil {
			return fmt.Errorf("decode validation occurrence %q discovery payload: %w", occurrence.ID, err)
		}
		immutableFinding.ID = occurrence.PublicFindingID
		immutableFinding.Namespace = occurrence.Namespace
		immutableFinding.RepositoryScan = occurrence.RepositoryScan
		immutableFinding.ScanRunID = occurrence.ScanRunID
		immutableFinding.CurrentOccurrenceID = occurrence.ID
		assessmentFinding = &immutableFinding
		projectCurrentOccurrence = finding.ScanRunID == occurrence.ScanRunID && finding.CurrentOccurrenceID == occurrence.ID
	}
	immutableAssessment := r.IntegrityStore != nil &&
		run != nil && security.ValidRunUID(run.RunUID) && assessmentFinding.CurrentOccurrenceID != "" && task.UID != ""
	if immutableAssessment && !scanRunMatchesRepositoryScan(run, scan) {
		return nil
	}
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		artifacts, validationProblem, err := r.loadValidationTaskArtifacts(ctx, scan, task, assessmentFinding, run)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			assessmentFinding.ValidationStatus = findingValidationStatusFailed
			assessmentFinding.ValidationJSON = normalizedValidationFailureJSON("artifact_invalid", validationProblem)
			if immutableAssessment {
				normalizedFailure := []byte(assessmentFinding.ValidationJSON)
				receiptCreated, err := r.appendStageReceiptCreated(
					ctx, task, run, security.ArtifactValidation, nil, normalizedFailure,
					store.StageReceiptRejected, "artifact_invalid", validationProblem,
				)
				if err != nil {
					return err
				}
				receiptID := r.stageReceiptIDFor(
					ctx, run, task, security.ArtifactValidation, nil, store.StageReceiptRejected,
				)
				assessmentCreated, assessmentErr := r.recordValidationAssessment(
					ctx, scan, run, assessmentFinding, task, nil, "error", "artifact_invalid",
					validationProblem, projectCurrentOccurrence, receiptID,
				)
				if receiptCreated || assessmentCreated {
					metrics.RecordSecurityValidationRejection("artifact_invalid")
				}
				return assessmentErr
			}
		} else if artifacts != nil {
			assessmentFinding.ValidationStatus = artifacts.artifact.Status
			assessmentFinding.ValidationJSON = artifacts.rawJSON
			for _, ref := range artifacts.artifact.Evidence {
				if ref.Kind == "artifact" && ref.TaskName == "" {
					ref.TaskName = task.Name
				}
				assessmentFinding.Evidence = mergeEvidenceRefs(assessmentFinding.Evidence, ref)
			}
			assessmentFinding.Evidence = mergeEvidenceRefs(assessmentFinding.Evidence, store.FindingEvidenceRef{
				Kind:     "artifact",
				TaskName: task.Name,
				Name:     security.ArtifactValidation,
				Label:    "Validation JSON",
			})
			if artifacts.transcript != "" {
				assessmentFinding.Evidence = mergeEvidenceRefs(assessmentFinding.Evidence, store.FindingEvidenceRef{
					Kind:     "artifact",
					TaskName: task.Name,
					Name:     security.ArtifactValidationText,
					Label:    "Validation transcript",
				})
			}
			if immutableAssessment {
				normalized := []byte(artifacts.rawJSON)
				if err := r.appendStageReceipt(ctx, task, run, security.ArtifactValidation, artifacts.rawSource, normalized,
					store.StageReceiptAccepted, "", ""); err != nil {
					return err
				}
				receiptID := r.stageReceiptIDFor(
					ctx, run, task, security.ArtifactValidation, artifacts.rawSource, store.StageReceiptAccepted,
				)
				_, err := r.recordValidationAssessment(
					ctx, scan, run, assessmentFinding, task, &artifacts.artifact,
					artifacts.artifact.Status, "", artifacts.artifact.Summary,
					projectCurrentOccurrence, receiptID,
				)
				return err
			}
		}
	} else {
		summary := r.pipelineTaskSummary(ctx, task, "validation task failed")
		failureClass := securityTaskFailureClass(task)
		assessmentFinding.ValidationStatus = findingValidationStatusFailed
		assessmentFinding.ValidationJSON = normalizedValidationFailureJSON(failureClass, summary)
		if immutableAssessment {
			normalizedFailure := []byte(assessmentFinding.ValidationJSON)
			if err := r.appendStageReceipt(ctx, task, run, security.ArtifactValidation, nil, normalizedFailure,
				store.StageReceiptRejected, failureClass, summary); err != nil {
				return err
			}
			receiptID := r.stageReceiptIDFor(
				ctx, run, task, security.ArtifactValidation, nil, store.StageReceiptRejected,
			)
			_, err := r.recordValidationAssessment(
				ctx, scan, run, assessmentFinding, task, nil, "error", failureClass,
				summary, projectCurrentOccurrence, receiptID,
			)
			return err
		}
	}

	if immutableAssessment || !projectCurrentOccurrence {
		return nil
	}
	return r.SecurityStore.UpsertFinding(ctx, assessmentFinding)
}

func (r *RepositoryScanReconciler) recordValidationAssessment(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	run *store.ScanRun,
	finding *store.Finding,
	task *corev1alpha1.Task,
	artifact *security.ValidationArtifact,
	outcome string,
	failureClass string,
	summary string,
	projectCurrentOccurrence bool,
	stageReceiptID string,
) (bool, error) {
	if r.IntegrityStore == nil || run == nil || finding == nil || task == nil {
		return false, nil
	}
	if !scanRunMatchesRepositoryScan(run, scan) {
		return false, nil
	}
	payload := []byte(finding.ValidationJSON)
	assessmentID := securityReceiptID("assessment-validation-v1", finding.CurrentOccurrenceID, stageReceiptID)
	attackAssessmentID := securityReceiptID("assessment-attack-path-v1", finding.CurrentOccurrenceID, stageReceiptID)
	existingValidationAssessment, validationErr := r.IntegrityStore.GetFindingAssessment(ctx, run.Namespace, assessmentID)
	if validationErr != nil && !errors.Is(validationErr, store.ErrNotFound) {
		return false, validationErr
	}
	existingAttackAssessment, attackErr := r.IntegrityStore.GetFindingAssessment(ctx, run.Namespace, attackAssessmentID)
	if attackErr != nil && !errors.Is(attackErr, store.ErrNotFound) {
		return false, attackErr
	}
	if existingValidationAssessment != nil && existingAttackAssessment != nil {
		if err := r.recomputeRunAssessmentQuality(ctx, run); err != nil {
			return false, err
		}
		if run.RepositoryScanUID != "" && run.RepositoryScanGeneration > 0 && !scanRunMatchesRepositoryScan(run, scan) {
			return false, nil
		}
		return false, r.refreshScanRunStatus(ctx, scan, run, run.ID, true)
	}
	assessment := &store.FindingAssessment{
		ID:                         assessmentID,
		Namespace:                  run.Namespace,
		RepositoryScan:             run.RepositoryScan,
		ScanRunID:                  run.ID,
		RunUID:                     run.RunUID,
		OccurrenceID:               finding.CurrentOccurrenceID,
		PublicFindingID:            finding.ID,
		Kind:                       store.FindingAssessmentValidation,
		StageReceiptID:             stageReceiptID,
		TargetReceiptID:            run.TargetReceiptID,
		TargetSHA:                  run.HeadCommit,
		Method:                     "validator-agent",
		Outcome:                    outcome,
		FailureClass:               failureClass,
		Summary:                    summary,
		NormalizedPayload:          payload,
		PayloadDigest:              securityDigest(payload),
		ProjectionValidationStatus: "",
	}
	if projectCurrentOccurrence {
		assessment.ProjectionValidationStatus = finding.ValidationStatus
		assessment.ProjectionEvidence = append([]store.FindingEvidenceRef(nil), finding.Evidence...)
	}
	validationCreated, err := r.IntegrityStore.RecordFindingAssessment(ctx, assessment)
	if err != nil {
		return validationCreated, err
	}
	attackOutcome := "deferred"
	attackFailureClass := "attack_path_not_provided"
	attackSummary := "attack-path assessment was not provided"
	attackPayload := append([]byte(nil), payload...)
	if failureClass != "" {
		attackFailureClass = failureClass
		attackSummary = "attack-path assessment unavailable because validation did not complete successfully"
	} else if artifact != nil && strings.TrimSpace(artifact.AttackPathAnalysis) != "" {
		attackOutcome = "complete"
		attackFailureClass = ""
		attackSummary = strings.TrimSpace(artifact.AttackPathAnalysis)
	}
	attackAssessment := &store.FindingAssessment{
		ID:                attackAssessmentID,
		Namespace:         run.Namespace,
		RepositoryScan:    run.RepositoryScan,
		ScanRunID:         run.ID,
		RunUID:            run.RunUID,
		OccurrenceID:      finding.CurrentOccurrenceID,
		PublicFindingID:   finding.ID,
		Kind:              store.FindingAssessmentAttackPath,
		StageReceiptID:    stageReceiptID,
		TargetReceiptID:   run.TargetReceiptID,
		TargetSHA:         run.HeadCommit,
		Method:            "validator-agent",
		Outcome:           attackOutcome,
		FailureClass:      attackFailureClass,
		Summary:           attackSummary,
		NormalizedPayload: attackPayload,
	}
	if len(attackPayload) > 0 {
		attackAssessment.PayloadDigest = securityDigest(attackPayload)
	}
	attackCreated, err := r.IntegrityStore.RecordFindingAssessment(ctx, attackAssessment)
	created := validationCreated || attackCreated
	if err != nil {
		return created, err
	}
	if err := r.recomputeRunAssessmentQuality(ctx, run); err != nil {
		return created, err
	}
	if run.RepositoryScanUID != "" && run.RepositoryScanGeneration > 0 && !scanRunMatchesRepositoryScan(run, scan) {
		return created, nil
	}
	return created, r.refreshScanRunStatus(ctx, scan, run, run.ID, true)
}

func normalizedValidationFailureJSON(failureClass, summary string) string {
	summary = strings.TrimSpace(summary)
	if len(summary) > security.MaxValidationSummaryBytes {
		summary = summary[:security.MaxValidationSummaryBytes]
		for len(summary) > 0 && !utf8.ValidString(summary) {
			summary = summary[:len(summary)-1]
		}
	}
	payload := struct {
		Version      int    `json:"version"`
		Status       string `json:"status"`
		FailureClass string `json:"failureClass"`
		Summary      string `json:"summary"`
	}{
		Version:      security.ValidationArtifactSchemaVersion,
		Status:       findingValidationStatusFailed,
		FailureClass: strings.TrimSpace(failureClass),
		Summary:      summary,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"version":1,"status":"failed","failureClass":"artifact_invalid","summary":"validation ingestion failed"}`
	}
	return string(data)
}

type patchVerificationResult struct {
	diffArtifact    string
	summaryArtifact string
}

func patchArtifactNames(findingID string) (string, string) {
	return fmt.Sprintf("security-patch-%s.diff", findingID), fmt.Sprintf("security-patch-%s.json", findingID)
}

func patchTaskRequiresArtifactVerification(task *corev1alpha1.Task, findingID string) bool {
	return task != nil && strings.TrimSpace(findingID) != ""
}

func normalizedPatchDiff(diff string) string {
	diff = strings.ReplaceAll(diff, "\r\n", "\n")
	diff = strings.ReplaceAll(diff, "\r", "\n")
	lines := strings.Split(diff, "\n")
	normalized := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "index ") {
			continue
		}
		normalized = append(normalized, line)
	}
	for len(normalized) > 0 && strings.TrimSpace(normalized[0]) == "" {
		normalized = normalized[1:]
	}
	for len(normalized) > 0 && strings.TrimSpace(normalized[len(normalized)-1]) == "" {
		normalized = normalized[:len(normalized)-1]
	}
	return strings.Join(normalized, "\n")
}

func (r *RepositoryScanReconciler) verifyPatchTaskArtifacts(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, findingID string, sr *common.StructuredResult) (patchVerificationResult, string, error) {
	if r.ArtifactStore == nil {
		return patchVerificationResult{}, "artifact store is not configured", nil
	}
	if sr == nil {
		return patchVerificationResult{}, "structured task result is missing", nil
	}
	if strings.TrimSpace(sr.Diff) == "" {
		return patchVerificationResult{}, "structured task result does not include a workspace diff", nil
	}

	diffName, summaryName := patchArtifactNames(findingID)
	diffData, err := r.taskArtifact(ctx, task, diffName)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return patchVerificationResult{}, fmt.Sprintf("%s is missing", diffName), nil
	default:
		return patchVerificationResult{}, "", err
	}
	summaryData, err := r.taskArtifact(ctx, task, summaryName)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return patchVerificationResult{}, fmt.Sprintf("%s is missing", summaryName), nil
	default:
		return patchVerificationResult{}, "", err
	}

	var summary security.PatchSummaryArtifact
	if err := json.Unmarshal(summaryData, &summary); err != nil {
		return patchVerificationResult{}, fmt.Sprintf("%s is invalid JSON: %v", summaryName, err), nil
	}
	if summary.SchemaVersion != security.SchemaVersionPatchSummary {
		return patchVerificationResult{}, fmt.Sprintf("%s has unsupported schemaVersion %d", summaryName, summary.SchemaVersion), nil
	}
	if strings.TrimSpace(summary.FindingID) != findingID {
		return patchVerificationResult{}, fmt.Sprintf("%s findingId does not match finding", summaryName), nil
	}
	if !sameStringSet(rootRelativePatchSummaryFiles(summary.ChangedFiles, scan), sr.Files) {
		return patchVerificationResult{}, "patch summary changedFiles do not match actual workspace changed files", nil
	}
	artifactDiff := normalizedPatchDiff(string(diffData))
	if artifactDiff == "" {
		return patchVerificationResult{}, "patch diff artifact is empty", nil
	}
	if artifactDiff != normalizedPatchDiff(sr.Diff) {
		return patchVerificationResult{}, "patch diff artifact does not match actual workspace diff", nil
	}
	return patchVerificationResult{diffArtifact: diffName, summaryArtifact: summaryName}, "", nil
}

func rootRelativePatchSummaryFiles(files []string, scan *corev1alpha1.RepositoryScan) []string {
	subPath := ""
	if scan != nil {
		subPath = strings.Trim(strings.TrimSpace(strings.ReplaceAll(scan.Spec.SubPath, "\\", "/")), "/")
	}
	if subPath == "" || subPath == "." || !security.SafeRepoPath(subPath) {
		return files
	}

	out := make([]string, 0, len(files))
	for _, file := range files {
		normalized := normalizeRepoPath(file)
		for strings.HasPrefix(normalized, "./") {
			normalized = strings.TrimPrefix(normalized, "./")
		}
		if normalized == "" || normalized == subPath || strings.HasPrefix(normalized, subPath+"/") || strings.HasPrefix(normalized, "/") {
			out = append(out, normalized)
			continue
		}
		out = append(out, subPath+"/"+normalized)
	}
	return out
}

func sameStringSet(left, right []string) bool {
	normalize := func(values []string) []string {
		out := make([]string, 0, len(values))
		seen := map[string]struct{}{}
		for _, value := range values {
			value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
		slices.Sort(out)
		return out
	}
	left = normalize(left)
	right = normalize(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (r *RepositoryScanReconciler) updatePatchProposalFromSucceededTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, findingID string, proposal *store.PatchProposal) error {
	switch {
	case r.ResultStore == nil:
		proposal.Status = scanRunPhasePending
		return nil
	case task.Status.ResultRef == nil || !task.Status.ResultRef.Available:
		proposal.Status = scanRunPhasePending
		return nil
	}

	result, err := r.taskResult(ctx, task)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			proposal.Status = scanRunPhasePending
			return nil
		}
		return err
	}

	sr := common.ParseStructuredResult(string(result))
	switch {
	case strings.TrimSpace(sr.PushError) != "":
		proposal.Status = scanRunPhaseFailed
	case strings.TrimSpace(sr.PushBranch) == "":
		proposal.Status = scanRunPhaseFailed
	default:
		var verified patchVerificationResult
		if patchTaskRequiresArtifactVerification(task, findingID) {
			var reason string
			verified, reason, err = r.verifyPatchTaskArtifacts(ctx, scan, task, findingID, sr)
			if err != nil {
				return err
			}
			if reason != "" {
				proposal.Status = scanRunPhaseFailed
				return nil
			}
		}
		proposal.Branch = strings.TrimSpace(sr.PushBranch)
		proposal.DiffArtifact = verified.diffArtifact
		proposal.SummaryArtifact = verified.summaryArtifact
		proposal.Status = scanRunPhaseSucceeded
	}
	return nil
}

func patchTaskWorkspaceRef(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	if task.Spec.Workspace != nil {
		return strings.TrimSpace(task.Spec.Workspace.Ref)
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
		return strings.TrimSpace(task.Spec.AgentRuntime.Workspace.Ref)
	}
	return ""
}

func legacyPatchProposalTaskBindingMatches(
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
	finding *store.Finding,
	proposal *store.PatchProposal,
	taskFindingID string,
	taskOccurrenceID string,
) bool {
	if scan == nil || scan.UID == "" || task == nil || finding == nil ||
		!metav1.IsControlledBy(task, scan) ||
		strings.TrimSpace(task.Labels[labels.LabelSecurityTarget]) != labels.SelectorValue(scan.Name) ||
		strings.TrimSpace(task.Labels[labels.LabelSecurityScanID]) == "" ||
		strings.TrimSpace(task.Labels[labels.LabelSecurityScanID]) != strings.TrimSpace(finding.ScanRunID) ||
		taskFindingID != finding.ID {
		return false
	}
	findingOccurrenceID := strings.TrimSpace(finding.CurrentOccurrenceID)
	proposalOccurrenceID := strings.TrimSpace(proposal.OccurrenceID)
	if findingOccurrenceID == "" {
		return taskOccurrenceID == "" && proposalOccurrenceID == ""
	}
	return taskOccurrenceID == findingOccurrenceID &&
		(proposalOccurrenceID == "" || proposalOccurrenceID == findingOccurrenceID)
}

func (r *RepositoryScanReconciler) backfillLegacyPatchProposalSource(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	task *corev1alpha1.Task,
	finding *store.Finding,
	proposal *store.PatchProposal,
) (bool, error) {
	if proposal == nil || strings.TrimSpace(proposal.SourceScanRunID) != "" {
		return true, nil
	}
	taskFindingID, err := taskSecurityFindingID(task)
	if err != nil {
		return false, err
	}
	taskOccurrenceID, err := taskSecurityOccurrenceID(task)
	if err != nil {
		return false, err
	}
	if !legacyPatchProposalTaskBindingMatches(scan, task, finding, proposal, taskFindingID, taskOccurrenceID) {
		return false, nil
	}
	findingOccurrenceID := strings.TrimSpace(finding.CurrentOccurrenceID)
	run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, finding.ScanRunID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !scanRunHasImmutableRepositoryBinding(run, scan) {
		return false, nil
	}
	runHead, ok := security.NormalizeFullGitObjectID(run.HeadCommit)
	if !ok {
		return false, nil
	}
	taskHead, ok := security.NormalizeFullGitObjectID(patchTaskWorkspaceRef(task))
	if !ok || taskHead != runHead {
		return false, nil
	}
	if proposal.SourceHeadSHA != "" {
		proposalHead, ok := security.NormalizeFullGitObjectID(proposal.SourceHeadSHA)
		if !ok || proposalHead != runHead {
			return false, nil
		}
	}
	proposal.SourceScanRunID = run.ID
	proposal.SourceHeadSHA = runHead
	if proposal.OccurrenceID == "" {
		proposal.OccurrenceID = findingOccurrenceID
	}
	if err := r.SecurityStore.UpdatePatchProposal(ctx, proposal); err != nil {
		return false, err
	}
	return true, nil
}

//nolint:gocyclo // integrity flow keeps ordered fail-closed validation branches
func (r *RepositoryScanReconciler) ingestPatchTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) error {
	findingID, err := taskSecurityFindingID(task)
	if err != nil {
		return err
	}
	if findingID == "" {
		return nil
	}
	taskOccurrenceID, err := taskSecurityOccurrenceID(task)
	if err != nil {
		return err
	}

	proposals, err := r.SecurityStore.ListPatchProposals(ctx, scan.Namespace, findingID)
	if err != nil {
		return err
	}

	var proposal *store.PatchProposal
	for i := range proposals {
		if proposals[i].TaskName == task.Name {
			proposal = &proposals[i]
			break
		}
	}
	if proposal == nil {
		return nil
	}
	finding, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, findingID)
	if err != nil {
		return err
	}
	verifiedSource, err := r.backfillLegacyPatchProposalSource(ctx, scan, task, finding, proposal)
	if err != nil {
		return err
	}
	if !verifiedSource {
		return nil
	}
	staleSourceRun := strings.TrimSpace(proposal.SourceScanRunID) != strings.TrimSpace(finding.ScanRunID)
	staleOccurrence := proposal.OccurrenceID != "" &&
		(taskOccurrenceID != proposal.OccurrenceID || finding.CurrentOccurrenceID != proposal.OccurrenceID)
	if staleSourceRun || staleOccurrence {
		proposal.Status = "stale"
		if err := r.SecurityStore.UpdatePatchProposal(ctx, proposal); err != nil {
			return err
		}
		if finding.PatchProposalID == proposal.ID {
			if err := r.SecurityStore.ClearFindingPatchProjection(ctx, finding.Namespace, finding.ID, proposal.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	}

	proposal.Status = taskPhaseToSecurityPhase(task.Status.Phase)
	requestedBranch := ""
	if task.Spec.Workspace != nil {
		requestedBranch = strings.TrimSpace(task.Spec.Workspace.PushBranch)
	} else if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
		requestedBranch = strings.TrimSpace(task.Spec.AgentRuntime.Workspace.PushBranch)
	}
	if requestedBranch != "" && strings.TrimSpace(proposal.Branch) == "" {
		proposal.Branch = requestedBranch
	}

	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		if err := r.updatePatchProposalFromSucceededTask(ctx, scan, task, findingID, proposal); err != nil {
			return err
		}
	}

	if r.ArtifactStore != nil && proposal.Status != scanRunPhaseSucceeded {
		artifacts, err := r.taskArtifacts(ctx, task)
		if err == nil {
			for _, artifact := range artifacts {
				if strings.HasSuffix(artifact.Filename, ".diff") && strings.HasPrefix(artifact.Filename, "security-patch-") {
					proposal.DiffArtifact = artifact.Filename
				}
				if strings.HasSuffix(artifact.Filename, ".json") && strings.HasPrefix(artifact.Filename, "security-patch-") {
					proposal.SummaryArtifact = artifact.Filename
				}
			}
		}
	}

	if err := r.SecurityStore.UpdatePatchProposal(ctx, proposal); err != nil {
		return err
	}

	finding.PatchProposalID = proposal.ID
	switch proposal.Status {
	case scanRunPhaseSucceeded:
		finding.State = findingStatePatchReady
	case scanRunPhasePending:
		finding.State = findingStatePatchPending
	default:
		finding.State = findingStateOpen
	}
	return r.SecurityStore.UpsertFinding(ctx, finding)
}

func (r *RepositoryScanReconciler) updateStatusWithRetry(ctx context.Context, scan *corev1alpha1.RepositoryScan, mutate func(*corev1alpha1.RepositoryScan)) error {
	return r.updateStatusWithRetryChecked(ctx, scan, func(current *corev1alpha1.RepositoryScan) (bool, error) {
		mutate(current)
		return true, nil
	})
}

func (r *RepositoryScanReconciler) updateStatusWithRetryChecked(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	mutate func(*corev1alpha1.RepositoryScan) (bool, error),
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.RepositoryScan{}
		if err := r.Get(ctx, types.NamespacedName{Name: scan.Name, Namespace: scan.Namespace}, current); err != nil {
			return err
		}
		statusBeforeMutate := current.Status.DeepCopy()
		writeStatus, err := mutate(current)
		if err != nil {
			return err
		}
		if !r.IntegrityConfig.QualityStateWritesEnabled {
			if !writeStatus {
				current.Status = *statusBeforeMutate
			}
			hadQuality := current.Status.Quality != nil
			hadQualityCondition := meta.FindStatusCondition(current.Status.Conditions, "QualityReady") != nil
			current.Status.Quality = nil
			meta.RemoveStatusCondition(&current.Status.Conditions, "QualityReady")
			writeStatus = writeStatus || hadQuality || hadQualityCondition
		}
		if !writeStatus {
			return nil
		}
		return r.Status().Update(ctx, current)
	})
}

// SetupWithManager sets up the controller with the manager.
func (r *RepositoryScanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.RepositoryScan{}).
		Owns(&corev1alpha1.Task{}).
		Named("repositoryscan").
		Complete(r)
}
