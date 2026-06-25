/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/workers/common"
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

	scanSummaryRunning            = "scan is running"
	scanSummaryThreatModelPending = "Threat model generated; deterministic mapper pending"

	// Kubernetes rejects condition messages longer than 32 KiB. Scan summaries can
	// exceed that, so keep the full summary in storage and only publish a capped
	// status message on the CRD.
	repositoryScanConditionMessageLimit  = 32 * 1024
	repositoryScanConditionMessageSuffix = "\n...[truncated]"
)

// RepositoryScanReconciler reconciles RepositoryScan resources.
type RepositoryScanReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	SecurityStore store.SecurityStore
	ArtifactStore store.ArtifactStore
	ResultStore   store.ResultStore
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

func validateRepositoryScan(scan *corev1alpha1.RepositoryScan) error {
	if strings.TrimSpace(scan.Spec.RepoURL) == "" {
		return fmt.Errorf("spec.repoURL is required")
	}
	if scan.Spec.Provider != "" && scan.Spec.Provider != corev1alpha1.SourceProviderGitHub {
		return fmt.Errorf("spec.provider must be %s", corev1alpha1.SourceProviderGitHub)
	}
	if _, _, err := security.ParseGitHubRepositoryURL(scan.Spec.RepoURL); err != nil {
		return err
	}
	return nil
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

	if validationErr := validateRepositoryScan(scan); validationErr != nil {
		if err := r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
			s.Status.Phase = repositoryScanPhaseError
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "InvalidRepositoryURL",
				Message:            repositoryScanConditionMessage(validationErr.Error(), "invalid repository URL"),
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.ingestOwnedTasks(ctx, scan); err != nil {
		logger.Error(err, "failed to ingest security tasks")
		return ctrl.Result{}, err
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

	if err := r.createScanRun(ctx, scan, scanModeIncremental, scan.Status.LastProcessedCommit, ""); err != nil {
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
	case corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseScheduled:
		return true
	default:
		return false
	}
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

	for _, task := range tasks.Items {
		if !isScanPipelineStage(taskSecurityStage(&task)) {
			continue
		}
		if isActiveTaskPhase(task.Status.Phase) {
			return true, nil
		}
	}
	return false, nil
}

func (r *RepositoryScanReconciler) createScanRun(ctx context.Context, scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit string) error {
	var threatModel string
	if r.SecurityStore != nil {
		model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
		if err == nil {
			threatModel = model.Content
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}

	taskName := security.ScanStageTaskName(scan.Name, mode, security.StageThreatModel, "")
	scanID := security.ScanRunID(taskName)
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      "repository-security",
				labels.LabelSecurityTarget: labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID: scanID,
				labels.LabelSecurityMode:   mode,
				labels.LabelSecurityStage:  security.StageThreatModel,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildThreatModelPrompt(scan, mode, baseCommit, headCommit, threatModel),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageThreatModel},
				{Name: security.EnvScanID, Value: scanID},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
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
		},
	}
	if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, task); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	if err := r.ensureScanRunRecord(ctx, &store.ScanRun{
		ID:             scanID,
		Namespace:      scan.Namespace,
		RepositoryScan: scan.Name,
		TaskName:       taskName,
		Mode:           mode,
		Phase:          scanRunPhasePending,
		BaseCommit:     baseCommit,
		HeadCommit:     headCommit,
		StartedAt:      time.Now(),
	}); err != nil {
		return err
	}

	return r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
		s.Status.Phase = repositoryScanPhaseScanning
		s.Status.LastScanID = scanID
		s.Status.LastScanTaskName = taskName
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

func (r *RepositoryScanReconciler) ensureScanRunRecord(ctx context.Context, run *store.ScanRun) error {
	if r.SecurityStore == nil || run == nil {
		return nil
	}

	_, err := r.SecurityStore.GetScanRun(ctx, run.Namespace, run.ID)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return err
	}

	if err := r.SecurityStore.CreateScanRun(ctx, run); err != nil {
		_, getErr := r.SecurityStore.GetScanRun(ctx, run.Namespace, run.ID)
		switch {
		case getErr == nil:
			return nil
		case errors.Is(getErr, store.ErrNotFound):
			return err
		default:
			return getErr
		}
	}

	return nil
}

func (r *RepositoryScanReconciler) createMapperTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	timeout := metav1.Duration{Duration: 30 * time.Minute}
	priority := int32(690)
	taskName := security.ScanStageTaskName(scan.Name, run.Mode, security.StageMapper, "")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:        "true",
				labels.LabelCreatedBy:      "repository-security",
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
				{Name: security.EnvScanBaseCommit, Value: run.BaseCommit},
				{Name: security.EnvScanHeadCommit, Value: run.HeadCommit},
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
	if err := r.Create(ctx, task); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

type latestScanPipelineState struct {
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
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)
	for _, reviewSlice := range reviewSlices {
		sliceJSON, err := json.Marshal(reviewSlice)
		if err != nil {
			return err
		}
		taskName := security.ScanStageTaskName(scan.Name, run.Mode, security.StageReview, reviewSlice.ID)
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:      taskName,
				Namespace: scan.Namespace,
				Labels: map[string]string{
					labels.LabelManaged:         "true",
					labels.LabelCreatedBy:       "repository-security",
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
				Prompt:   security.BuildReviewPrompt(scan, run.Mode, run.BaseCommit, run.HeadCommit, threatModel, reviewSlice),
				Timeout:  &timeout,
				Priority: &priority,
				Env: []corev1.EnvVar{
					{Name: security.EnvReviewSliceJSON, Value: string(sliceJSON)},
					{Name: security.EnvRepositoryScanName, Value: scan.Name},
					{Name: security.EnvStage, Value: security.StageReview},
					{Name: security.EnvScanID, Value: run.ID},
					{Name: security.EnvSliceID, Value: reviewSlice.ID},
				},
				AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
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
			},
		}
		if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, task); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
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

	if !state.hasSucceededThreatModel {
		return false, nil
	}

	run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		return false, err
	}
	if run.Phase == scanRunPhaseSucceeded || run.Phase == scanRunPhaseFailed {
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

	var threatModel string
	if r.SecurityStore != nil {
		model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
		if err == nil {
			threatModel = model.Content
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
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

func (r *RepositoryScanReconciler) progressScanRunAfterMapper(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) (bool, error) {
	if strings.TrimSpace(run.ErrorMessage) != "" {
		if err := r.refreshScanRunStatus(ctx, scan, run, run.ID, true); err != nil {
			return false, err
		}
		return true, nil
	}

	var threatModel string
	if r.SecurityStore != nil {
		model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
		if err == nil {
			threatModel = model.Content
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
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
		if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
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
	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return false, err
	}
	if err := r.updateNoopScanStatus(ctx, scan, run); err != nil {
		return false, err
	}

	return true, nil
}

func (r *RepositoryScanReconciler) updateNoopScanStatus(ctx context.Context, scan *corev1alpha1.RepositoryScan, run *store.ScanRun) error {
	counts, err := r.SecurityStore.GetFindingCounts(ctx, scan.Namespace, scan.Name)
	if err != nil {
		return err
	}

	var threatModelVersion int64
	if model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err == nil {
		threatModelVersion = model.Version
	}

	return r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
		s.Status.Phase = repositoryScanPhaseReady
		s.Status.LastScanID = run.ID
		s.Status.LastScanTaskName = run.TaskName
		s.Status.LastObservedHeadSHA = run.HeadCommit
		s.Status.LastProcessedCommit = run.HeadCommit
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
	})
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

	slices.SortFunc(tasks.Items, func(a, b corev1alpha1.Task) int {
		if cmp := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Name, b.Name)
	})

	latestScanRunID := latestOwnedScanPipelineRunID(tasks.Items)
	refreshLatestScanRun := false

	for i := range tasks.Items {
		task := &tasks.Items[i]
		switch task.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed:
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
		if latestScanRunID != "" && scanTaskRunID(task) == latestScanRunID {
			refreshLatestScanRun = true
		}
		if err := r.ingestScanTask(ctx, scan, task); err != nil {
			return err
		}
	}

	if refreshLatestScanRun {
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
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed:
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
	if latestErr == nil {
		if strings.TrimSpace(latest.Content) == content {
			return nil
		}
		if latest.GeneratedByScan != scanID && !scanStartedAt.IsZero() && scanStartedAt.Before(latest.UpdatedAt) {
			return nil
		}
	}

	model := &store.ThreatModel{
		Namespace:       scan.Namespace,
		RepositoryScan:  scan.Name,
		Content:         content,
		Source:          "generated",
		GeneratedByScan: scanID,
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

func (r *RepositoryScanReconciler) getArtifactWithRetry(ctx context.Context, namespace, taskName, filename string) ([]byte, error) {
	var lastErr error
	for range 5 {
		data, _, err := r.ArtifactStore.GetArtifact(ctx, namespace, taskName, filename)
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

func (r *RepositoryScanReconciler) loadThreatModelArtifact(ctx context.Context, task *corev1alpha1.Task) (string, string, error) {
	if r.ArtifactStore == nil {
		return "", "", nil
	}

	threatModelData, err := r.getArtifactWithRetry(ctx, task.Namespace, task.Name, security.ArtifactThreatModel)
	switch {
	case err == nil:
		content := strings.TrimSpace(string(threatModelData))
		if content == "" {
			return "", fmt.Sprintf("%s is empty", security.ArtifactThreatModel), nil
		}
		if threatModelLooksLikeToolTranscript(content) {
			return "", fmt.Sprintf("%s looks like tool transcript, not markdown", security.ArtifactThreatModel), nil
		}
		return content, "", nil
	case errors.Is(err, store.ErrNotFound):
		content, ok, resultErr := r.threatModelFromTaskResult(ctx, task)
		if resultErr != nil {
			return "", "", resultErr
		}
		if ok {
			return content, "", nil
		}
		return "", fmt.Sprintf("%s is missing", security.ArtifactThreatModel), nil
	default:
		return "", "", err
	}
}

func (r *RepositoryScanReconciler) threatModelFromTaskResult(ctx context.Context, task *corev1alpha1.Task) (string, bool, error) {
	if r.ResultStore == nil || task == nil {
		return "", false, nil
	}
	data, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
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

func (r *RepositoryScanReconciler) loadDiscoveryFindingsV2Artifact(ctx context.Context, task *corev1alpha1.Task) (*security.FindingsV2Artifact, *security.ReviewContextManifest, string, error) {
	if r.ArtifactStore == nil {
		return nil, nil, "", nil
	}

	findingsData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindingsV2)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(findingsData))) == 0 {
			return nil, nil, fmt.Sprintf("%s is empty", security.ArtifactFindingsV2), nil
		}
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, "", nil
	default:
		return nil, nil, "", err
	}

	findings, err := security.ParseFindingsV2Artifact(findingsData)
	if err != nil {
		return nil, nil, fmt.Sprintf("%s is invalid: %v", security.ArtifactFindingsV2, err), nil
	}
	trustedSliceID := strings.TrimSpace(task.Labels[labels.LabelSecuritySliceID])
	artifactSliceID := strings.TrimSpace(findings.Scan.SliceID)
	if trustedSliceID == "" {
		return nil, nil, "v2 findings require trusted security slice task label", nil
	}
	if artifactSliceID == "" {
		return nil, nil, "v2 findings artifact missing scan.sliceId", nil
	}
	if artifactSliceID != trustedSliceID {
		return nil, nil, fmt.Sprintf("v2 findings scan.sliceId %q does not match task slice %q", artifactSliceID, trustedSliceID), nil
	}
	contextName := security.ReviewContextArtifactName(trustedSliceID)
	contextData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, contextName)
	switch {
	case err == nil:
		manifest, err := security.ParseReviewContextManifest(contextData)
		if err != nil {
			return nil, nil, fmt.Sprintf("%s is invalid: %v", contextName, err), nil
		}
		if strings.TrimSpace(manifest.SliceID) != trustedSliceID {
			return nil, nil, fmt.Sprintf("%s sliceId %q does not match task slice %q", contextName, manifest.SliceID, trustedSliceID), nil
		}
		return findings, manifest, "", nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, fmt.Sprintf("%s is missing", contextName), nil
	default:
		return nil, nil, "", err
	}
}

func (r *RepositoryScanReconciler) loadReviewSlicesArtifact(ctx context.Context, task *corev1alpha1.Task) (*security.ReviewSlicesArtifact, string, error) {
	if r.ArtifactStore == nil {
		return nil, "", nil
	}

	data, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactSlices)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(data))) == 0 {
			return nil, fmt.Sprintf("%s is empty", security.ArtifactSlices), nil
		}
		artifact, err := security.ParseReviewSlicesArtifact(data)
		if err != nil {
			return nil, fmt.Sprintf("%s is invalid: %v", security.ArtifactSlices, err), nil
		}
		return artifact, "", nil
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Sprintf("%s is missing", security.ArtifactSlices), nil
	default:
		return nil, "", err
	}
}

func (r *RepositoryScanReconciler) loadValidationTaskArtifacts(ctx context.Context, task *corev1alpha1.Task) (*validationTaskArtifacts, string, error) {
	if r.ArtifactStore == nil {
		return nil, "", nil
	}

	data, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidation)
	switch {
	case err == nil:
		raw := strings.TrimSpace(string(data))
		if raw == "" {
			return nil, fmt.Sprintf("%s is empty", security.ArtifactValidation), nil
		}
		var artifact security.ValidationArtifact
		if err := json.Unmarshal(data, &artifact); err != nil {
			return nil, fmt.Sprintf("%s is invalid JSON: %v", security.ArtifactValidation, err), nil
		}
		result := &validationTaskArtifacts{artifact: artifact, rawJSON: raw}
		if transcript, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactValidationText); err == nil {
			result.transcript = strings.TrimSpace(string(transcript))
		}
		return result, "", nil
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Sprintf("%s is missing", security.ArtifactValidation), nil
	default:
		return nil, "", err
	}
}

func (r *RepositoryScanReconciler) getOrCreateScanRun(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) (*store.ScanRun, error) {
	scanID := scanTaskRunID(task)

	run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, scanID)
	if errors.Is(err, store.ErrNotFound) {
		run = &store.ScanRun{
			ID:             scanID,
			Namespace:      scan.Namespace,
			RepositoryScan: scan.Name,
			TaskName:       task.Name,
			Mode:           task.Labels[labels.LabelSecurityMode],
			StartedAt:      task.CreationTimestamp.Time,
		}
		if err := r.SecurityStore.CreateScanRun(ctx, run); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
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
		if result, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err == nil {
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
	if err := r.keepScanRunningForPendingReviewSlices(ctx, scan, run, progress); err != nil {
		return err
	}

	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return err
	}
	if !updateStatus {
		return nil
	}

	counts, err := r.SecurityStore.GetFindingCounts(ctx, scan.Namespace, scan.Name)
	if err != nil {
		return err
	}

	var threatModelVersion int64
	if model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name); err == nil {
		threatModelVersion = model.Version
	}

	return r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
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
		case scanRunPhaseSucceeded:
			s.Status.Phase = repositoryScanPhaseReady
			s.Status.LastProcessedCommit = run.HeadCommit
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
		}
	})
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

func (r *RepositoryScanReconciler) shouldAutoValidateFinding(scan *corev1alpha1.RepositoryScan, finding *store.Finding, createdForTask int) bool {
	switch security.EffectiveValidationMode(scan) {
	case "off":
		return false
	case "full":
		return true
	default:
		if createdForTask >= 2 {
			return false
		}
		return finding.Severity == "critical" || finding.Severity == confidenceHigh || finding.Confidence == confidenceHigh
	}
}

func (r *RepositoryScanReconciler) hasActiveValidationTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, findingID string) (bool, error) {
	if r.Client == nil {
		return false, nil
	}
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{
			labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
			labels.LabelSecurityFindingID: findingID,
			labels.LabelSecurityStage:     security.StageValidation,
		}),
	); err != nil {
		return false, err
	}
	for i := range tasks.Items {
		if isActiveTaskPhase(tasks.Items[i].Status.Phase) {
			return true, nil
		}
	}
	return false, nil
}

func (r *RepositoryScanReconciler) createValidationTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, finding *store.Finding) error {
	if r.Client == nil {
		return nil
	}
	timeout := metav1.Duration{Duration: 90 * time.Minute}
	priority := int32(725)
	taskName := security.ScanStageTaskName(scan.Name, "validation", security.StageValidation, finding.ID)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: scan.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-security",
				labels.LabelSecurityTarget:    labels.SelectorValue(scan.Name),
				labels.LabelSecurityScanID:    finding.ScanRunID,
				labels.LabelSecurityMode:      security.StageValidation,
				labels.LabelSecurityStage:     security.StageValidation,
				labels.LabelSecurityFindingID: finding.ID,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildValidationPrompt(scan, finding),
			Timeout:  &timeout,
			Priority: &priority,
			Env: []corev1.EnvVar{
				{Name: security.EnvRepositoryScanName, Value: scan.Name},
				{Name: security.EnvStage, Value: security.StageValidation},
				{Name: security.EnvScanID, Value: finding.ScanRunID},
				{Name: security.EnvFindingID, Value: finding.ID},
			},
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
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
		},
	}
	if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, task); err != nil {
		return err
	}

	finding.ValidationStatus = findingValidationStatusPending
	return r.SecurityStore.UpsertFinding(ctx, finding)
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
		if err := r.ArtifactStore.SaveArtifact(ctx, task.Namespace, task.Name, security.ArtifactDroppedFindings, "application/json", data); err != nil {
			return err
		}
	}
	return nil
}

func (r *RepositoryScanReconciler) enqueueAutoValidationTasks(ctx context.Context, scan *corev1alpha1.RepositoryScan, findings []*store.Finding) error {
	created := 0
	for _, finding := range findings {
		if finding == nil || !r.shouldAutoValidateFinding(scan, finding, created) {
			continue
		}
		if finding.ValidationStatus == findingValidationStatusValidated ||
			finding.ValidationStatus == findingValidationStatusPending {
			continue
		}
		active, err := r.hasActiveValidationTask(ctx, scan, finding.ID)
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
		threatModel, validationProblem, err := r.loadThreatModelArtifact(ctx, task)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			run.ErrorMessage = "required scan artifacts were missing or invalid: " + validationProblem
		} else {
			if err := r.persistThreatModelIfChanged(ctx, scan, run.ID, run.StartedAt, threatModel); err != nil {
				return err
			}
			clearThreatModelRunError(run)
			run.Summary = scanSummaryThreatModelPending
		}
	} else {
		run.ErrorMessage = r.pipelineTaskSummary(ctx, task, "threat model stage failed")
	}

	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
}

func (r *RepositoryScanReconciler) ingestReviewTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun) error {
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
		if sliceID != "" {
			if err := r.SecurityStore.UpdateReviewSliceStatus(ctx, scan.Namespace, scan.Name, sliceID, run.ID, reviewSliceStatusFailed); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
	}

	findingsV2, manifest, validationProblem, err := r.loadDiscoveryFindingsV2Artifact(ctx, task)
	if err != nil {
		return err
	}
	if findingsV2 == nil && validationProblem == "" {
		validationProblem = fmt.Sprintf("%s is missing", security.ArtifactFindingsV2)
	}
	if validationProblem != "" {
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
	upserted := make([]*store.Finding, 0, len(partition.Accepted))
	for _, finding := range partition.Accepted {
		if err := r.mergeExistingFinding(ctx, scan, finding); err != nil {
			return err
		}
		if err := r.SecurityStore.UpsertFinding(ctx, finding); err != nil {
			return err
		}
		upserted = append(upserted, finding)
	}
	if err := r.enqueueAutoValidationTasks(ctx, scan, upserted); err != nil {
		return err
	}
	if sliceID != "" {
		if err := r.SecurityStore.UpdateReviewSliceStatus(ctx, scan.Namespace, scan.Name, sliceID, run.ID, reviewSliceStatusReviewed); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
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
		Layer:  "controller",
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

func (r *RepositoryScanReconciler) ingestMapperTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun) error {
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		artifact, validationProblem, err := r.loadReviewSlicesArtifact(ctx, task)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			run.ErrorMessage = "mapper stage failed: " + validationProblem
		} else if artifact != nil {
			changedFiles := changedFileSet(artifact.ChangedFiles)
			incrementalSelection := run.Mode == scanModeIncremental && artifact.ChangedFilesComputed
			skippedSlices := 0
			for i := range artifact.Slices {
				slice := artifact.Slices[i]
				slice.Namespace = scan.Namespace
				slice.RepositoryScan = scan.Name
				slice.LastScanRunID = run.ID
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
	}

	return r.refreshScanRunStatus(ctx, scan, run, run.ID, false)
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
	run, err := r.getOrCreateScanRun(ctx, scan, task)
	if err != nil {
		return err
	}

	switch taskSecurityStage(task) {
	case security.StageThreatModel:
		return r.ingestThreatModelTask(ctx, scan, task, run)
	case security.StageMapper:
		return r.ingestMapperTask(ctx, scan, task, run)
	case security.StageReview:
		return r.ingestReviewTask(ctx, scan, task, run)
	default:
		return nil
	}
}

func (r *RepositoryScanReconciler) ingestValidationTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) error {
	findingID := task.Labels[labels.LabelSecurityFindingID]
	if findingID == "" {
		return nil
	}

	finding, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, findingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}

	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		artifacts, validationProblem, err := r.loadValidationTaskArtifacts(ctx, task)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			finding.ValidationStatus = findingValidationStatusFailed
			finding.ValidationJSON = fmt.Sprintf(
				"{\"status\":\"failed\",\"summary\":%q}",
				validationProblem,
			)
		} else if artifacts != nil {
			status := strings.TrimSpace(artifacts.artifact.Status)
			if status == "" {
				status = findingValidationStatusValidated
			}
			finding.ValidationStatus = status
			finding.ValidationJSON = artifacts.rawJSON
			for _, ref := range artifacts.artifact.Evidence {
				if ref.Kind == "artifact" && ref.TaskName == "" {
					ref.TaskName = task.Name
				}
				finding.Evidence = mergeEvidenceRefs(finding.Evidence, ref)
			}
			finding.Evidence = mergeEvidenceRefs(finding.Evidence, store.FindingEvidenceRef{
				Kind:     "artifact",
				TaskName: task.Name,
				Name:     security.ArtifactValidation,
				Label:    "Validation JSON",
			})
			if artifacts.transcript != "" {
				finding.Evidence = mergeEvidenceRefs(finding.Evidence, store.FindingEvidenceRef{
					Kind:     "artifact",
					TaskName: task.Name,
					Name:     security.ArtifactValidationText,
					Label:    "Validation transcript",
				})
			}
		}
	} else {
		finding.ValidationStatus = findingValidationStatusFailed
		finding.ValidationJSON = fmt.Sprintf(
			"{\"status\":\"failed\",\"summary\":%q}",
			r.pipelineTaskSummary(ctx, task, "validation task failed"),
		)
	}

	return r.SecurityStore.UpsertFinding(ctx, finding)
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
	diffData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, diffName)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		return patchVerificationResult{}, fmt.Sprintf("%s is missing", diffName), nil
	default:
		return patchVerificationResult{}, "", err
	}
	summaryData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, summaryName)
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

	result, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
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

func (r *RepositoryScanReconciler) ingestPatchTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) error {
	findingID := task.Labels[labels.LabelSecurityFindingID]
	if findingID == "" {
		return nil
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
		artifacts, err := r.ArtifactStore.ListArtifacts(ctx, task.Namespace, task.Name)
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

	finding, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, findingID)
	if err != nil {
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
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.RepositoryScan{}
		if err := r.Get(ctx, types.NamespacedName{Name: scan.Name, Namespace: scan.Namespace}, current); err != nil {
			return err
		}
		mutate(current)
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
