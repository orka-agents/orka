package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

//nolint:gocyclo // Cover refresh, inventory retirement, retained audit, and shared peers in the same status lifecycle.
func TestRepositoryMonitorReadinessRevokesOutOfScopeHead(t *testing.T) {
	for _, mode := range []string{"refresh", "inventory", "retained_retirement", "shared_peer", "refresh_shared_peer", "refresh_shared_blocked_peer", "refresh_shared_dirty_peer", "shared_head_dirty_peer"} {
		t.Run(mode, func(t *testing.T) {
			shared := strings.Contains(mode, "shared")
			ctx := t.Context()
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("scope-ready")
			monitor.Spec.Review.Publish.Enabled = true
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
			baseBranch := "main"
			peerPaused := false
			peerDirty := false
			latest := repositoryMonitorCommitStatus{}
			writes := 0
			prJSON := func(number int64) map[string]any {
				branch := baseBranch
				if number == 2 {
					branch = "main"
				}
				labels := []map[string]string{}
				if number == 2 && peerPaused {
					labels = append(labels, map[string]string{"name": "orka:pause"})
				}
				return map[string]any{"number": number, "state": "open", "head": map[string]string{"sha": "head"}, "base": map[string]string{"ref": branch}, "labels": labels}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
					prs := []any{}
					if baseBranch == "main" {
						prs = append(prs, prJSON(1))
					}
					if shared {
						prs = append(prs, prJSON(2))
					}
					_ = json.NewEncoder(w).Encode(prs)
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/pulls/"):
					number, _ := strconv.ParseInt(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], 10, 64)
					pr := prJSON(number)
					if number == 2 && peerDirty {
						pr["mergeable_state"] = "dirty"
					}
					_ = json.NewEncoder(w).Encode(pr)
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
					_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":99,"name":"tests","status":"completed","conclusion":"success"}]}`))
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "statuses": []repositoryMonitorCommitStatus{latest}})
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/statuses/head"):
					if err := json.NewDecoder(req.Body).Decode(&latest); err != nil {
						t.Error(err)
					}
					writes++
					latest.ID = int64(100 + writes)
					_ = json.NewEncoder(w).Encode(latest)
				default:
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			r.GitHubAPIBaseURL = server.URL
			items := make([]*store.MonitorItem, 0, 2)
			for _, number := range []int64{1, 2} {
				review := seedRepositoryMonitorAutomergeReview(t, ctx, db, monitor.Name, number, "head")
				item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: fmt.Sprint(number), Number: number, State: "open", BaseBranch: "main", HeadSHA: "head", LastReviewID: review, LastReviewedHeadSHA: "head", LastVerdict: repositoryMonitorReviewVerdictPassed}
				if number == 1 || shared {
					if err := db.UpsertMonitorItem(ctx, item); err != nil {
						t.Fatal(err)
					}
				}
				items = append(items, item)
			}
			pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: "head", State: "open", BaseBranch: "main"}
			if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, items[0]); err != nil || latest.State != repositoryMonitorStatusSuccess {
				t.Fatalf("initial readiness=%+v err=%v", latest, err)
			}
			if mode != "shared_head_dirty_peer" {
				baseBranch = "release"
			}
			peerPaused = mode == "refresh_shared_blocked_peer"
			peerDirty = strings.Contains(mode, "dirty")
			if mode == "retained_retirement" {
				items[0].State, items[0].SkipReason = repositoryMonitorItemStateOutOfScope, repositoryMonitorSkipReasonMissing
				if err := db.UpsertMonitorItem(ctx, items[0]); err != nil {
					t.Fatal(err)
				}
			}
			for iteration := range 2 {
				if strings.HasPrefix(mode, "refresh") {
					if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, items[0]); err != nil {
						t.Fatal(err)
					}
				} else {
					run := &store.MonitorRun{ID: fmt.Sprintf("scope-%d", iteration), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind}
					if _, _, _, err := r.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka"); err != nil {
						t.Fatal(err)
					}
				}
			}
			wantState, wantWrites := repositoryMonitorStatusFailure, 3
			if shared && !peerPaused && !peerDirty {
				wantState, wantWrites = repositoryMonitorStatusSuccess, 2
			}
			if peerDirty {
				wantState = repositoryMonitorStatusPending
			}
			if latest.State != wantState || writes != wantWrites {
				t.Fatalf("readiness=%+v writes=%d, want %s and %d", latest, writes, wantState, wantWrites)
			}
			if peerPaused && !strings.Contains(latest.Description, "paused") {
				t.Fatalf("departed PR replaced the active peer's blocker: %+v", latest)
			}
		})
	}
}
