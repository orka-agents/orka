package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestRepositoryMonitorReadinessRetiresHeadChangedDuringDetailRefresh(t *testing.T) {
	for _, tc := range []struct {
		name        string
		peer        bool
		peerPaused  bool
		stopped     bool
		advanced    bool
		closed      bool
		baseChanged bool
		wantOld     string
	}{
		{name: "no_remaining_peer", wantOld: repositoryMonitorStatusFailure},
		{name: "remaining_ready_peer", peer: true, wantOld: repositoryMonitorStatusSuccess},
		{name: "remaining_paused_peer", peer: true, peerPaused: true, wantOld: repositoryMonitorStatusFailure},
		{name: "stop_follows_new_head", stopped: true, wantOld: repositoryMonitorStatusFailure},
		{name: "inventory_head_advancement", advanced: true, wantOld: repositoryMonitorStatusFailure},
		{name: "closed_on_persisted_head", closed: true, wantOld: repositoryMonitorStatusFailure},
		{name: "base_changed_on_persisted_head", baseChanged: true, wantOld: repositoryMonitorStatusFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepositoryMonitorReadinessHeadDriftFixture(t, tc.peer)
			f.detailHead, f.peerPaused = "new-head", tc.peerPaused
			if tc.advanced {
				f.listHead, f.draft = "new-head", true
			}
			if tc.closed || tc.baseChanged {
				f.listHead, f.detailHead = "new-head", "old-head"
			}
			if tc.closed {
				f.detailState = "closed"
			}
			if tc.baseChanged {
				f.detailBase = "release"
			}
			if tc.stopped {
				item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "1")
				if err != nil {
					t.Fatal(err)
				}
				item.SkipReason = repositoryMonitorIssueSkipStoppedByCommand
				if err := f.r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
					t.Fatal(err)
				}
			}
			f.queueRun(t)
			f.reconcile(t)
			f.assertNoTasks(t)
			run, err := f.r.Store.GetMonitorRun(t.Context(), f.monitor.Namespace, "head-drift")
			if err != nil || run.Phase != repositoryMonitorRunPhaseSucceeded || run.CreatedTaskCount != 0 || run.SkippedCount == 0 {
				t.Fatalf("stale inventory was not skipped: run=%+v err=%v", run, err)
			}
			if got := f.latest["old-head"]; got.State != tc.wantOld {
				t.Fatalf("departed readiness=%+v, want %s", got, tc.wantOld)
			}
			if tc.peerPaused && !strings.Contains(f.latest["old-head"].Description, "paused") {
				t.Fatalf("remaining peer was not recomputed: %+v", f.latest["old-head"])
			}
			newStatus, published := f.latest["new-head"]
			if tc.stopped || tc.advanced {
				if !published || newStatus.State != repositoryMonitorStatusFailure {
					t.Fatalf("changed head was not blocked: %+v", newStatus)
				}
				item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "1")
				if err != nil {
					t.Fatal(err)
				}
				if tc.stopped && item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
					t.Fatalf("head drift cleared the stop guard: item=%+v", item)
				}
				if tc.advanced && item.HeadSHA != "new-head" {
					t.Fatalf("head advancement was not persisted after retirement: item=%+v", item)
				}
			} else if published {
				t.Fatalf("old review evidence produced new-head readiness: %+v", newStatus)
			}
		})
	}
}

func TestRepositoryMonitorReadinessRetriesDepartedHeadWrite(t *testing.T) {
	f := newRepositoryMonitorReadinessHeadDriftFixture(t, false)
	f.detailHead, f.failOldHeadWrite = "new-head", true
	f.queueRun(t)
	f.reconcile(t)
	run, err := f.r.Store.GetMonitorRun(t.Context(), f.monitor.Namespace, "head-drift")
	if err != nil || run.Phase != repositoryMonitorRunPhaseQueued || run.CompletedAt != nil {
		t.Fatalf("departed-head write failure did not retry: run=%+v err=%v", run, err)
	}
	mutation, err := f.r.Store.GetGitHubMutationRecord(t.Context(), f.monitor.Namespace, repositoryMonitorReadinessMutationID(f.monitor, "old-head"))
	if err != nil || mutation.Status != repositoryMonitorStatusSubmitting {
		t.Fatalf("failed write lost durable uncertainty: mutation=%+v err=%v", mutation, err)
	}
	if f.latest["old-head"].State != repositoryMonitorStatusSuccess {
		t.Fatal("fixture did not preserve success after the rejected write")
	}
	f.assertNoTasks(t)
	item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil || item.HeadSHA != "old-head" {
		t.Fatalf("failed retirement lost its prior head: item=%+v err=%v", item, err)
	}
	// On retry GitHub's list has caught up. The store must retain the old head
	// until cleanup succeeds. A draft keeps this test focused on readiness.
	f.listHead, f.draft = "new-head", true
	run.StartedAt = time.Now().Add(-time.Second)
	if err := f.r.Store.UpdateMonitorRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	run, err = f.r.Store.GetMonitorRun(t.Context(), f.monitor.Namespace, "head-drift")
	if err != nil || run.Phase != repositoryMonitorRunPhaseSucceeded || f.latest["old-head"].State != repositoryMonitorStatusFailure {
		t.Fatalf("departed-head retry failed: run=%+v status=%+v err=%v", run, f.latest["old-head"], err)
	}
	mutation, err = f.r.Store.GetGitHubMutationRecord(t.Context(), f.monitor.Namespace, repositoryMonitorReadinessMutationID(f.monitor, "old-head"))
	if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded {
		t.Fatalf("retry did not settle departed-head uncertainty: mutation=%+v err=%v", mutation, err)
	}
	if f.latest["new-head"].State != repositoryMonitorStatusFailure {
		t.Fatalf("retry reused old positive evidence: %+v", f.latest["new-head"])
	}
	item, err = f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil || item.HeadSHA != "new-head" {
		t.Fatalf("retry did not persist the replacement head: item=%+v err=%v", item, err)
	}
	f.assertNoTasks(t)
}

func TestRepositoryMonitorReadinessTerminalReviewRequiresCurrentHead(t *testing.T) {
	for _, verdict := range []string{repositoryMonitorReviewVerdictFailed, repositoryMonitorReviewVerdictNeedsHuman, repositoryMonitorReviewVerdictSecuritySensitive} {
		for _, current := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/current=%t", verdict, current), func(t *testing.T) {
				f := newRepositoryMonitorReadinessHeadDriftFixture(t, false)
				item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "1")
				if err != nil {
					t.Fatal(err)
				}
				item.LastReviewID, item.LastVerdict = "terminal-review", verdict
				if err := f.r.Store.CreateReviewRecord(t.Context(), &store.ReviewRecord{
					ID: item.LastReviewID, MonitorNamespace: f.monitor.Namespace, MonitorName: f.monitor.Name,
					Kind: repositoryMonitorPullRequestKind, Number: item.Number, HeadSHA: item.HeadSHA, Verdict: verdict,
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
					t.Fatal(err)
				}
				want := repositoryMonitorStatusFailure
				if !current {
					f.listHead, f.detailHead = "new-head", "new-head"
					want = repositoryMonitorStatusPending
				}
				pr := repositoryMonitorPullRequest{Number: item.Number, HeadSHA: f.listHead, State: repositoryMonitorItemStateOpen, BaseBranch: "main"}
				item = repositoryMonitorItemFromPullRequest(f.monitor, pr, item)
				if err := f.r.reconcileRepositoryMonitorReadiness(t.Context(), f.monitor, &pr, item); err != nil {
					t.Fatal(err)
				}
				if got := f.latest[pr.HeadSHA]; got.State != want {
					t.Fatalf("readiness=%+v, want %s", got, want)
				}
				if item.LastVerdict != verdict || item.LastReviewedHeadSHA != "old-head" {
					t.Fatal("readiness rewrote historical review evidence")
				}
			})
		}
	}
}

type repositoryMonitorReadinessHeadDriftFixture struct {
	r                *RepositoryMonitorReconciler
	monitor          *corev1alpha1.RepositoryMonitor
	detailHead       string
	detailState      string
	detailBase       string
	listHead         string
	draft            bool
	peerPaused       bool
	failOldHeadWrite bool
	latest           map[string]repositoryMonitorCommitStatus
}

//nolint:gocyclo // Fixture models GitHub reads, status writes, and seeded readiness evidence.
func newRepositoryMonitorReadinessHeadDriftFixture(t *testing.T, peer bool) *repositoryMonitorReadinessHeadDriftFixture {
	t.Helper()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("readiness-head-drift")
	monitor.Spec.Review.Publish.Enabled = true
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f := &repositoryMonitorReadinessHeadDriftFixture{
		r: &RepositoryMonitorReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build(),
			Scheme: scheme, Store: db,
		},
		monitor:     monitor,
		detailHead:  "old-head",
		detailState: "open",
		detailBase:  "main",
		listHead:    "old-head",
		latest:      map[string]repositoryMonitorCommitStatus{},
	}
	prJSON := func(number int64, sha string) map[string]any {
		labels := []map[string]string{}
		if number == 2 && f.peerPaused {
			labels = append(labels, map[string]string{"name": "orka:pause"})
		}
		return map[string]any{"number": number, "state": "open", "head": map[string]string{"sha": sha}, "base": map[string]string{"ref": "main"}, "labels": labels, "draft": number == 1 && f.draft}
	}
	writes := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(req.URL.Path, "/")
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			// The list can lag behind detail or catch up between retries.
			prs := []any{prJSON(1, f.listHead)}
			if peer {
				prs = append(prs, prJSON(2, "old-head"))
			}
			_ = json.NewEncoder(w).Encode(prs)
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/pulls/"):
			number, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			sha := "old-head"
			if number == 1 {
				sha = f.detailHead
			}
			pr := prJSON(number, sha)
			if number == 1 {
				pr["state"] = f.detailState
				pr["base"] = map[string]string{"ref": f.detailBase}
			}
			_ = json.NewEncoder(w).Encode(pr)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":99,"name":"tests","status":"completed","conclusion":"success"}]}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
			statuses := []repositoryMonitorCommitStatus{}
			if status, ok := f.latest[parts[len(parts)-2]]; ok {
				statuses = append(statuses, status)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(statuses), "statuses": statuses})
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
			var status repositoryMonitorCommitStatus
			if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
				t.Error(err)
			}
			sha := parts[len(parts)-1]
			if f.failOldHeadWrite && sha == "old-head" && status.State == repositoryMonitorStatusFailure {
				f.failOldHeadWrite = false
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			writes++
			status.ID = 100 + writes
			f.latest[sha] = status
			_ = json.NewEncoder(w).Encode(status)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	f.r.GitHubAPIBaseURL = server.URL
	for _, number := range []int64{1, 2} {
		if number == 2 && !peer {
			continue
		}
		review := seedRepositoryMonitorAutomergeReview(t, t.Context(), db, monitor.Name, number, "old-head")
		item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: fmt.Sprint(number), Number: number, State: "open", BaseBranch: "main", HeadSHA: "old-head", LastReviewID: review, LastReviewedHeadSHA: "old-head", LastVerdict: repositoryMonitorReviewVerdictPassed}
		if err := db.UpsertMonitorItem(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	item, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatal(err)
	}
	pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: "old-head", State: "open", BaseBranch: "main"}
	if err := f.r.reconcileRepositoryMonitorReadiness(t.Context(), monitor, &pr, item); err != nil || f.latest["old-head"].State != repositoryMonitorStatusSuccess {
		t.Fatalf("initial readiness=%+v err=%v", f.latest["old-head"], err)
	}
	return f
}

func (f *repositoryMonitorReadinessHeadDriftFixture) queueRun(t *testing.T) {
	t.Helper()
	if err := f.r.Store.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: "head-drift", MonitorNamespace: f.monitor.Namespace, MonitorName: f.monitor.Name, Trigger: "manual", TargetKind: repositoryMonitorPullRequestKind, Phase: repositoryMonitorRunPhaseQueued}); err != nil {
		t.Fatal(err)
	}
}

func (f *repositoryMonitorReadinessHeadDriftFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}}); err != nil {
		t.Fatal(err)
	}
}

func (f *repositoryMonitorReadinessHeadDriftFixture) assertNoTasks(t *testing.T) {
	t.Helper()
	var tasks corev1alpha1.TaskList
	if err := f.r.List(t.Context(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 0 {
		t.Fatalf("stale inventory dispatched Tasks: %+v", tasks.Items)
	}
}
