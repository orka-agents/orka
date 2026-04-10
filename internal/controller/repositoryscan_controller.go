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

	cron "github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/store"
)

const (
	repositoryScanPhasePending   = "Pending"
	repositoryScanPhaseScanning  = "Scanning"
	repositoryScanPhaseReady     = "Ready"
	repositoryScanPhaseError     = "Error"
	repositoryScanPhaseSuspended = "Suspended"
)

// RepositoryScanReconciler reconciles RepositoryScan resources.
type RepositoryScanReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	SecurityStore store.SecurityStore
	ArtifactStore store.ArtifactStore
	ResultStore   store.ResultStore
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

	active, err := r.hasActiveScanTask(ctx, scan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if active {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	if scan.Status.LastScanID == "" {
		if err := r.createScanTask(ctx, scan, "initial", "", ""); err != nil {
			return ctrl.Result{}, err
		}
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
				Message:            err.Error(),
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

	if err := r.createScanTask(ctx, scan, "incremental", scan.Status.LastProcessedCommit, ""); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func (r *RepositoryScanReconciler) hasActiveScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan) (bool, error) {
	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{labels.LabelSecurityTarget: scan.Name}),
	); err != nil {
		return false, err
	}

	for _, task := range tasks.Items {
		if task.Labels[labels.LabelSecurityFindingID] != "" {
			continue
		}
		switch task.Status.Phase {
		case corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseScheduled:
			return true, nil
		}
	}
	return false, nil
}

func (r *RepositoryScanReconciler) createScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit string) error {
	var threatModel string
	if r.SecurityStore != nil {
		model, err := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
		if err == nil {
			threatModel = model.Content
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}

	taskName := security.ScanTaskName(scan.Name, mode)
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
				labels.LabelSecurityTarget: scan.Name,
				labels.LabelSecurityScanID: scanID,
				labels.LabelSecurityMode:   mode,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &scan.Spec.AnalysisAgentRef,
			Prompt:   security.BuildScanPrompt(scan, mode, baseCommit, headCommit, threatModel),
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

	if r.SecurityStore != nil {
		if err := r.SecurityStore.CreateScanRun(ctx, &store.ScanRun{
			ID:             scanID,
			Namespace:      scan.Namespace,
			RepositoryScan: scan.Name,
			TaskName:       taskName,
			Mode:           mode,
			Phase:          "pending",
			BaseCommit:     baseCommit,
			HeadCommit:     headCommit,
			StartedAt:      time.Now(),
		}); err != nil {
			return err
		}
	}

	return r.updateStatusWithRetry(ctx, scan, func(s *corev1alpha1.RepositoryScan) {
		s.Status.Phase = repositoryScanPhaseScanning
		s.Status.LastScanID = scanID
		s.Status.LastScanTaskName = taskName
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "Scanning",
			Message:            fmt.Sprintf("%s scan is running", strings.Title(mode)),
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: s.Generation,
		})
	})
}

func (r *RepositoryScanReconciler) ingestOwnedTasks(ctx context.Context, scan *corev1alpha1.RepositoryScan) error {
	if r.SecurityStore == nil {
		return nil
	}

	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks,
		client.InNamespace(scan.Namespace),
		client.MatchingLabels(map[string]string{labels.LabelSecurityTarget: scan.Name}),
	); err != nil {
		return err
	}

	slices.SortFunc(tasks.Items, func(a, b corev1alpha1.Task) int {
		return a.CreationTimestamp.Time.Compare(b.CreationTimestamp.Time)
	})

	for i := range tasks.Items {
		task := &tasks.Items[i]
		switch task.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed:
		default:
			continue
		}

		if task.Labels[labels.LabelSecurityFindingID] != "" {
			if err := r.ingestPatchTask(ctx, scan, task); err != nil {
				return err
			}
			continue
		}

		if err := r.ingestScanTask(ctx, scan, task); err != nil {
			return err
		}
	}

	return nil
}

func taskPhaseToSecurityPhase(phase corev1alpha1.TaskPhase) string {
	if phase == corev1alpha1.TaskPhaseSucceeded {
		return "succeeded"
	}
	if phase == corev1alpha1.TaskPhaseFailed {
		return "failed"
	}
	if phase == corev1alpha1.TaskPhaseRunning {
		return "running"
	}
	return "pending"
}

func (r *RepositoryScanReconciler) ingestScanTask(ctx context.Context, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) error {
	scanID := task.Labels[labels.LabelSecurityScanID]
	if scanID == "" {
		scanID = security.ScanRunID(task.Name)
	}

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
			return err
		}
	} else if err != nil {
		return err
	}

	run.Phase = taskPhaseToSecurityPhase(task.Status.Phase)
	run.TaskName = task.Name
	if task.Status.CompletionTime != nil {
		run.CompletedAt = ptr.To(task.Status.CompletionTime.Time)
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

	var findingsArtifact security.FindingsArtifact
	if r.ArtifactStore != nil {
		if data, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactFindings); err == nil {
			if err := json.Unmarshal(data, &findingsArtifact); err == nil {
				if findingsArtifact.Scan.Mode != "" {
					run.Mode = findingsArtifact.Scan.Mode
				}
				run.BaseCommit = findingsArtifact.Repository.BaseSHA
				run.HeadCommit = findingsArtifact.Repository.HeadSHA
				run.CommitCount = findingsArtifact.Scan.CommitCount
				if findingsArtifact.Scan.Summary != "" {
					run.Summary = findingsArtifact.Scan.Summary
				}

				for _, item := range findingsArtifact.Findings {
					finding := security.ToFinding(scan.Namespace, scan.Name, scanID, item)
					if existing, err := r.SecurityStore.GetFinding(ctx, scan.Namespace, finding.ID); err == nil {
						if existing.State != "" && existing.State != "open" {
							finding.State = existing.State
						}
						finding.PatchProposalID = existing.PatchProposalID
						finding.PRNumber = existing.PRNumber
						finding.PRURL = existing.PRURL
						finding.CreatedAt = existing.CreatedAt
					}
					if err := r.SecurityStore.UpsertFinding(ctx, finding); err != nil {
						return err
					}
				}
			}
		}

		if data, _, err := r.ArtifactStore.GetArtifact(ctx, task.Namespace, task.Name, security.ArtifactThreatModel); err == nil {
			content := strings.TrimSpace(string(data))
			if content != "" {
				latest, latestErr := r.SecurityStore.GetLatestThreatModel(ctx, scan.Namespace, scan.Name)
				if errors.Is(latestErr, store.ErrNotFound) || (latestErr == nil && strings.TrimSpace(latest.Content) != content) {
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
				}
			}
		}
	}

	if err := r.SecurityStore.UpdateScanRun(ctx, run); err != nil {
		return err
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

		if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
			s.Status.Phase = repositoryScanPhaseReady
			s.Status.LastProcessedCommit = run.HeadCommit
			if task.Status.CompletionTime != nil {
				s.Status.LastSuccessfulScanAt = task.Status.CompletionTime
			}
			meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "ScanSucceeded",
				Message:            run.Summary,
				LastTransitionTime: metav1.Now(),
				ObservedGeneration: s.Generation,
			})
			return
		}

		s.Status.Phase = repositoryScanPhaseError
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ScanFailed",
			Message:            run.Summary,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: s.Generation,
		})
	})
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
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
		proposal.Branch = task.Spec.AgentRuntime.Workspace.PushBranch
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
	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
		finding.State = "patch_ready"
	} else {
		finding.State = "open"
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
