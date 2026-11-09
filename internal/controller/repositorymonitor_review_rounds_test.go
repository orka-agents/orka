package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorReviewRoundsFor(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "rounds", Namespace: "default"}}
	r := &RepositoryMonitorReconciler{Store: db}
	created := time.Now().Add(-time.Hour)
	seed := func(id string, number int64, head, verdict string) {
		t.Helper()
		created = created.Add(time.Minute)
		if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: id, MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: number, HeadSHA: head, Verdict: verdict, FindingsJSON: "[]", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
	}
	seed("pr7-h0", 7, "h0", repositoryMonitorReviewVerdictPassed)
	seed("pr7-h1", 7, "h1", repositoryMonitorReviewVerdictNeedsChanges)
	seed("pr7-h1-again", 7, "h1", repositoryMonitorReviewVerdictNeedsHuman)
	seed("pr7-h2", 7, "h2", repositoryMonitorReviewVerdictFailed)
	seed("pr8-h1", 8, "h1", repositoryMonitorReviewVerdictNeedsChanges)
	// An exhaustive pass that passed, then a later pass of the same head
	// that did not: the head never held a pass.
	seed("pr10-h1", 10, "h1", repositoryMonitorReviewVerdictNeedsChanges)
	seed("pr10-h2-pass-1", 10, "h2", repositoryMonitorReviewVerdictPassed)
	seed("pr10-h2-pass-2", 10, "h2", repositoryMonitorReviewVerdictNeedsChanges)

	for _, tc := range []struct {
		name   string
		number int64
		head   string
		want   repositoryMonitorReviewRounds
	}{
		{"counts heads since the last pass and ignores failed reviews", 7, "h3", repositoryMonitorReviewRounds{SincePass: 2, PassedEarlier: true}},
		{"counts a re-reviewed head once", 7, "h1", repositoryMonitorReviewRounds{SincePass: 1, PassedEarlier: true}},
		{"counts from the first review without a pass", 8, "h2", repositoryMonitorReviewRounds{SincePass: 2}},
		{"a first review is one round", 9, "h1", repositoryMonitorReviewRounds{SincePass: 1}},
		{"a pass superseded on its head does not reset the count", 10, "h3", repositoryMonitorReviewRounds{SincePass: 3}},
		{"an earlier pass of the head under review does not end the count", 10, "h2", repositoryMonitorReviewRounds{SincePass: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.repositoryMonitorReviewRoundsFor(ctx, monitor, tc.number, tc.head)
			if err != nil || got != tc.want {
				t.Fatalf("rounds = %+v err=%v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestRepositoryMonitorReviewRoundsSkipUnfinishedExhaustivePasses(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		added bool
		want  repositoryMonitorReviewRounds
	}{
		// A first pass that added a finding was due another pass, but the
		// author pushed h3 first, so h2 never finished passing.
		{"an unfinished pass does not reset the count", true, repositoryMonitorReviewRounds{SincePass: 3}},
		{"a finished pass resets the count", false, repositoryMonitorReviewRounds{SincePass: 1, PassedEarlier: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "unfinished", Namespace: "default"}}
			monitor.Spec.Review.Exhaustive = true
			passTask := repositoryMonitorReviewIngestTestTask("h2-pass-1-task", monitor.Name, 6, "h2")
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(passTask).Build(), Scheme: scheme, Store: db}
			firstReview := "earlier-review"
			if tc.added {
				firstReview = "h2-pass-1"
			}
			findingsJSON, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{{ID: "f-p2", Priority: "P2", Status: repositoryMonitorFindingStatusOpen, FirstReview: firstReview}})
			if err != nil {
				t.Fatal(err)
			}
			created := time.Now().Add(-time.Hour)
			for _, rec := range []store.ReviewRecord{
				{ID: "h1-review", HeadSHA: "h1", Verdict: repositoryMonitorReviewVerdictNeedsChanges, FindingsJSON: "[]"},
				{ID: "h2-pass-1", HeadSHA: "h2", TaskName: passTask.Name, TaskNamespace: "default", Verdict: repositoryMonitorReviewVerdictPassed, FindingsJSON: findingsJSON},
			} {
				created = created.Add(time.Minute)
				rec.MonitorNamespace, rec.MonitorName, rec.Kind, rec.Number, rec.CreatedAt = "default", monitor.Name, repositoryMonitorPullRequestKind, 6, created
				if err := db.CreateReviewRecord(ctx, &rec); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.repositoryMonitorReviewRoundsFor(ctx, monitor, 6, "h3")
			if err != nil || got != tc.want {
				t.Fatalf("rounds = %+v err=%v, want %+v", got, err, tc.want)
			}
		})
	}
}

func TestRepositoryMonitorMaxRoundsEscalatesNeedsChanges(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		maxRounds *int32
		want      string
	}{
		{"unset keeps needs_changes", nil, repositoryMonitorReviewVerdictNeedsChanges},
		{"below the limit keeps needs_changes", new(int32(3)), repositoryMonitorReviewVerdictNeedsChanges},
		{"reaching the limit raises needs_human", new(int32(2)), repositoryMonitorReviewVerdictNeedsHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "max-rounds", Namespace: "default", UID: types.UID("uid-max-rounds")}, Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: repositoryMonitorTestRepoURL, Branch: "main", Agents: corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}}}}
			monitor.Spec.Review.MaxRounds = tc.maxRounds
			if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "review-h1", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 5, HeadSHA: "h1", Verdict: repositoryMonitorReviewVerdictNeedsChanges, FindingsJSON: "[]", CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			task := repositoryMonitorReviewIngestTestTask("review-h2", monitor.Name, 5, "h2")
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, task).Build()
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db}
			if err := db.SaveResult(ctx, "default", task.Name, repositoryMonitorReviewResultEnvelopeWith(t, 5, "h2", repositoryMonitorReviewVerdictNeedsChanges, nil)); err != nil {
				t.Fatal(err)
			}
			item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "5", Number: 5, State: repositoryMonitorItemStateOpen, HeadSHA: "h2", BaseBranch: "main", LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: task.Name}
			if err := db.UpsertMonitorItem(ctx, item); err != nil {
				t.Fatal(err)
			}
			if handled, err := r.ingestCompletedRepositoryMonitorReviewTask(ctx, monitor, item, task); err != nil || !handled {
				t.Fatalf("ingest = (%v, %v)", handled, err)
			}
			record, err := db.GetReviewRecord(ctx, "default", repositoryMonitorReviewRecordID(task))
			if err != nil {
				t.Fatal(err)
			}
			if record.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (summary %q)", record.Verdict, tc.want, record.Summary)
			}
			escalated := tc.want == repositoryMonitorReviewVerdictNeedsHuman
			if got := strings.HasPrefix(record.Summary, "Verdict raised from needs_changes to needs_human: 2 heads"); got != escalated {
				t.Fatalf("summary %q, escalation note present = %t, want %t", record.Summary, got, escalated)
			}
			findings, err := repositoryMonitorReviewFindingsFromRecord(record)
			if err != nil || len(repositoryMonitorOpenFindings(findings)) != 1 {
				t.Fatalf("findings = %+v err=%v, want the finding kept open", findings, err)
			}
			events, _, err := db.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			sawEvent := false
			for _, event := range events {
				if event.EventType == repositoryMonitorReviewEscalatedEvent {
					sawEvent = true
				}
			}
			if sawEvent != escalated {
				t.Fatalf("escalation event recorded = %t, want %t", sawEvent, escalated)
			}
		})
	}
}

func TestRecordRepositoryMonitorReviewMetrics(t *testing.T) {
	reviews := func(mode, verdict string) float64 {
		return metrics.CounterVecValue(metrics.RepositoryMonitorReviewsTotal, mode, verdict)
	}
	findings := func(priority, outcome string) float64 {
		return metrics.CounterVecValue(metrics.RepositoryMonitorReviewFindingsTotal, priority, outcome)
	}
	beforeReviews := reviews(repositoryMonitorReviewModeIncremental, repositoryMonitorReviewVerdictPassed)
	beforeReported := findings("P2", repositoryMonitorFindingOutcomeReported)
	beforeUnknown := findings("unknown", repositoryMonitorFindingOutcomeReported)
	beforeFixed := findings("P1", repositoryMonitorFindingOutcomeFixed)

	plan := repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeIncremental, Ledger: []repositoryMonitorReviewFinding{
		ledgerFinding("f-fixed", "P1", "a.go", 1, "Fixed"),
		{ID: "f-history", Priority: "P1", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "older"},
	}}
	fixed := plan.Ledger[0]
	fixed.Status, fixed.ResolvedHead = repositoryMonitorFindingStatusFixed, "new"
	ledger := []repositoryMonitorReviewFinding{
		fixed,
		plan.Ledger[1],
		{ID: "f-new", Priority: "P2", Status: repositoryMonitorFindingStatusOpen, FirstReview: "review-new"},
		{ID: "f-odd", Priority: "urgent", Status: repositoryMonitorFindingStatusOpen, FirstReview: "review-new"},
	}
	record := &store.ReviewRecord{ID: "review-new", Verdict: repositoryMonitorReviewVerdictPassed}

	recordRepositoryMonitorReviewMetrics(plan, ledger, record)
	if got := reviews(repositoryMonitorReviewModeIncremental, repositoryMonitorReviewVerdictPassed) - beforeReviews; got != 1 {
		t.Fatalf("reviews delta = %v, want 1", got)
	}
	if got := findings("P2", repositoryMonitorFindingOutcomeReported) - beforeReported; got != 1 {
		t.Fatalf("reported P2 delta = %v, want 1", got)
	}
	if got := findings("unknown", repositoryMonitorFindingOutcomeReported) - beforeUnknown; got != 1 {
		t.Fatalf("reported unknown-priority delta = %v, want 1", got)
	}
	if got := findings("P1", repositoryMonitorFindingOutcomeFixed) - beforeFixed; got != 1 {
		t.Fatalf("fixed P1 delta = %v, want 1 (a finding fixed earlier must not count again)", got)
	}
}

func TestRepositoryMonitorRoundsToPassRecordsEachPullRequestOnce(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "first-pass", Namespace: "default"}}
	pendingTask := repositoryMonitorReviewIngestTestTask("pass-1-task", monitor.Name, 4, "h2")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pendingTask).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db}
	passes := func() uint64 {
		t.Helper()
		var m dto.Metric
		if err := metrics.RepositoryMonitorReviewRoundsToPass.Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetHistogram().GetSampleCount()
	}
	firstPassEvents := func() int {
		t.Helper()
		events, _, err := db.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: monitor.Name, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: 4, EventType: repositoryMonitorReviewFirstPassedEvent, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	item := &store.MonitorItem{Number: 4}
	before := passes()

	r.recordRepositoryMonitorRoundsToPassBestEffort(ctx, monitor, item, &store.ReviewRecord{ID: "needs-changes", HeadSHA: "h1", Verdict: repositoryMonitorReviewVerdictNeedsChanges}, repositoryMonitorReviewRounds{SincePass: 1})
	if got := passes() - before; got != 0 {
		t.Fatalf("samples = %d after a needs_changes review, want 0", got)
	}

	// An exhaustive pass that added a finding is followed by another pass,
	// so the pull request has not passed yet.
	monitor.Spec.Review.Exhaustive = true
	findingsJSON, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{{ID: "f-p2", Priority: "P2", Status: repositoryMonitorFindingStatusOpen, FirstReview: "pass-1"}})
	if err != nil {
		t.Fatal(err)
	}
	pending := &store.ReviewRecord{ID: "pass-1", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 4, HeadSHA: "h2", TaskName: pendingTask.Name, TaskNamespace: "default", Verdict: repositoryMonitorReviewVerdictPassed, FindingsJSON: findingsJSON}
	if err := db.CreateReviewRecord(ctx, pending); err != nil {
		t.Fatal(err)
	}
	r.recordRepositoryMonitorRoundsToPassBestEffort(ctx, monitor, item, pending, repositoryMonitorReviewRounds{SincePass: 2})
	if got := passes() - before; got != 0 || firstPassEvents() != 0 {
		t.Fatalf("samples = %d events = %d for a pending exhaustive pass, want none", got, firstPassEvents())
	}
	monitor.Spec.Review.Exhaustive = false

	// A pass recorded before review_first_passed existed, which the round
	// count ended at, means this pass is not the pull request's first.
	r.recordRepositoryMonitorRoundsToPassBestEffort(ctx, monitor, &store.MonitorItem{Number: 40}, &store.ReviewRecord{ID: "after-upgrade", HeadSHA: "h9", Verdict: repositoryMonitorReviewVerdictPassed}, repositoryMonitorReviewRounds{SincePass: 1, PassedEarlier: true})
	if got := passes() - before; got != 0 {
		t.Fatalf("samples = %d after an earlier pass, want 0", got)
	}

	passed := &store.ReviewRecord{ID: "pass-2", HeadSHA: "h2", Verdict: repositoryMonitorReviewVerdictPassed}
	r.recordRepositoryMonitorRoundsToPassBestEffort(ctx, monitor, item, passed, repositoryMonitorReviewRounds{SincePass: 2})
	if got := passes() - before; got != 1 || firstPassEvents() != 1 {
		t.Fatalf("samples = %d events = %d after the first pass, want 1 and 1", got, firstPassEvents())
	}
	// A review command that passes the same head again, or a later head,
	// is not the pull request's first pass.
	r.recordRepositoryMonitorRoundsToPassBestEffort(ctx, monitor, item, &store.ReviewRecord{ID: "pass-3", HeadSHA: "h2", Verdict: repositoryMonitorReviewVerdictPassed}, repositoryMonitorReviewRounds{SincePass: 1})
	r.recordRepositoryMonitorRoundsToPassBestEffort(ctx, monitor, item, &store.ReviewRecord{ID: "pass-4", HeadSHA: "h3", Verdict: repositoryMonitorReviewVerdictPassed}, repositoryMonitorReviewRounds{SincePass: 1})
	if got := passes() - before; got != 1 || firstPassEvents() != 1 {
		t.Fatalf("samples = %d events = %d after repeat passes, want 1 and 1", got, firstPassEvents())
	}
}

func TestRepositoryMonitorReviewMetricLabelsAreBounded(t *testing.T) {
	for mode, want := range map[string]string{
		"":                              repositoryMonitorReviewModeFull,
		repositoryMonitorReviewModeFull: repositoryMonitorReviewModeFull,
		" " + repositoryMonitorReviewModeIncremental: repositoryMonitorReviewModeIncremental,
		repositoryMonitorReviewModeSameHead:          repositoryMonitorReviewModeSameHead,
		"edited-annotation":                          "",
	} {
		if got := repositoryMonitorReviewMetricMode(mode); got != want {
			t.Errorf("mode %q -> %q, want %q", mode, got, want)
		}
	}
	for priority, want := range map[string]string{"p1": "P1", "P3": "P3", "urgent": "", "": ""} {
		if got := repositoryMonitorFindingMetricPriority(priority); got != want {
			t.Errorf("priority %q -> %q, want %q", priority, got, want)
		}
	}
}

func TestRepositoryMonitorRepairPromptAsksForEveryInstanceOnlyWhenFixingFindings(t *testing.T) {
	r := &RepositoryMonitorReconciler{}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "prompt", Namespace: "default"}}
	pr := repositoryMonitorPullRequest{Number: 3, HeadSHA: "h1"}
	for intent, want := range map[string]bool{
		repositoryMonitorCommandIntentFix:          true,
		repositoryMonitorCommandIntentFixCI:        false,
		repositoryMonitorCommandIntentUpdateBranch: false,
	} {
		prompt, err := r.buildRepositoryMonitorRepairPrompt(context.Background(), monitor, intent, "orka-agents/orka", pr, &store.MonitorItem{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(prompt, "fix every instance"); got != want {
			t.Errorf("intent %q: asks to fix every instance = %t, want %t", intent, got, want)
		}
	}
}
