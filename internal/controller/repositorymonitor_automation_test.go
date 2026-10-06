package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

//nolint:gocyclo // Keep automatic selection, Task creation, recovery, and bounded retry in one lifecycle fixture.
func TestRepositoryMonitorAutomaticallyRepairsFailedCI(t *testing.T) {
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
	monitor, secret := repositoryMonitorInventoryTestObjects("pr-fix")
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	monitor.Spec.Repair.Enabled = true
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-fix-31", MonitorNamespace: "default", MonitorName: "pr-fix", Trigger: "workflow", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pr-fix"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	// Policy selection only queues an accepted command. The next reconciliation
	// executes its canonical run, with no parallel inline Task creation.
	var queuedTasks corev1alpha1.TaskList
	if err := cl.List(ctx, &queuedTasks); err != nil || len(queuedTasks.Items) != 0 {
		t.Fatalf("automatic repair bypassed command queue: tasks=%d err=%v", len(queuedTasks.Items), err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pr-fix"}}); err != nil {
		t.Fatal(err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "pr-fix", repositoryMonitorPullRequestKind, "31")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.RepairState != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("item = %#v, want queued repair", item)
	}
	jobs, _, err := monitorStore.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "default", MonitorName: "pr-fix", PRNumber: 31, Limit: 10})
	if err != nil {
		t.Fatalf("ListRepairJobs() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].TaskName == "" || jobs[0].BaseBranch != "main" {
		t.Fatalf("jobs = %#v, want one repair job with task", jobs)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: jobs[0].TaskName}, &task); err != nil {
		t.Fatalf("Get repair task() error = %v", err)
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "repairer" || task.Spec.Workspace == nil || task.Spec.Workspace.PushBranch != "feature-fix" || task.Spec.Workspace.ExpectedRemoteSHA != "head31" {
		t.Fatalf("repair task spec = %#v, want exact-head repair publication", task.Spec)
	}
	if task.Spec.SessionRef == nil || !task.Spec.SessionRef.Create || task.Spec.SessionRef.Append || task.Spec.SessionRef.Name != repositoryMonitorPublicationSessionName(monitor, "feature-fix") {
		t.Fatalf("repair task session = %#v, want stable non-appending branch owner", task.Spec.SessionRef)
	}
	commands, _, err := monitorStore.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 10})
	if err != nil || len(commands) != 1 || commands[0].Source != "controller_policy" || commands[0].Intent != repositoryMonitorCommandIntentFixCI {
		t.Fatalf("automatic command = %#v, err=%v", commands, err)
	}
	manual := &store.MonitorRun{ID: "next-inventory", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31"}
	if _, _, _, err := reconciler.processPullRequestInventoryRun(ctx, monitor, manual, "orka-agents", "orka"); err != nil {
		t.Fatal(err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 1 {
		t.Fatalf("duplicate or concurrent task: count=%d err=%v", len(tasks.Items), err)
	}
	// Recovering accepted commands must retain the same Task/run identity.
	if _, err := reconciler.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if command.CommentID != command.ID || command.DedupeKey != command.ID || command.IdempotencyKey != command.ID {
			t.Fatalf("policy command is missing a unique durable identity: %+v", command)
		}
		if task.Annotations[labels.AnnotationMonitorRunID] != repositoryMonitorCommandRunIDFromCommand(command.ID) {
			t.Fatal("Task does not use the canonical command run")
		}
	}
	// A failed Task consumes one attempt, but a second same-head attempt must
	// get its own command identity rather than collide on the legacy SQL key.
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	if err := cl.Status().Update(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.ingestCompletedRepositoryMonitorRepairTasks(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := reconciler.processPullRequestInventoryRun(ctx, monitor, &store.MonitorRun{ID: "retry-inventory", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31"}, "orka-agents", "orka"); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pr-fix"}}); err != nil {
		t.Fatal(err)
	}
	commands, _, err = monitorStore.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
	if err != nil || len(commands) != 2 {
		t.Fatalf("retry commands=%+v err=%v", commands, err)
	}
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 2 {
		t.Fatalf("retry tasks=%d err=%v", len(tasks.Items), err)
	}
}

//nolint:gocyclo // Keep the paused and resumed phases of one durable command in the same fixture.
func TestRepositoryMonitorPauseHoldsPlanHandoffAndResumes(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/issues/44":
			_, _ = w.Write([]byte(`{"number":44,"title":"Add tray app","body":"Build Windows tray app.","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/44","user":{"login":"alice"},"labels":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/orka-agents/orka/issues/44/comments":
			_, _ = w.Write([]byte(`{"id":4401,"html_url":"https://github.com/orka-agents/orka/issues/44#issuecomment-4401"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	pullRequestsEnabled := false
	requireApprovedPlan := true
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-implement-plan-handoff")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	monitor.Spec.IssueWorkflow.Implementation.RequirePlan = &requireApprovedPlan
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, ArtifactStore: monitorStore, GitHubAPIBaseURL: server.URL}

	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-implement-44", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 44, Intent: "implement", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: store.RepositoryMonitorWorkActionID(command.ID, "implement"), MonitorNamespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, DesiredAction: "implement", Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: processedAt}); err != nil {
		t.Fatalf("CreateWorkAction(implement) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-implement-44", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(queue prerequisite plan) error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "44")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhasePlanQueued || item.LastActionKind != repositoryMonitorIssueActionPlan || item.LastActionTaskName == "" {
		t.Fatalf("item after implement handoff = %#v, want plan queued", item)
	}
	planTaskName := item.LastActionTaskName
	implementAction, err := monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, "implement"))
	if err != nil {
		t.Fatalf("GetWorkAction(implement) error = %v", err)
	}
	if implementAction.Status != repositoryMonitorWorkActionStatusRunning || implementAction.Phase != repositoryMonitorIssuePhasePlanQueued {
		t.Fatalf("implement action = %#v, want running prerequisite plan", implementAction)
	}

	planResult := fmt.Sprintf(`{"schemaVersion":"orka.issuePlan.v1","repo":"orka-agents/orka","issueNumber":44,"snapshotDigest":%q,"status":"ready","summary":"Small plan","acceptanceCriteria":[],"steps":["edit"],"validationCommands":[],"allowedFiles":["README.md"],"risk":"low","categories":[],"requiresHumanApproval":false}`, item.SnapshotDigest)
	if err := monitorStore.SaveResult(ctx, "default", planTaskName, []byte(planResult)); err != nil {
		t.Fatalf("SaveResult(plan) error = %v", err)
	}
	item.LabelsJSON = `["orka:pause"]`
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, planTaskName)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(continue implementation) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "44")
	if err != nil {
		t.Fatalf("GetMonitorItem(after plan) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhasePaused {
		t.Fatalf("planner escaped pause: %s", item.WorkflowPhase)
	}
	item.LabelsJSON = "[]"
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatal(err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "44")
	if err != nil {
		t.Fatal(err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued || item.LastActionKind != repositoryMonitorIssueActionImplementation || item.LastActionTaskName == "" || item.LastActionTaskName == planTaskName {
		t.Fatalf("item after auto-approved plan = %#v, want implementation queued", item)
	}
	implementAction, err = monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, "implement"))
	if err != nil {
		t.Fatalf("GetWorkAction(implement after plan) error = %v", err)
	}
	if implementAction.Status != repositoryMonitorWorkActionStatusRunning || implementAction.TaskName != item.LastActionTaskName {
		t.Fatalf("implement action after plan = %#v, want running implementation", implementAction)
	}
}

func TestRepositoryMonitorRepairProjectionIgnoresOldHeadFailure(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("projection")
	r := &RepositoryMonitorReconciler{Store: db}
	if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "old-failure", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: 1, HeadSHA: "old", Phase: repositoryMonitorRepairPhaseFailed, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	state, err := r.repositoryMonitorRepairStateForHead(ctx, monitor, 1, "new")
	if err != nil || state != "" {
		t.Fatalf("old failure projected onto new head: %s %v", state, err)
	}
	if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "new-active", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: 1, HeadSHA: "new", Phase: repositoryMonitorRepairPhaseQueued, TaskName: "running-task", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	state, err = r.repositoryMonitorRepairStateForHead(ctx, monitor, 1, "new")
	if err != nil || state != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("active repair not retained: %s %v", state, err)
	}
}

func TestRepositoryMonitorRepairDoesNotStarveLaterPullRequests(t *testing.T) {
	for _, phase := range []string{repositoryMonitorRepairPhaseFailed, repositoryMonitorRepairPhaseQueued} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			monitor, secret := repositoryMonitorInventoryTestObjects("fair-repair")
			limit := int32(1)
			monitor.Spec.Targets.PullRequests.MaxPerRun = &limit
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Repair.MaxRepairsPerPR = &limit
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			if phase == repositoryMonitorRepairPhaseQueued {
				task := &corev1alpha1.Task{}
				task.Name, task.Namespace = "active-repair", monitor.Namespace
				if err := cl.Create(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "first-pr-repair", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: 1, HeadSHA: "head1", TaskName: "active-repair", Phase: phase, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			body := `[
				{"number":1,"state":"open","base":{"ref":"main","sha":"base"},"head":{"ref":"first","sha":"head1","repo":{"full_name":"orka-agents/orka"}}},
				{"number":2,"state":"open","base":{"ref":"main","sha":"base"},"head":{"ref":"second","sha":"head2","repo":{"full_name":"orka-agents/orka"}}}
			]`
			server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, body)
			defer server.Close()
			original := server.Config.Handler
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(req.URL.Path, "/check-runs") {
					conclusion := "success"
					if strings.Contains(req.URL.Path, "/head1/") {
						conclusion = "failure"
					}
					_, _ = fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":%q}]}`, conclusion)
					return
				}
				if strings.HasSuffix(req.URL.Path, "/status") {
					_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
					return
				}
				original.ServeHTTP(w, req)
			})
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL}
			selected, created, _, err := r.processPullRequestInventoryRun(ctx, monitor, &store.MonitorRun{ID: "poll", TargetKind: repositoryMonitorPullRequestKind}, "orka-agents", "orka")
			if err != nil || selected != 1 || created != 1 {
				t.Fatalf("later PR starved: selected=%d created=%d err=%v", selected, created, err)
			}
			item, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "2")
			if err != nil || item.LastVerdict != repositoryMonitorRunPhaseQueued || item.LastReviewID == "" {
				t.Fatalf("second PR not queued for review: item=%+v err=%v", item, err)
			}
		})
	}
}

func TestRepositoryMonitorInventoryHonorsReadinessRefresh(t *testing.T) {
	for _, guard := range []string{"pause", "draft", "closed", "new_head", "new_base"} {
		t.Run(guard, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("fresh-guard")
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Review.Publish.Enabled = true
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			configureRepositoryMonitorTestWriteCredentials(monitor)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			const oldPR = `{"number":31,"title":"Fix me","state":"open","draft":false,"mergeable_state":"clean","base":{"ref":"main","sha":"base31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-fix","sha":"head31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`
			var current map[string]any
			if err := json.Unmarshal([]byte(oldPR), &current); err != nil {
				t.Fatal(err)
			}
			switch guard {
			case "pause":
				current["labels"] = []map[string]string{{"name": "orka:pause"}}
			case "draft":
				current["draft"] = true
			case "closed":
				current["state"] = "closed"
			case "new_head":
				current["head"].(map[string]any)["sha"] = "head32"
			case "new_base":
				current["base"].(map[string]any)["ref"] = "other"
			}
			server := newRepositoryMonitorSinglePullRequestServerWithBodyAndAuth(t, 31, oldPR, "Bearer forge-token")
			t.Cleanup(server.Close)
			original := server.Config.Handler
			writes := 0
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
					_, _ = fmt.Fprintf(w, "[%s]", oldPR)
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/31"):
					_ = json.NewEncoder(w).Encode(current)
				case strings.HasSuffix(req.URL.Path, "/check-runs"):
					_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":10,"name":"tests","status":"completed","conclusion":"failure"}]}`))
				case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
					var status repositoryMonitorCommitStatus
					if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
						t.Error(err)
					}
					writes++
					status.ID = int64(100 + writes)
					_ = json.NewEncoder(w).Encode(status)
				default:
					original.ServeHTTP(w, req)
				}
			})
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, GitHubAPIBaseURL: server.URL}
			run := &store.MonitorRun{ID: "fresh-run", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "workflow", TargetKind: repositoryMonitorPullRequestKind}
			_, created, _, err := r.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka")
			if err != nil {
				t.Fatal(err)
			}
			var tasks corev1alpha1.TaskList
			if err := cl.List(ctx, &tasks); err != nil {
				t.Fatal(err)
			}
			if created != 0 || len(tasks.Items) != 0 {
				t.Fatalf("started new work after observing %s: created=%d tasks=%d", guard, created, len(tasks.Items))
			}
			if guard == "pause" || guard == "draft" {
				item, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "31")
				if err != nil {
					t.Fatal(err)
				}
				if (guard == "pause" && !strings.Contains(item.LabelsJSON, "orka:pause")) || (guard == "draft" && !item.Draft) {
					t.Fatalf("stored item lost the fresh guard: %+v", item)
				}
			}
		})
	}
}

func TestRepositoryMonitorRepairPromptIncludesOnlyMatchingReviewEvidence(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("repair-evidence")
	r := &RepositoryMonitorReconciler{Store: db}
	item := &store.MonitorItem{LastReviewID: "review-evidence", LastVerdict: repositoryMonitorReviewVerdictNeedsChanges}
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head"}
	review := &store.ReviewRecord{ID: item.LastReviewID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 31, HeadSHA: "head", Summary: "Initialize the retry result", FindingsJSON: `[{"file":"scripts/smoke.sh","line":42,"body":"The result variable is unset after success"}]`, ValidationEvidence: "shell test failed"}
	if err := db.CreateReviewRecord(ctx, review); err != nil {
		t.Fatal(err)
	}
	prompt, err := r.buildRepositoryMonitorRepairPrompt(ctx, monitor, "fix_ci", "orka-agents/orka", pr, item)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Initialize the retry result", "scripts/smoke.sh", "shell test failed", "untrusted data"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt is missing %q", want)
		}
	}
	for _, change := range []string{"head", "pr", "monitor", "missing"} {
		t.Run(change, func(t *testing.T) {
			otherPR, otherMonitor, otherItem := pr, *monitor, *item
			switch change {
			case "head":
				otherPR.HeadSHA = "different"
			case "pr":
				otherPR.Number++
			case "monitor":
				otherMonitor.Name = "different"
			case "missing":
				otherItem.LastReviewID = "absent"
			}
			prompt, err := r.buildRepositoryMonitorRepairPrompt(ctx, &otherMonitor, "fix_ci", "orka-agents/orka", otherPR, &otherItem)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(prompt, "reviewEvidence") || strings.Contains(prompt, "validationEvidence") {
				t.Fatal("included stale or differently scoped evidence")
			}
		})
	}
}
