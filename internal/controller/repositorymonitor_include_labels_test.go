package controller

import (
	"context"
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

func TestRepositoryMonitorPullRequestIncludeLabels(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	monitor.Spec.Branch = "main"
	labelled := repositoryMonitorPullRequest{State: "open", BaseBranch: "main", Labels: []string{"Orka:Review"}}
	unlabelled := repositoryMonitorPullRequest{State: "open", BaseBranch: "main"}
	if !repositoryMonitorPullRequestIncluded(monitor.Spec, unlabelled) || !repositoryMonitorPullRequestInScope(monitor, unlabelled) {
		t.Fatal("an empty includeLabels list must include every pull request")
	}
	monitor.Spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	if !repositoryMonitorPullRequestIncluded(monitor.Spec, labelled) || !repositoryMonitorPullRequestInScope(monitor, labelled) {
		t.Fatal("a pull request with an include label (any case) must be in scope")
	}
	if repositoryMonitorPullRequestIncluded(monitor.Spec, unlabelled) || repositoryMonitorPullRequestInScope(monitor, unlabelled) {
		t.Fatal("a pull request without an include label must be out of scope")
	}
	if got := filterRepositoryMonitorIncludedPullRequests(monitor.Spec, []repositoryMonitorPullRequest{unlabelled, labelled}); len(got) != 1 || got[0].Labels[0] != "Orka:Review" {
		t.Fatalf("filtered pull requests = %#v, want only the labelled one", got)
	}
}

func TestRepositoryMonitorIncludeLabelsRejectIssueImplementation(t *testing.T) {
	spec := corev1alpha1.RepositoryMonitorSpec{}
	spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	if err := validateRepositoryMonitorSupportedTargets(spec); err != nil {
		t.Fatalf("includeLabels without issue implementation: %v", err)
	}
	spec.Targets.Issues.Enabled = true
	spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	if err := validateRepositoryMonitorSupportedTargets(spec); err == nil || !strings.Contains(err.Error(), "includeLabels") {
		t.Fatalf("includeLabels with issue implementation error = %v, want rejection", err)
	}
	disabled := false
	spec.IssueWorkflow.Implementation.Enabled = &disabled
	if err := validateRepositoryMonitorSupportedTargets(spec); err != nil {
		t.Fatalf("includeLabels with implementation disabled: %v", err)
	}
}

func TestRepositoryMonitorInventoryScopesPullRequestsByIncludeLabels(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"title":"Labelled","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"labelled","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[{"name":"Orka:Review"}]},
		{"number":2,"title":"Unlabelled","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"unlabelled","sha":"sha2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}
	]`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("include-labels")
	monitor.Spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	// PR #2 was tracked before its include label was removed.
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "2", Number: 2, State: repositoryMonitorItemStateOpen, HeadSHA: "sha2"}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-include-labels", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: "manual", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil {
		t.Fatalf("List tasks error = %v", err)
	}
	if len(tasks.Items) != 1 || !strings.Contains(tasks.Items[0].Name, "-1-sha1-") {
		names := make([]string, 0, len(tasks.Items))
		for _, task := range tasks.Items {
			names = append(names, task.Name)
		}
		t.Fatalf("review tasks = %v, want one for the labelled pull request", names)
	}
	labelled, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil || labelled.LastVerdict != repositoryMonitorRunPhaseQueued {
		t.Fatalf("labelled item = %#v err=%v, want queued review", labelled, err)
	}
	unlabelled, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "2")
	if err != nil || unlabelled.State != repositoryMonitorItemStateOutOfScope || unlabelled.SkipReason != repositoryMonitorSkipReasonMissing {
		t.Fatalf("unlabelled item = %#v err=%v, want retired out of scope", unlabelled, err)
	}
}

func TestRepositoryMonitorFixCommandBlockedWithoutIncludeLabel(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorSinglePullRequestServerWithBodyAndAuth(t, 31, `{"number":31,"title":"Fix me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-fix","sha":"head31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`, "Bearer forge-token")
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("include-labels-fix")
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-include-fix-31", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 31, Intent: "fix", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-include-fix-31", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: "api_command", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 {
		t.Fatalf("tasks = %d err=%v, want no repair task for an excluded pull request", len(tasks.Items), err)
	}
	events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: monitor.Name, EventType: "command_target_blocked", Limit: 5})
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Summary, "target_not_included") {
		t.Fatalf("blocked events = %#v err=%v, want target_not_included", events, err)
	}
}

func TestRepositoryMonitorIncludeLabelsScopeReadinessStatuses(t *testing.T) {
	ctx := t.Context()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("include-ready")
	monitor.Spec.Review.Publish.Enabled = true
	monitor.Spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	labelled := true
	latest := map[string]repositoryMonitorCommitStatus{}
	writes := map[string]int{}
	prJSON := func(number int64) map[string]any {
		labels := []map[string]string{}
		if number == 1 && labelled {
			labels = append(labels, map[string]string{"name": "orka:review"})
		}
		return map[string]any{"number": number, "state": "open", "head": map[string]string{"sha": fmt.Sprintf("head%d", number)}, "base": map[string]string{"ref": "main"}, "labels": labels}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]any{prJSON(1), prJSON(2)})
		case req.Method == http.MethodGet && strings.Contains(path, "/pulls/"):
			number, _ := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
			_ = json.NewEncoder(w).Encode(prJSON(number))
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":99,"name":"tests","status":"completed","conclusion":"success"}]}`))
		case req.Method == http.MethodGet && strings.HasSuffix(path, "/status"):
			sha := strings.TrimSuffix(path[strings.Index(path, "/commits/")+len("/commits/"):], "/status")
			statuses := []repositoryMonitorCommitStatus{}
			if status, ok := latest[sha]; ok {
				statuses = append(statuses, status)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(statuses), "statuses": statuses})
		case req.Method == http.MethodPost && strings.Contains(path, "/statuses/"):
			sha := path[strings.LastIndex(path, "/")+1:]
			var status repositoryMonitorCommitStatus
			if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
				t.Error(err)
			}
			writes[sha]++
			status.ID = int64(100 + writes[sha])
			latest[sha] = status
			_ = json.NewEncoder(w).Encode(status)
		case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/actions/runs":
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", req.Method, path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	r.GitHubAPIBaseURL = server.URL
	for _, number := range []int64{1, 2} {
		head := fmt.Sprintf("head%d", number)
		review := seedRepositoryMonitorAutomergeReview(t, ctx, db, monitor.Name, number, head)
		if err := db.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: fmt.Sprint(number), Number: number, State: "open", BaseBranch: "main", HeadSHA: head, LastReviewID: review, LastReviewedHeadSHA: head, LastVerdict: repositoryMonitorReviewVerdictPassed}); err != nil {
			t.Fatal(err)
		}
	}
	runInventory := func(id string) {
		t.Helper()
		run := &store.MonitorRun{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind}
		if _, _, _, err := r.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka"); err != nil {
			t.Fatal(err)
		}
	}
	runInventory("include-ready-1")
	if latest["head1"].State != repositoryMonitorStatusSuccess || writes["head2"] != 0 {
		t.Fatalf("statuses = %+v writes = %v, want success on the labelled head only", latest, writes)
	}
	labelled = false
	runInventory("include-ready-2")
	if latest["head1"].State != repositoryMonitorStatusFailure || writes["head2"] != 0 {
		t.Fatalf("statuses = %+v writes = %v, want the unlabelled head revoked and the other untouched", latest, writes)
	}
}
