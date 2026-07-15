package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	repositoryMonitorRepairPhaseQueued     = "queued"
	repositoryMonitorRepairPhaseSucceeded  = "succeeded"
	repositoryMonitorRepairPhaseFailed     = "failed"
	repositoryMonitorRepairPRBudgetReason  = "repair_pr_budget_exhausted"
	repositoryMonitorRepairTaskCreateError = "repair_task_create_failed"
	repositoryMonitorCommandIntentFix      = "fix"
)

//nolint:gocyclo // PR command safety gates are intentionally explicit.
func (r *RepositoryMonitorReconciler) tryProcessPullRequestCommandRun(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, owner, repository string, pr repositoryMonitorPullRequest, item *store.MonitorItem) (bool, int, error) {
	if run == nil || strings.TrimSpace(run.CommandEventID) == "" || item == nil {
		return false, 0, nil
	}
	command, err := r.Store.GetCommandEvent(ctx, monitor.Namespace, run.CommandEventID)
	if err != nil {
		if errorsIsStoreNotFound(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	switch command.Intent {
	case repositoryMonitorCommandIntentStop:
		if err := r.cancelRepositoryMonitorTargetTasks(ctx, monitor, repositoryMonitorPullRequestKind, pr.Number, repositoryMonitorIssueSkipStoppedByCommand); err != nil {
			return true, 0, err
		}
		if _, err := r.Store.CancelWorkActions(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, pr.Number, repositoryMonitorIssueSkipStoppedByCommand); err != nil {
			return true, 0, err
		}
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorCommandIntentStop, repositoryMonitorWorkActionStatusSucceeded, "blocked", "", repositoryMonitorIssueSkipStoppedByCommand); err != nil {
			return true, 0, err
		}
		item.RepairState = repositoryMonitorRepairPhaseFailed
		item.SkipReason = repositoryMonitorIssueSkipStoppedByCommand
		return true, 0, r.Store.UpsertMonitorItem(ctx, item)
	case repositoryMonitorCommandIntentResume:
		if repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels) != "" {
			if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorCommandIntentResume, repositoryMonitorWorkActionStatusBlocked, "resume_blocked", "", repositoryMonitorSkipReasonBlockedLabel); err != nil {
				return true, 0, err
			}
			return true, 0, nil
		}
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorCommandIntentResume, repositoryMonitorWorkActionStatusSucceeded, "resumed", "", ""); err != nil {
			return true, 0, err
		}
		item.RepairState = ""
		item.SkipReason = ""
		return true, 0, r.Store.UpsertMonitorItem(ctx, item)
	case repositoryMonitorCommandIntentAutomerge:
		if cancelled, err := r.repositoryMonitorWorkActionCancelled(ctx, monitor, command.ID, repositoryMonitorCommandIntentAutomerge); err != nil || cancelled {
			return true, 0, err
		}
		handled, err := r.tryProcessPullRequestAutomergeCommand(ctx, monitor, run, command, owner, repository, pr, item)
		return handled, 0, err
	case "review":
		if blockedLabel := repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels); blockedLabel != "" {
			item.LastVerdict = repositoryMonitorVerdictSkipped
			item.SkipReason = repositoryMonitorSkipReasonBlockedLabel
			if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", "pr_review", repositoryMonitorWorkActionStatusBlocked, "review_blocked", "", repositoryMonitorSkipReasonBlockedLabel); err != nil {
				return true, 0, err
			}
			return true, 0, r.Store.UpsertMonitorItem(ctx, item)
		}
		if cancelled, err := r.repositoryMonitorWorkActionCancelled(ctx, monitor, command.ID, "review"); err != nil || cancelled {
			return true, 0, err
		}
		if monitor.Spec.Review.RequireGreenCI {
			ci, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
			if err != nil {
				return true, 0, err
			}
			if !ci.passed {
				item.CIState = firstNonEmptyIssueAction(ci.reason, "ci_not_green")
				item.LastVerdict = repositoryMonitorVerdictSkipped
				item.SkipReason = item.CIState
				if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", "pr_review", repositoryMonitorWorkActionStatusBlocked, "review_blocked", "", item.CIState); err != nil {
					return true, 0, err
				}
				return true, 0, r.Store.UpsertMonitorItem(ctx, item)
			}
			item.CIState = "passed"
		}
		taskName, created, err := r.createRepositoryMonitorReviewTask(ctx, monitor, run, owner, repository, pr)
		if err != nil {
			return true, 0, err
		}
		item.LastVerdict = repositoryMonitorRunPhaseQueued
		item.LastReviewID = taskName
		item.SkipReason = ""
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", "pr_review", repositoryMonitorWorkActionStatusRunning, "review_queued", taskName, ""); err != nil {
			return true, 0, err
		}
		if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
			return true, 0, err
		}
		if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "review_task_created", fmt.Sprintf("Pull request #%d review task queued by command", pr.Number), map[string]any{"taskName": taskName, "created": created}); err != nil {
			return true, 0, err
		}
		if created {
			return true, 1, nil
		}
		return true, 0, nil
	case repositoryMonitorCommandIntentFix, "fix_ci", "update_branch":
		if blockedLabel := repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels); blockedLabel != "" {
			item.RepairState = repositoryMonitorRepairPhaseFailed
			item.SkipReason = repositoryMonitorSkipReasonBlockedLabel
			if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorRepairWorkflowActionKind(command.Intent), repositoryMonitorWorkActionStatusBlocked, "repair_blocked", "", repositoryMonitorSkipReasonBlockedLabel); err != nil {
				return true, 0, err
			}
			return true, 0, r.Store.UpsertMonitorItem(ctx, item)
		}
		if cancelled, err := r.repositoryMonitorWorkActionCancelled(ctx, monitor, command.ID, repositoryMonitorRepairWorkflowActionKind(command.Intent)); err != nil || cancelled {
			return true, 0, err
		}
		monitoredRepo := owner + "/" + repository
		currentRepairJobID := "repair-" + repositoryMonitorShortHash(command.ID)
		reason, repairCountPR, repairCountHead, err := r.repositoryMonitorRepairPolicy(ctx, monitor, monitoredRepo, pr, currentRepairJobID)
		if err != nil {
			return true, 0, err
		}
		if reason != "" {
			item.RepairState = repositoryMonitorRepairPhaseFailed
			item.SkipReason = reason
			if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorRepairWorkflowActionKind(command.Intent), repositoryMonitorWorkActionStatusBlocked, "repair_blocked", "", reason); err != nil {
				return true, 0, err
			}
			if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
				return true, 0, err
			}
			return true, 0, r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "repair_blocked", fmt.Sprintf("Pull request #%d repair blocked: %s", pr.Number, reason), map[string]any{"intent": command.Intent, "reason": reason})
		}
		created, err := r.createRepositoryMonitorRepairTask(ctx, monitor, run, command, owner, repository, pr, item, repairCountPR+1, repairCountHead+1)
		if err != nil {
			return true, created, err
		}
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "", repositoryMonitorRepairWorkflowActionKind(command.Intent), repositoryMonitorWorkActionStatusRunning, repositoryMonitorRepairPhaseQueued, repositoryMonitorRepairTaskName(monitor, pr, command), ""); err != nil {
			return true, created, err
		}
		return true, created, nil
	default:
		return false, 0, nil
	}
}

func (r *RepositoryMonitorReconciler) repositoryMonitorRepairPolicy(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, monitoredRepo string, pr repositoryMonitorPullRequest, currentJobID string) (string, int, int, error) {
	if monitor == nil || !monitor.Spec.Repair.Enabled {
		return "repair_disabled", 0, 0, nil
	}
	if monitor.Spec.Agents.Repairer == nil || strings.TrimSpace(monitor.Spec.Agents.Repairer.Name) == "" {
		return "missing_repairer_agent", 0, 0, nil
	}
	if !strings.EqualFold(strings.TrimSpace(pr.HeadRepo), strings.TrimSpace(monitoredRepo)) {
		return "fork_pr_repair_not_writable", 0, 0, nil
	}
	if r.Store == nil {
		return "", 0, 0, nil
	}
	repairCountPR := 0
	repairCountHead := 0
	cursor := ""
	for {
		jobs, next, err := r.Store.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: pr.Number, Limit: 200, Cursor: cursor})
		if err != nil {
			return "", 0, 0, err
		}
		for _, job := range jobs {
			if strings.TrimSpace(currentJobID) != "" && job.ID == currentJobID {
				continue
			}
			consumesBudget, err := r.repositoryMonitorRepairJobConsumesBudget(ctx, monitor.Namespace, &job)
			if err != nil {
				return "", 0, 0, err
			}
			if !consumesBudget {
				continue
			}
			repairCountPR++
			if strings.TrimSpace(job.HeadSHA) == strings.TrimSpace(pr.HeadSHA) {
				repairCountHead++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if max := monitor.Spec.Repair.MaxRepairsPerPR; max != nil && repairCountPR >= int(*max) {
		return repositoryMonitorRepairPRBudgetReason, repairCountPR, repairCountHead, nil
	}
	if max := monitor.Spec.Repair.MaxRepairsPerHead; max != nil && repairCountHead >= int(*max) {
		return "repair_head_budget_exhausted", repairCountPR, repairCountHead, nil
	}
	return "", repairCountPR, repairCountHead, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorRepairJobConsumesBudget(ctx context.Context, namespace string, job *store.RepairJob) (bool, error) {
	if job == nil {
		return false, nil
	}
	if strings.TrimSpace(job.Phase) != repositoryMonitorRepairPhaseQueued {
		return true, nil
	}
	if strings.TrimSpace(job.TaskName) == "" {
		return false, nil
	}
	if r.Client == nil {
		return true, nil
	}
	var task corev1alpha1.Task
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: job.TaskName}, &task); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *RepositoryMonitorReconciler) createRepositoryMonitorRepairTask(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, command *store.CommandEvent, owner, repository string, pr repositoryMonitorPullRequest, item *store.MonitorItem, repairCountPR, repairCountHead int) (int, error) {
	monitoredRepo := owner + "/" + repository
	taskName := repositoryMonitorRepairTaskName(monitor, pr, command)
	job := &store.RepairJob{
		ID:               "repair-" + repositoryMonitorShortHash(command.ID),
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Repo:             monitoredRepo,
		PRNumber:         pr.Number,
		Intent:           command.Intent,
		Source:           command.Source,
		HeadSHA:          pr.HeadSHA,
		BaseSHA:          pr.BaseSHA,
		Phase:            repositoryMonitorRepairPhaseQueued,
		RepairCountPR:    repairCountPR,
		RepairCountHead:  repairCountHead,
		TaskName:         taskName,
		Branch:           pr.HeadBranch,
	}
	if err := r.Store.CreateRepairJob(ctx, job); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "constraint") {
			return 0, err
		}
		existing, getErr := r.Store.GetRepairJob(ctx, monitor.Namespace, job.ID)
		if getErr != nil {
			return 0, getErr
		}
		job = existing
	}
	pushMutationID := "ghmut-" + repositoryMonitorShortHash(job.ID+"-push")
	if _, err := r.ensureRepositoryMonitorGitHubMutationStarted(ctx, monitor, &store.GitHubMutationRecord{ID: pushMutationID, CommandEventID: command.ID, Operation: "push_branch", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: pr.Number, TargetSHA: pr.HeadSHA, Reason: command.Intent, GitHubURL: pr.HeadBranch}); err != nil {
		return 0, err
	}
	priority := int32(820)
	timeout := metav1.Duration{Duration: repositoryMonitorReviewTaskTimeout}
	repairer := *monitor.Spec.Agents.Repairer
	workspace := &corev1alpha1.WorkspaceConfig{
		GitRepo:      repositoryMonitorHTTPSCloneURL(owner, repository),
		Branch:       pr.HeadBranch,
		Ref:          pr.HeadSHA,
		PRBaseBranch: pr.BaseBranch,
		PushBranch:   pr.HeadBranch,
	}
	gitRef := monitor.Spec.GitSecretRef
	workspace.GitSecretRef = gitRef
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: monitor.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-monitor",
				labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name),
				labels.LabelMonitorRun:        labels.SelectorValue(run.ID),
				labels.LabelGitHubRepository:  labels.SelectorValue(monitoredRepo),
				labels.LabelGitHubTarget:      labels.SelectorValue(repositoryMonitorPullRequestKind),
				labels.LabelGitHubNumber:      labels.SelectorValue(strconv.FormatInt(pr.Number, 10)),
			},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName:    monitor.Name,
				labels.AnnotationMonitorRunID:             run.ID,
				labels.AnnotationMonitorItemKind:          repositoryMonitorPullRequestKind,
				labels.AnnotationMonitorItemNumber:        strconv.FormatInt(pr.Number, 10),
				labels.AnnotationMonitorHeadSHA:           pr.HeadSHA,
				labels.AnnotationGitHubRepository:         monitoredRepo,
				repositoryMonitorIssueAnnotationCommandID: command.ID,
				labels.AnnotationAgentRuntimeAuthOnly:     scheduledRunLabelValue,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:         corev1alpha1.TaskTypeAgent,
			AgentRef:     &repairer,
			Prompt:       buildRepositoryMonitorRepairPrompt(command.Intent, monitoredRepo, pr, item),
			Timeout:      &timeout,
			Priority:     &priority,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{Workspace: workspace},
			Env: []corev1.EnvVar{
				{Name: workerenv.PRBaseRepo, Value: repositoryMonitorHTTPSCloneURL(owner, repository)},
				{Name: workerenv.PRBaseSHA, Value: pr.BaseSHA},
				{Name: workerenv.RequirePushBranch, Value: "true"},
			},
		},
	}
	if command.Intent == "update_branch" {
		task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: workerenv.AllowEmptyPushBranch, Value: "true"})
	}
	if err := controllerutil.SetControllerReference(monitor, task, r.Scheme); err != nil {
		return 0, err
	}
	created := 1
	if err := r.Create(ctx, task); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			job.Phase = repositoryMonitorRepairPhaseQueued
			job.LastError = repositoryMonitorRepairTaskCreateError
			job.CompletedAt = nil
			_ = r.Store.UpdateRepairJob(ctx, job)
			return 0, err
		}
		created = 0
	}
	if job.LastError == repositoryMonitorRepairTaskCreateError {
		job.Phase = repositoryMonitorRepairPhaseQueued
		job.LastError = ""
		job.CompletedAt = nil
		if err := r.Store.UpdateRepairJob(ctx, job); err != nil {
			return created, err
		}
	}
	item.RepairState = repositoryMonitorRepairPhaseQueued
	item.SkipReason = ""
	if err := r.Store.UpsertMonitorItem(ctx, item); err != nil {
		return created, err
	}
	return created, r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, "repair_task_created", fmt.Sprintf("Pull request #%d %s repair task queued", pr.Number, command.Intent), map[string]any{"taskName": taskName, "intent": command.Intent})
}

func repositoryMonitorRepairWorkflowActionKind(intent string) string {
	if strings.TrimSpace(intent) == repositoryMonitorCommandIntentFix {
		return "pr_repair"
	}
	return strings.TrimSpace(intent)
}

func repositoryMonitorRepairTaskName(monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, command *store.CommandEvent) string {
	return repositoryMonitorBoundedDNSName(fmt.Sprintf("monrepair-%s-%d-%s", monitor.Name, pr.Number, command.ID), 63)
}

func buildRepositoryMonitorRepairPrompt(intent, repo string, pr repositoryMonitorPullRequest, item *store.MonitorItem) string {
	payload := map[string]any{"schemaVersion": "orka.prRepair.input.v1", "repo": repo, "prNumber": pr.Number, "headSHA": pr.HeadSHA, "intent": intent, "lastVerdict": item.LastVerdict, "skipReason": item.SkipReason}
	payloadJSON, _ := json.MarshalIndent(payload, "", "  ")
	return fmt.Sprintf("Repair this exact pull request head for intent %q. Keep scope limited, run relevant validation, and leave final changes for Orka to commit and push to the configured push branch. Do not merge or close the PR.\n\nInput:\n%s\n", intent, string(payloadJSON))
}

func (r *RepositoryMonitorReconciler) ingestCompletedRepositoryMonitorRepairTasks(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (bool, error) {
	jobs, _, err := r.Store.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Phase: repositoryMonitorRepairPhaseQueued, Limit: 100})
	if err != nil {
		return false, err
	}
	ingested := false
	for i := range jobs {
		job := jobs[i]
		if strings.TrimSpace(job.TaskName) == "" {
			continue
		}
		var task corev1alpha1.Task
		if err := r.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: job.TaskName}, &task); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return ingested, err
		}
		if !repositoryMonitorReviewTaskTerminal(task.Status.Phase) {
			continue
		}
		if cancelled, cancelErr := r.repositoryMonitorWorkActionCancelled(ctx, monitor, task.Annotations[repositoryMonitorIssueAnnotationCommandID], repositoryMonitorRepairWorkflowActionKind(job.Intent)); cancelErr != nil {
			return ingested, cancelErr
		} else if cancelled {
			completedAt := time.Now()
			job.Phase = repositoryMonitorRepairPhaseFailed
			job.LastError = repositoryMonitorIssueSkipStoppedByCommand
			job.CompletedAt = &completedAt
			if err := r.Store.UpdateRepairJob(ctx, &job); err != nil {
				return ingested, err
			}
			ingested = true
			continue
		}
		if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded {
			job.Phase = repositoryMonitorRepairPhaseFailed
			job.LastError = "repair task result is missing"
			if r.ResultStore != nil {
				if raw, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err == nil {
					sr := common.ParseStructuredResult(string(raw))
					job.PushedSHA = sr.HeadSHA
					if strings.TrimSpace(sr.PushError) != "" {
						job.LastError = sr.PushError
					} else if strings.TrimSpace(sr.PushBranch) == "" {
						job.LastError = "repair task did not report a pushed branch"
					} else {
						job.Phase = repositoryMonitorRepairPhaseSucceeded
						job.LastError = ""
					}
				} else {
					job.LastError = err.Error()
				}
			}
		} else {
			job.Phase = repositoryMonitorRepairPhaseFailed
			job.LastError = fmt.Sprintf("task ended in phase %s", task.Status.Phase)
		}
		commandID := task.Annotations[repositoryMonitorIssueAnnotationCommandID]
		pushMutationID := "ghmut-" + repositoryMonitorShortHash(job.ID+"-push")
		pushMutation, err := r.Store.GetGitHubMutationRecord(ctx, monitor.Namespace, pushMutationID)
		if err != nil {
			return ingested, err
		}
		pushMutation.CommandEventID = commandID
		pushMutation.ExternalID = job.PushedSHA
		pushMutation.GitHubURL = job.Branch
		pushMutation.Error = job.LastError
		pushMutation.Status = repositoryMonitorRunPhaseSucceeded
		if job.Phase != repositoryMonitorRepairPhaseSucceeded {
			pushMutation.Status = repositoryMonitorRunPhaseFailed
		}
		if err := r.updateRepositoryMonitorGitHubMutation(ctx, monitor, pushMutation); err != nil {
			return ingested, err
		}
		completedAt := time.Now()
		job.CompletedAt = &completedAt
		if err := r.Store.UpdateRepairJob(ctx, &job); err != nil {
			return ingested, err
		}
		workStatus := repositoryMonitorWorkActionStatusSucceeded
		if job.Phase != repositoryMonitorRepairPhaseSucceeded {
			workStatus = repositoryMonitorWorkActionStatusFailed
		}
		if err := r.recordRepositoryMonitorWorkActionState(ctx, monitor, nil, &store.CommandEvent{ID: commandID, Intent: job.Intent}, repositoryMonitorPullRequestKind, job.PRNumber, job.HeadSHA, "", repositoryMonitorRepairWorkflowActionKind(job.Intent), workStatus, job.Phase, job.TaskName, job.LastError); err != nil {
			return ingested, err
		}
		item, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, strconv.FormatInt(job.PRNumber, 10))
		if err == nil {
			item.RepairState = job.Phase
			if job.Phase == repositoryMonitorRepairPhaseSucceeded {
				item.LastReviewedHeadSHA = ""
				item.LastVerdict = ""
				item.AutomergeState = ""
				item.SkipReason = ""
			}
			if updateErr := r.Store.UpsertMonitorItem(ctx, item); updateErr != nil {
				return ingested, updateErr
			}
		} else if !errorsIsStoreNotFound(err) {
			return ingested, err
		}
		ingested = true
	}
	return ingested, nil
}
