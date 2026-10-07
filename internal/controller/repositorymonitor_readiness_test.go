package controller

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRepositoryMonitorReadinessStatusLifecycle(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprint("github_auto_merge_", auto), func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("ready-status")
			monitor.Spec.Review.Publish.Enabled = true
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			audit := &readinessStatusAuditFailureStore{RepositoryMonitorStore: db}
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: audit}
			sha, paused := "head-1", false
			writes := []repositoryMonitorCommitStatus{}
			latest := map[string]repositoryMonitorCommitStatus{}
			failAudit := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1"):
					labels := []map[string]string{}
					if paused {
						labels = append(labels, map[string]string{"name": "orka:pause"})
					}
					var automerge any
					if auto {
						automerge = map[string]string{"merge_method": "squash"}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "state": "open", "head": map[string]string{"sha": sha}, "base": map[string]string{"ref": "main"}, "labels": labels, "auto_merge": automerge})
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
					_, _ = w.Write([]byte(`[]`))
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
					_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":99,"name":"tests","status":"completed","conclusion":"success"}]}`))
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
					statuses := []repositoryMonitorCommitStatus{}
					if status, ok := latest[sha]; ok {
						statuses = append(statuses, status)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "total_count": len(statuses), "statuses": statuses})
				case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
					var status repositoryMonitorCommitStatus
					if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
						t.Error(err)
					}
					if status.Context != repositoryMonitorReadinessContext(monitor) || len(status.Description) > 140 {
						t.Errorf("invalid public status: %+v", status)
					}
					status.ID = int64(100 + len(writes))
					writes = append(writes, status)
					latest[sha] = status
					if failAudit {
						audit.failNext = true
						failAudit = false
					}
					_ = json.NewEncoder(w).Encode(status)
				default:
					t.Errorf("unexpected mutation or request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			r.GitHubAPIBaseURL = server.URL
			review := seedRepositoryMonitorAutomergeReview(t, ctx, db, monitor.Name, 1, sha)
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "1", State: "open", Number: 1, HeadSHA: sha, LastReviewID: review, LastReviewedHeadSHA: sha, LastVerdict: repositoryMonitorReviewVerdictPassed}
			pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: sha, State: "open", BaseBranch: "main"}
			reconcile := func(want string) {
				t.Helper()
				if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, item); err != nil {
					t.Fatal(err)
				}
				if latest[sha].State != want {
					t.Fatalf("status=%+v, want %s", latest[sha], want)
				}
			}
			reconcile(repositoryMonitorStatusSuccess)
			reconcile(repositoryMonitorStatusSuccess)
			if len(writes) != 2 || writes[0].State != repositoryMonitorStatusPending {
				t.Fatalf("non-idempotent initial statuses: %+v", writes)
			}

			// A queued rerun must revoke old success before its Task has started.
			action := &store.WorkAction{ID: "new-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 1, TargetSHA: sha, DesiredAction: "review", Status: "queued"}
			if err := db.CreateWorkAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			reconcile(repositoryMonitorStatusPending)
			action.Status = "succeeded"
			if err := db.UpdateWorkAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			reconcile(repositoryMonitorStatusSuccess)

			// Simulate a successful pending POST followed by a failed durable audit.
			action.Status = "queued"
			if err := db.UpdateWorkAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			failAudit = true
			if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, item); err == nil {
				t.Fatal("expected audit failure")
			}
			action.Status = "succeeded"
			if err := db.UpdateWorkAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			before := len(writes)
			reconcile(repositoryMonitorStatusSuccess)
			if writes[before].State != repositoryMonitorStatusPending {
				t.Fatal("uncertain write did not recover through pending")
			}

			paused = true
			reconcile(repositoryMonitorStatusFailure)
			paused = false
			reconcile(repositoryMonitorStatusSuccess)
			sha, pr.HeadSHA = "head-2", "head-2"
			reconcile(repositoryMonitorStatusPending)
		})
	}
}

type readinessStatusAuditFailureStore struct {
	store.RepositoryMonitorStore
	failNext bool
}

func (s *readinessStatusAuditFailureStore) UpdateGitHubMutationRecord(ctx context.Context, record *store.GitHubMutationRecord) error {
	if s.failNext && record.Status == repositoryMonitorRunPhaseSucceeded {
		s.failNext = false
		return errors.New("injected audit write failure")
	}
	return s.RepositoryMonitorStore.UpdateGitHubMutationRecord(ctx, record)
}

func TestRepositoryMonitorCIExcludesOnlyPersistedOwnedStatusIDs(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("first")
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	for i, namespace := range []string{monitor.Namespace, "another-namespace"} {
		peer := monitor.DeepCopy()
		peer.Name, peer.Namespace = fmt.Sprintf("peer-%d", i), namespace
		if err := r.recordRepositoryMonitorGitHubMutation(ctx, peer, &store.GitHubMutationRecord{ID: peer.Name, Operation: repositoryMonitorReadinessOperation, TargetSHA: "head", ExternalID: strconv.Itoa(10 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	foreign, failedCheck := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/check-runs") {
			conclusion := "success"
			if failedCheck {
				conclusion = "failure"
			}
			// A check-run ID matching a status ID must never be excluded.
			_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":%q}]}`, conclusion)
			return
		}
		statuses := []repositoryMonitorCommitStatus{{ID: 10, Context: "orka/peer-0/ready", State: "pending"}, {ID: 11, Context: "orka/peer-1/ready", State: "pending"}}
		if foreign {
			statuses = append(statuses, repositoryMonitorCommitStatus{ID: 12, Context: "orka/peer-0/ready", State: "failure"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "total_count": len(statuses), "statuses": statuses})
	}))
	defer server.Close()
	r.GitHubAPIBaseURL = server.URL
	assertCI := func(sha string, want bool) {
		t.Helper()
		ci, err := r.repositoryMonitorCheckCI(ctx, monitor, sha)
		if err != nil || ci.passed != want {
			t.Fatalf("CI=%+v err=%v, want passed=%v", ci, err, want)
		}
	}
	assertCI("head", true)
	assertCI("different-head", false)
	foreign = true
	assertCI("head", false)
	foreign, failedCheck = false, true
	assertCI("head", false)
}

func TestRepositoryMonitorStopGuardSurvivesInventory(t *testing.T) {
	monitor, _ := repositoryMonitorInventoryTestObjects("stop-audit")
	old := &store.MonitorItem{HeadSHA: "head", SkipReason: repositoryMonitorIssueSkipStoppedByCommand, RepairState: repositoryMonitorRepairPhaseFailed}
	current := repositoryMonitorItemFromPullRequest(monitor, repositoryMonitorPullRequest{Number: 1, HeadSHA: "head"}, old)
	if current.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
		t.Fatalf("stop guard lost during inventory: %q", current.SkipReason)
	}
	repositoryMonitorResetItemAfterRepairPush(current)
	if current.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
		t.Fatal("late repair cleared an explicit stop")
	}
}
func TestRepositoryMonitorOldFailureDoesNotBlockNewHead(t *testing.T) {
	monitor, _ := repositoryMonitorInventoryTestObjects("head-audit")
	old := &store.MonitorItem{HeadSHA: "old", RepairState: repositoryMonitorRepairPhaseFailed}
	current := repositoryMonitorItemFromPullRequest(monitor, repositoryMonitorPullRequest{Number: 1, HeadSHA: "new"}, old)
	if current.RepairState == repositoryMonitorRepairPhaseFailed {
		t.Fatal("old-head repair failure is projected onto a new head")
	}
}

func TestRepositoryMonitorCommitStatusPagination(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint("truncated_", truncated), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				page, err := strconv.Atoi(req.URL.Query().Get("page"))
				if err != nil || req.URL.Query().Get("per_page") != "100" {
					t.Errorf("invalid pagination: %s", req.URL.RawQuery)
				}
				statuses := []repositoryMonitorCommitStatus{}
				count := 100
				if truncated {
					count = 99
				}
				if page == 3 {
					count = 1
				}
				for i := 0; i < count; i++ {
					state := repositoryMonitorStatusSuccess
					if page == 3 {
						state = repositoryMonitorStatusFailure
					}
					statuses = append(statuses, repositoryMonitorCommitStatus{ID: int64(page*100 + i), Context: fmt.Sprint("ci-", page, "-", i), State: state})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 201, "state": "success", "statuses": statuses})
			}))
			defer server.Close()
			r := &RepositoryMonitorReconciler{}
			ci, err := r.repositoryMonitorCheckCommitStatus(context.Background(), server.URL, "owner", "repo", "", "head")
			wantReason, wantCalls := repositoryMonitorCINotGreen, 3
			if truncated {
				wantReason, wantCalls = "ci_checks_incomplete", 1
			}
			if err != nil || ci.passed || ci.reason != wantReason || calls != wantCalls {
				t.Fatalf("CI=%+v err=%v calls=%d, want reason=%s calls=%d", ci, err, calls, wantReason, wantCalls)
			}
		})
	}
}

func TestRepositoryMonitorReadinessContextSeparatesNamespaces(t *testing.T) {
	first, _ := repositoryMonitorInventoryTestObjects("same-name")
	second := first.DeepCopy()
	second.Namespace = "another-namespace"
	if repositoryMonitorReadinessContext(first) == repositoryMonitorReadinessContext(second) {
		t.Fatal("peer namespaces share a readiness context")
	}
}

func TestRepositoryMonitorReadinessRefreshQueuesConflictRepair(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("fresh-conflict")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Review.Publish.Enabled = true
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	writes := 0
	latest := repositoryMonitorCommitStatus{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/31"):
			_, _ = w.Write([]byte(`{"number":31,"state":"open","mergeable_state":"dirty","head":{"sha":"head"},"base":{"ref":"main"}}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":99,"name":"tests","status":"completed","conclusion":"success"}]}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "statuses": []repositoryMonitorCommitStatus{latest}})
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
			var status repositoryMonitorCommitStatus
			if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
				t.Error(err)
			}
			writes++
			status.ID = int64(writes)
			latest = status
			if status.State == repositoryMonitorStatusSuccess {
				t.Error("conflicted PR received a successful readiness status")
			}
			_ = json.NewEncoder(w).Encode(status)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	r.GitHubAPIBaseURL = server.URL
	pr := repositoryMonitorPullRequest{Number: 31, State: "open", HeadSHA: "head", BaseBranch: "main", HeadRepo: "orka-agents/orka"}
	item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
	item.LastReviewID = seedRepositoryMonitorAutomergeReview(t, ctx, db, monitor.Name, pr.Number, pr.HeadSHA)
	item.LastReviewedHeadSHA, item.LastVerdict = pr.HeadSHA, repositoryMonitorReviewVerdictPassed
	if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, item, []repositoryMonitorPullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	if latest.State != repositoryMonitorStatusPending {
		t.Fatalf("conflict readiness=%+v, want pending", latest)
	}
	handled, err := r.tryRepositoryMonitorAutomaticRepair(ctx, monitor, &store.MonitorRun{ID: "fresh-conflict-run"}, "orka-agents", "orka", pr, item)
	if err != nil || !handled {
		t.Fatalf("conflict repair selection: handled=%v err=%v", handled, err)
	}
	commands, _, err := db.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
	if err != nil || len(commands) != 1 || commands[0].Intent != repositoryMonitorCommandIntentUpdateBranch {
		t.Fatalf("conflict did not queue update-branch: commands=%+v err=%v", commands, err)
	}
}

func TestRepositoryMonitorAutomaticAgentRepairRequiresRepairer(t *testing.T) {
	for _, verdict := range []string{"", repositoryMonitorReviewVerdictNeedsChanges} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("missing-repairer")
			monitor.Spec.Repair.Enabled = true
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			defer server.Close()
			r.GitHubAPIBaseURL = server.URL
			pr := repositoryMonitorPullRequest{Number: 31, State: "open", HeadSHA: "head", BaseBranch: "main", HeadRepo: "orka-agents/orka", MergeableState: "clean"}
			item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
			item.LastVerdict, item.LastReviewedHeadSHA, item.LastReviewID = verdict, pr.HeadSHA, "review"
			handled, err := r.tryRepositoryMonitorAutomaticRepair(t.Context(), monitor, &store.MonitorRun{}, "orka-agents", "orka", pr, item)
			if err != nil || handled || requests != 0 {
				t.Fatalf("agent repair without repairer: handled=%v requests=%d err=%v", handled, requests, err)
			}
			commands, _, err := db.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
			if err != nil || len(commands) != 0 {
				t.Fatalf("missing repairer queued commands: %+v err=%v", commands, err)
			}
		})
	}
}

func TestRepositoryMonitorInventoryReusesReadinessPullRequestList(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("reuse-pr-inventory")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Review.Publish.Enabled = true
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	prs := make([]map[string]any, 0, 3)
	for number := range 3 {
		prs = append(prs, map[string]any{"number": number + 1, "state": "open", "head": map[string]string{"sha": fmt.Sprint("head-", number)}, "base": map[string]string{"ref": "main"}, "labels": []map[string]string{{"name": "orka:pause"}}})
	}
	lists, writes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			lists++
			_ = json.NewEncoder(w).Encode(prs)
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/pulls/"):
			number, err := strconv.Atoi(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:])
			if err != nil || number < 1 || number > len(prs) {
				t.Errorf("unexpected PR request: %s", req.URL.Path)
				http.Error(w, "unexpected", 500)
				return
			}
			_ = json.NewEncoder(w).Encode(prs[number-1])
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
			var status repositoryMonitorCommitStatus
			if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
				t.Error(err)
			}
			writes++
			status.ID = int64(writes)
			_ = json.NewEncoder(w).Encode(status)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	r.GitHubAPIBaseURL = server.URL
	run := &store.MonitorRun{ID: "reuse-pr-inventory-run", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind}
	_, _, skipped, err := r.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka")
	if err != nil || skipped != len(prs) || lists != 1 || writes != 2*len(prs) {
		t.Fatalf("inventory requests: lists=%d writes=%d skipped=%d err=%v", lists, writes, skipped, err)
	}
}

//nolint:gocyclo // Exercise blockers, ordering, idempotency, inventory reuse, and peer closure on one shared head.
func TestRepositoryMonitorReadinessAggregatesSharedHeads(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("shared-head")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Review.Publish.Enabled = true
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	peerClosed, firstDraft, reversePeers := false, false, false
	latest := repositoryMonitorCommitStatus{}
	writes, lists := 0, 0
	prJSON := func(number int) map[string]any {
		labels := []map[string]string{}
		if number == 2 {
			labels = append(labels, map[string]string{"name": "orka:pause"})
		}
		return map[string]any{"number": number, "state": "open", "head": map[string]string{"sha": "head"}, "base": map[string]string{"ref": "main"}, "labels": labels, "draft": number == 1 && firstDraft}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1"):
			_ = json.NewEncoder(w).Encode(prJSON(1))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/2"):
			_ = json.NewEncoder(w).Encode(prJSON(2))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			lists++
			prs := []any{prJSON(1)}
			if !peerClosed {
				prs = append(prs, prJSON(2))
				if reversePeers {
					prs[0], prs[1] = prs[1], prs[0]
				}
			}
			_ = json.NewEncoder(w).Encode(prs)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":20,"name":"tests","status":"completed","conclusion":"success"}]}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "statuses": []repositoryMonitorCommitStatus{latest}})
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
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
		item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: strconv.FormatInt(number, 10), Number: number, State: "open", HeadSHA: "head", LastReviewID: review, LastReviewedHeadSHA: "head", LastVerdict: repositoryMonitorReviewVerdictPassed}
		if err := db.UpsertMonitorItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	reconcile := func(item *store.MonitorItem, want string, inventory ...[]repositoryMonitorPullRequest) {
		t.Helper()
		pr := repositoryMonitorPullRequest{Number: item.Number, HeadSHA: "head", State: "open", BaseBranch: "main"}
		if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, item, inventory...); err != nil {
			t.Fatal(err)
		}
		if latest.State != want {
			t.Fatalf("shared status=%+v, want %s", latest, want)
		}
	}
	reconcile(items[1], repositoryMonitorStatusFailure)
	reconcile(items[0], repositoryMonitorStatusFailure)
	reconcile(items[1], repositoryMonitorStatusFailure)
	if writes != 2 {
		t.Fatalf("identical blocking outcome wrote %d statuses, want pending then failure", writes)
	}
	// Different persistent blockers must produce one deterministic status,
	// regardless of the PR being reconciled or the inventory response order.
	firstDraft = true
	reconcile(items[0], repositoryMonitorStatusFailure)
	stableWrites := writes
	for range 3 {
		reversePeers = !reversePeers
		reconcile(items[1], repositoryMonitorStatusFailure)
		reconcile(items[0], repositoryMonitorStatusFailure)
	}
	if writes != stableWrites {
		t.Fatalf("unchanged distinct blockers wrote %d extra statuses", writes-stableWrites)
	}
	// Reusing inventory must preserve the peer's blocker without another list.
	beforeLists := lists
	inventory := []repositoryMonitorPullRequest{
		{Number: 1, HeadSHA: "head", State: "open", BaseBranch: "main", Draft: firstDraft},
		{Number: 2, HeadSHA: "head", State: "open", BaseBranch: "main", Labels: []string{"orka:pause"}},
	}
	reconcile(items[0], repositoryMonitorStatusFailure, inventory)
	reconcile(items[1], repositoryMonitorStatusFailure, inventory)
	if lists != beforeLists || writes != stableWrites {
		t.Fatalf("inventory reuse changed the shared outcome: lists=%d writes=%d", lists-beforeLists, writes-stableWrites)
	}
	firstDraft = false
	// Once the peer closes, the shared audit must permit a fresh success. A
	// per-PR digest would incorrectly suppress it as an unchanged old success.
	peerClosed = true
	reconcile(items[0], repositoryMonitorStatusSuccess)
	records, _, err := db.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Operation: repositoryMonitorReadinessOperation})
	if err != nil || len(records) != 1 || records[0].TargetKind != "commit" || records[0].TargetNumber != 0 {
		t.Fatalf("audit is not commit-scoped: %+v %v", records, err)
	}
}
