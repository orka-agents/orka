package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func (l *repositoryMonitorFeedbackLifecycle) job(t *testing.T) *store.RepairJob {
	t.Helper()
	job, err := l.f.store.GetRepairJob(context.Background(), "default", "repair-"+repositoryMonitorShortHash(l.command.ID))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// A Task that succeeds after its repair's outcome already failed, as when a
// pause label refused a retried handoff, never projects success: the stored
// outcome decides every projection. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackTerminalOutcomeWinsOverTask(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	l.pr.Labels = []string{"orka:pause"}
	l.mustHandoff(t)
	l.pr.Labels = nil
	if state := l.outcome(t).State; state != store.MonitorFeedbackOutcomeFailed {
		t.Fatalf("outcome = %s, want failed", state)
	}
	l.endTask(t, corev1alpha1.TaskPhaseSucceeded, &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNoChange, Outcome: corev1alpha1.TaskDeliveryOutcomeNoChange, StartingSHA: "head1111"})
	state, err := l.f.reconciler.repositoryMonitorRepairStateForHead(ctx, l.f.monitor, 1, "head1111")
	if err != nil {
		t.Fatal(err)
	}
	if job := l.job(t); job.Phase != repositoryMonitorRepairPhaseFailed || state != repositoryMonitorRepairPhaseFailed {
		t.Fatalf("outcome %s projected=%t, job %s, repair state %s; want the failed outcome projected", l.outcome(t).State, l.outcome(t).ProjectedAt != nil, job.Phase, state)
	}
	action, err := l.f.store.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(l.command.ID, store.RepositoryMonitorDesiredActionForIntent(l.command.Intent)))
	if err != nil || action.Status == repositoryMonitorWorkActionStatusSucceeded {
		t.Fatalf("action = %+v err=%v, want no success", action, err)
	}
}

// A projection that contradicts its stored outcome, such as a job some
// writer marked succeeded for a failed repair, is corrected on replay.
func TestRepositoryMonitorReviewFeedbackProjectionCorrectsContradictions(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	l.endTask(t, corev1alpha1.TaskPhaseFailed, nil)
	job := l.job(t)
	job.Phase, job.PushedSHA = repositoryMonitorRepairPhaseSucceeded, "pushed-9"
	if err := l.f.store.UpdateRepairJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	outcome := l.outcome(t)
	if err := l.f.reconciler.projectRepositoryMonitorFeedbackOutcome(ctx, l.f.monitor, *outcome); err != nil {
		t.Fatal(err)
	}
	if job := l.job(t); job.Phase != repositoryMonitorRepairPhaseFailed || job.PushedSHA != "" {
		t.Fatalf("job = %+v, want the failed outcome", job)
	}
}

// A repair withdrawn after the API server refused its Task create closes its
// job and push mutation, and the withdrawal still reads as if no repair was
// requested. (Codex review probe and Copilot thread.)
func TestRepositoryMonitorReviewFeedbackWithdrawalClosesItsJob(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	base := l.f.reconciler.Client
	l.f.reconciler.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*corev1alpha1.Task); ok {
			return apierrors.NewForbidden(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), obj.GetName(), errors.New("quota"))
		}
		return c.Create(ctx, obj, opts...)
	}})
	if err := l.handoff(t); err == nil {
		t.Fatal("expected the refused create to fail the handoff")
	}
	l.f.reconciler.Client = base
	l.resolveFeedback()
	l.mustHandoff(t)
	for range 2 {
		if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
			t.Fatal(err)
		}
	}
	job := l.job(t)
	mutation, err := l.f.store.GetGitHubMutationRecord(ctx, "default", "ghmut-"+repositoryMonitorShortHash(job.ID+"-push"))
	if err != nil {
		t.Fatal(err)
	}
	if job.Phase != repositoryMonitorRepairPhaseWithdrawn || mutation.Status == repositoryMonitorAutomergeStateStarted || l.outcome(t).ProjectedAt == nil {
		t.Fatalf("outcome %s projected=%t, job %s, mutation %s; want them closed", l.outcome(t).State, l.outcome(t).ProjectedAt != nil, job.Phase, mutation.Status)
	}
	if state, err := l.f.reconciler.repositoryMonitorRepairStateForHead(ctx, l.f.monitor, 1, "head1111"); err != nil || state != "" {
		t.Fatalf("repair state = %q err=%v, want none for a withdrawal", state, err)
	}
	if ledger := l.f.ledger(t); ledger.spent["review_comment:11"] != 0 {
		t.Fatalf("withdrawn repair charged: %+v", ledger)
	}
}

// Ingestion visits every queued job, so abandoned newer jobs cannot keep an
// older repair's result from being recorded.
func TestRepositoryMonitorReviewFeedbackIngestionReachesOlderJobs(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	for i := range 101 {
		if err := l.f.store.CreateRepairJob(ctx, &store.RepairJob{ID: fmt.Sprintf("abandoned-%03d", i), MonitorNamespace: "default", MonitorName: l.f.monitor.Name,
			Repo: "orka-agents/orka", PRNumber: 2, Intent: repositoryMonitorCommandIntentFix, HeadSHA: "other", Phase: repositoryMonitorRepairPhaseQueued, TaskName: fmt.Sprintf("missing-%03d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	l.endTask(t, corev1alpha1.TaskPhaseFailed, nil)
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeFailed || outcome.ProjectedAt == nil {
		t.Fatalf("outcome = %+v, want the older repair's result recorded", outcome)
	}
}

// A retried handoff whose cache reports its Task missing adopts the Task an
// uncached read found, with its frozen prompt, even after the feedback
// changed, instead of reading feedback again. (Copilot thread.)
func TestRepositoryMonitorReviewFeedbackStaleCacheAdoptsFrozenPrompt(t *testing.T) {
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	l.resolveFeedback()
	l.staleTaskReads = true
	if err := l.handoff(t); err != nil {
		t.Fatalf("retried handoff: %v", err)
	}
	if outcome := l.outcome(t); outcome.State != store.MonitorFeedbackOutcomeStarted {
		t.Fatalf("outcome = %+v, want the started repair adopted", outcome)
	}
}

// pollFeedbackSnapshots runs one poll in which every pull request's snapshot
// looks up its reviewers, and reports which snapshots completed.
func pollFeedbackSnapshots(t *testing.T, f *repositoryMonitorFeedbackRepairFixture, prs, reviewers int) map[int]bool {
	t.Helper()
	ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
	defer end()
	complete := map[int]bool{}
	for pr := range prs {
		scoped := withRepositoryMonitorPermissionScope(ctx, fmt.Sprintf("pull_request:%d", pr))
		complete[pr] = true
		for reviewer := range reviewers {
			_, err := f.schedulingPermission(scoped, fmt.Sprintf("pr%d-reviewer%02d", pr, reviewer))
			switch {
			case errors.Is(err, errRepositoryMonitorPermissionLookupBudget):
				complete[pr] = false
			case err != nil:
				t.Fatal(err)
			}
		}
	}
	return complete
}

// schedulingPermission looks login up for scheduling, as readiness does, for
// a caller any write permission satisfies.
func (f *repositoryMonitorFeedbackRepairFixture) schedulingPermission(ctx context.Context, login string) (string, error) {
	return f.reconciler.repositoryMonitorSchedulingPermission(ctx, f.reconciler.GitHubAPIBaseURL, "orka-agents", "orka", "forge-token", login, func(permission string) bool {
		return repositoryMonitorPermissionMeets("", nil, permission)
	})
}

// grantEveryoneWrite makes every account the fixture's GitHub does not name
// a writer, so lookups made only to exercise the budget are remembered.
func (f *repositoryMonitorFeedbackRepairFixture) grantEveryoneWrite() {
	f.github.mu.Lock()
	f.github.defaultPermission = "write"
	f.github.mu.Unlock()
}

func forgetRememberedPermissions(f *repositoryMonitorFeedbackRepairFixture) {
	value, _ := f.reconciler.permissionMemory.Load(f.monitor.UID)
	memory := value.(*repositoryMonitorPermissionMemory)
	memory.mu.Lock()
	clear(memory.results)
	memory.mu.Unlock()
}

// Five pull requests with 20 reviewers each need more lookups than a poll's
// budget. With remembered permissions expiring between polls, every pull
// request's snapshot still completes within a bounded number of polls,
// because a snapshot the budget cut short has all its reviewers reserved
// together. (Codex review probe.)
func TestRepositoryMonitorPermissionLookupsCompleteEverySnapshot(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	f.grantEveryoneWrite()
	lastComplete := map[int]int{}
	for poll := 1; poll <= 12; poll++ {
		for pr, complete := range pollFeedbackSnapshots(t, f, 5, 20) {
			if complete {
				lastComplete[pr] = poll
			}
		}
		forgetRememberedPermissions(f)
	}
	for pr := range 5 {
		if lastComplete[pr] < 8 {
			t.Fatalf("pull request %d last completed in poll %d of 12: %v", pr, lastComplete[pr], lastComplete)
		}
	}
}

// A reconcile that looks nothing up leaves the snapshots waiting for budget
// waiting, so the next inventory poll still serves them. (Copilot thread.)
func TestRepositoryMonitorPermissionLookupsSurviveIdleReconciles(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	f.grantEveryoneWrite()
	first := pollFeedbackSnapshots(t, f, 5, 20)
	if first[3] || first[4] {
		t.Fatalf("first poll completed %v, want the last pull requests cut short", first)
	}
	_, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
	end()
	forgetRememberedPermissions(f)
	if second := pollFeedbackSnapshots(t, f, 5, 20); !second[3] {
		t.Fatalf("after an idle reconcile, the longest-waiting snapshot did not complete: %v", second)
	}
	value, _ := f.reconciler.permissionMemory.Load(f.monitor.UID)
	memory := value.(*repositoryMonitorPermissionMemory)
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, ok := memory.waiting["pull_request:4"]; !ok {
		t.Fatalf("waiting = %v, want the snapshot not yet served still waiting", strings.Join(sortedKeys(memory.waiting), ","))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
