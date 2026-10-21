package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRepositoryMonitorRepairRepository(t *testing.T) {
	tests := []struct {
		name       string
		allowForks bool
		headRepo   string
		canModify  bool
		wantRepo   string
		wantReason string
	}{
		{name: "same repository", headRepo: "Orka-Agents/Orka", wantRepo: "orka-agents/orka"},
		{name: "fork without opt-in", headRepo: "contributor/orka", canModify: true, wantReason: "fork_pr_repair_not_writable"},
		{name: "deleted fork", allowForks: true, canModify: true, wantReason: "fork_pr_repair_not_writable"},
		{name: "malformed fork", allowForks: true, headRepo: "contributor/orka/extra", canModify: true, wantReason: "fork_pr_repair_not_writable"},
		{name: "maintainer edits disabled", allowForks: true, headRepo: "contributor/orka", wantReason: "fork_pr_maintainer_edits_disabled"},
		{name: "writable fork", allowForks: true, headRepo: "contributor/orka", canModify: true, wantRepo: "contributor/orka"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			monitor := &corev1alpha1.RepositoryMonitor{}
			monitor.Spec.Repair.AllowForkPullRequests = tt.allowForks
			pr := repositoryMonitorPullRequest{HeadRepo: tt.headRepo, MaintainerCanModify: tt.canModify}
			repo, reason := repositoryMonitorRepairRepository(monitor, "orka-agents/orka", pr)
			if repo != tt.wantRepo || reason != tt.wantReason {
				t.Fatalf("repositoryMonitorRepairRepository() = (%q, %q), want (%q, %q)", repo, reason, tt.wantRepo, tt.wantReason)
			}
		})
	}
}

func repositoryMonitorForkPullRequestBody(canModify bool) string {
	return fmt.Sprintf(`{"number":31,"title":"Fix me","state":"open","draft":false,"mergeable_state":"clean","maintainer_can_modify":%t,"user":{"login":"contributor"},"base":{"ref":"main","sha":"base31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"main","sha":"head31","repo":{"full_name":"contributor/orka","clone_url":"https://github.com/contributor/orka.git"}},"labels":[]}`, canModify)
}

func newRepositoryMonitorForkRepairTestReconciler(t *testing.T, name, body string, allowForks bool) (*RepositoryMonitorReconciler, crclient.Client, *corev1alpha1.RepositoryMonitor) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorSinglePullRequestServerWithBodyAndAuth(t, 31, body, "Bearer forge-token")
	original := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/check-runs") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":"failure"}]}`))
			return
		}
		original.ServeHTTP(w, req)
	})
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects(name)
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Repair.AllowForkPullRequests = allowForks
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	monitorStore := setupControllerSQLiteStore(t)
	return &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}, cl, monitor
}

func assertRepositoryMonitorForkRepairTask(t *testing.T, ctx context.Context, cl crclient.Client, monitor *corev1alpha1.RepositoryMonitor, taskName string) {
	t.Helper()
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: taskName}, &task); err != nil {
		t.Fatalf("Get repair task() error = %v", err)
	}
	workspace := task.Spec.Workspace
	if workspace == nil || workspace.GitRepo != "https://github.com/contributor/orka" || workspace.PublicationGitRepo != "https://github.com/contributor/orka" ||
		workspace.Branch != "main" || workspace.PushBranch != "main" || workspace.Ref != "head31" || workspace.ExpectedRemoteSHA != "head31" {
		t.Fatalf("repair workspace = %#v, want exact-head publication to the fork branch", workspace)
	}
	wantSession := repositoryMonitorPublicationSessionName(monitor, "contributor/orka:main")
	if task.Spec.SessionRef == nil || task.Spec.SessionRef.Name != wantSession || task.Spec.SessionRef.Name == repositoryMonitorPublicationSessionName(monitor, "main") {
		t.Fatalf("repair task session = %#v, want fork-scoped branch owner %q", task.Spec.SessionRef, wantSession)
	}
}

func TestRepositoryMonitorForkPullRequestFixCommand(t *testing.T) {
	tests := []struct {
		name       string
		allowForks bool
		canModify  bool
		wantReason string
	}{
		{name: "maintainer edits allowed", allowForks: true, canModify: true},
		{name: "maintainer edits disabled", allowForks: true, wantReason: "fork_pr_maintainer_edits_disabled"},
		{name: "forks not enabled", canModify: true, wantReason: "fork_pr_repair_not_writable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			reconciler, cl, monitor := newRepositoryMonitorForkRepairTestReconciler(t, "pr-fork-fix", repositoryMonitorForkPullRequestBody(tt.canModify), tt.allowForks)
			processedAt := time.Now()
			command := &store.CommandEvent{ID: "cmd-fork-fix-31", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 31, Intent: "fix", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
			if err := reconciler.Store.CreateCommandEvent(ctx, command); err != nil {
				t.Fatalf("CreateCommandEvent() error = %v", err)
			}
			if err := reconciler.Store.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-fork-fix-31", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: "github_label_command", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatalf("CreateMonitorRun() error = %v", err)
			}
			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			item, err := reconciler.Store.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "31")
			if err != nil {
				t.Fatalf("GetMonitorItem() error = %v", err)
			}
			jobs, _, err := reconciler.Store.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "default", MonitorName: monitor.Name, PRNumber: 31, Limit: 10})
			if err != nil {
				t.Fatalf("ListRepairJobs() error = %v", err)
			}
			if tt.wantReason != "" {
				var tasks corev1alpha1.TaskList
				if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 || len(jobs) != 0 {
					t.Fatalf("blocked repair created tasks=%d jobs=%d err=%v", len(tasks.Items), len(jobs), err)
				}
				if item.RepairState != repositoryMonitorRepairPhaseFailed || item.SkipReason != tt.wantReason {
					t.Fatalf("item = %#v, want repair blocked by %q", item, tt.wantReason)
				}
				return
			}
			if item.RepairState != repositoryMonitorRepairPhaseQueued || len(jobs) != 1 || jobs[0].TaskName == "" || jobs[0].Repo != "orka-agents/orka" || jobs[0].Branch != "main" {
				t.Fatalf("item = %#v jobs = %#v, want one queued repair job", item, jobs)
			}
			assertRepositoryMonitorForkRepairTask(t, ctx, cl, monitor, jobs[0].TaskName)

			// The delivery receipt must name the fork as its publication repository.
			markRepositoryMonitorTestTaskDelivered(t, ctx, cl, jobs[0].TaskName, "main", "pushed31")
			if _, err := reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil {
				t.Fatalf("ingestCompletedRepositoryMonitorRepairTasks() error = %v", err)
			}
			job, err := reconciler.Store.GetRepairJob(ctx, "default", jobs[0].ID)
			if err != nil {
				t.Fatalf("GetRepairJob() error = %v", err)
			}
			if job.Phase != repositoryMonitorRepairPhaseSucceeded || job.PushedSHA != "pushed31" || job.LastError != "" {
				t.Fatalf("repair job = %#v, want verified fork delivery", job)
			}
		})
	}
}

func TestRepositoryMonitorAutomaticallyRepairsWritableFork(t *testing.T) {
	ctx := context.Background()
	reconciler, cl, monitor := newRepositoryMonitorForkRepairTestReconciler(t, "pr-fork-auto", repositoryMonitorForkPullRequestBody(true), true)
	if err := reconciler.Store.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-fork-auto-31", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: "pull_request_event", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	// The first reconciliation queues the policy command; the second runs it.
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	commands, _, err := reconciler.Store.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 10})
	if err != nil || len(commands) != 1 || commands[0].Intent != repositoryMonitorCommandIntentFixCI {
		t.Fatalf("automatic commands = %#v err=%v, want one fix_ci command", commands, err)
	}
	jobs, _, err := reconciler.Store.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "default", MonitorName: monitor.Name, PRNumber: 31, Limit: 10})
	if err != nil || len(jobs) != 1 || jobs[0].TaskName == "" {
		t.Fatalf("repair jobs = %#v err=%v, want one queued repair job", jobs, err)
	}
	assertRepositoryMonitorForkRepairTask(t, ctx, cl, monitor, jobs[0].TaskName)
}

func TestRepositoryMonitorReadinessRefreshTracksMaintainerEdits(t *testing.T) {
	for _, canModify := range []bool{true, false} {
		server := newRepositoryMonitorSinglePullRequestServerWithBodyAndAuth(t, 31, repositoryMonitorForkPullRequestBody(canModify), "Bearer forge-token")
		reconciler := &RepositoryMonitorReconciler{}
		pr, err := reconciler.refreshRepositoryMonitorReadinessPullRequest(context.Background(), repositoryMonitorPullRequest{Number: 31, MaintainerCanModify: !canModify}, server.URL+"/repos/orka-agents/orka", "forge-token")
		server.Close()
		if err != nil {
			t.Fatalf("refreshRepositoryMonitorReadinessPullRequest() error = %v", err)
		}
		if pr.MaintainerCanModify != canModify {
			t.Fatalf("refreshed MaintainerCanModify = %t, want %t", pr.MaintainerCanModify, canModify)
		}
	}
}
