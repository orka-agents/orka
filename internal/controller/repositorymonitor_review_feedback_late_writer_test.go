package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

type repositoryMonitorFeedbackTestTransport func(*http.Request) (*http.Response, error)

func (f repositoryMonitorFeedbackTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// repositoryMonitorFeedbackFailEventStore fails the first repair_task_created
// event, as a storage EOF after the Task was created does.
type repositoryMonitorFeedbackFailEventStore struct {
	store.RepositoryMonitorStore
	fail bool
}

func (s *repositoryMonitorFeedbackFailEventStore) CreateMonitorEvent(ctx context.Context, e *store.MonitorEvent) error {
	if s.fail && e.EventType == "repair_task_created" {
		s.fail = false
		return io.EOF
	}
	return s.RepositoryMonitorStore.CreateMonitorEvent(ctx, e)
}

func (l *repositoryMonitorFeedbackLifecycle) action(t *testing.T) *store.WorkAction {
	t.Helper()
	action, err := l.f.store.GetWorkAction(context.Background(), "default", store.RepositoryMonitorWorkActionID(l.command.ID, store.RepositoryMonitorDesiredActionForIntent(l.command.Intent)))
	if err != nil {
		t.Fatal(err)
	}
	return action
}

// A handoff whose repair_task_created event hit a storage EOF queues a
// retry. When the Task's no-change result is projected before the retry runs
// and the head then moves, the retry's stale-head block cannot overwrite the
// projected success, and replay keeps it. (Codex review probe.)
func TestRepositoryMonitorReviewFeedbackQueuedRetryCannotOverwriteTerminalProjection(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	run, err := l.f.store.GetMonitorRun(ctx, "default", repositoryMonitorCommandRunIDFromCommand(l.command.ID))
	if err != nil {
		t.Fatal(err)
	}
	l.f.reconciler.Store = &repositoryMonitorFeedbackFailEventStore{RepositoryMonitorStore: l.f.reconciler.Store, fail: true}
	failed, _, err := l.f.reconciler.executeClaimedMonitorRun(ctx, l.f.monitor, *run, "orka-agents", "orka")
	if err != nil || failed == nil || failed.Phase != repositoryMonitorRunPhaseFailed {
		t.Fatalf("failed run = %+v err=%v", failed, err)
	}
	if _, err := l.f.reconciler.enqueueAcceptedRepositoryMonitorCommands(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	if run, err = l.f.store.GetMonitorRun(ctx, "default", run.ID); err != nil || run.Phase != repositoryMonitorRunPhaseQueued {
		t.Fatalf("run = %+v err=%v, want a queued retry", run, err)
	}
	run.StartedAt = time.Now().Add(-time.Minute)
	if err := l.f.store.UpdateMonitorRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	l.endTask(t, corev1alpha1.TaskPhaseSucceeded, &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNoChange, Outcome: corev1alpha1.TaskDeliveryOutcomeNoChange, StartingSHA: "head1111"})
	if _, err := l.f.reconciler.enqueueAcceptedRepositoryMonitorCommands(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
	// The pull request head moves after the Task settled.
	l.f.reconciler.HTTPClient = &http.Client{Transport: repositoryMonitorFeedbackTestTransport(func(req *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(req)
		if err != nil || !strings.HasSuffix(req.URL.Path, "/pulls/1") {
			return response, err
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		body := strings.ReplaceAll(string(data), "head1111", "head2222")
		response.Body, response.ContentLength = io.NopCloser(strings.NewReader(body)), int64(len(body))
		return response, nil
	})}
	if _, _, err := l.f.reconciler.processNextQueuedMonitorRun(ctx, l.f.monitor, "orka-agents", "orka"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
			t.Fatal(err)
		}
	}
	if action := l.action(t); action.Status != repositoryMonitorWorkActionStatusSucceeded || l.outcome(t).State != store.MonitorFeedbackOutcomeNoChange || l.job(t).Phase != repositoryMonitorRepairPhaseSucceeded {
		t.Fatalf("outcome %s, job %s, action %s/%s; want the no-change result kept", l.outcome(t).State, l.job(t).Phase, action.Status, action.BlockedReason)
	}
}

// No writer outside projection can make a terminal feedback repair's work
// action contradict its stored outcome.
func TestRepositoryMonitorReviewFeedbackLateWriterCannotContradictOutcome(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	l.endTask(t, corev1alpha1.TaskPhaseFailed, nil)
	for _, status := range []string{repositoryMonitorWorkActionStatusSucceeded, repositoryMonitorWorkActionStatusBlocked, repositoryMonitorWorkActionStatusRunning} {
		if err := l.f.reconciler.recordRepositoryMonitorWorkActionState(ctx, l.f.monitor, nil, &l.command, repositoryMonitorPullRequestKind, 1, "head2222", "", repositoryMonitorRepairWorkflowActionKind(l.command.Intent), status, "late", "", "stale_head_sha"); err != nil {
			t.Fatal(err)
		}
		if action := l.action(t); action.Status != repositoryMonitorWorkActionStatusFailed {
			t.Fatalf("after a late %s write the action is %s, want the failed outcome", status, action.Status)
		}
	}
}

// A projection a writer contradicted after the outcome was marked projected
// is found and corrected by the bounded check on later ingestions.
func TestRepositoryMonitorReviewFeedbackReplayCorrectsProjectedOutcomes(t *testing.T) {
	ctx := context.Background()
	l := newRepositoryMonitorFeedbackLifecycle(t)
	l.mustHandoff(t)
	l.endTask(t, corev1alpha1.TaskPhaseFailed, nil)
	if l.outcome(t).ProjectedAt == nil {
		t.Fatal("outcome not projected")
	}
	action := l.action(t)
	action.Status = repositoryMonitorWorkActionStatusSucceeded
	if err := l.f.store.UpdateWorkAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	job := l.job(t)
	job.Phase = repositoryMonitorRepairPhaseSucceeded
	if err := l.f.store.UpdateRepairJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
			t.Fatal(err)
		}
	}
	if action := l.action(t); action.Status != repositoryMonitorWorkActionStatusFailed || l.job(t).Phase != repositoryMonitorRepairPhaseFailed {
		t.Fatalf("action %s, job %s; want the failed outcome restored", action.Status, l.job(t).Phase)
	}
}

// A pull request whose reviewers change while it waits waits with its
// current reviewers only, so it never outgrows the reservation, and a later
// pull request is still served. (Codex review probe and Copilot thread.)
func TestRepositoryMonitorPermissionLookupsSurviveReviewerChurn(t *testing.T) {
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	f.grantEveryoneWrite()
	lastComplete := map[int]int{}
	for poll := 1; poll <= 12; poll++ {
		ctx, end := f.reconciler.withRepositoryMonitorPermissionPoll(context.Background(), f.monitor)
		for pr := range 5 {
			scoped := withRepositoryMonitorPermissionScope(ctx, fmt.Sprintf("pull_request:%d", pr))
			complete := true
			for reviewer := range 20 {
				generation := 0
				if pr == 3 && poll >= 2 {
					generation = 1
				}
				_, err := f.schedulingPermission(scoped, fmt.Sprintf("pr%d-gen%d-reviewer%02d", pr, generation, reviewer))
				switch {
				case errors.Is(err, errRepositoryMonitorPermissionLookupBudget):
					complete = false
				case err != nil:
					t.Fatal(err)
				}
			}
			if complete {
				lastComplete[pr] = poll
			}
		}
		end()
		forgetRememberedPermissions(f)
	}
	for pr := range 5 {
		if lastComplete[pr] < 8 {
			t.Fatalf("pull request %d last completed in poll %d of 12: %v", pr, lastComplete[pr], lastComplete)
		}
	}
}

// A deleted monitor's permission memory and verify cursor are evicted when
// its reconcile finds it gone, a monitor recreated under the same name drops
// the state of the one it replaced, and memories no poll uses are swept.
// (Codex review threads.)
func TestRepositoryMonitorPermissionMemoryIsEvicted(t *testing.T) {
	ctx := context.Background()
	f := newRepositoryMonitorFeedbackRepairFixture(t, nil)
	held := func(uid types.UID) bool {
		_, permissions := f.reconciler.permissionMemory.Load(uid)
		_, cursor := f.reconciler.feedbackVerifyCursors.Load(uid)
		return permissions || cursor
	}
	_, end := f.reconciler.withRepositoryMonitorPermissionPoll(ctx, f.monitor)
	end()
	f.reconciler.feedbackVerifyCursors.Store(f.monitor.UID, "")
	original := f.monitor.UID
	if !held(original) {
		t.Fatal("no memory held for the monitor")
	}
	// Recreated under the same name, the monitor has a new UID.
	recreated := f.monitor.DeepCopy()
	recreated.UID = "recreated-uid"
	_, end = f.reconciler.withRepositoryMonitorPermissionPoll(ctx, recreated)
	end()
	if held(original) || !held(recreated.UID) {
		t.Fatal("the replaced monitor's memory was not evicted")
	}
	// Deleted, its reconcile finds it gone.
	if err := f.reconciler.Delete(ctx, f.monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}}); err != nil {
		t.Fatal(err)
	}
	if held(recreated.UID) {
		t.Fatal("the deleted monitor's memory was not evicted")
	}
	// A memory no poll has used for the idle expiry is swept.
	idle := f.monitor.DeepCopy()
	idle.Name, idle.UID = "idle", "idle-uid"
	_, end = f.reconciler.withRepositoryMonitorPermissionPoll(ctx, idle)
	end()
	value, _ := f.reconciler.permissionMemory.Load(idle.UID)
	memory := value.(*repositoryMonitorPermissionMemory)
	memory.mu.Lock()
	memory.used = time.Now().Add(-repositoryMonitorPermissionMemoryIdleExpiry)
	memory.mu.Unlock()
	f.reconciler.permissionSweptAt = time.Time{}
	f.reconciler.sweepRepositoryMonitorPermissionMemories()
	if held(idle.UID) {
		t.Fatal("an idle memory was not swept")
	}
}
