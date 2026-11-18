package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestRepositoryMonitorWorkflowPollCooldownTracksPullRequestInventory(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recentKind   string
		recentNumber int64
		recentCount  int
		priorPR      bool
		wantPoll     bool
		onlyIssues   bool
		pollConflict bool
	}{
		{name: "issue_only", recentKind: repositoryMonitorIssueKind, recentNumber: 42, recentCount: 1, wantPoll: true},
		{name: "concurrent_poll_insert", recentKind: repositoryMonitorIssueKind, recentCount: 1, wantPoll: true, pollConflict: true},
		{name: "open_issues_without_pull_requests", recentKind: repositoryMonitorIssueKind, recentNumber: 42, recentCount: 1, onlyIssues: true},
		{name: "commit_only", recentKind: "commit", recentCount: 1, wantPoll: true},
		{name: "pull_request_inventory", recentKind: repositoryMonitorPullRequestKind, recentCount: 1},
		{name: "pull_request_target", recentKind: repositoryMonitorPullRequestKind, recentNumber: 1, recentCount: 1},
		{name: "full_inventory", recentCount: 1},
		{name: "legacy_pull_request_target", recentNumber: 1, recentCount: 1},
		{name: "issue_after_recent_pull_request", recentKind: repositoryMonitorIssueKind, recentCount: 1, priorPR: true},
		{name: "pull_request_on_next_page", recentKind: repositoryMonitorIssueKind, recentCount: 201, priorPR: true},
		{name: "issue_only_multiple_pages", recentKind: repositoryMonitorIssueKind, recentCount: 201, wantPoll: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			monitor, secret := repositoryMonitorInventoryTestObjects("workflow-poll-cooldown")
			monitor.Spec.Review.Publish.Enabled = true
			monitor.Spec.Targets.Issues.Enabled = true
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			var inventoryReads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/repos/orka-agents/orka/pulls" {
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				inventoryReads.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			t.Cleanup(server.Close)
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL}
			if tc.pollConflict {
				r.Store = repositoryMonitorWorkflowPollConflictStore{RepositoryMonitorStore: db}
			}
			itemKind := repositoryMonitorPullRequestKind
			if tc.onlyIssues {
				itemKind = repositoryMonitorIssueKind
			}
			if err := db.UpsertMonitorItem(t.Context(), &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: itemKind, Number: 1, State: repositoryMonitorItemStateOpen, HeadSHA: "head1"}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			prior := now.Add(-time.Minute)
			if tc.priorPR {
				prior = now.Add(-5 * time.Second)
			}
			if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "prior-pr-inventory", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "manual", TargetKind: repositoryMonitorPullRequestKind, Phase: repositoryMonitorRunPhaseSucceeded, StartedAt: prior, CompletedAt: &prior}); err != nil {
				t.Fatal(err)
			}
			for i := range tc.recentCount {
				started := now.Add(-time.Second)
				if err := db.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: fmt.Sprint("recent-run-", i), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "manual", TargetKind: tc.recentKind, TargetNumber: tc.recentNumber, Phase: repositoryMonitorRunPhaseSucceeded, StartedAt: started, CompletedAt: &started}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}); err != nil {
				t.Fatal(err)
			}
			polls, _, err := db.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "workflow"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantPoll {
				if len(polls) != 1 || polls[0].TargetKind != repositoryMonitorPullRequestKind || polls[0].Phase != repositoryMonitorRunPhaseSucceeded || inventoryReads.Load() != 1 {
					t.Fatalf("issue traffic starved PR inventory: polls=%+v inventoryReads=%d", polls, inventoryReads.Load())
				}
			} else if len(polls) != 0 || inventoryReads.Load() != 0 {
				t.Fatalf("PR inventory cooldown was bypassed: polls=%+v inventoryReads=%d", polls, inventoryReads.Load())
			}
		})
	}
}

type repositoryMonitorWorkflowPollConflictStore struct {
	store.RepositoryMonitorStore
}

func (s repositoryMonitorWorkflowPollConflictStore) CreateMonitorRun(ctx context.Context, run *store.MonitorRun) error {
	// Another writer inserts the same deterministic poll after its existence
	// checks. The second insert returns the real SQLite conflict error.
	if run.Trigger == "workflow" {
		if err := s.RepositoryMonitorStore.CreateMonitorRun(ctx, run); err != nil {
			return err
		}
	}
	return s.RepositoryMonitorStore.CreateMonitorRun(ctx, run)
}
