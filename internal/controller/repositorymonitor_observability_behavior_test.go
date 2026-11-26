package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

	wantContext := repositoryMonitorReadinessContext(monitor)
	poll()
	if got := forge.descriptions("head-1", wantContext); len(got) != 2 || got[0] != claimed || got[1] != held {
		t.Fatalf("head-1 statuses after first poll = %q, want the claim then one draft hold", got)
	}
	for range 4 {
		poll()
	}
	if got := forge.descriptions("head-1", wantContext); len(got) != 2 || got[0] != claimed || got[1] != held {
		t.Fatalf("head-1 statuses after 5 polls = %q, want no extra writes after the first poll", got)
	}
	mutation, err := db.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorReadinessMutationID(monitor, "head-1"))
	if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.Reason != "The pull request is a draft." || mutation.ExternalID != "101" {
		t.Fatalf("head-1 readiness mutation = %+v err=%v, want the draft hold recorded", mutation, err)
	}

	// A new head is still a draft: it gets its own status on the first poll, written once.
	forge.set("head-2", true)
	pr.HeadSHA = "head-2"
	item = repositoryMonitorItemFromPullRequest(monitor, pr, item)
	poll()
	if got := forge.descriptions("head-2", wantContext); len(got) != 2 || got[0] != claimed || got[1] != held {
		t.Fatalf("head-2 statuses after first poll = %q, want the claim then one draft hold", got)
	}
	for range 2 {
		poll()
	}
	if got := forge.descriptions("head-2", wantContext); len(got) != 2 || got[0] != claimed || got[1] != held {
		t.Fatalf("head-2 statuses = %q, want no extra writes after the first poll", got)
	}
	head2Mutation, err := db.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorReadinessMutationID(monitor, "head-2"))
	wantHead2ID := strconv.FormatInt(forge.latestID("head-2"), 10)
	if err != nil || head2Mutation.Status != repositoryMonitorRunPhaseSucceeded || head2Mutation.Reason != "The pull request is a draft." || head2Mutation.ExternalID != wantHead2ID || head2Mutation.ExternalID == mutation.ExternalID {
		t.Fatalf("head-2 readiness mutation = %+v err=%v, want distinct draft hold ID %s", head2Mutation, err, wantHead2ID)
	}

	// Marking it ready replaces the hold on the first poll with the next blocker.
	forge.set("head-2", false)
	pr.Draft = false
	item = repositoryMonitorItemFromPullRequest(monitor, pr, item)
	poll()
	got := forge.descriptions("head-2", wantContext)
	if len(got) != 3 || got[2] == held || !strings.HasPrefix(got[2], repositoryMonitorStatusPending+": ") {
		t.Fatalf("head-2 statuses after first ready poll = %q, want one pending status replacing the draft hold", got)
	}
	for range 2 {
		poll()
	}
	if got = forge.descriptions("head-2", wantContext); len(got) != 3 {
		t.Fatalf("head-2 statuses after repeated ready polls = %q, want no extra writes", got)
	}
}

// A repair handoff writes its repair job and started push record before it
// creates the Task, all keyed by the command. A controller that stops at the
// Task create, or restarts after the Task exists or after the result was
// ingested, replays the handoff without a second Task, job or push record, and
// never reopens a settled push.
func TestRepositoryMonitorRepairRestartReusesTaskAndPushRecord(t *testing.T) {
	ctx := context.Background()
	monitor, first, monitorStore := newRepositoryMonitorPolicyReconciler(t, "repair-restart", nil, nil)
	newCommand := func() *store.CommandEvent {
		return &store.CommandEvent{ID: "cmd-repair-restart", Intent: repositoryMonitorCommandIntentFix, Source: "api"}
	}
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", BaseSHA: "base31", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	newItem := func() *store.MonitorItem {
		return &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: pr.HeadSHA}
	}
	newRun := func() *store.MonitorRun {
		return &store.MonitorRun{ID: "run-repair-restart"}
	}
	commandID := newCommand().ID
	cluster, ok := first.Client.(crclient.WithWatch)
	if !ok {
		t.Fatalf("test client %T does not support interception", first.Client)
	}
	// restart returns a new reconciler over the same cluster and store.
	restart := func() *RepositoryMonitorReconciler {
		return &RepositoryMonitorReconciler{Client: cluster, Scheme: first.Scheme, Store: first.Store, ResultStore: first.ResultStore, GitHubAPIBaseURL: first.GitHubAPIBaseURL}
	}
	handoff := func(r *RepositoryMonitorReconciler, wantCreated int) {
		t.Helper()
		if created, _, err := r.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, newRun(), newCommand(), "orka-agents", "orka", pr, newItem(), 1, 1); err != nil || created != wantCreated {
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
		if tasks := repositoryMonitorTestTaskCount(t, restart()); tasks != 1 {
			t.Fatalf("%s: tasks = %d, want 1", stage, tasks)
		}
		if got := jobs(); len(got) != 1 {
			t.Fatalf("%s: repair jobs = %+v, want one", stage, got)
		}
		if got := pushes(); len(got) != 1 || got[0].Status != wantPush || got[0].CommandEventID != commandID {
			t.Fatalf("%s: push records = %+v, want one %s record for the command", stage, got, wantPush)
		}
	}

	// The controller crashes at the Task create of its first handoff, before
	// error handling can update the job. By then the repair job and the started
	// push record must already be durable.
	type crashBeforeTaskCreate struct{}
	crashed := false
	first.Client = interceptor.NewClient(cluster, interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask || crashed {
				return c.Create(ctx, obj, opts...)
			}
			crashed = true
			if got := jobs(); len(got) != 1 || got[0].LastError != "" || got[0].Phase != repositoryMonitorRepairPhaseQueued {
				t.Errorf("repair jobs at Task create = %+v, want one queued job with no error recorded first", got)
			}
			if got := pushes(); len(got) != 1 || got[0].Status != repositoryMonitorAutomergeStateStarted || got[0].CommandEventID != commandID {
				t.Errorf("push records at Task create = %+v, want one started record for the command first", got)
			}
			panic(crashBeforeTaskCreate{})
		},
	})
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected simulated crash at Task creation")
			}
			if _, ok := r.(crashBeforeTaskCreate); !ok {
				panic(r)
			}
		}()
		_, _, _ = first.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, newRun(), newCommand(), "orka-agents", "orka", pr, newItem(), 1, 1)
	}()
	if !crashed {
		t.Fatal("interrupted handoff did not reach Task creation")
	}
	if tasks := repositoryMonitorTestTaskCount(t, restart()); tasks != 0 {
		t.Fatalf("tasks after the crashed handoff = %d, want none", tasks)
	}
	if got := jobs(); len(got) != 1 || got[0].LastError != "" || got[0].Phase != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("repair job after crash = %+v, want queued without create error", got)
	}

	// Next, an ordinary Create error records LastError without creating a Task;
	// a subsequent restart still recovers against the same records and clears it.
	failedCreate := false
	retrying := restart()
	retrying.Client = interceptor.NewClient(cluster, interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask || failedCreate {
				return c.Create(ctx, obj, opts...)
			}
			failedCreate = true
			return errors.New("transient apiserver create error")
		},
	})
	if created, _, err := retrying.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, newRun(), newCommand(), "orka-agents", "orka", pr, newItem(), 1, 1); err == nil || created != 0 || !failedCreate {
		t.Fatalf("failed create handoff = (%d, %v) failedCreate=%t, want error at Task creation", created, err, failedCreate)
	}
	if tasks := repositoryMonitorTestTaskCount(t, restart()); tasks != 0 {
		t.Fatalf("tasks after failed Task creation = %d, want none", tasks)
	}
	if got := jobs(); len(got) != 1 || got[0].LastError == "" {
		t.Fatalf("repair job after create error = %+v, want LastError recorded", got)
	}

	// The restarted controller creates the Task in the API server, then crashes
	// before it can observe the Create response or run post-Create bookkeeping.
	type crashAfterTaskCreate struct{}
	crashedAfterCreate := false
	creating := restart()
	creating.Client = interceptor.NewClient(cluster, interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask || crashedAfterCreate {
				return c.Create(ctx, obj, opts...)
			}
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			crashedAfterCreate = true
			panic(crashAfterTaskCreate{})
		},
	})
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected simulated crash after Task creation")
			}
			if _, ok := r.(crashAfterTaskCreate); !ok {
				panic(r)
			}
		}()
		_, _, _ = creating.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, newRun(), newCommand(), "orka-agents", "orka", pr, newItem(), 1, 1)
	}()
	if !crashedAfterCreate {
		t.Fatal("handoff did not reach post-Create crash")
	}
	assertOne("crash after API server stored Task", repositoryMonitorAutomergeStateStarted)

	// The next restart adopts the already-created Task against the same records
	// and clears the earlier create error.
	handoff(restart(), 0)
	assertOne("restart after Task creation", repositoryMonitorAutomergeStateStarted)
	if got := jobs(); got[0].LastError != "" || got[0].Phase != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("repair job after adopting Task = %+v, want queued without the create error", got[0])
	}

	markRepositoryMonitorTestTaskDelivered(t, ctx, cluster, jobs()[0].TaskName, pr.HeadBranch, "pushed31")
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
		watchdog := time.NewTimer(5 * time.Second)
		defer watchdog.Stop()
		select {
		case <-req.Context().Done():
		case <-release:
		case <-watchdog.C:
			t.Errorf("GitHub request %s %s did not time out within watchdog window", req.Method, req.URL.Path)
			http.Error(w, "watchdog timeout", http.StatusGatewayTimeout)
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
		terminalEvents := 0
		for _, failure := range failures {
			if failure.RunID != run.ID || !strings.Contains(failure.Summary, "pull request inventory request failed") || !strings.Contains(failure.Summary, "Client.Timeout exceeded") {
				t.Fatalf("final failure event = %+v, want inventory timeout for this run", failure)
			}
			if !strings.Contains(failure.MetadataJSON, `"state":"`+repositoryMonitorRunRetryScheduled+`"`) {
				terminalEvents++
			}
		}
		if terminalEvents != 1 {
			t.Fatalf("terminal run_failed events = %d (all=%+v), want 1 non-retry terminal event", terminalEvents, failures)
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
	nextID int64
	writes map[string][]repositoryMonitorCommitStatus
}

func (f *repositoryMonitorDraftReadinessForge) set(sha string, draft bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sha, f.draft = sha, draft
}

func (f *repositoryMonitorDraftReadinessForge) latestID(head string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	statuses := f.writes[head]
	if len(statuses) == 0 {
		return 0
	}
	return statuses[len(statuses)-1].ID
}

func (f *repositoryMonitorDraftReadinessForge) descriptions(head, wantContext string) []string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.writes[head]))
	for _, status := range f.writes[head] {
		if status.Context != wantContext {
			f.t.Errorf("head %s status context = %q, want %q", head, status.Context, wantContext)
		}
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
		prefix := strings.TrimSuffix(req.URL.Path, "/status")
		target := prefix[strings.LastIndex(prefix, "/")+1:]
		latestByContext := map[string]repositoryMonitorCommitStatus{}
		order := []string{}
		for _, s := range f.writes[target] {
			if _, seen := latestByContext[s.Context]; !seen {
				order = append(order, s.Context)
			}
			latestByContext[s.Context] = s
		}
		statuses := make([]repositoryMonitorCommitStatus, 0, len(order))
		combined := repositoryMonitorStatusPending
		if len(order) > 0 {
			combined = repositoryMonitorStatusSuccess
		}
		for _, ctxName := range order {
			s := latestByContext[ctxName]
			statuses = append(statuses, s)
			switch s.State {
			case repositoryMonitorStatusFailure, "error":
				combined = repositoryMonitorStatusFailure
			case repositoryMonitorStatusPending:
				if combined != repositoryMonitorStatusFailure {
					combined = repositoryMonitorStatusPending
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": combined, "total_count": len(statuses), "statuses": statuses})
	case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
		var status repositoryMonitorCommitStatus
		if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
			f.t.Error(err)
		}
		target := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		status.ID = 100 + f.nextID
		f.nextID++
		f.writes[target] = append(f.writes[target], status)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(status)
	default:
		f.t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}
