package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// repositoryMonitorFeedbackLifecycle is one admitted feedback repair of the
// fixture's pull request, addressing review_comment:11 once.
type repositoryMonitorFeedbackLifecycle struct {
	f       *repositoryMonitorFeedbackRepairFixture
	command store.CommandEvent
	pr      repositoryMonitorPullRequest
	item    *store.MonitorItem
	// lostCreate makes the next Task create succeed but report a failure,
	// as when the controller stops right after creating the Task.
	lostCreate bool
}

func newRepositoryMonitorFeedbackLifecycle(t *testing.T) *repositoryMonitorFeedbackLifecycle {
	t.Helper()
	f := newRepositoryMonitorFeedbackRepairFixture(t, func(monitor *corev1alpha1.RepositoryMonitor) {
		monitor.Spec.Repair.HumanFeedback.MaxAttemptsPerComment = new(int32(1))
	})
	f.github.reviewPages = nil
	l := &repositoryMonitorFeedbackLifecycle{f: f, pr: repositoryMonitorFeedbackRepairTestPR()}
	withWatch := f.reconciler.Client.(client.WithWatch)
	f.reconciler.Client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if _, task := obj.(*corev1alpha1.Task); task && l.lostCreate {
				l.lostCreate = false
				return errors.New("controller stopped after creating the Task")
			}
			return nil
		},
	})
	l.item = repositoryMonitorItemFromPullRequest(f.monitor, l.pr, nil)
	if started, err := f.reconciler.tryRepositoryMonitorFeedbackRepair(context.Background(), f.monitor, &store.MonitorRun{ID: "poll"}, "orka-agents", "orka", l.pr, l.item, false); err != nil || !started {
		t.Fatalf("admission started=%t err=%v", started, err)
	}
	l.command = f.feedbackCommands(t)[0]
	return l
}

// handoff runs the command once, as a restarted controller would.
func (l *repositoryMonitorFeedbackLifecycle) handoff(t *testing.T) error {
	t.Helper()
	_, _, err := l.f.reconciler.tryProcessPullRequestCommandRun(context.Background(), l.f.monitor, l.f.commandRun(l.command), "orka-agents", "orka", l.pr, l.item)
	return err
}

func (l *repositoryMonitorFeedbackLifecycle) mustHandoff(t *testing.T) {
	t.Helper()
	if err := l.handoff(t); err != nil {
		t.Fatalf("handoff: %v", err)
	}
}

func (l *repositoryMonitorFeedbackLifecycle) outcome(t *testing.T) *store.MonitorFeedbackOutcome {
	t.Helper()
	outcome, err := l.f.store.GetMonitorFeedbackOutcome(context.Background(), "default", l.command.ID)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

// endTask ends the repair Task in phase, with delivery, and ingests it.
func (l *repositoryMonitorFeedbackLifecycle) endTask(t *testing.T, phase corev1alpha1.TaskPhase, delivery *corev1alpha1.TaskDeliveryStatus) {
	t.Helper()
	ctx := context.Background()
	task := l.f.repairTasks(t)[0]
	task.Status.Phase, task.Status.Delivery = phase, delivery
	if err := l.f.reconciler.Status().Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := l.f.reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, l.f.monitor); err != nil {
		t.Fatal(err)
	}
}

func (l *repositoryMonitorFeedbackLifecycle) resolveFeedback() {
	l.f.github.threadPages = []string{strings.Replace(l.f.github.threadPages[0], `"isResolved":false`, `"isResolved":true`, 1)}
}

// endCommand ends the command with a terminal action of status, as a
// cancellation or exhausted retries do, and settles the command.
func (l *repositoryMonitorFeedbackLifecycle) endCommand(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	if err := l.f.reconciler.recordRepositoryMonitorWorkActionState(ctx, l.f.monitor, nil, &l.command, repositoryMonitorPullRequestKind, 1, "head1111", "", repositoryMonitorRepairWorkflowActionKind(l.command.Intent), status, status, "", "stopped"); err != nil {
		t.Fatal(err)
	}
	if terminal, err := l.f.reconciler.repositoryMonitorCommandWorkActionTerminal(ctx, l.f.monitor, l.command); err != nil || !terminal {
		t.Fatalf("command terminal = %t err=%v", terminal, err)
	}
}

// The feedback outcome walks reserved → started → one terminal state, and
// each step, a restart in each state, and the Task's deletion leave the
// charge, the Task, and the head's repair state consistent.
func TestRepositoryMonitorReviewFeedbackOutcomeLifecycle(t *testing.T) {
	noChange := &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNoChange, Outcome: corev1alpha1.TaskDeliveryOutcomeNoChange, StartingSHA: "head1111"}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, l *repositoryMonitorFeedbackLifecycle)
		// want is the final outcome state; targets whether it started.
		want        string
		started     bool
		tasks       int
		repairState string
	}{
		{
			name: "reserved, restarted",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.f.github.failThreadPage = 1
				for range 2 {
					if err := l.handoff(t); !errors.Is(err, errRepositoryMonitorReviewFeedbackUnavailable) {
						t.Fatalf("unreadable feedback err=%v, want a deferral", err)
					}
				}
			},
			want: store.MonitorFeedbackOutcomeReserved, repairState: "",
		},
		{
			name: "withdrawn before its Task, restarted",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.resolveFeedback()
				l.mustHandoff(t)
				// Reopened feedback does not revive a withdrawn repair.
				l.f.github.threadPages = []string{"[" + repositoryMonitorFeedbackTestThreadJSON("T1", 11, "writer", "Please handle the nil case.") + "]"}
				l.mustHandoff(t)
			},
			want: store.MonitorFeedbackOutcomeWithdrawn, repairState: "",
		},
		{
			name: "withdrawn for a person when the feedback is truncated",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.f.github.threadPages = slices.Repeat([]string{"[" + repositoryMonitorFeedbackTestThreadJSON("T1", 11, "writer", "x") + "]"}, repositoryMonitorFeedbackThreadPages+1)
				l.mustHandoff(t)
				if reason := l.outcome(t).Reason; reason != repositoryMonitorRepairSkipFeedbackNeedsPerson {
					t.Fatalf("reason = %q", reason)
				}
			},
			want: store.MonitorFeedbackOutcomeWithdrawn, repairState: "",
		},
		{
			name: "refused before its Task, restarted",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.f.monitor.Spec.Repair.MaxRepairsPerPR = new(int32(0))
				l.mustHandoff(t)
				l.f.monitor.Spec.Repair.MaxRepairsPerPR = nil
				l.mustHandoff(t)
			},
			want: store.MonitorFeedbackOutcomeFailed, repairState: repositoryMonitorRepairPhaseFailed,
		},
		{
			name: "ended by a cancellation before its Task",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.endCommand(t, repositoryMonitorWorkActionStatusCancelled)
			},
			want: store.MonitorFeedbackOutcomeFailed, repairState: "",
		},
		{
			name: "started, restarted",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.mustHandoff(t)
				l.resolveFeedback()
				l.mustHandoff(t)
			},
			want: store.MonitorFeedbackOutcomeStarted, started: true, tasks: 1, repairState: repositoryMonitorRepairPhaseQueued,
		},
		{
			name: "restarted between creating the Task and starting",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.lostCreate = true
				if err := l.handoff(t); err == nil {
					t.Fatal("handoff succeeded despite the lost create")
				}
				if state := l.outcome(t).State; state != store.MonitorFeedbackOutcomeReserved {
					t.Fatalf("state after the lost create = %q", state)
				}
				l.mustHandoff(t)
			},
			want: store.MonitorFeedbackOutcomeStarted, started: true, tasks: 1, repairState: repositoryMonitorRepairPhaseQueued,
		},
		{
			name: "Task ended before the restarted controller started it",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.lostCreate = true
				_ = l.handoff(t)
				l.endTask(t, corev1alpha1.TaskPhaseFailed, nil)
			},
			want: store.MonitorFeedbackOutcomeFailed, started: true, tasks: 1, repairState: repositoryMonitorRepairPhaseFailed,
		},
		{
			name: "Task failed",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.mustHandoff(t)
				l.endTask(t, corev1alpha1.TaskPhaseFailed, nil)
			},
			want: store.MonitorFeedbackOutcomeFailed, started: true, tasks: 1, repairState: repositoryMonitorRepairPhaseFailed,
		},
		{
			name: "Task made no change",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.mustHandoff(t)
				l.endTask(t, corev1alpha1.TaskPhaseSucceeded, noChange)
				// Ingesting the same Task again records the same end.
				l.endTask(t, corev1alpha1.TaskPhaseSucceeded, noChange)
			},
			want: store.MonitorFeedbackOutcomeNoChange, started: true, tasks: 1, repairState: repositoryMonitorRepairPhaseSucceeded,
		},
		{
			name: "Task deleted before its result was recorded, restarted",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.mustHandoff(t)
				task := l.f.repairTasks(t)[0]
				if err := l.f.reconciler.Delete(context.Background(), &task); err != nil {
					t.Fatal(err)
				}
				// The repair ran, so it is not run again.
				l.mustHandoff(t)
				l.mustHandoff(t)
				if reason := l.outcome(t).Reason; reason != repositoryMonitorRepairTaskMissing {
					t.Fatalf("reason = %q", reason)
				}
			},
			want: store.MonitorFeedbackOutcomeFailed, started: true, tasks: 0, repairState: repositoryMonitorRepairPhaseFailed,
		},
		{
			name: "ended by a cancellation after its Task started",
			run: func(t *testing.T, l *repositoryMonitorFeedbackLifecycle) {
				l.mustHandoff(t)
				l.endCommand(t, repositoryMonitorWorkActionStatusCancelled)
			},
			want: store.MonitorFeedbackOutcomeFailed, started: true, tasks: 1, repairState: repositoryMonitorRepairPhaseQueued,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			l := newRepositoryMonitorFeedbackLifecycle(t)
			tc.run(t, l)
			outcome := l.outcome(t)
			if outcome.State != tc.want {
				t.Fatalf("outcome = %+v, want state %s", outcome, tc.want)
			}
			if started := outcome.StartedAt != nil; started != tc.started || started != slices.Equal(outcome.Targets, []string{"review_comment:11"}) {
				t.Fatalf("outcome = %+v, want started=%t with the frozen target", outcome, tc.started)
			}
			if got := len(l.f.repairTasks(t)); got != tc.tasks {
				t.Fatalf("tasks = %d, want %d", got, tc.tasks)
			}
			// The charge follows the outcome alone: reserved or started
			// repairs charge the comment, others only if their Task started.
			wantCharge := 0
			if outcome.State == store.MonitorFeedbackOutcomeReserved || tc.started {
				wantCharge = 1
			}
			if ledger := l.f.ledger(t); ledger.spent["review_comment:11"] != wantCharge || ledger.recorded["review_comment:11"] != 1 {
				t.Fatalf("ledger = %+v, want charge %d", ledger, wantCharge)
			}
			state, err := l.f.reconciler.repositoryMonitorRepairStateForHead(ctx, l.f.monitor, 1, "head1111")
			if err != nil || state != tc.repairState {
				t.Fatalf("repair state = %q err=%v, want %q", state, err, tc.repairState)
			}
			// Every repair past reserved counts against the pull request.
			_, prCount, _, err := l.f.reconciler.repositoryMonitorRepairPolicy(ctx, l.f.monitor, "orka-agents/orka", l.pr, "", repositoryMonitorCommandIntentFix)
			if wantCount := map[bool]int{true: 0, false: 1}[outcome.State == store.MonitorFeedbackOutcomeReserved]; err != nil || prCount != wantCount {
				t.Fatalf("pull request repair count = %d err=%v, want %d", prCount, err, wantCount)
			}
		})
	}
}
