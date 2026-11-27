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
	assertDraftHoldOnce := func(head string, extraPolls int) *store.GitHubMutationRecord {
		t.Helper()
		poll()
		if got := forge.descriptions(head, wantContext); len(got) != 2 || got[0] != claimed || got[1] != held {
			t.Fatalf("%s statuses after first poll = %q, want the claim then one draft hold", head, got)
		}
		for range extraPolls {
			poll()
		}
		if got := forge.descriptions(head, wantContext); len(got) != 2 || got[0] != claimed || got[1] != held {
			t.Fatalf("%s statuses after %d polls = %q, want no extra writes after the first poll", head, extraPolls+1, got)
		}
		mutation, err := db.GetGitHubMutationRecord(ctx, monitor.Namespace, repositoryMonitorReadinessMutationID(monitor, head))
		wantID := strconv.FormatInt(forge.latestID(head), 10)
		if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.Reason != "The pull request is a draft." || mutation.ExternalID != wantID {
			t.Fatalf("%s readiness mutation = %+v err=%v, want the draft hold recorded with ID %s", head, mutation, err, wantID)
		}
		return mutation
	}

	mutation := assertDraftHoldOnce("head-1", 4)

	forge.set("head-2", true)
	pr.HeadSHA = "head-2"
	item = repositoryMonitorItemFromPullRequest(monitor, pr, item)
	head2Mutation := assertDraftHoldOnce("head-2", 2)
	if head2Mutation.ExternalID == mutation.ExternalID {
		t.Fatalf("head-2 readiness mutation = %+v, want distinct external ID from head-1 %s", head2Mutation, mutation.ExternalID)
	}

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

func TestRepositoryMonitorRepairRestartReusesTaskAndPushRecord(t *testing.T) {
	ctx := context.Background()
	monitor, first, monitorStore := newRepositoryMonitorPolicyReconciler(t, "repair-restart", nil, nil)
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", BaseSHA: "base31", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	cluster, ok := first.Client.(crclient.WithWatch)
	if !ok {
		t.Fatalf("test client %T does not support interception", first.Client)
	}
	h := &repairRestartHarness{
		t:            t,
		ctx:          ctx,
		monitor:      monitor,
		monitorStore: monitorStore,
		cluster:      cluster,
		base:         first,
		pr:           pr,
		commandID:    "cmd-repair-restart",
	}

	h.runCrashBeforeTaskCreate()

	h.runTransientCreateError()

	h.runCrashAfterTaskCreate()
	h.assertOne("crash after API server stored Task", repositoryMonitorAutomergeStateStarted)

	h.handoff(h.restart(), 0)
	h.assertOne("restart after Task creation", repositoryMonitorAutomergeStateStarted)
	if got := h.jobs(); got[0].LastError != "" || got[0].Phase != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("repair job after adopting Task = %+v, want queued without the create error", got[0])
	}

	markRepositoryMonitorTestTaskDelivered(t, ctx, cluster, h.jobs()[0].TaskName, pr.HeadBranch, "pushed31")
	if ingested, err := h.restart().ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil || !ingested {
		t.Fatalf("ingest = (%t, %v), want the delivered repair ingested", ingested, err)
	}
	h.assertOne("ingest", repositoryMonitorRunPhaseSucceeded)

	after := h.restart()
	h.handoff(after, 0)
	if ingested, err := after.ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil || ingested {
		t.Fatalf("replayed ingest = (%t, %v), want nothing to ingest", ingested, err)
	}
	h.assertOne("restart after ingest", repositoryMonitorRunPhaseSucceeded)
	if got := h.pushes(); got[0].ExternalID != "pushed31" {
		t.Fatalf("push record = %+v, want pushed31 kept", got[0])
	}
}

type repairRestartHarness struct {
	t            *testing.T
	ctx          context.Context
	monitor      *corev1alpha1.RepositoryMonitor
	monitorStore store.RepositoryMonitorStore
	cluster      crclient.WithWatch
	base         *RepositoryMonitorReconciler
	pr           repositoryMonitorPullRequest
	commandID    string
}

func (h *repairRestartHarness) newCommand() *store.CommandEvent {
	return &store.CommandEvent{ID: h.commandID, Intent: repositoryMonitorCommandIntentFix, Source: "api"}
}

func (h *repairRestartHarness) newItem() *store.MonitorItem {
	return &store.MonitorItem{MonitorNamespace: h.monitor.Namespace, MonitorName: h.monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: h.pr.HeadSHA}
}

func (h *repairRestartHarness) newRun() *store.MonitorRun {
	return &store.MonitorRun{ID: "run-repair-restart"}
}

func (h *repairRestartHarness) restart() *RepositoryMonitorReconciler {
	return &RepositoryMonitorReconciler{Client: h.cluster, Scheme: h.base.Scheme, Store: h.base.Store, ResultStore: h.base.ResultStore, GitHubAPIBaseURL: h.base.GitHubAPIBaseURL}
}

func (h *repairRestartHarness) handoff(r *RepositoryMonitorReconciler, wantCreated int) {
	h.t.Helper()
	if created, _, err := r.createRepositoryMonitorRepairTaskWithEvidence(h.ctx, h.monitor, h.newRun(), h.newCommand(), "orka-agents", "orka", h.pr, h.newItem(), 1, 1); err != nil || created != wantCreated {
		h.t.Fatalf("repair Task handoff = (%d, %v), want created=%d", created, err, wantCreated)
	}
}

func (h *repairRestartHarness) jobs() []store.RepairJob {
	h.t.Helper()
	records, _, err := h.monitorStore.ListRepairJobs(h.ctx, store.RepairJobFilter{Namespace: h.monitor.Namespace, MonitorName: h.monitor.Name, PRNumber: 31, Limit: 10})
	if err != nil {
		h.t.Fatal(err)
	}
	return records
}

func (h *repairRestartHarness) pushes() []store.GitHubMutationRecord {
	h.t.Helper()
	records, _, err := h.monitorStore.ListGitHubMutationRecords(h.ctx, store.GitHubMutationRecordFilter{Namespace: h.monitor.Namespace, MonitorName: h.monitor.Name, Operation: pushBranchAction, Limit: 10})
	if err != nil {
		h.t.Fatal(err)
	}
	return records
}

func (h *repairRestartHarness) assertOne(stage, wantPush string) {
	h.t.Helper()
	if tasks := repositoryMonitorTestTaskCount(h.t, h.restart()); tasks != 1 {
		h.t.Fatalf("%s: tasks = %d, want 1", stage, tasks)
	}
	if got := h.jobs(); len(got) != 1 {
		h.t.Fatalf("%s: repair jobs = %+v, want one", stage, got)
	}
	if got := h.pushes(); len(got) != 1 || got[0].Status != wantPush || got[0].CommandEventID != h.commandID {
		h.t.Fatalf("%s: push records = %+v, want one %s record for the command", stage, got, wantPush)
	}
}

func expectSimulatedCrash(t *testing.T, label string, sentinel any, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected simulated crash %s", label)
		}
		if r != sentinel {
			panic(r)
		}
	}()
	fn()
}

func (h *repairRestartHarness) runCrashBeforeTaskCreate() {
	h.t.Helper()
	type crashBeforeTaskCreate struct{}
	crashed := false
	h.base.Client = interceptor.NewClient(h.cluster, interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask || crashed {
				return c.Create(ctx, obj, opts...)
			}
			crashed = true
			if got := h.jobs(); len(got) != 1 || got[0].LastError != "" || got[0].Phase != repositoryMonitorRepairPhaseQueued {
				h.t.Errorf("repair jobs at Task create = %+v, want one queued job with no error recorded first", got)
			}
			if got := h.pushes(); len(got) != 1 || got[0].Status != repositoryMonitorAutomergeStateStarted || got[0].CommandEventID != h.commandID {
				h.t.Errorf("push records at Task create = %+v, want one started record for the command first", got)
			}
			panic(crashBeforeTaskCreate{})
		},
	})
	expectSimulatedCrash(h.t, "at Task creation", crashBeforeTaskCreate{}, func() {
		_, _, _ = h.base.createRepositoryMonitorRepairTaskWithEvidence(h.ctx, h.monitor, h.newRun(), h.newCommand(), "orka-agents", "orka", h.pr, h.newItem(), 1, 1)
	})
	if !crashed {
		h.t.Fatal("interrupted handoff did not reach Task creation")
	}
	if tasks := repositoryMonitorTestTaskCount(h.t, h.restart()); tasks != 0 {
		h.t.Fatalf("tasks after the crashed handoff = %d, want none", tasks)
	}
	if got := h.jobs(); len(got) != 1 || got[0].LastError != "" || got[0].Phase != repositoryMonitorRepairPhaseQueued {
		h.t.Fatalf("repair job after crash = %+v, want queued without create error", got)
	}
}

func (h *repairRestartHarness) runTransientCreateError() {
	h.t.Helper()
	failedCreate := false
	retrying := h.restart()
	retrying.Client = interceptor.NewClient(h.cluster, interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask || failedCreate {
				return c.Create(ctx, obj, opts...)
			}
			failedCreate = true
			return errors.New("transient apiserver create error")
		},
	})
	if created, _, err := retrying.createRepositoryMonitorRepairTaskWithEvidence(h.ctx, h.monitor, h.newRun(), h.newCommand(), "orka-agents", "orka", h.pr, h.newItem(), 1, 1); err == nil || created != 0 || !failedCreate {
		h.t.Fatalf("failed create handoff = (%d, %v) failedCreate=%t, want error at Task creation", created, err, failedCreate)
	}
	if tasks := repositoryMonitorTestTaskCount(h.t, h.restart()); tasks != 0 {
		h.t.Fatalf("tasks after failed Task creation = %d, want none", tasks)
	}
	if got := h.jobs(); len(got) != 1 || got[0].LastError == "" {
		h.t.Fatalf("repair job after create error = %+v, want LastError recorded", got)
	}
}

func (h *repairRestartHarness) runCrashAfterTaskCreate() {
	h.t.Helper()
	type crashAfterTaskCreate struct{}
	crashedAfterCreate := false
	creating := h.restart()
	creating.Client = interceptor.NewClient(h.cluster, interceptor.Funcs{
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
	expectSimulatedCrash(h.t, "after Task creation", crashAfterTaskCreate{}, func() {
		_, _, _ = creating.createRepositoryMonitorRepairTaskWithEvidence(h.ctx, h.monitor, h.newRun(), h.newCommand(), "orka-agents", "orka", h.pr, h.newItem(), 1, 1)
	})
	if !crashedAfterCreate {
		h.t.Fatal("handoff did not reach post-Create crash")
	}
}

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
			assertRepositoryMonitorTimeoutRetryScheduled(t, ctx, monitorStore, run, failures, attempt)
			continue
		}
		assertRepositoryMonitorTimeoutTerminalFailure(t, run, failures)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 {
		t.Fatalf("tasks = %d err=%v, want none", len(tasks.Items), err)
	}
}

func assertRepositoryMonitorTimeoutRetryScheduled(t *testing.T, ctx context.Context, monitorStore store.RepositoryMonitorStore, run *store.MonitorRun, failures []store.MonitorEvent, attempt int) {
	t.Helper()
	if run.Phase != repositoryMonitorRunPhaseQueued || run.Error != "" || !run.StartedAt.After(time.Now()) {
		t.Fatalf("attempt %d run = %+v, want a delayed retry", attempt, run)
	}
	for _, failure := range failures {
		if failure.RunID != run.ID || !strings.Contains(failure.MetadataJSON, `"state":"`+repositoryMonitorRunRetryScheduled+`"`) || !strings.Contains(failure.Summary, "pull request inventory request failed") || !strings.Contains(failure.Summary, "Client.Timeout exceeded") {
			t.Fatalf("attempt %d failure event = %+v, want a scheduled retry", attempt, failure)
		}
	}
	run.StartedAt = time.Now().Add(-time.Second)
	if err := monitorStore.UpdateMonitorRun(ctx, run); err != nil {
		t.Fatal(err)
	}
}

func assertRepositoryMonitorTimeoutTerminalFailure(t *testing.T, run *store.MonitorRun, failures []store.MonitorEvent) {
	t.Helper()
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
