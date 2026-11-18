package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// repositoryMonitorFeedbackFailingActionStore fails, or with refuse
// silently drops, every work action update that would record success.
type repositoryMonitorFeedbackFailingActionStore struct {
	store.RepositoryMonitorStore
	refuse bool
}

func (s repositoryMonitorFeedbackFailingActionStore) UpdateWorkAction(ctx context.Context, action *store.WorkAction) error {
	if action.Status == repositoryMonitorWorkActionStatusSucceeded {
		if s.refuse {
			return nil
		}
		return io.EOF
	}
	return s.RepositoryMonitorStore.UpdateWorkAction(ctx, action)
}

// A stop command's bulk cancel reaches the action of a feedback repair
// whose no-change outcome is still waiting behind a backlog of unprojected
// outcomes. Replay still projects the stored no-change success over the
// cancellation. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackStopDuringProjectionBacklog(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	task := l.f.repairTasks(t)[0]
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNoChange, Outcome: corev1alpha1.TaskDeliveryOutcomeNoChange, StartingSHA: "head1111"}
	if err := l.f.reconciler.Status().Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	original := l.f.reconciler.Store
	l.f.reconciler.Store = repositoryMonitorFeedbackFailingActionStore{RepositoryMonitorStore: original}
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); !errors.Is(err, io.EOF) {
		t.Fatalf("ingestion err = %v, want the action projection's EOF", err)
	}
	l.f.reconciler.Store = original
	// A backlog of unprojected outcomes, as after the ledger migration.
	for i := range repositoryMonitorFeedbackProjectionBatch {
		id := fmt.Sprintf("backlog-%03d", i)
		command := l.command
		command.ID, command.CommentID, command.IdempotencyKey, command.DedupeKey = id, id, id, id
		command.Number = int64(i + 2)
		command.CreatedAt = time.Now().Add(time.Duration(i+1) * time.Second)
		attempt := store.MonitorFeedbackAttempt{ID: id, MonitorNamespace: command.MonitorNamespace, MonitorName: command.MonitorName, Repo: command.Repo, PRNumber: command.Number,
			FeedbackID: "review_comment:11", Attempt: 1, HeadSHA: command.HeadSHA, SnapshotDigest: "digest", CommandEventID: id, RepairJobID: "repair-" + repositoryMonitorShortHash(id), CreatedAt: command.CreatedAt}
		if _, err := l.f.store.AdmitMonitorFeedbackRepair(ctx, store.MonitorFeedbackAdmission{Command: command, Attempts: []store.MonitorFeedbackAttempt{attempt}}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.f.store.FinishMonitorFeedbackRepair(ctx, store.FinishMonitorFeedbackRepairRequest{Namespace: "default", CommandEventID: id, State: store.MonitorFeedbackOutcomeFailed}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	if l.outcome(t).ProjectedAt != nil {
		t.Fatal("the older outcome was projected inside the first batch")
	}
	stop := l.command
	stop.ID, stop.CommentID, stop.IdempotencyKey, stop.DedupeKey = "stop", "stop", "stop", "stop"
	stop.Intent, stop.Command = repositoryMonitorCommandIntentStop, repositoryMonitorCommandIntentStop
	if err := l.f.store.CreateCommandEvent(ctx, &stop); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.f.reconciler.tryProcessPullRequestCommandRun(ctx, l.f.monitor, l.f.commandRun(stop), "orka-agents", "orka", l.pr, l.item); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
			t.Fatal(err)
		}
	}
	if action := l.action(t); action.Status != repositoryMonitorWorkActionStatusSucceeded || l.outcome(t).ProjectedAt == nil || l.job(t).Phase != repositoryMonitorRepairPhaseSucceeded {
		t.Fatalf("outcome %s projected=%t, job %s, action %s; want the no-change success projected", l.outcome(t).State, l.outcome(t).ProjectedAt != nil, l.job(t).Phase, action.Status)
	}
}

// A projection whose write the store silently drops is never marked
// projected: the projector reads its records back first.
func TestRepositoryMonitorReviewFeedbackRefusedProjectionIsNotMarked(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	task := l.f.repairTasks(t)[0]
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNoChange, Outcome: corev1alpha1.TaskDeliveryOutcomeNoChange, StartingSHA: "head1111"}
	if err := l.f.reconciler.Status().Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	original := l.f.reconciler.Store
	l.f.reconciler.Store = repositoryMonitorFeedbackFailingActionStore{RepositoryMonitorStore: original, refuse: true}
	_, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor)
	l.f.reconciler.Store = original
	if !errors.Is(err, errRepositoryMonitorFeedbackProjectionContradicted) {
		t.Fatalf("ingestion err = %v, want the refused projection reported", err)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeNoChange || outcome.ProjectedAt != nil {
		t.Fatalf("outcome = %+v, want no_change and not projected while its action was refused", outcome)
	}
	outcome := l.outcome(t)
	l.f.reconciler.Store = repositoryMonitorFeedbackFailingActionStore{RepositoryMonitorStore: original, refuse: true}
	err = l.f.reconciler.projectRepositoryMonitorFeedbackOutcome(ctx, l.f.monitor, *outcome)
	l.f.reconciler.Store = original
	if !errors.Is(err, errRepositoryMonitorFeedbackProjectionContradicted) || l.outcome(t).ProjectedAt != nil {
		t.Fatalf("projection err = %v projected=%t, want a contradiction and no mark", err, l.outcome(t).ProjectedAt != nil)
	}
	// Once the write goes through, the outcome is projected.
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	if l.outcome(t).ProjectedAt == nil || l.action(t).Status != repositoryMonitorWorkActionStatusSucceeded {
		t.Fatalf("outcome projected=%t action %s", l.outcome(t).ProjectedAt != nil, l.action(t).Status)
	}
}

// A queued branch update re-checks the repair labeller's permission fresh:
// a permission a poll remembered is not used once revoked, so the update is
// refused. (Copilot review summary.)
func TestRepositoryMonitorQueuedBranchUpdateRechecksRevokedPermission(t *testing.T) {
	ctx := context.Background()
	f := newRepositoryMonitorFeedbackRepairFixture(t, func(monitor *corev1alpha1.RepositoryMonitor) {
		monitor.Spec.Repair.IncludeLabels = []string{"orka:fix"}
	})
	*f.labelEvents = `[{"event":"labeled","actor":{"login":"labeller"},"label":{"name":"orka:fix"}}]`
	f.github.permissions["labeller"] = "write"
	pr := repositoryMonitorFeedbackRepairTestPR()
	pr.Labels = []string{"orka:fix"}
	// A poll remembers the labeller's write access.
	poll, end := f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
	if reason, err := f.reconciler.repositoryMonitorRepairLabelActorRefusal(poll, repositoryMonitorPermissionForScheduling, f.monitor, "orka-agents", "orka", pr, true, true); err != nil || reason != "" {
		t.Fatalf("labeller refused before revocation: %q err=%v", reason, err)
	}
	end()
	// The access is revoked; a branch update the policy queued then runs.
	f.github.permissions["labeller"] = "read"
	command := store.CommandEvent{ID: "policy-update", CommentID: "policy-update", DedupeKey: "policy-update", MonitorNamespace: "default", MonitorName: f.monitor.Name,
		Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 1, Source: repositoryMonitorControllerPolicySource, Author: repositoryMonitorControllerActor,
		Permission: "repository_repair_policy", Intent: repositoryMonitorCommandIntentUpdateBranch, Command: repositoryMonitorCommandIntentUpdateBranch, HeadSHA: "head1111", Status: repositoryMonitorCommandAccepted}
	if err := f.store.CreateCommandEvent(ctx, &command); err != nil {
		t.Fatal(err)
	}
	poll, end = f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
	defer end()
	item := repositoryMonitorItemFromPullRequest(f.monitor, pr, nil)
	if _, _, err := f.reconciler.tryProcessPullRequestCommandRun(poll, f.monitor, f.commandRun(command), "orka-agents", "orka", pr, item); err != nil {
		t.Fatal(err)
	}
	action, err := f.store.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent)))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != repositoryMonitorRepairSkipLabelActorNotPermitted {
		t.Fatalf("action = %+v err=%v, want the branch update refused for the revoked labeller", action, err)
	}
	if _, err := f.store.GetGitHubMutationRecord(ctx, "default", repositoryMonitorUpdateBranchMutationID(command.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update-branch mutation recorded (err=%v), want none", err)
	}
}
