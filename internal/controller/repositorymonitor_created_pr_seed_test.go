package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRepositoryMonitorControllerCreatedPullRequestSeedsReview(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		t.Run(fmt.Sprint("suspended_", suspended), func(t *testing.T) {
			f := newRepositoryMonitorCreatedPRSeedFixture(t, true, suspended)
			f.reconcile(t)
			pr, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "142")
			if err != nil || pr.LastVerdict != repositoryMonitorRunPhaseQueued || pr.HeadSHA != "current-pr-head" {
				t.Fatalf("created PR was not inventoried at its current head: item=%+v err=%v", pr, err)
			}
			review := f.reviewTasks(t)
			if len(review) != 1 || review[0].Spec.Workspace.Ref != "current-pr-head" {
				t.Fatalf("targeted inventory did not create one current-head review: %+v", review)
			}
			f.completeReview(t, &review[0])
			for range 2 {
				f.reconcile(t)
			}
			pr, err = f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "142")
			if err != nil || pr.LastVerdict != repositoryMonitorReviewVerdictPassed || pr.LastReviewedHeadSHA != "current-pr-head" {
				t.Fatalf("linked PR was not reviewed: item=%+v err=%v", pr, err)
			}
			f.assertSingleHandoff(t)
			if len(f.reviewTasks(t)) != 1 {
				t.Fatal("replayed issue result duplicated PR review work")
			}
		})
	}
}

func TestRepositoryMonitorControllerCreatedPullRequestChecksFreshScope(t *testing.T) {
	for _, scope := range []string{"disabled", "closed", "base_changed", "paused", "stopped"} {
		t.Run(scope, func(t *testing.T) {
			f := newRepositoryMonitorCreatedPRSeedFixture(t, scope != "disabled", false)
			switch scope {
			case "closed":
				f.prState = "closed"
			case "base_changed":
				f.prBase = "release"
			case "paused":
				f.prPaused = true
			case "stopped":
				item := &store.MonitorItem{MonitorNamespace: f.monitor.Namespace, MonitorName: f.monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "142", Number: 142, State: "open", BaseBranch: "main", HeadSHA: "current-pr-head", SkipReason: repositoryMonitorIssueSkipStoppedByCommand}
				if err := f.r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
					t.Fatal(err)
				}
				// An existing PR item can already participate in background polling.
				// Its recent inventory keeps this test focused on the new handoff.
				if err := f.r.Store.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "prior-pr", MonitorNamespace: f.monitor.Namespace, MonitorName: f.monitor.Name, Trigger: "manual", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 142, Phase: repositoryMonitorRunPhaseSucceeded, StartedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			f.reconcile(t)
			if len(f.reviewTasks(t)) != 0 || f.publications.Load() != 1 {
				t.Fatalf("out-of-scope PR created review work or repeated publication: scope=%s publications=%d", scope, f.publications.Load())
			}
			runs := f.seedRuns(t)
			if scope == "disabled" {
				if len(runs) != 0 || f.prReads.Load() != 0 {
					t.Fatalf("disabled PR monitoring was seeded: runs=%+v reads=%d", runs, f.prReads.Load())
				}
			} else {
				if len(runs) != 1 || runs[0].Phase != repositoryMonitorRunPhaseSucceeded || f.prReads.Load() == 0 {
					t.Fatalf("handoff did not verify current PR scope: runs=%+v reads=%d", runs, f.prReads.Load())
				}
				if scope == "stopped" {
					item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "142")
					if err != nil || item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
						t.Fatalf("handoff cleared the stop guard: item=%+v err=%v", item, err)
					}
				}
			}
		})
	}
}

func TestRepositoryMonitorControllerCreatedPullRequestSeedRetries(t *testing.T) {
	for _, failure := range []string{"queue_write", "queue_acknowledgment", "item_write"} {
		t.Run(failure, func(t *testing.T) {
			f := newRepositoryMonitorCreatedPRSeedFixture(t, true, false)
			injected := errors.New("linked PR handoff temporarily unavailable")
			f.r.Store = &repositoryMonitorCreatedPRSeedFailureStore{RepositoryMonitorStore: f.r.Store, failure: failure, injected: injected}
			if _, err := f.r.Reconcile(t.Context(), f.request()); !errors.Is(err, injected) {
				t.Fatalf("handoff failure was not retryable: %v", err)
			}
			item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorIssueKind, "42")
			if err != nil || item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued || item.LastActionTaskName == "" {
				t.Fatalf("failed handoff lost the result retry identity: item=%+v err=%v", item, err)
			}
			mutations, _, err := f.r.Store.ListGitHubMutationRecords(t.Context(), store.GitHubMutationRecordFilter{Namespace: f.monitor.Namespace, MonitorName: f.monitor.Name, Operation: "create_pr"})
			if err != nil || len(mutations) != 1 || mutations[0].Status != repositoryMonitorRunPhaseSucceeded || mutations[0].ExternalID != "142" {
				t.Fatalf("failed handoff lost the confirmed PR receipt: mutations=%+v err=%v", mutations, err)
			}
			if len(f.reviewTasks(t)) != 0 {
				t.Fatal("review started before the issue result settled")
			}
			for range 2 {
				f.reconcile(t)
			}
			f.assertSingleHandoff(t)
			if len(f.reviewTasks(t)) != 1 {
				t.Fatal("retry did not create exactly one review Task")
			}
		})
	}
}

type repositoryMonitorCreatedPRSeedFixture struct {
	r            *RepositoryMonitorReconciler
	monitor      *corev1alpha1.RepositoryMonitor
	prState      string
	prBase       string
	prPaused     bool
	publications atomic.Int64
	prReads      atomic.Int64
}

func newRepositoryMonitorCreatedPRSeedFixture(t *testing.T, enabled, suspended bool) *repositoryMonitorCreatedPRSeedFixture {
	t.Helper()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("created-pr-seed")
	requirePlan := false
	monitor.Spec.Targets.PullRequests.Enabled = &enabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	monitor.Spec.IssueWorkflow.Implementation.RequirePlan = &requirePlan
	monitor.Spec.Suspend = &suspended
	monitor.Spec.Review.ExactEventEnabled = false
	configureRepositoryMonitorTestWriteCredentials(monitor)
	f := &repositoryMonitorCreatedPRSeedFixture{monitor: monitor, prState: "open", prBase: "main"}
	server := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(server.Close)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	f.r = &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, ArtifactStore: db, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "create-pr-implement", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 42, Intent: repositoryMonitorCommandIntentImplement, Status: repositoryMonitorCommandAccepted}
	if err := db.CreateCommandEvent(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	item, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
	if err != nil || item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued {
		t.Fatalf("implementation was not queued: item=%+v err=%v", item, err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: monitor.Namespace, Name: item.LastActionTaskName}, &task); err != nil {
		t.Fatal(err)
	}
	result := fmt.Appendf(nil, `{"schemaVersion":"orka.issueImplementation.v1","issueNumber":42,"snapshotDigest":%q,"status":"patch_ready","summary":"Implemented the requested change."}`, item.SnapshotDigest)
	if err := db.SaveResult(t.Context(), task.Namespace, task.Name, result); err != nil {
		t.Fatal(err)
	}
	// PR discovery must use a fresh read, even if its head changed after delivery.
	markRepositoryMonitorTestTaskDelivered(t, t.Context(), cl, task.Name, task.Spec.Workspace.PushBranch, "delivered-head")
	return f
}

func (f *repositoryMonitorCreatedPRSeedFixture) serveHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/issues/42":
		_, _ = w.Write([]byte(`{"number":42,"title":"Fix issue","body":"Keep scope small","state":"open","labels":[{"name":"bug"}]}`))
	case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/pulls":
		_, _ = w.Write([]byte(`[]`))
	case req.Method == http.MethodPost && req.URL.Path == "/repos/orka-agents/orka/pulls":
		f.publications.Add(1)
		_, _ = w.Write([]byte(`{"number":142,"html_url":"https://github.com/orka-agents/orka/pull/142"}`))
	case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/pulls/142":
		f.prReads.Add(1)
		prLabels := []map[string]string{}
		if f.prPaused {
			prLabels = append(prLabels, map[string]string{"name": "orka:pause"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 142, "title": "Fix issue", "state": f.prState, "labels": prLabels, "changed_files": 1, "head": map[string]any{"sha": "current-pr-head", "ref": "implementation", "repo": map[string]string{"full_name": "orka-agents/orka"}}, "base": map[string]string{"ref": f.prBase, "sha": "base42"}})
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/compare/"):
		_, _ = w.Write([]byte(`{"files":[{"filename":"docs/change.md","status":"added","additions":1,"deletions":0,"changes":1,"patch":"@@ -0,0 +1 @@\n+Change"}]}`))
	case req.Method == http.MethodPost && req.URL.Path == "/repos/orka-agents/orka/issues/42/comments":
		_, _ = w.Write([]byte(`{"id":4201,"html_url":"https://github.com/orka-agents/orka/issues/42#issuecomment-4201"}`))
	case req.Method == http.MethodPatch && req.URL.Path == "/repos/orka-agents/orka/issues/comments/4201":
		_, _ = w.Write([]byte(`{"id":4201,"html_url":"https://github.com/orka-agents/orka/issues/42#issuecomment-4201"}`))
	case req.Method == http.MethodPost && req.URL.Path == "/graphql":
		_, _ = w.Write([]byte(`{"data":{"repository":{"nameWithOwner":"orka-agents/orka","pullRequest":{"id":"PR_142","number":142,"url":"https://github.com/orka-agents/orka/pull/142","state":"OPEN","headRefOid":"current-pr-head"}}}}`))
	default:
		http.Error(w, "unexpected request", http.StatusNotFound)
	}
}

func (f *repositoryMonitorCreatedPRSeedFixture) request() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}}
}

func (f *repositoryMonitorCreatedPRSeedFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(t.Context(), f.request()); err != nil {
		t.Fatal(err)
	}
}

func (f *repositoryMonitorCreatedPRSeedFixture) seedRuns(t *testing.T) []store.MonitorRun {
	t.Helper()
	runs, _, err := f.r.Store.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: f.monitor.Namespace, MonitorName: f.monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 142, Trigger: "workflow"})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (f *repositoryMonitorCreatedPRSeedFixture) reviewTasks(t *testing.T) []corev1alpha1.Task {
	t.Helper()
	var tasks corev1alpha1.TaskList
	if err := f.r.List(t.Context(), &tasks); err != nil {
		t.Fatal(err)
	}
	var reviews []corev1alpha1.Task
	for _, task := range tasks.Items {
		if task.Annotations[labels.AnnotationMonitorItemKind] == repositoryMonitorPullRequestKind {
			reviews = append(reviews, task)
		}
	}
	return reviews
}

func (f *repositoryMonitorCreatedPRSeedFixture) completeReview(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	result := repositoryMonitorReviewResultEnvelopeWith(t, 142, "current-pr-head", repositoryMonitorReviewVerdictPassed, func(payload map[string]any) {
		payload["findings"] = []map[string]any{}
		payload["summary"] = "Change reviewed."
	})
	if err := f.r.ResultStore.SaveResult(t.Context(), task.Namespace, task.Name, result); err != nil {
		t.Fatal(err)
	}
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	if err := f.r.Status().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
}

func (f *repositoryMonitorCreatedPRSeedFixture) assertSingleHandoff(t *testing.T) {
	t.Helper()
	runs := f.seedRuns(t)
	if len(runs) != 1 || runs[0].Phase != repositoryMonitorRunPhaseSucceeded || runs[0].TargetSHA != "" || runs[0].CommandEventID != "" || f.publications.Load() != 1 {
		t.Fatalf("publication or handoff was duplicated/incorrect: runs=%+v publications=%d", runs, f.publications.Load())
	}
	item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorIssueKind, "42")
	if err != nil || item.WorkflowPhase != repositoryMonitorIssuePhasePROpened || item.LinkedPRNumber != 142 {
		t.Fatalf("issue result did not settle: item=%+v err=%v", item, err)
	}
}

type repositoryMonitorCreatedPRSeedFailureStore struct {
	store.RepositoryMonitorStore
	failure  string
	injected error
}

func (s *repositoryMonitorCreatedPRSeedFailureStore) CreateMonitorRun(ctx context.Context, run *store.MonitorRun) error {
	if run.TargetKind == repositoryMonitorPullRequestKind && run.TargetNumber == 142 && strings.HasPrefix(s.failure, "queue_") {
		failure := s.failure
		s.failure = ""
		if failure == "queue_acknowledgment" {
			if err := s.RepositoryMonitorStore.CreateMonitorRun(ctx, run); err != nil {
				return err
			}
		}
		return s.injected
	}
	return s.RepositoryMonitorStore.CreateMonitorRun(ctx, run)
}

func (s *repositoryMonitorCreatedPRSeedFailureStore) UpsertMonitorItem(ctx context.Context, item *store.MonitorItem) error {
	if item.Kind == repositoryMonitorIssueKind && item.WorkflowPhase == repositoryMonitorIssuePhasePROpened && s.failure == "item_write" {
		s.failure = ""
		return s.injected
	}
	return s.RepositoryMonitorStore.UpsertMonitorItem(ctx, item)
}
