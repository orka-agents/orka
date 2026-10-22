package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func repositoryMonitorRepairLabelsTestSpec() corev1alpha1.RepositoryMonitorSpec {
	spec := corev1alpha1.RepositoryMonitorSpec{Branch: "main"}
	spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	spec.Repair.IncludeLabels = []string{"orka:fix"}
	spec.Repair.CIOnlyLabels = []string{"orka:fix-ci"}
	return spec
}

func TestRepositoryMonitorPullRequestRepairLabelScope(t *testing.T) {
	tests := []struct {
		name                                   string
		mutate                                 func(*corev1alpha1.RepositoryMonitorSpec)
		labels                                 []string
		wantIncluded, wantReviewed             bool
		wantFull, wantCIOnly, wantLabelGranted bool
	}{
		{name: "no label scoping repairs every pull request", mutate: func(spec *corev1alpha1.RepositoryMonitorSpec) { *spec = corev1alpha1.RepositoryMonitorSpec{} }, wantIncluded: true, wantReviewed: true, wantFull: true},
		{name: "review label reviews without repair", labels: []string{"orka:review"}, wantIncluded: true, wantReviewed: true},
		{name: "fix label reviews and repairs", labels: []string{"Orka:Fix"}, wantIncluded: true, wantReviewed: true, wantFull: true, wantLabelGranted: true},
		{name: "ci-only label repairs CI without review", labels: []string{"orka:fix-ci"}, wantIncluded: true, wantCIOnly: true, wantLabelGranted: true},
		{name: "fix label wins over ci-only label", labels: []string{"orka:fix-ci", "orka:fix"}, wantIncluded: true, wantReviewed: true, wantFull: true, wantLabelGranted: true},
		{name: "unlabelled pull request is out of scope"},
		{
			name:   "ci-only label alongside full review scope",
			mutate: func(spec *corev1alpha1.RepositoryMonitorSpec) { spec.Targets.PullRequests.IncludeLabels = nil },
			labels: []string{"orka:fix-ci"}, wantIncluded: true, wantReviewed: true, wantCIOnly: true, wantLabelGranted: true,
		},
		{
			name:   "ci-only labels leave full repair to reviewed pull requests",
			mutate: func(spec *corev1alpha1.RepositoryMonitorSpec) { spec.Repair.IncludeLabels = nil },
			labels: []string{"orka:review"}, wantIncluded: true, wantReviewed: true, wantFull: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := repositoryMonitorRepairLabelsTestSpec()
			if tt.mutate != nil {
				tt.mutate(&spec)
			}
			pr := repositoryMonitorPullRequest{State: "open", BaseBranch: "main", Labels: tt.labels}
			full, ciOnly, labelled := repositoryMonitorPullRequestRepairScope(spec, pr)
			got := []bool{repositoryMonitorPullRequestIncluded(spec, pr), repositoryMonitorPullRequestReviewed(spec, pr), full, ciOnly, labelled}
			want := []bool{tt.wantIncluded, tt.wantReviewed, tt.wantFull, tt.wantCIOnly, tt.wantLabelGranted}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("included, reviewed, full, ciOnly, labelled = %v, want %v", got, want)
			}
			monitor := &corev1alpha1.RepositoryMonitor{Spec: spec}
			if repositoryMonitorPullRequestInScope(monitor, pr) != tt.wantReviewed {
				t.Fatalf("readiness scope = %t, want %t", !tt.wantReviewed, tt.wantReviewed)
			}
		})
	}
}

func TestRepositoryMonitorRepairLabelsRejectIssueImplementation(t *testing.T) {
	for field, mutate := range map[string]func(*corev1alpha1.RepositoryMonitorSpec){
		"spec.repair.includeLabels": func(spec *corev1alpha1.RepositoryMonitorSpec) { spec.Repair.IncludeLabels = []string{"orka:fix"} },
		"spec.repair.ciOnlyLabels":  func(spec *corev1alpha1.RepositoryMonitorSpec) { spec.Repair.CIOnlyLabels = []string{"orka:fix-ci"} },
	} {
		spec := corev1alpha1.RepositoryMonitorSpec{}
		mutate(&spec)
		if err := validateRepositoryMonitorSupportedTargets(spec); err != nil {
			t.Fatalf("%s without issue implementation: %v", field, err)
		}
		spec.Targets.Issues.Enabled = true
		spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
		if err := validateRepositoryMonitorSupportedTargets(spec); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("%s with issue implementation error = %v, want rejection", field, err)
		}
	}
}

// repositoryMonitorRepairLabelsGitHub serves per-commit check runs, the
// pull request's issue events, and collaborator roles.
type repositoryMonitorRepairLabelsGitHub struct {
	checks map[string]map[string]string // commit SHA -> check name -> conclusion
	events []map[string]any
	roles  map[string]string
	// heldRuns lists, per commit SHA, workflow runs waiting for approval.
	heldRuns map[string][]string
}

func (g *repositoryMonitorRepairLabelsGitHub) labeled(actor, label string) *repositoryMonitorRepairLabelsGitHub {
	g.events = append(g.events, map[string]any{"event": "labeled", "actor": map[string]any{"login": actor}, "label": map[string]any{"name": label}})
	return g
}

func (g *repositoryMonitorRepairLabelsGitHub) unlabeled(actor, label string) *repositoryMonitorRepairLabelsGitHub {
	g.events = append(g.events, map[string]any{"event": "unlabeled", "actor": map[string]any{"login": actor}, "label": map[string]any{"name": label}})
	return g
}

func (g *repositoryMonitorRepairLabelsGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := req.URL.Path
	switch {
	case strings.HasSuffix(path, "/check-runs"):
		sha := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/orka-agents/orka/commits/"), "/check-runs")
		runs := make([]map[string]any, 0, len(g.checks[sha]))
		for name, conclusion := range g.checks[sha] {
			runs = append(runs, map[string]any{"id": len(runs) + 1, "name": name, "status": "completed", "conclusion": conclusion})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
	case strings.HasSuffix(path, "/status"):
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
	case path == "/repos/orka-agents/orka/actions/runs":
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(g.heldRuns[req.URL.Query().Get("head_sha")])})
	case path == "/repos/orka-agents/orka/issues/31/events":
		if req.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode(g.events)
	case strings.HasPrefix(path, "/repos/orka-agents/orka/collaborators/"):
		login := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/orka-agents/orka/collaborators/"), "/permission")
		role, ok := g.roles[login]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"permission": "read", "role_name": role})
	default:
		http.Error(w, "unexpected request "+path, http.StatusInternalServerError)
	}
}

//nolint:gocyclo // One table covers every label, review, and CI combination the policy distinguishes.
func TestRepositoryMonitorAutomaticRepairFollowsRepairLabels(t *testing.T) {
	tests := []struct {
		name        string
		labels      []string
		mergeable   string
		github      *repositoryMonitorRepairLabelsGitHub
		review      string // verdict of a stored review for the head, if any
		mutate      func(*corev1alpha1.RepositoryMonitor)
		wantHandled bool
		wantIntent  string
		wantSkip    string
	}{
		{
			name:   "fix label waits for the head review",
			labels: []string{"orka:fix"},
			github: (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("maintainer", "orka:fix"),
		},
		{
			name:   "fix label fixes findings before CI",
			labels: []string{"orka:fix"},
			// A triage account's earlier label does not taint the current one.
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("triager", "orka:fix").unlabeled("triager", "orka:fix").labeled("maintainer", "orka:fix"),
			review:      repositoryMonitorReviewVerdictNeedsChanges,
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentFix,
		},
		{
			name:        "fix label fixes CI the pull request broke once the review is clean",
			labels:      []string{"orka:fix"},
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure", "lint": "success"}, "base31": {"tests": "success", "lint": "success"}}}).labeled("maintainer", "orka:fix"),
			review:      repositoryMonitorReviewVerdictPassed,
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentFixCI,
		},
		{
			name:   "CI already failing on the base commit is left alone",
			labels: []string{"orka:fix"},
			github: (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"Tests": "failure"}, "base31": {"tests": "failure"}}}).labeled("maintainer", "orka:fix"),
			review: repositoryMonitorReviewVerdictPassed,
		},
		{
			name:      "fix label reviews a conflicted branch before updating it",
			labels:    []string{"orka:fix"},
			mergeable: repositoryMonitorMergeableStateDirty,
			github:    (&repositoryMonitorRepairLabelsGitHub{}).labeled("maintainer", "orka:fix"),
		},
		{
			name:        "fix label updates a reviewed conflicted branch",
			labels:      []string{"orka:fix"},
			mergeable:   repositoryMonitorMergeableStateDirty,
			github:      (&repositoryMonitorRepairLabelsGitHub{}).labeled("maintainer", "orka:fix"),
			review:      repositoryMonitorReviewVerdictPassed,
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentUpdateBranch,
		},
		{
			name:        "ci-only label fixes CI without a review",
			labels:      []string{"orka:fix-ci"},
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("maintainer", "orka:fix-ci"),
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentFixCI,
		},
		{
			name:      "ci-only label ignores findings and conflicts",
			labels:    []string{"orka:fix-ci"},
			mergeable: repositoryMonitorMergeableStateDirty,
			github:    (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "success"}}}).labeled("maintainer", "orka:fix-ci"),
			review:    repositoryMonitorReviewVerdictNeedsChanges,
		},
		{
			name:   "review label alone never repairs",
			labels: []string{"orka:review"},
			github: &repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}},
			review: repositoryMonitorReviewVerdictPassed,
		},
		{
			name:        "label added by a triage account is refused",
			labels:      []string{"orka:fix-ci"},
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("maintainer", "orka:fix-ci").unlabeled("maintainer", "orka:fix-ci").labeled("triager", "orka:fix-ci"),
			wantHandled: true,
			wantSkip:    repositoryMonitorRepairSkipLabelActorNotPermitted,
		},
		{
			name:   "fix label ignores a review from before validation was configured",
			labels: []string{"orka:fix"},
			github: (&repositoryMonitorRepairLabelsGitHub{}).labeled("maintainer", "orka:fix"),
			review: repositoryMonitorReviewVerdictNeedsChanges,
			mutate: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.Validation.Image = "registry.example/validation@sha256:" + strings.Repeat("a", 64)
			},
		},
		{
			name:     "spent budget leaves an unreviewed head to review selection",
			github:   &repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}},
			mutate:   withoutRepositoryMonitorRepairLabelsAndBudget,
			wantSkip: repositoryMonitorRepairPRBudgetReason,
		},
		{
			name:        "spent budget on a reviewed head handles the pull request",
			github:      &repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}},
			review:      repositoryMonitorReviewVerdictPassed,
			mutate:      withoutRepositoryMonitorRepairLabelsAndBudget,
			wantHandled: true,
			wantSkip:    repositoryMonitorRepairPRBudgetReason,
		},
		{
			name:   "a failed check wins over a held run",
			labels: []string{"orka:fix-ci"},
			github: (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}},
				heldRuns: map[string][]string{"head31": {"CI"}}}).labeled("maintainer", "orka:fix-ci"),
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentFixCI,
		},
		{
			name:        "label added by a non-collaborator is refused",
			labels:      []string{"orka:fix-ci"},
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("github-actions[bot]", "orka:fix-ci"),
			wantHandled: true,
			wantSkip:    repositoryMonitorRepairSkipLabelActorNotPermitted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tt.github.roles = map[string]string{"maintainer": "write", "triager": "triage"}
			server := httptest.NewServer(tt.github)
			t.Cleanup(server.Close)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			monitor, secret := repositoryMonitorInventoryTestObjects("repair-labels")
			labelSpec := repositoryMonitorRepairLabelsTestSpec()
			monitor.Spec.Targets.PullRequests.IncludeLabels = labelSpec.Targets.PullRequests.IncludeLabels
			monitor.Spec.Repair.IncludeLabels = labelSpec.Repair.IncludeLabels
			monitor.Spec.Repair.CIOnlyLabels = labelSpec.Repair.CIOnlyLabels
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			configureRepositoryMonitorTestWriteCredentials(monitor)
			if tt.mutate != nil {
				tt.mutate(monitor)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			monitorStore := setupControllerSQLiteStore(t)
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}

			pr := repositoryMonitorPullRequest{
				Number: 31, State: "open", BaseBranch: "main", BaseSHA: "base31", HeadBranch: "feature", HeadRepo: "orka-agents/orka",
				HeadSHA: "head31", MergeableState: firstNonEmptyString(tt.mergeable, "clean"), Labels: tt.labels,
			}
			item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
			if tt.review != "" {
				record := &store.ReviewRecord{
					ID: "review-31", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind,
					Number: 31, HeadSHA: "head31", Verdict: tt.review, Repairable: true, ValidationStatus: repositoryMonitorValidationStatusNotRun,
				}
				if err := monitorStore.CreateReviewRecord(ctx, record); err != nil {
					t.Fatal(err)
				}
				item.LastReviewID, item.LastVerdict, item.LastReviewedHeadSHA = record.ID, record.Verdict, record.HeadSHA
			}
			run := &store.MonitorRun{ID: "poll", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "workflow", StartedAt: time.Now()}
			handled, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, run, "orka-agents", "orka", pr, item)
			if err != nil {
				t.Fatalf("tryRepositoryMonitorAutomaticRepair() error = %v", err)
			}
			if handled != tt.wantHandled {
				t.Fatalf("handled = %t, want %t", handled, tt.wantHandled)
			}
			commands, _, err := monitorStore.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantIntent == "" {
				if len(commands) != 0 {
					t.Fatalf("commands = %#v, want none", commands)
				}
			} else if len(commands) != 1 || commands[0].Intent != tt.wantIntent {
				t.Fatalf("commands = %#v, want one %s command", commands, tt.wantIntent)
			}
			if item.SkipReason != tt.wantSkip {
				t.Fatalf("item skip reason = %q, want %q", item.SkipReason, tt.wantSkip)
			}
		})
	}
}

// withoutRepositoryMonitorRepairLabelsAndBudget makes every pull request
// repairable and leaves no repair budget.
func withoutRepositoryMonitorRepairLabelsAndBudget(m *corev1alpha1.RepositoryMonitor) {
	m.Spec.Targets.PullRequests.IncludeLabels = nil
	m.Spec.Repair.IncludeLabels = nil
	m.Spec.Repair.CIOnlyLabels = nil
	m.Spec.Repair.MaxRepairsPerPR = new(int32(0))
}

func TestRepositoryMonitorInventorySkipsReviewForCIOnlyPullRequests(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"title":"Fix my build","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"dependabot"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"bump","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[{"name":"orka:fix-ci"}]},
		{"number":2,"title":"Review and fix me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature","sha":"sha2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[{"name":"orka:fix"}]}
	]`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("ci-only")
	labelSpec := repositoryMonitorRepairLabelsTestSpec()
	monitor.Spec.Targets.PullRequests.IncludeLabels = labelSpec.Targets.PullRequests.IncludeLabels
	monitor.Spec.Repair.IncludeLabels = labelSpec.Repair.IncludeLabels
	monitor.Spec.Repair.CIOnlyLabels = labelSpec.Repair.CIOnlyLabels
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-ci-only", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: "manual", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 || !strings.Contains(tasks.Items[0].Name, "-2-sha2-") {
		t.Fatalf("review tasks = %d, want one for the orka:fix pull request", len(tasks.Items))
	}
	ciOnly, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil || ciOnly.State != repositoryMonitorItemStateOpen || ciOnly.SkipReason != repositoryMonitorSkipReasonCIOnly {
		t.Fatalf("ci-only item = %#v err=%v, want an open item skipped as ci_only", ciOnly, err)
	}
}

func TestRepositoryMonitorInventoryReviewsHeadAfterRepairRefusal(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("refused-repair")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Repair.MaxRepairsPerPR = new(int32(0))
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"state":"open","base":{"ref":"main","sha":"base"},"head":{"ref":"first","sha":"head1","repo":{"full_name":"orka-agents/orka"}}}
	]`)
	defer server.Close()
	original := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/check-runs"):
			conclusion := "success"
			if strings.Contains(req.URL.Path, "/head1/") {
				conclusion = "failure"
			}
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":%q}]}`, conclusion)
		case strings.HasSuffix(req.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
		default:
			original.ServeHTTP(w, req)
		}
	})
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL}
	// The head broke CI but the repair budget is spent. The refusal must not
	// keep the head from ever being reviewed.
	if _, created, _, err := r.processPullRequestInventoryRun(ctx, monitor, &store.MonitorRun{ID: "poll", TargetKind: repositoryMonitorPullRequestKind}, "orka-agents", "orka"); err != nil || created != 1 {
		t.Fatalf("refused repair starved review: created=%d err=%v", created, err)
	}
	item, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil || item.LastVerdict != repositoryMonitorRunPhaseQueued || item.LastReviewID == "" || item.RepairState != repositoryMonitorRepairPhaseFailed {
		t.Fatalf("item = %+v err=%v, want a queued review with the refused repair recorded", item, err)
	}
}

func TestRepositoryMonitorCheckCIReportsHeldWorkflowRuns(t *testing.T) {
	ctx := context.Background()
	actionsStatus := http.StatusOK
	held := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":1,"name":"license/cla","status":"completed","conclusion":"success"}]}`))
		case strings.HasSuffix(req.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"success","total_count":0,"statuses":[]}`))
		case req.URL.Path == "/repos/orka-agents/orka/actions/runs":
			if req.URL.Query().Get("head_sha") != "head1" || req.URL.Query().Get("status") != "action_required" {
				t.Errorf("approval lookup query = %q", req.URL.RawQuery)
			}
			w.WriteHeader(actionsStatus)
			_, _ = fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[]}`, held)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	monitor, secret := repositoryMonitorInventoryTestObjects("held-ci")
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, GitHubAPIBaseURL: server.URL}
	for _, tc := range []struct {
		name   string
		status int
		held   int
		want   repositoryMonitorCIResult
	}{
		{name: "held run", status: http.StatusOK, held: 1, want: repositoryMonitorCIResult{reason: repositoryMonitorCIAwaitingApproval}},
		{name: "nothing held", status: http.StatusOK, held: 0, want: repositoryMonitorCIResult{passed: true}},
		{name: "token without Actions access", status: http.StatusForbidden, held: 1, want: repositoryMonitorCIResult{passed: true}},
	} {
		actionsStatus, held = tc.status, tc.held
		got, err := r.repositoryMonitorCheckCI(ctx, monitor, "head1")
		if err != nil || got.passed != tc.want.passed || got.reason != tc.want.reason {
			t.Fatalf("%s: CI = %+v err=%v, want %+v", tc.name, got, err, tc.want)
		}
	}
}
