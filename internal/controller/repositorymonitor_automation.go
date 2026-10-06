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

// Repair.Enabled is the repository administrator's authorization for bounded
// automatic repair. A controller policy action is never recorded as a human.
func (r *RepositoryMonitorReconciler) tryRepositoryMonitorAutomaticRepair(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, run *store.MonitorRun, owner, repo string, pr repositoryMonitorPullRequest, item *store.MonitorItem) (bool, error) {
	if !monitor.Spec.Repair.Enabled || run.CommandEventID != "" || pr.Draft || pr.HeadSHA == "" || !strings.EqualFold(strings.TrimSpace(pr.HeadRepo), owner+"/"+repo) {
		return false, nil
	}
	if repositoryMonitorBlockedLabel(monitor.Spec, pr.Labels) != "" || item.SkipReason == repositoryMonitorIssueSkipStoppedByCommand || item.LastVerdict == repositoryMonitorRunPhaseQueued {
		return false, nil
	}
	if item.RepairState == repositoryMonitorRepairPhaseQueued {
		return true, nil
	}
	intent := ""
	if pr.MergeableState == repositoryMonitorMergeableStateDirty {
		intent = repositoryMonitorCommandIntentUpdateBranch
	} else {
		if monitor.Spec.Agents.Repairer == nil || strings.TrimSpace(monitor.Spec.Agents.Repairer.Name) == "" {
			return false, nil
		}
		ci, err := r.repositoryMonitorCheckCI(ctx, monitor, pr.HeadSHA)
		if err != nil {
			return false, err
		}
		if ci.reason == repositoryMonitorCINotGreen {
			intent = repositoryMonitorCommandIntentFixCI
		}
		if intent == "" && item.LastVerdict == repositoryMonitorReviewVerdictNeedsChanges && item.LastReviewedHeadSHA == pr.HeadSHA && item.LastReviewID != "" {
			review, err := r.Store.GetReviewRecord(ctx, monitor.Namespace, item.LastReviewID)
			if err != nil {
				return false, err
			}
			if review.HeadSHA == pr.HeadSHA && review.Repairable {
				intent = repositoryMonitorCommandIntentFix
			}
		}
	}
	if intent == "" {
		return false, nil
	}
	// The policy helper counts durable attempts, including commands exhausted
	// before Task creation, before another command identity can be created.
	reason, prCount, headCount, err := r.repositoryMonitorRepairPolicy(ctx, monitor, owner+"/"+repo, pr, "", intent)
	if err != nil {
		return false, err
	}
	if reason != "" {
		item.RepairState = repositoryMonitorRepairPhaseFailed
		item.SkipReason = reason
		return true, r.Store.UpsertMonitorItem(ctx, item)
	}
	id := "policy-" + repositoryMonitorShortHash(fmt.Sprintf("%s|%d|%d|%s|%s|%d|%d", monitor.UID, monitor.Generation, pr.Number, pr.HeadSHA, intent, prCount, headCount))
	command, err := r.Store.GetCommandEvent(ctx, monitor.Namespace, id)
	if errors.Is(err, store.ErrNotFound) {
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
	// Use the same durable command queue as label/API requests. Executing inline
	// under the inventory run would race command recovery with a different run
	// identity and could reject the already-created Task as a metadata mismatch.
	if _, err := r.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor); err != nil {
		return false, err
	}
	item.RepairState = repositoryMonitorRepairPhaseQueued
	return true, r.Store.UpsertMonitorItem(ctx, item)
}

// Polling advances CI and review/repair outcomes even when GitHub emits no new
// pull_request event. It does not repeatedly submit implement commands.
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
	// Only runs that inventory pull requests throttle their workflow poll.
	// Bound the scan to the cooldown window, including full inventory runs.
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
	if err := r.Store.CreateMonitorRun(ctx, &store.MonitorRun{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "workflow", TargetKind: repositoryMonitorPullRequestKind, Phase: repositoryMonitorRunPhaseQueued, StartedAt: now}); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}

func repositoryMonitorManagedWorkflow(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor.Spec.Review.Publish.Enabled || monitor.Spec.Repair.Enabled || (monitor.Spec.Targets.Issues.Enabled && monitor.Spec.Agents.Implementer != nil && (monitor.Spec.IssueWorkflow.Implementation.Enabled == nil || *monitor.Spec.IssueWorkflow.Implementation.Enabled))
}
