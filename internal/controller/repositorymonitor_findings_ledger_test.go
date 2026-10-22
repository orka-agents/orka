package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ledgerFinding(id, priority, file string, line int64, title string) repositoryMonitorReviewFinding {
	return repositoryMonitorReviewFinding{ID: id, Priority: priority, File: file, Line: line, Title: title, Status: repositoryMonitorFindingStatusOpen, FirstHead: "old", FirstReview: "review-old"}
}

func TestRepositoryMonitorMergeFindingLedger(t *testing.T) {
	base := &store.ReviewRecord{ID: "review-old", HeadSHA: "old"}
	previous := []repositoryMonitorReviewFinding{
		ledgerFinding("f-fixed", "P1", "a.go", 10, "Nil dereference"),
		ledgerFinding("f-open", "P2", "b.go", 20, "Missing error check"),
		{ID: "f-history", Priority: "P1", File: "c.go", Line: 5, Title: "Old bug", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "older"},
	}
	review := &repositoryMonitorReviewResult{
		PreviousFindings: []repositoryMonitorReviewPreviousFinding{{ID: "f-fixed", Status: "fixed"}, {ID: "f-open", Status: "open"}, {ID: "f-unknown", Status: "fixed"}},
		Findings: []repositoryMonitorReviewFinding{
			// The reviewer cannot set Orka's record fields.
			{Priority: "P2", File: "b.go", Line: 31, Title: "Missing error-check!", ID: "f-forged", Status: repositoryMonitorFindingStatusFixed},
			{Priority: "P1", File: "d.go", Line: 7, Title: "Race", OutsideChanges: true},
			{Priority: "P2", File: "e.go", Line: 99, Title: "Unrelated nit"},
		},
	}
	changed := repositoryMonitorChangedLines{"d.go": {{1, 30}}, "e.go": {{1, 10}}}

	t.Run("incremental", func(t *testing.T) {
		ledger := repositoryMonitorMergeFindingLedger(repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeIncremental, Base: base, Ledger: previous}, review, "new", "review-new", changed)
		byTitle := map[string]repositoryMonitorReviewFinding{}
		for _, finding := range ledger {
			byTitle[finding.Title] = finding
		}
		if len(ledger) != 5 {
			t.Fatalf("ledger = %+v, want 3 earlier and 2 new findings (the repeat dropped)", ledger)
		}
		if got := byTitle["Nil dereference"]; got.Status != repositoryMonitorFindingStatusFixed || got.ResolvedHead != "new" {
			t.Fatalf("fixed finding = %+v", got)
		}
		if got := byTitle["Missing error check"]; got.Status != repositoryMonitorFindingStatusOpen || got.ResolvedHead != "" {
			t.Fatalf("still-open finding = %+v", got)
		}
		if got := byTitle["Old bug"]; got.ResolvedHead != "older" {
			t.Fatalf("history changed: %+v", got)
		}
		if _, ok := byTitle["Missing error-check!"]; ok {
			t.Fatal("a restatement of an open finding was recorded again")
		}
		race := byTitle["Race"]
		if race.ID == "" || race.ID == "f-forged" || race.Status != repositoryMonitorFindingStatusOpen || race.FirstHead != "new" || race.FirstReview != "review-new" || race.OutsideChanges {
			t.Fatalf("new finding = %+v, want Orka-assigned record fields inside the changes", race)
		}
		if nit := byTitle["Unrelated nit"]; !nit.OutsideChanges {
			t.Fatalf("finding outside the changed lines = %+v, want outsideChanges", nit)
		}
	})

	t.Run("same head fixes nothing", func(t *testing.T) {
		ledger := repositoryMonitorMergeFindingLedger(repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeSameHead, Base: base, Ledger: previous}, review, "old", "review-again", nil)
		for _, finding := range ledger {
			if finding.ID == "f-fixed" && finding.Status != repositoryMonitorFindingStatusOpen {
				t.Fatalf("same-head review resolved %+v", finding)
			}
			if finding.OutsideChanges {
				t.Fatalf("same-head finding marked outside: %+v", finding)
			}
		}
	})

	t.Run("full review starts over", func(t *testing.T) {
		ledger := repositoryMonitorMergeFindingLedger(repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeFull}, review, "rebased", "review-full", nil)
		if len(ledger) != 3 {
			t.Fatalf("full ledger = %+v, want only this review's findings", ledger)
		}
		for _, finding := range ledger {
			if finding.FirstHead != "rebased" || finding.Status != repositoryMonitorFindingStatusOpen {
				t.Fatalf("full finding = %+v", finding)
			}
		}
	})
}

func TestRepositoryMonitorDerivedReviewVerdict(t *testing.T) {
	open := func(priority string, outside bool) repositoryMonitorReviewFinding {
		return repositoryMonitorReviewFinding{Priority: priority, Status: repositoryMonitorFindingStatusOpen, OutsideChanges: outside}
	}
	for _, tc := range []struct {
		name     string
		blocking string
		reviewer string
		ledger   []repositoryMonitorReviewFinding
		want     string
	}{
		{name: "open P1 overrides passed", reviewer: repositoryMonitorReviewVerdictPassed, ledger: []repositoryMonitorReviewFinding{open("P1", false)}, want: repositoryMonitorReviewVerdictNeedsChanges},
		{name: "only P2 passes at the default", reviewer: repositoryMonitorReviewVerdictNeedsChanges, ledger: []repositoryMonitorReviewFinding{open("P2", false)}, want: repositoryMonitorReviewVerdictPassed},
		{name: "P2 blocks when configured", blocking: "P2", reviewer: repositoryMonitorReviewVerdictPassed, ledger: []repositoryMonitorReviewFinding{open("P2", false)}, want: repositoryMonitorReviewVerdictNeedsChanges},
		{name: "P1 outside the changes still blocks", reviewer: repositoryMonitorReviewVerdictPassed, ledger: []repositoryMonitorReviewFinding{open("P1", true)}, want: repositoryMonitorReviewVerdictNeedsChanges},
		{name: "P2 outside the changes never blocks", blocking: "P2", reviewer: repositoryMonitorReviewVerdictNeedsChanges, ledger: []repositoryMonitorReviewFinding{open("P2", true)}, want: repositoryMonitorReviewVerdictPassed},
		{name: "fixed findings do not block", reviewer: repositoryMonitorReviewVerdictNeedsChanges, ledger: []repositoryMonitorReviewFinding{{Priority: "P0", Status: repositoryMonitorFindingStatusFixed}}, want: repositoryMonitorReviewVerdictPassed},
		{name: "unknown priority counts as P1", reviewer: repositoryMonitorReviewVerdictPassed, ledger: []repositoryMonitorReviewFinding{open("urgent", false)}, want: repositoryMonitorReviewVerdictNeedsChanges},
		{name: "needs_human stands", reviewer: repositoryMonitorReviewVerdictNeedsHuman, want: repositoryMonitorReviewVerdictNeedsHuman},
		{name: "security_sensitive stands", reviewer: repositoryMonitorReviewVerdictSecuritySensitive, ledger: []repositoryMonitorReviewFinding{open("P3", false)}, want: repositoryMonitorReviewVerdictSecuritySensitive},
		{name: "skipped stands", reviewer: repositoryMonitorVerdictSkipped, want: repositoryMonitorVerdictSkipped},
	} {
		spec := corev1alpha1.RepositoryMonitorSpec{}
		spec.Review.BlockingPriority = tc.blocking
		if got := repositoryMonitorDerivedReviewVerdict(spec, tc.reviewer, tc.ledger); got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRepositoryMonitorChangedLinesFromFiles(t *testing.T) {
	changed := repositoryMonitorChangedLinesFromFiles([]repositoryMonitorPullRequestFileResponse{
		{Filename: "a.go", Patch: "@@ -1,3 +1,4 @@\n line\n+added\n@@ -40 +41 @@\n-old\n+new\n@@ -60,2 +62,0 @@\n-gone\n-gone"},
		{Filename: "image.png"},
	})
	for _, tc := range []struct {
		path string
		line int64
		want bool
	}{
		{"a.go", 1, true}, {"a.go", 4, true}, {"a.go", 5, false}, {"a.go", 41, true}, {"a.go", 62, false},
		{"image.png", 500, true}, {"b.go", 1, false}, {"a.go", 0, true},
	} {
		if got := changed.covers(tc.path, tc.line); got != tc.want {
			t.Errorf("covers(%s, %d) = %t, want %t", tc.path, tc.line, got, tc.want)
		}
	}
}

func TestRepositoryMonitorPlanReview(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("plan")
	compareStatus, capped := "ahead", false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path != "/repos/orka-agents/orka/compare/reviewed...newer" {
			http.NotFound(w, req)
			return
		}
		files := []map[string]any{
			{"filename": "pr.go", "patch": "@@ -1 +1 @@\n-a\n+b"},
			{"filename": "from-main.go", "patch": "@@ -1 +1 @@\n-a\n+b"},
		}
		for i := 0; capped && len(files) < repositoryMonitorGitHubCompareMaxFiles; i++ {
			files = append(files, map[string]any{"filename": fmt.Sprintf("merged-%d.go", i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": compareStatus, "files": files})
	}))
	defer server.Close()
	r := &RepositoryMonitorReconciler{Store: db, GitHubAPIBaseURL: server.URL}
	pr := repositoryMonitorPullRequest{Number: 7, HeadSHA: "reviewed"}
	plan := func(head string) repositoryMonitorReviewPlan {
		t.Helper()
		pr.HeadSHA = head
		got, err := r.planRepositoryMonitorReview(ctx, monitor, "orka-agents", "orka", "token", pr, map[string]struct{}{"pr.go": {}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := plan("reviewed"); got.Mode != repositoryMonitorReviewModeFull {
		t.Fatalf("plan without a review = %+v, want full", got)
	}
	record := func(id, head, verdict string) {
		t.Helper()
		if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 7, HeadSHA: head, Verdict: verdict, FindingsJSON: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	record("accepted", "reviewed", repositoryMonitorReviewVerdictNeedsChanges)
	record("rejected", "newer", repositoryMonitorReviewVerdictFailed)
	if got := plan("reviewed"); got.Mode != repositoryMonitorReviewModeSameHead || got.baseID() != "accepted" {
		t.Fatalf("same-head plan = %+v", got)
	}
	got := plan("newer")
	if got.Mode != repositoryMonitorReviewModeIncremental || got.baseID() != "accepted" || len(got.Delta) != 1 || got.Delta[0].Filename != "pr.go" {
		t.Fatalf("incremental plan = %+v, want the accepted base and only the pull request's own changes", got)
	}
	compareStatus = "diverged"
	if got := plan("newer"); got.Mode != repositoryMonitorReviewModeFull {
		t.Fatalf("rewritten history plan = %+v, want full", got)
	}
	compareStatus, capped = "ahead", true
	if got := plan("newer"); got.Mode != repositoryMonitorReviewModeFull {
		t.Fatalf("plan for a comparison at GitHub's file limit = %+v, want full", got)
	}
	capped = false
	if got := plan("unknown"); got.Mode != repositoryMonitorReviewModeFull {
		t.Fatalf("compare failure plan = %+v, want full", got)
	}
}

func TestRepositoryMonitorFollowUpReviewPrompt(t *testing.T) {
	monitor := repositoryMonitorInventoryTestMonitor()
	pr := repositoryMonitorPullRequest{Number: 7, BaseSHA: "base", HeadSHA: "newer"}
	reviewContext := newRepositoryMonitorReviewContext("orka-agents", "orka", pr)
	base := &store.ReviewRecord{ID: "accepted", HeadSHA: "reviewed"}
	ledger := []repositoryMonitorReviewFinding{
		ledgerFinding("f-open", "P1", "a.go", 3, "Open bug"),
		{ID: "f-done", Priority: "P1", Title: "Already fixed", Status: repositoryMonitorFindingStatusFixed},
	}
	incremental := buildRepositoryMonitorReviewPrompt(monitor, "orka-agents", "orka", pr, reviewContext, repositoryMonitorReviewPlan{
		Mode: repositoryMonitorReviewModeIncremental, Base: base, Ledger: ledger,
		Delta: []repositoryMonitorPullRequestFileResponse{{Filename: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"}},
	})
	for _, want := range []string{"Orka last reviewed this pull request at commit reviewed", `"id": "f-open"`, `"baseSHA": "reviewed"`, `"path": "a.go"`, `"previousFindings": [{"id": "f-...", "status": "fixed|open"}],`} {
		if !strings.Contains(incremental, want) {
			t.Fatalf("incremental prompt missing %q:\n%s", want, incremental)
		}
	}
	if strings.Contains(incremental, "f-done") {
		t.Fatal("incremental prompt lists a fixed finding")
	}
	sameHead := buildRepositoryMonitorReviewPrompt(monitor, "orka-agents", "orka", pr, reviewContext, repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeSameHead, Base: base, Ledger: ledger})
	if !strings.Contains(sameHead, "Orka already reviewed this exact head") || !strings.Contains(sameHead, `"id": "f-open"`) {
		t.Fatalf("same-head prompt:\n%s", sameHead)
	}
	full := buildRepositoryMonitorReviewPrompt(monitor, "orka-agents", "orka", pr, reviewContext, repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeFull})
	if strings.Contains(full, "previousFindings") || strings.Contains(full, "follow-up") {
		t.Fatalf("full prompt mentions follow-up review:\n%s", full)
	}
}

func TestRepositoryMonitorIngestsIncrementalReview(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "follow-up", Namespace: "default", UID: types.UID("uid-follow-up")}, Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: repositoryMonitorTestRepoURL, Branch: "main", Agents: corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}}}}
	baseLedger, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{ledgerFinding("f-bug", "P1", "internal/example.go", 42, "Bug")})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "review-old", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 91, HeadSHA: "old", Verdict: repositoryMonitorReviewVerdictNeedsChanges, FindingsJSON: baseLedger}); err != nil {
		t.Fatal(err)
	}
	ingest := func(taskName, head, mode, reviewerVerdict string) *store.ReviewRecord {
		t.Helper()
		task := repositoryMonitorReviewIngestTestTask(taskName, monitor.Name, 91, head)
		task.Annotations[repositoryMonitorAnnotationReviewMode] = mode
		task.Annotations[repositoryMonitorAnnotationReviewBase] = "review-old"
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, task).Build()
		r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db}
		result := repositoryMonitorReviewResultEnvelopeWith(t, 91, head, reviewerVerdict, func(payload map[string]any) {
			payload["findings"] = []map[string]any{}
			payload["previousFindings"] = []map[string]any{{"id": "f-bug", "status": "fixed"}}
		})
		if err := db.SaveResult(ctx, "default", taskName, result); err != nil {
			t.Fatal(err)
		}
		item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "91", Number: 91, State: repositoryMonitorItemStateOpen, HeadSHA: head, BaseBranch: "main", LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: taskName}
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
		return record
	}

	// Nothing changed on the same head, so the reviewer cannot close the
	// finding there, and Orka keeps needs_changes despite a passed verdict.
	again := ingest("review-same-head", "old", repositoryMonitorReviewModeSameHead, repositoryMonitorReviewVerdictPassed)
	if again.Verdict != repositoryMonitorReviewVerdictNeedsChanges || !again.Repairable || !strings.HasPrefix(again.Summary, "Orka set the verdict to needs_changes") {
		t.Fatalf("same-head record = verdict %q repairable %t summary %q", again.Verdict, again.Repairable, again.Summary)
	}
	followUp := ingest("review-follow-up", "new", repositoryMonitorReviewModeIncremental, repositoryMonitorReviewVerdictNeedsChanges)
	findings, err := repositoryMonitorReviewFindingsFromRecord(followUp)
	if err != nil || len(findings) != 1 || findings[0].Status != repositoryMonitorFindingStatusFixed || findings[0].ResolvedHead != "new" {
		t.Fatalf("follow-up findings = %+v err=%v, want the finding fixed at new", findings, err)
	}
	if followUp.Verdict != repositoryMonitorReviewVerdictPassed {
		t.Fatalf("follow-up verdict = %q, want passed once the only finding is fixed", followUp.Verdict)
	}
}

func TestRepositoryMonitorRendersFindingRecord(t *testing.T) {
	monitor := repositoryMonitorInventoryTestMonitor()
	record := &store.ReviewRecord{ID: "review-new", HeadSHA: "new1234567", Verdict: repositoryMonitorReviewVerdictNeedsChanges}
	findings := []repositoryMonitorReviewFinding{
		{ID: "f-carried", Priority: "P1", File: "a.go", Line: 1, Title: "Carried bug", Status: repositoryMonitorFindingStatusOpen, FirstHead: "old1234567"},
		{ID: "f-new", Priority: "P1", File: "a.go", Line: 2, Title: "New bug", Status: repositoryMonitorFindingStatusOpen, FirstHead: "new1234567"},
		{ID: "f-outside", Priority: "P3", File: "b.go", Line: 3, Title: "Old nit", Status: repositoryMonitorFindingStatusOpen, FirstHead: "new1234567", OutsideChanges: true},
		{ID: "f-fixed", Priority: "P1", File: "a.go", Line: 4, Title: "Fixed bug", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "new1234567"},
		{ID: "f-history", Priority: "P1", Title: "Fixed long ago", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "older"},
	}
	task := repositoryMonitorReviewIngestTestTask("review-task", monitor.Name, 1, "new1234567")
	item := &store.MonitorItem{Number: 1}
	body := renderRepositoryMonitorReviewBody(monitor, item, task, record, "publish-1", findings)
	for _, want := range []string{"Carried bug", "_(open since `old12345`)_", "New bug", "### Fixed in this commit\n\n- P1: **Fixed bug**", "### Outside the new changes (not blocking)", "Old nit"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Fixed long ago") {
		t.Fatalf("body lists a finding fixed at an earlier head:\n%s", body)
	}
	inline := repositoryMonitorNewInlineFindings(record, findings)
	if len(inline) != 1 || inline[0].ID != "f-new" {
		t.Fatalf("inline findings = %+v, want only the new one inside the changes", inline)
	}
	comment := renderRepositoryMonitorInlineFinding(record, inline[0])
	if !strings.Contains(comment, "finding=f-new -->") {
		t.Fatalf("inline comment lacks its finding marker: %q", comment)
	}
}

func TestRepositoryMonitorOpenTriggerReviewsOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		repairable bool
		wantTasks  int
	}{
		{name: "later head skipped"},
		{name: "repair label keeps reviewing", repairable: true, wantTasks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			labels := `[]`
			monitor, secret := repositoryMonitorInventoryTestObjects("open-trigger")
			monitor.Spec.Review.Trigger = repositoryMonitorReviewTriggerOpen
			if tc.repairable {
				labels = `[{"name":"orka:fix"}]`
				monitor.Spec.Repair.Enabled = true
				monitor.Spec.Repair.IncludeLabels = []string{"orka:fix"}
			}
			server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, fmt.Sprintf(`[
				{"number":1,"state":"open","base":{"ref":"main","sha":"base"},"head":{"ref":"feature","sha":"second","repo":{"full_name":"orka-agents/orka"}},"labels":%s}
			]`, labels))
			defer server.Close()
			if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "first-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "first", Verdict: repositoryMonitorReviewVerdictPassed, FindingsJSON: "[]"}); err != nil {
				t.Fatal(err)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL}
			if _, created, _, err := r.processPullRequestInventoryRun(ctx, monitor, &store.MonitorRun{ID: "poll", TargetKind: repositoryMonitorPullRequestKind}, "orka-agents", "orka"); err != nil || created != tc.wantTasks {
				t.Fatalf("created = %d err=%v, want %d review tasks", created, err, tc.wantTasks)
			}
			if tc.wantTasks == 0 {
				item, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
				if err != nil || item.SkipReason != repositoryMonitorSkipReasonTriggerOpen {
					t.Fatalf("item = %+v err=%v, want skipped by the open trigger", item, err)
				}
			}
		})
	}
}

func TestRepositoryMonitorExhaustiveReviewPasses(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("exhaustive")
	monitor.Spec.Review.Exhaustive = true
	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"state":"open","base":{"ref":"main","sha":"base"},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"orka-agents/orka"}}}
	]`)
	defer server.Close()
	firstTask := repositoryMonitorReviewIngestTestTask("pass-1", monitor.Name, 1, "sha1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(repositoryMonitorControllerObjects(monitor, secret), firstTask)...).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, GitHubAPIBaseURL: server.URL}
	record := func(id, task string, added bool) *store.ReviewRecord {
		t.Helper()
		finding := ledgerFinding("f-"+id, "P2", "a.go", 1, "Finding "+id)
		finding.FirstHead, finding.FirstReview = "sha1", "review-before"
		if added {
			finding.FirstReview = id
		}
		findingsJSON, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{finding})
		if err != nil {
			t.Fatal(err)
		}
		rec := &store.ReviewRecord{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "sha1", TaskName: task, TaskNamespace: monitor.Namespace, Verdict: repositoryMonitorReviewVerdictPassed, FindingsJSON: findingsJSON}
		if err := db.CreateReviewRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	pending := func(rec *store.ReviewRecord) bool {
		t.Helper()
		got, err := r.repositoryMonitorExhaustivePassPending(ctx, monitor, rec)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first := record("pass-1", firstTask.Name, true)
	if !pending(first) {
		t.Fatal("a first full pass that added a finding must be followed by another pass")
	}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "1", Number: 1, State: repositoryMonitorItemStateOpen, HeadSHA: "sha1", BaseBranch: "main"}
	if err := r.applyRepositoryMonitorReviewRecordToItem(ctx, monitor, item, first, ""); err != nil {
		t.Fatal(err)
	}
	if item.LastReviewedHeadSHA != "" {
		t.Fatalf("head counted as reviewed while a pass is due: %+v", item)
	}
	if fresh, err := r.repositoryMonitorReviewedHeadFresh(ctx, monitor, &store.MonitorItem{Number: 1, LastReviewedHeadSHA: "sha1"}, "sha1"); err != nil || fresh {
		t.Fatalf("fresh = %t err=%v while a pass is due", fresh, err)
	}
	if _, created, _, err := r.processPullRequestInventoryRun(ctx, monitor, &store.MonitorRun{ID: "poll", TargetKind: repositoryMonitorPullRequestKind}, "orka-agents", "orka"); err != nil || created != 1 {
		t.Fatalf("next pass not queued: created=%d err=%v", created, err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	queued := false
	for _, task := range tasks.Items {
		if task.Annotations[repositoryMonitorAnnotationReviewMode] == repositoryMonitorReviewModeSameHead && task.Annotations[repositoryMonitorAnnotationReviewBase] == "pass-1" {
			queued = true
		}
	}
	if !queued {
		t.Fatal("the next pass is not a same-head review built on the first pass")
	}
	monitor.Spec.Review.Publish.Enabled = true
	if err := r.publishRepositoryMonitorReview(ctx, monitor, item, firstTask, first); err != nil {
		t.Fatal(err)
	}
	publishes, _, err := db.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, ReviewRecordID: first.ID, Limit: 5})
	if err != nil || len(publishes) != 1 || publishes[0].SkipReason != repositoryMonitorPublishSkipExhaustivePassPending {
		t.Fatalf("intermediate pass publish records = %+v err=%v, want a deferred skip", publishes, err)
	}
	monitor.Spec.Review.Publish.Enabled = false

	if second := record("pass-2", "pass-2-task", false); pending(second) {
		t.Fatal("a pass that added nothing must end the review")
	}
	if pending(first) {
		t.Fatal("only the head's latest review decides whether a pass is due")
	}
	if third := record("pass-3", "pass-3-task", true); pending(third) {
		t.Fatalf("a %d-pass review must stop", repositoryMonitorExhaustiveMaxPasses)
	}
	monitor.Spec.Review.Exhaustive = false
	if pending(first) {
		t.Fatal("exhaustive review is off")
	}
}

func TestRepositoryMonitorExhaustiveReviewSkipsFollowUps(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, _ := repositoryMonitorInventoryTestObjects("exhaustive-follow-up")
	monitor.Spec.Review.Exhaustive = true
	task := repositoryMonitorReviewIngestTestTask("follow-up", monitor.Name, 1, "sha2")
	task.Annotations[repositoryMonitorAnnotationReviewMode] = repositoryMonitorReviewModeIncremental
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build(), Scheme: scheme, Store: db}
	findingsJSON, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{{ID: "f-new", Priority: "P2", FirstHead: "sha2", FirstReview: "follow-up-review", Status: repositoryMonitorFindingStatusOpen}})
	if err != nil {
		t.Fatal(err)
	}
	rec := &store.ReviewRecord{ID: "follow-up-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "sha2", TaskName: task.Name, TaskNamespace: monitor.Namespace, Verdict: repositoryMonitorReviewVerdictPassed, FindingsJSON: findingsJSON}
	if err := db.CreateReviewRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if pending, err := r.repositoryMonitorExhaustivePassPending(ctx, monitor, rec); err != nil || pending {
		t.Fatalf("pending = %t err=%v, want follow-up reviews to run once", pending, err)
	}
}

func TestRepositoryMonitorFindingRepeats(t *testing.T) {
	open := []repositoryMonitorReviewFinding{{Priority: "P2", File: "a.go", Line: 10, Title: "Error ignored"}}
	for _, tc := range []struct {
		name    string
		finding repositoryMonitorReviewFinding
		want    bool
	}{
		{name: "unrelated defect on a nearby line", finding: repositoryMonitorReviewFinding{Priority: "P2", File: "a.go", Line: 12, Title: "Unchecked error"}},
		{name: "same title after lines moved", finding: repositoryMonitorReviewFinding{Priority: "P3", File: "a.go", Line: 90, Title: "error ignored!"}, want: true},
		{name: "more severe nearby finding is new", finding: repositoryMonitorReviewFinding{Priority: "P1", File: "a.go", Line: 12, Title: "Nil dereference"}},
		{name: "more severe same title is new", finding: repositoryMonitorReviewFinding{Priority: "P0", File: "a.go", Line: 10, Title: "Error ignored"}},
		{name: "less severe nearby finding at another priority is new", finding: repositoryMonitorReviewFinding{Priority: "P3", File: "a.go", Line: 11, Title: "Naming"}},
		{name: "other file", finding: repositoryMonitorReviewFinding{Priority: "P2", File: "b.go", Line: 10, Title: "Error ignored"}},
	} {
		if got := repositoryMonitorFindingRepeats(tc.finding, open); got != tc.want {
			t.Errorf("%s: repeats = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestRepositoryMonitorLegacyFindingsGetIDs(t *testing.T) {
	legacy := &store.ReviewRecord{ID: "review-legacy", HeadSHA: "old", FindingsJSON: `[{"priority":"P1","file":"a.go","line":1,"title":"One"},{"priority":"P1","file":"b.go","line":2,"title":"Two"}]`}
	ledger, err := repositoryMonitorLedgerFromRecord(legacy)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := repositoryMonitorLedgerFromRecord(legacy)
	if ledger[0].ID == "" || ledger[0].ID == ledger[1].ID || ledger[0].ID != again[0].ID || ledger[0].FirstHead != "old" {
		t.Fatalf("legacy ledger = %+v, want distinct, deterministic IDs", ledger)
	}
	review := &repositoryMonitorReviewResult{PreviousFindings: []repositoryMonitorReviewPreviousFinding{{ID: ledger[0].ID, Status: "fixed"}, {ID: "", Status: "fixed"}}}
	merged := repositoryMonitorMergeFindingLedger(repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeIncremental, Base: legacy, Ledger: ledger}, review, "new", "review-new", nil)
	if merged[0].Status != repositoryMonitorFindingStatusFixed || merged[1].Status != repositoryMonitorFindingStatusOpen {
		t.Fatalf("merged = %+v, want only the named legacy finding fixed", merged)
	}
}

func TestRepositoryMonitorCarriedSensitiveFindingsStayRestricted(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "sensitive", Namespace: "default", UID: types.UID("uid-sensitive")}, Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: repositoryMonitorTestRepoURL, Branch: "main", Agents: corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}}}}
	monitor.Spec.Review.Publish.Enabled = true
	// The first pass was security-sensitive and was never published; a record
	// written before findings carried the flag must still be treated that way.
	if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "pass-1", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 5, HeadSHA: "h5", Verdict: repositoryMonitorReviewVerdictSecuritySensitive, SecurityStatus: repositoryMonitorReviewVerdictSecuritySensitive,
		FindingsJSON: `[{"priority":"P1","file":"auth.go","line":9,"title":"Token logged","body":"secret detail"}]`}); err != nil {
		t.Fatal(err)
	}
	task := repositoryMonitorReviewIngestTestTask("pass-2", monitor.Name, 5, "h5")
	task.Annotations[repositoryMonitorAnnotationReviewMode] = repositoryMonitorReviewModeSameHead
	task.Annotations[repositoryMonitorAnnotationReviewBase] = "pass-1"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, task).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db}
	if err := db.SaveResult(ctx, "default", task.Name, repositoryMonitorReviewResultEnvelope(t, 5, "h5", repositoryMonitorReviewVerdictPassed)); err != nil {
		t.Fatal(err)
	}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "5", Number: 5, State: repositoryMonitorItemStateOpen, HeadSHA: "h5", BaseBranch: "main", LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: task.Name}
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
	if record.Verdict != repositoryMonitorReviewVerdictSecuritySensitive || record.SecurityStatus != repositoryMonitorReviewVerdictSecuritySensitive {
		t.Fatalf("combined record = verdict %q security %q, want both security_sensitive", record.Verdict, record.SecurityStatus)
	}
	publishes, _, err := db.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: monitor.Name, ReviewRecordID: record.ID, Limit: 5})
	if err != nil || len(publishes) != 1 || publishes[0].SkipReason != repositoryMonitorPublishSkipSecuritySensitiveNotPublic {
		t.Fatalf("publish records = %+v err=%v, want a security-sensitive skip", publishes, err)
	}

	// Once fixed, the finding is still never named in a published review.
	body := renderRepositoryMonitorReviewBody(monitor, item, task, &store.ReviewRecord{ID: "later", HeadSHA: "h6", Verdict: repositoryMonitorReviewVerdictPassed},
		"publish-2", []repositoryMonitorReviewFinding{{ID: "f-s", Priority: "P1", Title: "Token logged", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "h6", SecuritySensitive: true}})
	if strings.Contains(body, "Token logged") {
		t.Fatalf("published body names a security-sensitive finding:\n%s", body)
	}
}

func TestRepositoryMonitorReviewShownSensitiveFindingStaysUnpublished(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "sensitive-fixed", Namespace: "default", UID: types.UID("uid-sensitive-fixed")}, Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: repositoryMonitorTestRepoURL, Branch: "main", Agents: corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}}}}
	if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "sensitive-base", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 6, HeadSHA: "h6", Verdict: repositoryMonitorReviewVerdictSecuritySensitive,
		FindingsJSON: `[{"id":"f-s","priority":"P1","file":"auth.go","line":9,"title":"Token logged","status":"open","securitySensitive":true}]`}); err != nil {
		t.Fatal(err)
	}
	task := repositoryMonitorReviewIngestTestTask("fixes-it", monitor.Name, 6, "h7")
	task.Annotations[repositoryMonitorAnnotationReviewMode] = repositoryMonitorReviewModeIncremental
	task.Annotations[repositoryMonitorAnnotationReviewBase] = "sensitive-base"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, task).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db}
	result := repositoryMonitorReviewResultEnvelopeWith(t, 6, "h7", repositoryMonitorReviewVerdictPassed, func(payload map[string]any) {
		payload["summary"] = "The token was being written to the debug log; now fixed."
		payload["previousFindings"] = []map[string]any{{"id": "f-s", "status": "fixed"}}
	})
	if err := db.SaveResult(ctx, "default", task.Name, result); err != nil {
		t.Fatal(err)
	}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "6", Number: 6, State: repositoryMonitorItemStateOpen, HeadSHA: "h7", BaseBranch: "main", LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: task.Name}
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
	// The fix counts, but the review that saw the finding is not published.
	if record.Verdict != repositoryMonitorReviewVerdictPassed || record.SecurityStatus != repositoryMonitorReviewVerdictSecuritySensitive {
		t.Fatalf("record = verdict %q security %q, want passed and still security_sensitive", record.Verdict, record.SecurityStatus)
	}
}
