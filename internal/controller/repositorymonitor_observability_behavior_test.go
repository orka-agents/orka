package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// These tests pin what the repository monitor makes visible today: the
// readiness commit status a held pull request receives, the records a repair
// leaves across a controller restart, and how a GitHub timeout fails a run.

// A held draft gets its readiness status once per head: repeated polls of an
// unchanged head write nothing new, and each new head is evaluated afresh.
func TestRepositoryMonitorReadinessDraftHoldIsWrittenOncePerHead(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("draft-hold")
	monitor.Spec.Review.Publish.Enabled = true
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	forge := &repositoryMonitorDraftReadinessForge{t: t, sha: "head-1", draft: true, writes: map[string][]repositoryMonitorCommitStatus{}}
	server := httptest.NewServer(repositoryMonitorTestWithoutHeldWorkflowRuns(forge.ServeHTTP))
	defer server.Close()
	r.GitHubAPIBaseURL = server.URL

	pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: "head-1", State: "open", BaseBranch: "main", Draft: true}
	item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
	poll := func() {
		t.Helper()
		if err := r.reconcileRepositoryMonitorReadiness(ctx, monitor, &pr, item); err != nil {
			t.Fatal(err)
		}
	}
	const held = repositoryMonitorStatusFailure + ": The pull request is a draft."
	const claimed = repositoryMonitorStatusPending + ": Waiting for verified workflow completion."

	for range 5 {
		poll()
	}
	if got := forge.descriptions("head-1"); len(got) != 2 || got[0] != claimed || got[1] != held {
		t.Fatalf("head-1 statuses after 5 polls = %q, want the claim then one draft hold", got)
	}
	mutation, err := db.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorReadinessMutationID(monitor, "head-1"))
	if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.Reason != "The pull request is a draft." || mutation.ExternalID != "101" {
		t.Fatalf("head-1 readiness mutation = %+v err=%v, want the draft hold recorded", mutation, err)
	}

	// A new head is still a draft: it gets its own status, written once.
	forge.set("head-2", true)
	pr.HeadSHA = "head-2"
	item = repositoryMonitorItemFromPullRequest(monitor, pr, item)
	for range 3 {
		poll()
	}
	if got := forge.descriptions("head-2"); len(got) != 2 || got[0] != claimed || got[1] != held {
		t.Fatalf("head-2 statuses = %q, want the claim then one draft hold", got)
	}

	// Marking it ready replaces the hold once with the next blocker.
	forge.set("head-2", false)
	for range 3 {
		poll()
	}
	got := forge.descriptions("head-2")
	if len(got) != 3 || got[2] == held || !strings.HasPrefix(got[2], repositoryMonitorStatusPending+": ") {
		t.Fatalf("head-2 statuses after ready = %q, want one pending status replacing the draft hold", got)
	}
}

// A repair handoff writes its repair job and started push record before it
// creates the Task, all keyed by the command. A controller that restarts at
// any point, before the Task exists, after it exists, or after the result was
// ingested, replays the handoff without a second Task, job or push record, and
// never reopens a settled push.
func TestRepositoryMonitorRepairRestartReusesTaskAndPushRecord(t *testing.T) {
	ctx := context.Background()
	monitor, first, monitorStore := newRepositoryMonitorPolicyReconciler(t, "repair-restart", nil, nil)
	command := &store.CommandEvent{ID: "cmd-repair-restart", Intent: repositoryMonitorCommandIntentFix, Source: "api"}
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", BaseSHA: "base31", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: pr.HeadSHA}
	run := &store.MonitorRun{ID: "run-repair-restart"}
	// restart returns a new reconciler over the same cluster and store.
	restart := func() *RepositoryMonitorReconciler {
		return &RepositoryMonitorReconciler{Client: first.Client, Scheme: first.Scheme, Store: first.Store, ResultStore: first.ResultStore, GitHubAPIBaseURL: first.GitHubAPIBaseURL}
	}
	handoff := func(r *RepositoryMonitorReconciler, wantCreated int) {
		t.Helper()
		if created, _, err := r.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, run, command, "orka-agents", "orka", pr, item, 1, 1); err != nil || created != wantCreated {
			t.Fatalf("repair Task handoff = (%d, %v), want created=%d", created, err, wantCreated)
		}
	}
	jobs := func() []store.RepairJob {
		t.Helper()
		records, _, err := monitorStore.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: 31, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return records
	}
	pushes := func() []store.GitHubMutationRecord {
		t.Helper()
		records, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Operation: pushBranchAction, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return records
	}
	assertOne := func(stage, wantPush string) {
		t.Helper()
		if tasks := repositoryMonitorTestTaskCount(t, first); tasks != 1 {
			t.Fatalf("%s: tasks = %d, want 1", stage, tasks)
		}
		if got := jobs(); len(got) != 1 {
			t.Fatalf("%s: repair jobs = %+v, want one", stage, got)
		}
		if got := pushes(); len(got) != 1 || got[0].Status != wantPush || got[0].CommandEventID != command.ID {
			t.Fatalf("%s: push records = %+v, want one %s record for the command", stage, got, wantPush)
		}
	}

	handoff(first, 1)
	assertOne("first handoff", repositoryMonitorAutomergeStateStarted)

	// The controller stopped after recording the job and push but before the
	// Task was created: the restarted controller creates it against the same
	// records.
	task := &corev1alpha1.Task{}
	if err := first.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: jobs()[0].TaskName}, task); err != nil {
		t.Fatal(err)
	}
	if err := first.Delete(ctx, task); err != nil {
		t.Fatal(err)
	}
	handoff(restart(), 1)
	assertOne("restart before Task creation", repositoryMonitorAutomergeStateStarted)

	// The controller stopped after creating the Task: the restart adopts it.
	handoff(restart(), 0)
	assertOne("restart after Task creation", repositoryMonitorAutomergeStateStarted)

	markRepositoryMonitorTestTaskDelivered(t, ctx, first.Client, jobs()[0].TaskName, pr.HeadBranch, "pushed31")
	if ingested, err := restart().ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil || !ingested {
		t.Fatalf("ingest = (%t, %v), want the delivered repair ingested", ingested, err)
	}
	assertOne("ingest", repositoryMonitorRunPhaseSucceeded)

	// A restart after ingestion that replays the handoff and the ingest
	// neither creates work nor reopens the settled push.
	after := restart()
	handoff(after, 0)
	if ingested, err := after.ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil || ingested {
		t.Fatalf("replayed ingest = (%t, %v), want nothing to ingest", ingested, err)
	}
	assertOne("restart after ingest", repositoryMonitorRunPhaseSucceeded)
	if got := pushes(); got[0].ExternalID != "pushed31" {
		t.Fatalf("push record = %+v, want pushed31 kept", got[0])
	}
}

// A GitHub call that exceeds the client timeout fails the whole inventory
// run. The run is retried a bounded number of times, each failure writes one
// run_failed event naming the failed request, and the last one marks the run
// failed. No Task starts.
func TestRepositoryMonitorGitHubTimeoutFailsRunWithBoundedRetries(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	monitor, secret := repositoryMonitorInventoryTestObjects("github-timeout")
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-timeout", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "manual", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}
	for attempt := 1; attempt <= repositoryMonitorCommandMaxRetries+1; attempt++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("attempt %d Reconcile() error = %v", attempt, err)
		}
		run, err := monitorStore.GetMonitorRun(ctx, monitor.Namespace, "run-timeout")
		if err != nil {
			t.Fatal(err)
		}
		failures := repositoryMonitorTestEvents(t, monitorStore, monitor, repositoryMonitorRunFailurePermanent)
		if len(failures) != attempt {
			t.Fatalf("attempt %d run_failed events = %d, want %d", attempt, len(failures), attempt)
		}
		if attempt <= repositoryMonitorCommandMaxRetries {
			if run.Phase != repositoryMonitorRunPhaseQueued || run.Error != "" || !run.StartedAt.After(time.Now()) {
				t.Fatalf("attempt %d run = %+v, want a delayed retry", attempt, run)
			}
			for _, failure := range failures {
				if failure.RunID != run.ID || !strings.Contains(failure.MetadataJSON, `"state":"`+repositoryMonitorRunRetryScheduled+`"`) || !strings.Contains(failure.Summary, "pull request inventory request failed") || !strings.Contains(failure.Summary, "Client.Timeout exceeded") {
					t.Fatalf("attempt %d failure event = %+v, want a scheduled retry", attempt, failure)
				}
			}
			// Make the delayed retry due now.
			run.StartedAt = time.Now().Add(-time.Second)
			if err := monitorStore.UpdateMonitorRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if run.Phase != repositoryMonitorRunPhaseFailed || !strings.HasPrefix(run.Error, "["+repositoryMonitorRunFailurePermanent+"] retry_attempts_exhausted:") || !strings.Contains(run.Error, "Client.Timeout exceeded") {
			t.Fatalf("final run = %+v, want failed after exhausting retries on the timeout", run)
		}
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 {
		t.Fatalf("tasks = %d err=%v, want none", len(tasks.Items), err)
	}
}

// repositoryMonitorDraftReadinessForge serves one pull request and records
// the readiness statuses written to each commit.
type repositoryMonitorDraftReadinessForge struct {
	t      *testing.T
	mu     sync.Mutex
	sha    string
	draft  bool
	writes map[string][]repositoryMonitorCommitStatus
}

func (f *repositoryMonitorDraftReadinessForge) set(sha string, draft bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sha, f.draft = sha, draft
}

func (f *repositoryMonitorDraftReadinessForge) descriptions(head string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.writes[head]))
	for _, status := range f.writes[head] {
		out = append(out, status.State+": "+status.Description)
	}
	return out
}

func (f *repositoryMonitorDraftReadinessForge) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1"):
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "state": "open", "draft": f.draft, "head": map[string]string{"sha": f.sha}, "base": map[string]string{"ref": "main"}})
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
		_, _ = w.Write([]byte(`[]`))
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
		_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
		_, _ = w.Write([]byte(`{"state":"pending","total_count":0,"statuses":[]}`))
	case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
		var status repositoryMonitorCommitStatus
		if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
			f.t.Error(err)
		}
		target := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		status.ID = int64(100 + len(f.writes[target]))
		f.writes[target] = append(f.writes[target], status)
		_ = json.NewEncoder(w).Encode(status)
	default:
		f.t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}
