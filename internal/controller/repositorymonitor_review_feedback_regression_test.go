package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// A controller that stopped after recording a feedback repair's end, before
// its job and work action, records them on a later ingestion even though the
// Task is gone and the command run already succeeded. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackSettlementReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	run, err := l.f.store.GetMonitorRun(ctx, "default", repositoryMonitorCommandRunIDFromCommand(l.command.ID))
	if err != nil {
		t.Fatal(err)
	}
	run.Phase = repositoryMonitorRunPhaseSucceeded
	if err := l.f.store.UpdateMonitorRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	// The controller stops after the first write of a missing-Task end.
	if err := l.f.reconciler.finishRepositoryMonitorFeedbackOutcome(ctx, l.f.monitor, l.command.ID, store.MonitorFeedbackOutcomeFailed, repositoryMonitorRepairTaskMissing, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
			t.Fatal(err)
		}
		if _, err := l.f.reconciler.enqueueAcceptedRepositoryMonitorCommands(ctx, l.f.monitor); err != nil {
			t.Fatal(err)
		}
	}
	job, err := l.f.store.GetRepairJob(ctx, "default", "repair-"+repositoryMonitorShortHash(l.command.ID))
	if err != nil || job.Phase != repositoryMonitorRepairPhaseFailed || job.LastError != repositoryMonitorRepairTaskMissing {
		t.Fatalf("job = %+v err=%v, want the recorded failure", job, err)
	}
	action, err := l.f.store.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(l.command.ID, store.RepositoryMonitorDesiredActionForIntent(l.command.Intent)))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed {
		t.Fatalf("action = %+v err=%v, want failed", action, err)
	}
	if outcome := l.outcome(t); outcome.ProjectedAt == nil {
		t.Fatalf("outcome = %+v, want it marked projected", outcome)
	}
	if state, err := l.f.reconciler.repositoryMonitorRepairStateForHead(ctx, l.f.monitor, 1, "head1111"); err != nil || state != repositoryMonitorRepairPhaseFailed {
		t.Fatalf("repair state = %q err=%v", state, err)
	}
}

// A succeeded repair whose end was recorded before its job keeps the head it
// pushed in its outcome, so the job and its push mutation record it after
// the Task is gone.
func TestRepositoryMonitorReviewFeedbackSettlementReplaysPushedHead(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	if err := l.f.reconciler.finishRepositoryMonitorFeedbackOutcome(ctx, l.f.monitor, l.command.ID, store.MonitorFeedbackOutcomeSucceeded, "", "pushed-2"); err != nil {
		t.Fatal(err)
	}
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	job, err := l.f.store.GetRepairJob(ctx, "default", "repair-"+repositoryMonitorShortHash(l.command.ID))
	if err != nil || job.Phase != repositoryMonitorRepairPhaseSucceeded || job.PushedSHA != "pushed-2" {
		t.Fatalf("job = %+v err=%v, want the pushed head", job, err)
	}
	mutation, err := l.f.store.GetGitHubMutationRecord(ctx, "default", "ghmut-"+repositoryMonitorShortHash(job.ID+"-push"))
	if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.ExternalID != "pushed-2" {
		t.Fatalf("push mutation = %+v err=%v", mutation, err)
	}
	if outcome := l.outcome(t); outcome.ProjectedAt == nil || outcome.ResultSHA != "pushed-2" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// Cancelling a repair whose Task create may have succeeded, after the Task
// ran and was deleted, keeps the comment's charge. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackCancelAfterAmbiguousCreateKeepsCharge(t *testing.T) {
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.lostCreate = true
	if err := l.handoff(t); err == nil {
		t.Fatal("expected the lost create to fail the handoff")
	}
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(context.Background(), &task); err != nil {
		t.Fatal(err)
	}
	l.cancelCommand(t)
	if got := l.f.ledger(t).spent["review_comment:11"]; got != 1 {
		t.Fatalf("charge = %d after cancelling a repair that may have run; outcome = %+v", got, l.outcome(t))
	}
}

// A repair whose Task may have been created and is gone is not created
// again, even when its job no longer says so, as after recovery from an
// earlier layout. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackPossiblyRunTaskIsNotRecreated(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.lostCreate = true
	if err := l.handoff(t); err == nil {
		t.Fatal("expected the lost create to fail the handoff")
	}
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	job, err := l.f.store.GetRepairJob(ctx, "default", "repair-"+repositoryMonitorShortHash(l.command.ID))
	if err != nil {
		t.Fatal(err)
	}
	job.LastError = ""
	if err := l.f.store.UpdateRepairJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	l.mustHandoff(t)
	if got := len(l.f.repairTasks(t)); got != 0 {
		t.Fatalf("recreated %d Task for a repair that may have run", got)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeFailed || outcome.Reason != repositoryMonitorRepairTaskOutcomeUnknown {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// An unresolved thread whose fetched comments are all left out, here bots
// that are not allowlisted, and whose middle replies were not read may hide
// authorized feedback, so a person must look. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackOmittedRepliesWithoutShownEvidence(t *testing.T) {
	comments := make([]repositoryMonitorFeedbackCommentNode, 0, 21)
	for i := range 21 {
		comments = append(comments, repositoryMonitorFeedbackTestComment(100+i, "automation", "Bot", "automated note", i))
	}
	thread := repositoryMonitorFeedbackTestThread("T1", 10, false, comments...)
	thread.Recent.Nodes = comments[1:]
	thread.Root.TotalCount = 30
	permissionOf, _ := repositoryMonitorFeedbackTestPermissions(nil)
	snapshot := repositoryMonitorFeedbackTestNormalize(repositoryMonitorRawReviewFeedback{threads: []repositoryMonitorFeedbackThreadNode{thread}}, nil, repositoryMonitorReviewFeedbackPolicy{MinPermission: "write"}, permissionOf)
	if verdict, _ := snapshot.verdict(); verdict != repositoryMonitorFeedbackNeedsPerson {
		t.Fatalf("verdict = %q, included threads %d, omitted threads %d", verdict, len(snapshot.Threads), snapshot.OmittedThreads)
	}
}

// A cache that lags the Task's create never finalizes the repair: absence is
// confirmed with an uncached read, both by a retried handoff and by
// ingestion. Only a Task that is really gone fails the repair.
func TestRepositoryMonitorReviewFeedbackStaleCacheDoesNotFinalize(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	l.staleTaskReads = true
	if err := l.handoff(t); err != nil {
		t.Fatal(err)
	}
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeStarted {
		t.Fatalf("outcome = %+v, want it still started", outcome)
	}
	l.staleTaskReads = false
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	l.staleTaskReads = true
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeFailed || outcome.Reason != repositoryMonitorRepairTaskMissing {
		t.Fatalf("outcome = %+v, want the missing Task to fail it", outcome)
	}
}

// Accounts a poll defers for lack of budget are served first by the next
// poll, so pull requests visited late in every poll are not starved; a
// remembered permission answers later polls within its TTL, except in a
// repair handoff, which reads fresh.
func TestRepositoryMonitorPermissionLookupsRotateAcrossPolls(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	baseURL := f.reconciler.GitHubAPIBaseURL
	requests := func(login string) int {
		count := 0
		for _, request := range f.github.requests {
			if request == "permission:"+login {
				count++
			}
		}
		return count
	}
	early := make([]string, 0, repositoryMonitorPermissionLookupBudget)
	for i := range repositoryMonitorPermissionLookupBudget {
		early = append(early, fmt.Sprintf("early%d", i))
	}
	late := []string{"late0", "late1", "late2", "late3", "late4"}
	poll := func(logins []string) (refused []string) {
		t.Helper()
		ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
		defer end()
		for _, login := range logins {
			if _, err := f.reconciler.repositoryMonitorCollaboratorPermission(ctx, baseURL, "orka-agents", "orka", "forge-token", login); errors.Is(err, errRepositoryMonitorPermissionLookupBudget) {
				refused = append(refused, login)
			} else if err != nil {
				t.Fatal(err)
			}
		}
		return refused
	}
	forget := func() {
		value, _ := f.reconciler.permissionMemory.Load(f.monitor.UID)
		memory := value.(*repositoryMonitorPermissionMemory)
		memory.mu.Lock()
		for key, remembered := range memory.results {
			remembered.at = time.Now().Add(-repositoryMonitorPermissionMemoryTTL)
			memory.results[key] = remembered
		}
		memory.mu.Unlock()
	}
	// The early pull requests spend the budget; the late ones are deferred.
	if refused := poll(append(append([]string{}, early...), late...)); strings.Join(refused, ",") != strings.Join(late, ",") {
		t.Fatalf("first poll refused %v, want the late accounts", refused)
	}
	// Once the remembered permissions expire, the next poll serves the
	// deferred accounts first, whatever order it reaches them in.
	forget()
	refused := poll(append(append([]string{}, early...), late...))
	for _, login := range late {
		if requests(login) != 1 {
			t.Fatalf("deferred account %s looked up %d times, want once; refused %v", login, requests(login), refused)
		}
	}
	if len(refused) != len(late) {
		t.Fatalf("second poll refused %v, want as many early accounts as were reserved", refused)
	}
	// Within the TTL a remembered permission answers without a request.
	before := requests("late0")
	if refused := poll([]string{"late0"}); len(refused) != 0 || requests("late0") != before {
		t.Fatalf("remembered permission was looked up again: refused %v requests %d", refused, requests("late0"))
	}
	// A repair handoff reads fresh.
	ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
	defer end()
	if _, err := f.reconciler.repositoryMonitorCollaboratorPermission(withRepositoryMonitorFreshPermissions(ctx), baseURL, "orka-agents", "orka", "forge-token", "late0"); err != nil || requests("late0") != before+1 {
		t.Fatalf("fresh lookup err=%v requests %d, want a new request", err, requests("late0"))
	}
}

// A projection interrupted after the job ended still finishes the push
// mutation and the item's review reset on replay.
func TestRepositoryMonitorReviewFeedbackProjectionResumesAfterJobEnds(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	item, err := l.f.store.GetMonitorItem(ctx, "default", l.f.monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatal(err)
	}
	item.LastReviewedHeadSHA, item.LastVerdict = "head1111", repositoryMonitorReviewVerdictPassed
	if err := l.f.store.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := l.f.reconciler.finishRepositoryMonitorFeedbackOutcome(ctx, l.f.monitor, l.command.ID, store.MonitorFeedbackOutcomeSucceeded, "", "pushed-3"); err != nil {
		t.Fatal(err)
	}
	// The controller stopped after ending the job.
	job, err := l.f.store.GetRepairJob(ctx, "default", "repair-"+repositoryMonitorShortHash(l.command.ID))
	if err != nil {
		t.Fatal(err)
	}
	job.Phase, job.PushedSHA = repositoryMonitorRepairPhaseSucceeded, "pushed-3"
	if err := l.f.store.UpdateRepairJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := l.f.reconciler.settleRepositoryMonitorFeedbackOutcomes(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	mutation, err := l.f.store.GetGitHubMutationRecord(ctx, "default", "ghmut-"+repositoryMonitorShortHash(job.ID+"-push"))
	if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.ExternalID != "pushed-3" {
		t.Fatalf("push mutation = %+v err=%v", mutation, err)
	}
	item, err = l.f.store.GetMonitorItem(ctx, "default", l.f.monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil || item.LastReviewedHeadSHA != "" {
		t.Fatalf("item = %+v err=%v, want the replaced head's review reset", item, err)
	}
}

// More deferred accounts than the budget never stall lookups: each poll
// reserves at most half its budget for them and keeps making progress.
func TestRepositoryMonitorPermissionLookupsProgressPastLargeDeferrals(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	baseURL := f.reconciler.GitHubAPIBaseURL
	accounts := make([]string, 0, 2*repositoryMonitorPermissionLookupBudget+1)
	for i := range 2*repositoryMonitorPermissionLookupBudget + 1 {
		accounts = append(accounts, fmt.Sprintf("account%03d", i))
	}
	looked := map[string]bool{}
	for poll := 0; poll < 6 && len(looked) < len(accounts); poll++ {
		ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
		served := 0
		for _, account := range accounts {
			_, err := f.reconciler.repositoryMonitorCollaboratorPermission(ctx, baseURL, "orka-agents", "orka", "forge-token", account)
			switch {
			case err == nil:
				served++
				looked[account] = true
			case !errors.Is(err, errRepositoryMonitorPermissionLookupBudget):
				t.Fatal(err)
			}
		}
		end()
		if served == 0 {
			t.Fatalf("poll %d served no account", poll)
		}
	}
	if len(looked) != len(accounts) {
		t.Fatalf("looked up %d of %d accounts", len(looked), len(accounts))
	}
}

// A started repair's Task is never created again: a retried handoff whose
// cache still holds the deleted Task settles the repair instead of creating
// a second Task under the same name.
func TestRepositoryMonitorReviewFeedbackStartedRepairIsNotRecreated(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	l.cachedTask = &task
	l.mustHandoff(t)
	l.cachedTask = nil
	if got := len(l.f.repairTasks(t)); got != 0 {
		t.Fatalf("created %d Task again for a started repair", got)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeFailed || outcome.Reason != repositoryMonitorRepairTaskMissing || l.f.ledger(t).spent["review_comment:11"] != 1 {
		t.Fatalf("outcome = %+v, want the started repair failed and charged", outcome)
	}
}

// A repair whose Task create was requested but never confirmed is not
// created again either: when its cache still holds the Task after the Task
// ran and was deleted, the retried handoff settles it as an uncertain
// failure that keeps the charge.
func TestRepositoryMonitorReviewFeedbackCreatingRepairIsNotRecreated(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.lostCreate = true
	if err := l.handoff(t); err == nil {
		t.Fatal("expected the lost create to fail the handoff")
	}
	task := l.f.repairTasks(t)[0]
	if err := l.f.reconciler.Delete(ctx, &task); err != nil {
		t.Fatal(err)
	}
	l.cachedTask = &task
	l.mustHandoff(t)
	l.cachedTask = nil
	if got := len(l.f.repairTasks(t)); got != 0 {
		t.Fatalf("created %d Task again for a repair whose Task create was requested", got)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeFailed || outcome.Reason != repositoryMonitorRepairTaskOutcomeUnknown || l.f.ledger(t).spent["review_comment:11"] != 1 {
		t.Fatalf("outcome = %+v, want an uncertain failure that keeps the charge", outcome)
	}
}

// When remembered permissions expire between every poll and a stable set of
// accounts is reached in the same order each time, reservations go to the
// accounts that have waited longest, so every account is looked up within a
// few polls instead of the same ones every time. (Autoreview finding.)
func TestRepositoryMonitorPermissionLookupsServeEveryAccountInOrderOfWaiting(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	baseURL := f.reconciler.GitHubAPIBaseURL
	accounts := make([]string, 0, 2*repositoryMonitorPermissionLookupBudget+1)
	for i := range 2*repositoryMonitorPermissionLookupBudget + 1 {
		accounts = append(accounts, fmt.Sprintf("account%03d", i))
	}
	lastServed := map[string]int{}
	for poll := 1; poll <= 12; poll++ {
		ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
		for _, account := range accounts {
			_, err := f.reconciler.repositoryMonitorCollaboratorPermission(ctx, baseURL, "orka-agents", "orka", "forge-token", account)
			switch {
			case err == nil:
				lastServed[account] = poll
			case !errors.Is(err, errRepositoryMonitorPermissionLookupBudget):
				t.Fatal(err)
			}
		}
		end()
		// Every remembered permission expires before the next poll.
		value, _ := f.reconciler.permissionMemory.Load(f.monitor.UID)
		memory := value.(*repositoryMonitorPermissionMemory)
		memory.mu.Lock()
		clear(memory.results)
		memory.mu.Unlock()
	}
	for _, account := range accounts {
		// Each account is served at least once in the last few polls.
		if lastServed[account] < 8 {
			t.Fatalf("account %s last served in poll %d of 12", account, lastServed[account])
		}
	}
}
