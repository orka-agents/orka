package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const repositoryMonitorCommandAccepted = "accepted"
const repositoryMonitorControllerActor = "orka-controller"
const repositoryMonitorControllerPolicySource = "controller_policy"

const repositoryMonitorWorkflowPollInterval = 30 * time.Second

const repositoryMonitorRunTriggerWorkflow = "workflow"

const repositoryMonitorEventRepairRefused = "repair_refused"

func (r *RepositoryMonitorReconciler) tryRepositoryMonitorAutomaticRepair(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, owner, repo string, pr repositoryMonitorPullRequest, item *store.MonitorItem) (bool, error) {
	if !monitor.Spec.Repair.Enabled || run.CommandEventID != "" || pr.Draft || pr.HeadSHA == "" {
		return false, nil
	}
	full, ciOnly, labelled := repositoryMonitorPullRequestRepairScope(monitor.Spec, pr)
	if !full && !ciOnly {
		return false, nil
	}
	if _, reason := repositoryMonitorRepairRepository(monitor, owner+"/"+repo, pr); reason != "" {
		return false, nil
	}
	if repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels) != "" || item.SkipReason == repositoryMonitorIssueSkipStoppedByCommand || item.LastVerdict == repositoryMonitorRunPhaseQueued {
		return false, nil
	}
	if item.RepairState == repositoryMonitorRepairPhaseQueued {
		return true, nil
	}
	reviewFirst := full && labelled
	ciFirst := false
	if reviewFirst {
		fresh, err := r.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, pr.HeadSHA)
		if err != nil {
			return false, err
		}
		if !fresh {
			if !monitor.Spec.Review.RequireGreenCI {
				return false, nil
			}
			ciFirst = true
		}
	}
	intent, err := r.repositoryMonitorAutomaticRepairIntent(ctx, monitor, owner, repo, pr, item, full, reviewFirst, ciFirst)
	if err != nil || intent == "" {
		return false, err
	}
	reason, prCount, headCount, err := r.repositoryMonitorRepairPolicy(ctx, monitor, owner+"/"+repo, pr, "", intent)
	if err != nil {
		return false, err
	}
	if reason != "" {
		return r.refuseRepositoryMonitorAutomaticRepair(ctx, monitor, run, pr, item, reason)
	}
	id := "policy-" + repositoryMonitorShortHash(fmt.Sprintf("%s|%d|%d|%s|%s|%d|%d", monitor.UID, monitor.Generation, pr.Number, pr.HeadSHA, intent, prCount, headCount))
	command, err := r.Store.GetCommandEvent(ctx, monitor.Namespace, id)
	if errors.Is(err, store.ErrNotFound) {
		if labelled {
			permitted, err := r.repositoryMonitorRepairLabelActorPermitted(ctx, monitor, owner, repo, pr, repositoryMonitorRepairLabels(monitor.Spec, full))
			if err != nil {
				return false, err
			}
			if !permitted {
				return r.refuseRepositoryMonitorAutomaticRepair(ctx, monitor, run, pr, item, repositoryMonitorRepairSkipLabelActorNotPermitted)
			}
		}
		now := time.Now()
		command = &store.CommandEvent{ID: id, CommentID: id, DedupeKey: id, IdempotencyKey: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, MonitorGeneration: monitor.Generation, Repo: owner + "/" + repo, Kind: repositoryMonitorPullRequestKind, Number: pr.Number, Source: repositoryMonitorControllerPolicySource, Author: repositoryMonitorControllerActor, Permission: "repository_repair_policy", Intent: intent, Command: intent, HeadSHA: pr.HeadSHA, Status: repositoryMonitorCommandAccepted, CreatedAt: now}
		if err := r.Store.CreateCommandEvent(ctx, command); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	if command.Status != repositoryMonitorCommandAccepted {
		return false, nil
	}
	if _, err := r.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor); err != nil {
		return false, err
	}
	item.RepairState = repositoryMonitorRepairPhaseQueued
	return true, r.Store.UpsertMonitorItem(ctx, item)
}

func (r *RepositoryMonitorReconciler) repositoryMonitorAutomaticRepairIntent(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, owner, repo string, pr repositoryMonitorPullRequest, item *store.MonitorItem, full, reviewFirst, ciFirst bool) (string, error) {
	if pending, err := r.repositoryMonitorHeadExhaustivePassPending(ctx, monitor, pr); err != nil || pending {
		return "", err
	}
	if full && !ciFirst && pr.MergeableState == repositoryMonitorMergeableStateBehind {
		return repositoryMonitorCommandIntentUpdateBranch, nil
	}
	if monitor.Spec.Agents.Repairer == nil || strings.TrimSpace(monitor.Spec.Agents.Repairer.Name) == "" {
		return "", nil
	}
	var externalErr error
	if reviewFirst && !ciFirst {
		fix, err := r.repositoryMonitorFixWorkPending(ctx, monitor, owner, repo, pr, item, full)
		if err != nil {
			externalErr = err
		} else if fix {
			return repositoryMonitorCommandIntentFix, nil
		}
	}
	broke, err := r.repositoryMonitorPullRequestBrokeCI(ctx, monitor, pr)
	if err != nil {
		return "", err
	}
	if broke {
		return repositoryMonitorCommandIntentFixCI, nil
	}
	if externalErr != nil {
		return "", externalErr
	}
	if reviewFirst {
		return "", nil
	}
	fix, err := r.repositoryMonitorFixWorkPending(ctx, monitor, owner, repo, pr, item, full)
	if err != nil || !fix {
		return "", err
	}
	return repositoryMonitorCommandIntentFix, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorFixWorkPending(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, owner, repo string, pr repositoryMonitorPullRequest, item *store.MonitorItem, full bool) (bool, error) {
	fix, err := r.repositoryMonitorReviewFindingsRepairable(ctx, monitor, pr, item, full)
	if err != nil || fix {
		return fix, err
	}
	if !full || !repositoryMonitorExternalCommentsEnabled(monitor) {
		return false, nil
	}
	threads, err := r.fetchRepositoryMonitorExternalReviewThreads(ctx, monitor, owner, repo, pr)
	if err != nil {
		return false, err
	}
	return len(threads) > 0, nil
}

func (r *RepositoryMonitorReconciler) repositoryMonitorReviewFindingsRepairable(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, item *store.MonitorItem, full bool) (bool, error) {
	if !full || item.LastVerdict != repositoryMonitorReviewVerdictNeedsChanges || item.LastReviewedHeadSHA != pr.HeadSHA || item.LastReviewID == "" {
		return false, nil
	}
	review, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, item.LastReviewID)
	if err != nil {
		return false, err
	}
	return review.HeadSHA == pr.HeadSHA && review.Repairable, nil
}

func (r *RepositoryMonitorReconciler) queueRepositoryMonitorWorkflowPoll(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) error {
	if repositoryMonitorSuspended(monitor) || !repositoryMonitorManagedWorkflow(monitor) || !repositoryMonitorPullRequestsEnabled(monitor.Spec) {
		return nil
	}
	items, _, err := r.Store.ListMonitorItems(ctx, store.MonitorItemFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, State: repositoryMonitorItemStateOpen, Limit: 1})
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	for _, phase := range []string{repositoryMonitorRunPhaseQueued, repositoryMonitorRunPhaseRunning} {
		runs, _, err := r.Store.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Phase: phase, Limit: 1})
		if err != nil {
			return err
		}
		if len(runs) > 0 {
			return nil
		}
	}
	now := time.Now()
	cursor := ""
	for {
		recent, next, err := r.Store.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 200, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, run := range recent {
			if now.Sub(run.StartedAt) >= repositoryMonitorWorkflowPollInterval {
				next = ""
				break
			}
			switch strings.TrimSpace(run.TargetKind) {
			case "", repositoryMonitorPullRequestKind:
				return nil
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	id := "workflow-" + repositoryMonitorShortHash(fmt.Sprintf("%s|%d|%d", monitor.UID, monitor.Generation, now.Unix()/30))
	if err := r.Store.CreateMonitorRun(ctx, &store.MonitorRun{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: repositoryMonitorRunTriggerWorkflow, TargetKind: repositoryMonitorPullRequestKind, Phase: repositoryMonitorRunPhaseQueued, StartedAt: now}); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}

func (r *RepositoryMonitorReconciler) queueRepositoryMonitorExhaustivePass(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, record *store.ReviewRecord) error {
	if repositoryMonitorSuspended(monitor) || repositoryMonitorManagedWorkflow(monitor) {
		return nil
	}
	id := "exhaustive-" + repositoryMonitorShortHash(monitor.Namespace+"/"+monitor.Name+"|"+record.ID)
	run := &store.MonitorRun{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: repositoryMonitorRunTriggerWorkflow, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: record.Number, TargetSHA: record.HeadSHA, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now()}
	if err := r.Store.CreateMonitorRun(ctx, run); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}

func (r *RepositoryMonitorReconciler) refuseRepositoryMonitorAutomaticRepair(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, pr repositoryMonitorPullRequest, item *store.MonitorItem, reason string) (bool, error) {
	item.RepairState = repositoryMonitorRepairPhaseFailed
	item.SkipReason = reason
	summary := fmt.Sprintf("Pull request #%d automatic repair refused: %s", pr.Number, reason)
	recent, _, err := r.Store.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: pr.Number, EventType: repositoryMonitorEventRepairRefused, Limit: 1})
	if err != nil {
		return false, err
	}
	if len(recent) == 0 || recent[0].ItemSHA != pr.HeadSHA || recent[0].Summary != summary {
		if err := r.createMonitorEvent(ctx, monitor, run.ID, repositoryMonitorPullRequestKind, pr.Number, pr.HeadSHA, repositoryMonitorEventRepairRefused, summary, map[string]any{eventReasonField: reason}); err != nil {
			return false, err
		}
	}
	if repositoryMonitorPullRequestReviewed(monitor.Spec, pr) {
		fresh, err := r.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, pr.HeadSHA)
		if err != nil || !fresh {
			return false, err
		}
	}
	return true, r.Store.UpsertMonitorItem(ctx, item)
}

func repositoryMonitorManagedWorkflow(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor.Spec.Review.Publish.Enabled || monitor.Spec.Repair.Enabled || repositoryMonitorIssueImplementationEnabled(monitor.Spec)
}
