package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
			name: "ci-only label narrows a pull request every review would repair",
			mutate: func(spec *corev1alpha1.RepositoryMonitorSpec) {
				spec.Targets.PullRequests.IncludeLabels, spec.Repair.IncludeLabels = nil, nil
			},
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

// repositoryMonitorRepairLabelsGitHub serves per-commit check runs and
// commit statuses, the pull request's issue events, and collaborator roles.
type repositoryMonitorRepairLabelsGitHub struct {
	checks   map[string]map[string]string // commit SHA -> check name -> conclusion
	statuses map[string]map[string]string // commit SHA -> status context -> state
	events   []map[string]any
	roles    map[string]string
	// heldRuns lists, per commit SHA, workflow runs waiting for approval.
	heldRuns map[string][]string
	// partial reports one more check run than it returns for these commits.
	partial map[string]bool
	// checkReads counts check-run reads per commit SHA.
	checkReads map[string]int
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
		if g.checkReads == nil {
			g.checkReads = map[string]int{}
		}
		g.checkReads[sha]++
		runs := make([]map[string]any, 0, len(g.checks[sha]))
		total := len(g.checks[sha])
		if page := req.URL.Query().Get("page"); page == "" || page == "1" {
			for name, conclusion := range g.checks[sha] {
				runs = append(runs, map[string]any{"id": len(runs) + 1, "name": name, "status": "completed", "conclusion": conclusion})
			}
		}
		if g.partial[sha] {
			total++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "check_runs": runs})
	case strings.HasSuffix(path, "/status"):
		sha := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/orka-agents/orka/commits/"), "/status")
		statuses := make([]map[string]any, 0, len(g.statuses[sha]))
		for name, state := range g.statuses[sha] {
			statuses = append(statuses, map[string]any{"id": len(statuses) + 1, "context": name, "state": state})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(statuses), "statuses": statuses})
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
		greenCI     bool // spec.review.requireGreenCI
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
			name:        "fix label fixes CI first when reviews wait for green CI",
			labels:      []string{"orka:fix"},
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("maintainer", "orka:fix"),
			greenCI:     true,
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentFixCI,
		},
		{
			name:      "fix label under green-CI reviews leaves an unreviewed conflict alone",
			labels:    []string{"orka:fix"},
			mergeable: repositoryMonitorMergeableStateDirty,
			github:    (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "success"}}}).labeled("maintainer", "orka:fix"),
			greenCI:   true,
		},
		{
			name:   "a commit status the pull request broke counts beside a shared failing check run",
			labels: []string{"orka:fix-ci"},
			github: (&repositoryMonitorRepairLabelsGitHub{
				checks:   map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "failure"}},
				statuses: map[string]map[string]string{"head31": {"jenkins": "failure"}},
			}).labeled("maintainer", "orka:fix-ci"),
			wantHandled: true,
			wantIntent:  repositoryMonitorCommandIntentFixCI,
		},
		{
			name:   "a status failing only on the head is not hidden by a check run of the same name",
			labels: []string{"orka:fix-ci"},
			github: (&repositoryMonitorRepairLabelsGitHub{
				checks:   map[string]map[string]string{"head31": {"tests": "success"}, "base31": {"tests": "failure"}},
				statuses: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}},
			}).labeled("maintainer", "orka:fix-ci"),
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
			name:        "a label removed after the snapshot revokes its labeller",
			labels:      []string{"orka:fix-ci"},
			github:      (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("maintainer", "orka:fix-ci").unlabeled("maintainer", "orka:fix-ci"),
			wantHandled: true,
			wantSkip:    repositoryMonitorRepairSkipLabelActorNotPermitted,
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
			monitor.Spec.Review.RequireGreenCI = tt.greenCI
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
			if tt.wantSkip != "" {
				// The refusal is kept as one event even when it repeats.
				if _, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, run, "orka-agents", "orka", pr, item); err != nil {
					t.Fatal(err)
				}
				events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, EventType: repositoryMonitorEventRepairRefused, Limit: 5})
				if err != nil || len(events) != 1 || !strings.Contains(events[0].Summary, tt.wantSkip) {
					t.Fatalf("refusal events = %+v err=%v, want one naming %s", events, err, tt.wantSkip)
				}
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
	actionsRateLimited := false
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
			if actionsRateLimited {
				_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation"}`))
				return
			}
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
	// A rate-limited lookup cannot establish that nothing is held.
	actionsStatus, actionsRateLimited = http.StatusForbidden, true
	if got, err := r.repositoryMonitorCheckCI(ctx, monitor, "head1"); err == nil {
		t.Fatalf("rate-limited approval lookup: CI = %+v, want an error", got)
	}
}

func TestRepositoryMonitorCIRepairPromptNamesTheChecksThePullRequestBroke(t *testing.T) {
	github := &repositoryMonitorRepairLabelsGitHub{
		checks:   map[string]map[string]string{"head31": {"tests": "failure", "lint": "failure"}, "base31": {"tests": "failure", "lint": "success"}},
		statuses: map[string]map[string]string{"head31": {"jenkins": "failure"}},
	}
	server := httptest.NewServer(github)
	t.Cleanup(server.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("ci-prompt")
	reconciler := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), GitHubAPIBaseURL: server.URL}
	pr := repositoryMonitorPullRequest{Number: 31, BaseSHA: "base31", HeadSHA: "head31"}
	prompt, err := reconciler.buildRepositoryMonitorRepairPrompt(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{})
	if err != nil {
		t.Fatal(err)
	}
	_, input, ok := strings.Cut(prompt, "Input:\n")
	if !ok {
		t.Fatalf("prompt has no input: %s", prompt)
	}
	var payload struct {
		BaseSHA           string   `json:"baseSHA"`
		FailingChecks     []string `json:"failingChecks"`
		BaseFailingChecks []string `json:"baseFailingChecks"`
	}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatal(err)
	}
	slices.Sort(payload.FailingChecks)
	if payload.BaseSHA != "base31" || fmt.Sprint(payload.FailingChecks) != "[jenkins lint]" || fmt.Sprint(payload.BaseFailingChecks) != "[tests]" {
		t.Fatalf("CI repair input = %+v, want base31, [jenkins lint], [tests]", payload)
	}
	if !strings.Contains(prompt, "Fix only the checks in failingChecks") {
		t.Fatalf("prompt does not limit the repair to failingChecks: %s", prompt)
	}
}

func TestRepositoryMonitorReadinessRepairEligibilityFollowsRepairLabels(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{Spec: repositoryMonitorRepairLabelsTestSpec()}
	monitor.Spec.Repair.Enabled = true
	for _, labels := range [][]string{{"orka:review"}, {"orka:review", "orka:fix-ci"}} {
		pr := repositoryMonitorPullRequest{Number: 31, State: "open", BaseBranch: "main", HeadSHA: "head31", Labels: labels}
		// No repair is selected, so eligibility is decided before any lookup.
		eligible, err := (&RepositoryMonitorReconciler{}).repositoryMonitorNeedsChangesRepairEligible(context.Background(), monitor, pr, &store.MonitorItem{})
		if err != nil || eligible {
			t.Fatalf("labels %v: eligible = %t err=%v, want no automatic repair of findings", labels, eligible, err)
		}
	}
}

func TestRepositoryMonitorReadinessNamesHeldCIForAnUnreviewedHead(t *testing.T) {
	ctx := context.Background()
	held := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":1,"name":"license/cla","status":"completed","conclusion":"success"}]}`))
		case strings.HasSuffix(req.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"success","total_count":0,"statuses":[]}`))
		case req.URL.Path == "/repos/orka-agents/orka/actions/runs":
			_, _ = fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[]}`, held)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	monitor, secret := repositoryMonitorInventoryTestObjects("held-ci-readiness")
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: setupControllerSQLiteStore(t), GitHubAPIBaseURL: server.URL}
	pr := repositoryMonitorPullRequest{Number: 1, State: "open", BaseBranch: "main", HeadSHA: "head1", MergeableState: "clean"}
	item := &store.MonitorItem{Number: 1, HeadSHA: "head1"}
	for _, tc := range []struct {
		name           string
		requireGreenCI bool
		held           int
		want           string
	}{
		{name: "held runs gate the review", requireGreenCI: true, held: 1, want: repositoryMonitorReadinessCIAwaitingApproval},
		{name: "nothing held", requireGreenCI: true, held: 0, want: "Waiting for a clean review of this commit."},
		{name: "review not gated on CI", held: 1, want: "Waiting for a clean review of this commit."},
	} {
		monitor.Spec.Review.RequireGreenCI, held = tc.requireGreenCI, tc.held
		state, description, err := r.repositoryMonitorReadyOutcome(ctx, monitor, pr, item)
		if err != nil || state != repositoryMonitorStatusPending || description != tc.want {
			t.Fatalf("%s: readiness = %s %q err=%v, want pending %q", tc.name, state, description, err, tc.want)
		}
	}
}

func TestRepositoryMonitorQueuedPolicyRepairRechecksItsGrantBeforeRunning(t *testing.T) {
	failingHead := func() *repositoryMonitorRepairLabelsGitHub {
		return (&repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head31": {"tests": "failure"}, "base31": {"tests": "success"}}}).labeled("maintainer", "orka:fix-ci")
	}
	tests := []struct {
		name     string
		change   func(github *repositoryMonitorRepairLabelsGitHub, pr *repositoryMonitorPullRequest)
		jobOnly  bool // a RepairJob survives an earlier failed Task create
		wantSkip string
		wantErr  bool
	}{
		{name: "an unchanged grant creates the repair Task"},
		{
			name: "a removed label withdraws a repair whose Task create failed",
			change: func(_ *repositoryMonitorRepairLabelsGitHub, pr *repositoryMonitorPullRequest) {
				pr.Labels = []string{"orka:review"}
			},
			jobOnly:  true,
			wantSkip: repositoryMonitorRepairSkipScopeWithdrawn,
		},
		{
			name: "partial base CI results defer the CI repair",
			change: func(github *repositoryMonitorRepairLabelsGitHub, _ *repositoryMonitorPullRequest) {
				github.partial = map[string]bool{"base31": true}
			},
			wantErr: true,
		},
		{
			name: "a removed repair label withdraws the repair",
			change: func(_ *repositoryMonitorRepairLabelsGitHub, pr *repositoryMonitorPullRequest) {
				pr.Labels = []string{"orka:review"}
			},
			wantSkip: repositoryMonitorRepairSkipScopeWithdrawn,
		},
		{
			name: "a label reapplied by a triage account withdraws the repair",
			change: func(github *repositoryMonitorRepairLabelsGitHub, _ *repositoryMonitorPullRequest) {
				github.unlabeled("maintainer", "orka:fix-ci").labeled("triager", "orka:fix-ci")
			},
			wantSkip: repositoryMonitorRepairSkipLabelActorNotPermitted,
		},
		{
			name: "a failure now shared with the base withdraws the CI repair",
			change: func(github *repositoryMonitorRepairLabelsGitHub, _ *repositoryMonitorPullRequest) {
				github.checks["base31"]["tests"] = "failure"
			},
			wantSkip: repositoryMonitorRepairSkipNoIntroducedCIFailure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			github := failingHead()
			github.roles = map[string]string{"maintainer": "write", "triager": "triage"}
			server := httptest.NewServer(github)
			t.Cleanup(server.Close)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			monitor, secret := repositoryMonitorInventoryTestObjects("repair-recheck")
			labelSpec := repositoryMonitorRepairLabelsTestSpec()
			monitor.Spec.Targets.PullRequests.IncludeLabels = labelSpec.Targets.PullRequests.IncludeLabels
			monitor.Spec.Repair.IncludeLabels = labelSpec.Repair.IncludeLabels
			monitor.Spec.Repair.CIOnlyLabels = labelSpec.Repair.CIOnlyLabels
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			configureRepositoryMonitorTestWriteCredentials(monitor)
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			monitorStore := setupControllerSQLiteStore(t)
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
			pr := repositoryMonitorPullRequest{
				Number: 31, State: "open", BaseBranch: "main", BaseSHA: "base31", HeadBranch: "feature", HeadRepo: "orka-agents/orka",
				HeadSHA: "head31", MergeableState: "clean", Labels: []string{"orka:review", "orka:fix-ci"},
			}
			item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
			poll := &store.MonitorRun{ID: "poll", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "workflow", StartedAt: time.Now()}
			if handled, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, poll, "orka-agents", "orka", pr, item); err != nil || !handled {
				t.Fatalf("tryRepositoryMonitorAutomaticRepair() = %t, %v, want a queued repair", handled, err)
			}
			commands, _, err := monitorStore.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
			if err != nil || len(commands) != 1 || commands[0].Intent != repositoryMonitorCommandIntentFixCI {
				t.Fatalf("commands = %#v err=%v, want one fix_ci command", commands, err)
			}
			if tt.change != nil {
				tt.change(github, &pr)
			}
			if tt.jobOnly {
				job := &store.RepairJob{ID: "repair-" + repositoryMonitorShortHash(commands[0].ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", PRNumber: 31, Intent: commands[0].Intent, Source: commands[0].Source, HeadSHA: "head31", Phase: repositoryMonitorRepairPhaseQueued, TaskName: repositoryMonitorRepairTaskName(monitor, pr, &commands[0]), LastError: repositoryMonitorRepairTaskCreateError}
				if err := monitorStore.CreateRepairJob(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			run := &store.MonitorRun{ID: "command-run", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "command", CommandEventID: commands[0].ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, StartedAt: time.Now()}
			handled, _, err := reconciler.tryProcessPullRequestCommandRun(ctx, monitor, run, "orka-agents", "orka", pr, item)
			var tasks corev1alpha1.TaskList
			if err := cl.List(ctx, &tasks); err != nil {
				t.Fatal(err)
			}
			if tt.wantErr {
				if !errors.Is(err, errRepositoryMonitorCIIncomplete) || len(tasks.Items) != 0 {
					t.Fatalf("err = %v, tasks = %d, want the repair deferred without a Task", err, len(tasks.Items))
				}
				return
			}
			if err != nil || !handled {
				t.Fatalf("tryProcessPullRequestCommandRun() = %t, %v", handled, err)
			}
			if tt.wantSkip == "" {
				if len(tasks.Items) != 1 {
					t.Fatalf("tasks = %d, want the repair Task", len(tasks.Items))
				}
				return
			}
			if len(tasks.Items) != 0 || item.SkipReason != tt.wantSkip || item.RepairState != "" {
				t.Fatalf("tasks = %d, item repair state %q skip %q, want no Task and skip %q", len(tasks.Items), item.RepairState, item.SkipReason, tt.wantSkip)
			}
		})
	}
}

func TestRepositoryMonitorPolicyRepairRecheckLeavesExplicitCommandsAlone(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{Spec: repositoryMonitorRepairLabelsTestSpec()}
	pr := repositoryMonitorPullRequest{Number: 31, State: "open", BaseBranch: "main", HeadSHA: "head31", Labels: []string{"orka:review"}}
	command := &store.CommandEvent{Source: "api", Intent: repositoryMonitorCommandIntentFixCI}
	// No repair label and no GitHub server: an explicit command needs neither.
	reason, err := (&RepositoryMonitorReconciler{}).repositoryMonitorPolicyRepairWithdrawnReason(context.Background(), monitor, command, "orka-agents", "orka", pr)
	if err != nil || reason != "" {
		t.Fatalf("reason = %q err=%v, want explicit commands unaffected", reason, err)
	}
}

func TestRepositoryMonitorCIRepairPromptQualifiesChecksSharingAName(t *testing.T) {
	github := &repositoryMonitorRepairLabelsGitHub{
		checks:   map[string]map[string]string{"head32": {"tests": "failure"}, "base32": {"tests": "failure"}},
		statuses: map[string]map[string]string{"head32": {"tests": "failure"}},
	}
	server := httptest.NewServer(github)
	t.Cleanup(server.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("ci-prompt-shared")
	reconciler := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), GitHubAPIBaseURL: server.URL}
	pr := repositoryMonitorPullRequest{Number: 32, BaseSHA: "base32", HeadSHA: "head32"}
	prompt, err := reconciler.buildRepositoryMonitorRepairPrompt(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{})
	if err != nil {
		t.Fatal(err)
	}
	_, input, _ := strings.Cut(prompt, "Input:\n")
	var payload struct {
		FailingChecks     []string `json:"failingChecks"`
		BaseFailingChecks []string `json:"baseFailingChecks"`
	}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(payload.FailingChecks) != "[tests (commit status)]" || fmt.Sprint(payload.BaseFailingChecks) != "[tests (check run from GitHub App 0)]" {
		t.Fatalf("CI repair input = %+v, want the shared name qualified by source", payload)
	}
}

func TestRepositoryMonitorReadinessRepairEligibilityIgnoresInheritedCIFailures(t *testing.T) {
	github := &repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{"head33": {"tests": "failure"}, "base33": {"tests": "failure"}}}
	server := httptest.NewServer(github)
	t.Cleanup(server.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("ready-inherited")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	reconciler := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: setupControllerSQLiteStore(t), GitHubAPIBaseURL: server.URL}
	pr := repositoryMonitorPullRequest{Number: 33, State: "open", BaseBranch: "main", BaseSHA: "base33", HeadSHA: "head33", HeadRepo: "orka-agents/orka", MergeableState: "clean"}
	eligible, err := reconciler.repositoryMonitorNeedsChangesRepairEligible(context.Background(), monitor, pr, &store.MonitorItem{})
	if err != nil || eligible {
		t.Fatalf("eligible = %t err=%v, want no repair for a failure the base shares", eligible, err)
	}
	github.checks["base33"]["tests"] = "success"
	eligible, err = reconciler.repositoryMonitorNeedsChangesRepairEligible(context.Background(), monitor, pr, &store.MonitorItem{})
	if err != nil || !eligible {
		t.Fatalf("eligible = %t err=%v, want a repair for a failure the pull request introduced", eligible, err)
	}
}

func TestRepositoryMonitorCIFailuresWaitForCompleteResultsAndShareBaseLookups(t *testing.T) {
	github := &repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{
		"head41": {"tests": "failure"}, "head42": {"tests": "failure"}, "base4": {"tests": "failure"},
	}}
	server := httptest.NewServer(github)
	t.Cleanup(server.Close)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("ci-failures-base")
	reconciler := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), GitHubAPIBaseURL: server.URL}
	ctx := withRepositoryMonitorBaseCICache(context.Background())
	for _, head := range []string{"head41", "head42"} {
		failures, err := reconciler.repositoryMonitorPullRequestCIFailures(ctx, monitor, repositoryMonitorPullRequest{BaseSHA: "base4", HeadSHA: head})
		if err != nil || failures.broke || failures.incomplete || len(failures.inherited) != 1 {
			t.Fatalf("%s failures = %+v err=%v, want the failure inherited", head, failures, err)
		}
	}
	if github.checkReads["base4"] != 1 {
		t.Fatalf("base check reads = %d, want one per inventory run", github.checkReads["base4"])
	}
	// A partial base result could hide the inherited failure.
	github.partial = map[string]bool{"base5": true}
	github.checks["base5"] = map[string]string{}
	failures, err := reconciler.repositoryMonitorPullRequestCIFailures(context.Background(), monitor, repositoryMonitorPullRequest{BaseSHA: "base5", HeadSHA: "head41"})
	if err != nil || failures.broke || !failures.incomplete {
		t.Fatalf("failures = %+v err=%v, want classification deferred on partial base results", failures, err)
	}
}
