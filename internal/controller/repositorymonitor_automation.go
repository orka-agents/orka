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
// Repair labels narrow it to labelled pull requests; the account that added
// the label is checked before each new repair.
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
	// A repair label runs a review-fix loop: each head is reviewed, its
	// findings are fixed first, and CI is fixed once the review has nothing to fix.
	reviewFirst := full && len(monitor.Spec.Repair.IncludeLabels) > 0
	if reviewFirst {
		// The review must still be current: a stale or pre-validation review
		// of this head is not a reason to change it.
		fresh, err := r.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, pr.HeadSHA)
		if err != nil || !fresh {
			return false, err
		}
	}
	intent, err := r.repositoryMonitorAutomaticRepairIntent(ctx, monitor, pr, item, full, reviewFirst)
	if err != nil || intent == "" {
		return false, err
	}
	// The policy helper counts durable attempts, including commands exhausted
	// before Task creation, before another command identity can be created.
	reason, prCount, headCount, err := r.repositoryMonitorRepairPolicy(ctx, monitor, owner+"/"+repo, pr, "", intent)
	if err != nil {
		return false, err
	}
	if reason != "" {
		return r.refuseRepositoryMonitorAutomaticRepair(ctx, monitor, pr, item, reason)
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
				return r.refuseRepositoryMonitorAutomaticRepair(ctx, monitor, pr, item, repositoryMonitorRepairSkipLabelActorNotPermitted)
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
	// Use the same durable command queue as label/API requests. Executing inline
	// under the inventory run would race command recovery with a different run
	// identity and could reject the already-created Task as a metadata mismatch.
	if _, err := r.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor); err != nil {
		return false, err
	}
	item.RepairState = repositoryMonitorRepairPhaseQueued
	return true, r.Store.UpsertMonitorItem(ctx, item)
}

// repositoryMonitorAutomaticRepairIntent picks the repair for the pull
// request's head, or "" for none. A full repair of a dirty head updates the
// branch. Otherwise a repair label fixes the review's findings before CI, and
// automatic repair fixes CI first.
func (r *RepositoryMonitorReconciler) repositoryMonitorAutomaticRepairIntent(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, item *store.MonitorItem, full, reviewFirst bool) (string, error) {
	if full && pr.MergeableState == repositoryMonitorMergeableStateDirty {
		return repositoryMonitorCommandIntentUpdateBranch, nil
	}
	if monitor.Spec.Agents.Repairer == nil || strings.TrimSpace(monitor.Spec.Agents.Repairer.Name) == "" {
		return "", nil
	}
	if reviewFirst {
		fix, err := r.repositoryMonitorReviewFindingsRepairable(ctx, monitor, pr, item, full)
		if err != nil {
			return "", err
		}
		if fix {
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
	if reviewFirst {
		return "", nil
	}
	fix, err := r.repositoryMonitorReviewFindingsRepairable(ctx, monitor, pr, item, full)
	if err != nil || !fix {
		return "", err
	}
	return repositoryMonitorCommandIntentFix, nil
}

// repositoryMonitorReviewFindingsRepairable reports whether a full repair can
// fix the findings of the pull request head's latest review.
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

// refuseRepositoryMonitorAutomaticRepair records why no repair starts. A
// refusal starts no work, so a reviewed pull request whose head has no current
// review goes on to review selection instead of repeating the refusal every
// poll; otherwise the refusal handles the pull request for this poll.
func (r *RepositoryMonitorReconciler) refuseRepositoryMonitorAutomaticRepair(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, pr repositoryMonitorPullRequest, item *store.MonitorItem, reason string) (bool, error) {
	item.RepairState = repositoryMonitorRepairPhaseFailed
	item.SkipReason = reason
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
