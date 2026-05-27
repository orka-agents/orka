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
	repositoryScanPhasePending   = "Pending"
	repositoryScanPhaseScanning  = "Scanning"
	repositoryScanPhaseReady     = "Ready"
	repositoryScanPhaseError     = "Error"
	repositoryScanPhaseSuspended = "Suspended"

	scanRunPhasePending   = "pending"
	scanRunPhaseRunning   = "running"
	scanRunPhaseSucceeded = "succeeded"
	scanRunPhaseFailed    = "failed"

	findingStateOpen                 = "open"
	findingStatePatchPending         = "patch_pending"
	findingStatePatchReady           = "patch_ready"
	findingValidationStatusPending   = "pending"
	findingValidationStatusValidated = "validated"
	findingValidationStatusFailed    = "failed"

	scanSummaryRunning             = "scan is running"
	scanSummaryThreatModelPending  = "Threat model generated; independent discovery agents pending"
	scanSummaryThreatModelComplete = "Threat model generated successfully"

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

	if err := r.createScanRun(ctx, scan, "incremental", scan.Status.LastProcessedCommit, ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func taskSecurityStage(task *corev1alpha1.Task) string {
	if task == nil {
		return security.StageCombined
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
	return security.StageCombined
}

func isScanPipelineStage(stage string) bool {
	switch stage {
	case security.StageCombined, security.StageThreatModel, security.StageDiscovery:
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
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveBranch(scan),
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

	hasSucceededThreatModel := false
	hasDiscoveryTasks := false
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if scanTaskRunID(task) != scanID {
			continue
		}
		stage := taskSecurityStage(task)
		switch stage {
		case security.StageThreatModel:
			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
				hasSucceededThreatModel = true
			}
		case security.StageDiscovery:
			hasDiscoveryTasks = true
		}
		if isActiveTaskPhase(task.Status.Phase) {
			return false, nil
		}
	}

	if !hasSucceededThreatModel || hasDiscoveryTasks {
		return false, nil
	}

	run, err := r.SecurityStore.GetScanRun(ctx, scan.Namespace, scanID)
	if err != nil {
		return false, err
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

	timeout := metav1.Duration{Duration: 2 * time.Hour}
	priority := int32(700)
	for index, scope := range security.DiscoveryScopes() {
		taskName := security.ScanStageTaskName(scan.Name, run.Mode, security.StageDiscovery, fmt.Sprintf("%s-%d", scope.Name, index))
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
					labels.LabelSecurityStage:  security.StageDiscovery,
					labels.LabelSecurityScope:  scope.Name,
				},
			},
			Spec: corev1alpha1.TaskSpec{
				Type:     corev1alpha1.TaskTypeAgent,
				AgentRef: &scan.Spec.AnalysisAgentRef,
				Prompt:   security.BuildDiscoveryPrompt(scan, run.Mode, run.BaseCommit, run.HeadCommit, threatModel, scope),
				Timeout:  &timeout,
				Priority: &priority,
				AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
					Workspace: &corev1alpha1.WorkspaceConfig{
						GitRepo:      scan.Spec.RepoURL,
						Branch:       security.EffectiveBranch(scan),
						GitSecretRef: scan.Spec.GitSecretRef,
						SubPath:      scan.Spec.SubPath,
						ForkRepo:     scan.Spec.ForkRepo,
						PRBaseBranch: scan.Spec.PRBaseBranch,
					},
				},
			},
		}
		if err := controllerutil.SetControllerReference(scan, task, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, task); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return false, err
		}
	}

	if run.Summary == "" || strings.Contains(run.Summary, "discovery pending") {
		run.Summary = "Threat model generated; independent discovery agents started"
	}
	run.Phase = scanRunPhaseRunning
	run.CompletedAt = nil
	run.ErrorMessage = ""
	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return false, err
	}

	return true, nil
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

		updateStatus := false
		if latestScanRunID != "" && scanTaskRunID(task) == latestScanRunID {
			if stage == security.StageCombined {
				updateStatus = true
			} else {
				refreshLatestScanRun = true
			}
		}
		if err := r.ingestScanTask(ctx, scan, task, updateStatus); err != nil {
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

type scanTaskArtifacts struct {
	findings    security.FindingsArtifact
	threatModel string
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

func (r *RepositoryScanReconciler) loadThreatModelArtifact(ctx context.Context, task *corev1alpha1.Task) (string, string, error) {
	if r.ArtifactStore == nil {
		return "", "", nil
	}

	threatModelData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel)
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
		return "", fmt.Sprintf("%s is missing", security.ArtifactThreatModel), nil
	default:
		return "", "", err
	}
}

func (r *RepositoryScanReconciler) loadDiscoveryFindingsArtifact(ctx context.Context, task *corev1alpha1.Task) (*security.FindingsArtifact, string, error) {
	if r.ArtifactStore == nil {
		return nil, "", nil
	}

	findingsData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(findingsData))) == 0 {
			return nil, fmt.Sprintf("%s is empty", security.ArtifactFindings), nil
		}
		var findings security.FindingsArtifact
		if err := json.Unmarshal(findingsData, &findings); err != nil {
			return nil, fmt.Sprintf("%s is invalid JSON: %v", security.ArtifactFindings, err), nil
		}
		return &findings, "", nil
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Sprintf("%s is missing", security.ArtifactFindings), nil
	default:
		return nil, "", err
	}
}

func (r *RepositoryScanReconciler) loadScanTaskArtifacts(ctx context.Context, task *corev1alpha1.Task) (*scanTaskArtifacts, string, error) {
	if r.ArtifactStore == nil {
		return nil, "", nil
	}

	var validationProblems []string
	artifacts := &scanTaskArtifacts{}

	findingsData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(findingsData))) == 0 {
			validationProblems = append(validationProblems, fmt.Sprintf("%s is empty", security.ArtifactFindings))
			break
		}
		if err := json.Unmarshal(findingsData, &artifacts.findings); err != nil {
			validationProblems = append(validationProblems, fmt.Sprintf("%s is invalid JSON: %v", security.ArtifactFindings, err))
		}
	case errors.Is(err, store.ErrNotFound):
		validationProblems = append(validationProblems, fmt.Sprintf("%s is missing", security.ArtifactFindings))
	case err != nil:
		return nil, "", err
	}

	threatModelData, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel)
	switch {
	case err == nil:
		artifacts.threatModel = strings.TrimSpace(string(threatModelData))
		if artifacts.threatModel == "" {
			validationProblems = append(validationProblems, fmt.Sprintf("%s is empty", security.ArtifactThreatModel))
		} else if threatModelLooksLikeToolTranscript(artifacts.threatModel) {
			validationProblems = append(validationProblems, fmt.Sprintf("%s looks like tool transcript, not markdown", security.ArtifactThreatModel))
		}
	case errors.Is(err, store.ErrNotFound):
		validationProblems = append(validationProblems, fmt.Sprintf("%s is missing", security.ArtifactThreatModel))
	case err != nil:
		return nil, "", err
	}

	if len(validationProblems) > 0 {
		return artifacts, "required scan artifacts were missing or invalid: " + strings.Join(validationProblems, "; "), nil
	}
	return artifacts, "", nil
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
	if stage == security.StageDiscovery {
		if scope := strings.TrimSpace(task.Labels[labels.LabelSecurityScope]); scope != "" {
			return fmt.Sprintf("discovery:%s", scope)
		}
	}
	return stage
}

type scanRunProgress struct {
	hasActive           bool
	hasThreatModelReady bool
	hasDiscovery        bool
	hasCombined         bool
	discoveryCount      int
	discoverySucceeded  int
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
		case security.StageDiscovery, security.StageCombined:
			if stage == security.StageCombined {
				progress.hasCombined = true
			}
			progress.hasDiscovery = true
			progress.discoveryCount++
			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
				progress.discoverySucceeded++
			}
			if task.Status.Phase == corev1alpha1.TaskPhaseFailed {
				recordScanProgressFailure(&progress, task, r.pipelineTaskSummary(ctx, task, "discovery stage failed"))
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
			if progress.hasThreatModelReady && !progress.hasDiscovery {
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

	if progress.hasThreatModelReady && !progress.hasDiscovery {
		run.Phase = scanRunPhaseRunning
		run.CompletedAt = nil
		run.ErrorMessage = ""
		run.Summary = scanSummaryThreatModelPending
		return
	}

	if progress.hasThreatModelReady && progress.hasDiscovery && !progress.hasCombined {
		expectedDiscoveryCount := len(security.DiscoveryScopes())
		if expectedDiscoveryCount > 0 && progress.discoveryCount < expectedDiscoveryCount {
			run.Phase = scanRunPhaseRunning
			run.CompletedAt = nil
			run.ErrorMessage = ""
			run.Summary = fmt.Sprintf(
				"Threat model generated and %d/%d discovery scopes completed successfully",
				progress.discoverySucceeded,
				expectedDiscoveryCount,
			)
			return
		}
	}

	run.Phase = scanRunPhaseSucceeded
	run.ErrorMessage = ""
	if progress.latestCompletion != nil {
		run.CompletedAt = progress.latestCompletion
	}
	if progress.discoveryCount > 0 {
		run.Summary = fmt.Sprintf(
			"Threat model generated and %d/%d discovery scopes completed successfully",
			progress.discoverySucceeded,
			progress.discoveryCount,
		)
		return
	}
	run.Summary = scanSummaryThreatModelComplete
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
		return finding.Severity == "critical" || finding.Severity == "high" || finding.Confidence == "high"
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
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:      scan.Spec.RepoURL,
					Branch:       security.EffectiveBranch(scan),
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
		key := strings.Join([]string{ref.Kind, ref.TaskName, ref.Name, ref.Label}, "|")
		seen[key] = struct{}{}
	}
	for _, ref := range refs {
		key := strings.Join([]string{ref.Kind, ref.TaskName, ref.Name, ref.Label}, "|")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, ref)
	}
	return merged
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

func clearDiscoveryRunError(run *store.ScanRun, scope string) {
	if run == nil || run.ErrorMessage == "" {
		return
	}
	if scope != "" {
		if strings.Contains(run.ErrorMessage, fmt.Sprintf("scope %s:", scope)) {
			clearRunError(run)
		}
		if strings.Contains(run.ErrorMessage, "scope ") {
			return
		}
	}
	if strings.Contains(run.ErrorMessage, security.ArtifactFindings) ||
		strings.Contains(run.ErrorMessage, "discovery stage failed") {
		clearRunError(run)
	}
}

func (r *RepositoryScanReconciler) ingestThreatModelTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun, updateStatus bool) error {
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

	return r.refreshScanRunStatus(ctx, scan, run, run.ID, updateStatus)
}

func (r *RepositoryScanReconciler) ingestDiscoveryTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun, updateStatus bool) error {
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		findingsArtifact, validationProblem, err := r.loadDiscoveryFindingsArtifact(ctx, task)
		if err != nil {
			return err
		}
		if validationProblem != "" {
			scope := strings.TrimSpace(task.Labels[labels.LabelSecurityScope])
			if scope != "" {
				run.ErrorMessage = fmt.Sprintf("scope %s: %s", scope, validationProblem)
			} else {
				run.ErrorMessage = validationProblem
			}
		} else if findingsArtifact != nil {
			clearDiscoveryRunError(run, strings.TrimSpace(task.Labels[labels.LabelSecurityScope]))
			if findingsArtifact.Scan.Mode != "" {
				run.Mode = findingsArtifact.Scan.Mode
			}
			if findingsArtifact.Repository.BaseSHA != "" {
				run.BaseCommit = findingsArtifact.Repository.BaseSHA
			}
			if findingsArtifact.Repository.HeadSHA != "" {
				run.HeadCommit = findingsArtifact.Repository.HeadSHA
			}
			run.CommitCount = findingsArtifact.Scan.CommitCount

			upserted := make([]*store.Finding, 0, len(findingsArtifact.Findings))
			for _, item := range findingsArtifact.Findings {
				finding := security.ToFinding(scan.Namespace, scan.Name, run.ID, task.Name, item)
				if existing, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, finding.ID); err == nil {
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
				}
				if err := r.SecurityStore.UpsertFinding(ctx, finding); err != nil {
					return err
				}
				upserted = append(upserted, finding)
			}
			if err := r.enqueueAutoValidationTasks(ctx, scan, upserted); err != nil {
				return err
			}
		}
	} else {
		run.ErrorMessage = r.pipelineTaskSummary(ctx, task, "discovery stage failed")
	}

	return r.refreshScanRunStatus(ctx, scan, run, run.ID, updateStatus)
}

func (r *RepositoryScanReconciler) ingestCombinedScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, run *store.ScanRun, updateStatus bool) error {
	effectivePhase := task.Status.Phase
	run.TaskName = task.Name
	run.ErrorMessage = ""
	if task.Status.CompletionTime != nil {
		completedAt := task.Status.CompletionTime.Time
		run.CompletedAt = &completedAt
	} else {
		now := time.Now()
		run.CompletedAt = &now
	}

	if r.ResultStore != nil && task.Status.ResultRef != nil && task.Status.ResultRef.Available {
		if result, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err == nil {
			run.Summary = strings.TrimSpace(string(result))
		}
	}
	if task.Status.Message != "" && run.Summary == "" {
		run.Summary = task.Status.Message
	}

	var scanArtifacts *scanTaskArtifacts
	var err error
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		scanArtifacts, run.ErrorMessage, err = r.loadScanTaskArtifacts(ctx, task)
		if err != nil {
			return err
		}
		if scanArtifacts != nil {
			if err := r.persistThreatModelIfChanged(ctx, scan, run.ID, run.StartedAt, scanArtifacts.threatModel); err != nil {
				return err
			}
		}
		if run.ErrorMessage != "" {
			effectivePhase = corev1alpha1.TaskPhaseFailed
			run.Summary = run.ErrorMessage
		}
	}

	run.Phase = taskPhaseToSecurityPhase(effectivePhase)

	if effectivePhase == corev1alpha1.TaskPhaseSucceeded && scanArtifacts != nil {
		if scanArtifacts.findings.Scan.Mode != "" {
			run.Mode = scanArtifacts.findings.Scan.Mode
		}
		run.BaseCommit = scanArtifacts.findings.Repository.BaseSHA
		run.HeadCommit = scanArtifacts.findings.Repository.HeadSHA
		run.CommitCount = scanArtifacts.findings.Scan.CommitCount
		if scanArtifacts.findings.Scan.Summary != "" {
			run.Summary = scanArtifacts.findings.Scan.Summary
		}

		for _, item := range scanArtifacts.findings.Findings {
			finding := security.ToFinding(scan.Namespace, scan.Name, run.ID, task.Name, item)
			if existing, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, finding.ID); err == nil {
				if existing.State != "" && existing.State != findingStateOpen {
					finding.State = existing.State
				}
				finding.PatchProposalID = existing.PatchProposalID
				finding.PRNumber = existing.PRNumber
				finding.PRURL = existing.PRURL
				finding.CreatedAt = existing.CreatedAt
				if existing.ValidationJSON != "" {
					finding.ValidationJSON = existing.ValidationJSON
				}
				if len(existing.Evidence) > 0 {
					finding.Evidence = mergeEvidenceRefs(existing.Evidence, finding.Evidence...)
				}
			}
			if err := r.SecurityStore.UpsertFinding(ctx, finding); err != nil {
				return err
			}
		}
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
		s.Status.LastScanTaskName = task.Name
		s.Status.LastObservedHeadSHA = run.HeadCommit
		s.Status.ThreatModelVersion = threatModelVersion
		s.Status.FindingCounts = corev1alpha1.FindingCountsStatus{
			Total:    counts.Total,
			Critical: counts.Critical,
			High:     counts.High,
			Medium:   counts.Medium,
			Low:      counts.Low,
		}

		applyCombinedScanPhaseStatus(s, effectivePhase, run)
	})
}

// applyCombinedScanPhaseStatus updates the CRD status phase, timestamps, and
// conditions based on the effective phase of a combined scan task.
func applyCombinedScanPhaseStatus(s *corev1alpha1.RepositoryScan, effectivePhase corev1alpha1.TaskPhase, run *store.ScanRun) {
	if effectivePhase == corev1alpha1.TaskPhaseSucceeded {
		s.Status.Phase = repositoryScanPhaseReady
		s.Status.LastProcessedCommit = run.HeadCommit
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
		return
	}

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

func (r *RepositoryScanReconciler) ingestScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task, updateStatus bool) error {
	run, err := r.getOrCreateScanRun(ctx, scan, task)
	if err != nil {
		return err
	}

	switch taskSecurityStage(task) {
	case security.StageThreatModel:
		return r.ingestThreatModelTask(ctx, scan, task, run, updateStatus)
	case security.StageDiscovery:
		return r.ingestDiscoveryTask(ctx, scan, task, run, updateStatus)
	default:
		return r.ingestCombinedScanTask(ctx, scan, task, run, updateStatus)
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
			for _, ref := range []store.FindingEvidenceRef(artifacts.artifact.Evidence) {
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
		switch {
		case r.ResultStore == nil:
			proposal.Status = scanRunPhasePending
		case task.Status.ResultRef == nil || !task.Status.ResultRef.Available:
			proposal.Status = scanRunPhasePending
		default:
			result, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					proposal.Status = scanRunPhasePending
				} else {
					return err
				}
			} else {
				sr := common.ParseStructuredResult(string(result))
				switch {
				case strings.TrimSpace(sr.PushError) != "":
					proposal.Status = scanRunPhaseFailed
				case strings.TrimSpace(sr.PushBranch) != "":
					proposal.Branch = strings.TrimSpace(sr.PushBranch)
					proposal.Status = scanRunPhaseSucceeded
				default:
					proposal.Status = scanRunPhaseFailed
				}
			}
		}
	}

	if r.ArtifactStore != nil {
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
