package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func repositoryMonitorPauseTestReconciler(t *testing.T, paused bool) (*RepositoryMonitorReconciler, *corev1alpha1.RepositoryMonitor, repositoryMonitorIssue) {
	t.Helper()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("pause-workflow")
	monitor.Spec.Targets.Issues.Enabled = true
	issue := repositoryMonitorIssue{Number: 42, Title: "Fix issue", Body: "Keep scope small", State: "open", Labels: []string{"bug"}}
	responseLabels := `[{"name":"bug"}]`
	if paused {
		responseLabels = `[{"name":"bug"},{"name":"orka:pause"}]`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/issues/42":
			_, _ = fmt.Fprintf(w, `{"number":42,"title":"Fix issue","body":"Keep scope small","state":"open","labels":%s}`, responseLabels)
		case req.Method == http.MethodPost && req.URL.Path == "/repos/orka-agents/orka/issues/42/comments":
			_, _ = w.Write([]byte(`{"id":4201,"html_url":"https://github.com/orka-agents/orka/issues/42#issuecomment-4201"}`))
		default:
			t.Errorf("unexpected GitHub request: %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	return &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, GitHubAPIBaseURL: server.URL}, monitor, issue
}

func TestRepositoryMonitorPauseInventoryPreservesWorkflow(t *testing.T) {
	for _, action := range []struct{ kind, phase string }{
		{repositoryMonitorIssueActionTriage, repositoryMonitorIssuePhaseTriaging},
		{repositoryMonitorIssueActionResearch, repositoryMonitorIssuePhaseResearching},
		{repositoryMonitorIssueActionPlan, repositoryMonitorIssuePhasePlanning},
		{repositoryMonitorIssueActionPlan, repositoryMonitorIssuePhasePlanned},
		{repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseImplementing},
		{repositoryMonitorIssueActionMutateToPR, repositoryMonitorIssuePhaseMutatingToPR},
	} {
		t.Run(action.kind+"/"+action.phase, func(t *testing.T) {
			r, monitor, issue := repositoryMonitorPauseTestReconciler(t, true)
			item := repositoryMonitorItemFromIssue(monitor, issue, nil)
			item.WorkflowPhase = action.phase
			item.LastActionKind = action.kind
			item.LastActionTaskName = "active-task"
			item.LastVerdict = repositoryMonitorRunPhaseQueued
			if err := r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "pause-inventory", TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number}, "orka-agents", "orka"); err != nil {
				t.Fatal(err)
			}
			item, err := r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
			if err != nil {
				t.Fatal(err)
			}
			if item.WorkflowPhase != action.phase || item.LastActionTaskName != "active-task" || item.LastActionKind != action.kind || item.LastVerdict != repositoryMonitorRunPhaseQueued {
				t.Fatalf("pause lost active task: %+v", item)
			}
			if item.SkipReason != repositoryMonitorSkipReasonBlockedLabel {
				t.Fatalf("pause reason = %q", item.SkipReason)
			}
		})
	}
}

func TestRepositoryMonitorPauseRetainsTerminalResults(t *testing.T) {
	for _, action := range []struct{ kind, verdict string }{
		{repositoryMonitorIssueActionTriage, "actionable"},
		{repositoryMonitorIssueActionResearch, "ready"},
		{repositoryMonitorIssueActionPlan, "ready"},
		{repositoryMonitorIssueActionImplementation, "patch_ready"},
		{repositoryMonitorIssueActionMutateToPR, "success"},
	} {
		t.Run(action.kind, func(t *testing.T) {
			r, monitor, issue := repositoryMonitorPauseTestReconciler(t, true)
			item := repositoryMonitorItemFromIssue(monitor, issue, nil)
			item.WorkflowPhase = repositoryMonitorIssuePhasePaused
			item.LabelsJSON = `["bug","orka:pause"]`
			record := &store.ActionRecord{ID: "terminal-result", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: issue.Number, ActionKind: action.kind, TaskName: "completed-task", Verdict: action.verdict, SnapshotDigest: item.SnapshotDigest}
			if err := r.Store.CreateActionRecord(t.Context(), record); err != nil {
				t.Fatal(err)
			}
			if handled, err := r.applyIssueActionRecord(t.Context(), monitor, item, record, &corev1alpha1.Task{}); err != nil || !handled {
				t.Fatalf("apply paused result = %v, %v", handled, err)
			}
			stored, err := r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
			if err != nil || stored.WorkflowPhase != repositoryMonitorIssuePhasePaused || stored.LastActionID != record.ID || stored.LastActionTaskName != record.TaskName {
				t.Fatalf("paused terminal result lost: %+v, %v", stored, err)
			}
			if handled, err := r.applyIssueActionRecord(t.Context(), monitor, stored, record, &corev1alpha1.Task{}); err != nil || handled {
				t.Fatalf("repeated paused result = %v, %v", handled, err)
			}
		})
	}
}

func TestRepositoryMonitorPauseStillReleasesFailedImplementationBudget(t *testing.T) {
	r, monitor, issue := repositoryMonitorPauseTestReconciler(t, true)
	item := repositoryMonitorItemFromIssue(monitor, issue, nil)
	item.WorkflowPhase = repositoryMonitorIssuePhaseImplementing
	item.LastActionKind = repositoryMonitorIssueActionImplementation
	item.LastActionTaskName = "failed-implementation"
	if err := r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: item.LastActionTaskName, Namespace: monitor.Namespace}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed}}
	if err := r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	job := &store.ImplementationJob{ID: repositoryMonitorImplementationJobID(task.Name), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, IssueNumber: issue.Number, TaskName: task.Name, Phase: item.WorkflowPhase, CreatedAt: time.Now()}
	if err := r.Store.CreateImplementationJob(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "pause-inventory", TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number}, "orka-agents", "orka"); err != nil {
		t.Fatal(err)
	}
	if handled, err := r.ingestCompletedRepositoryMonitorIssueTasks(t.Context(), monitor); err != nil || !handled {
		t.Fatalf("ingest failed paused Task = %v, %v", handled, err)
	}
	if _, _, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "pause-after-failure", TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number}, "orka-agents", "orka"); err != nil {
		t.Fatal(err)
	}
	item, err := r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
	if err != nil || item.LastVerdict != repositoryMonitorReviewVerdictFailed || item.SkipReason != repositoryMonitorReviewVerdictFailed {
		t.Fatalf("pause inventory lost failed result: %+v, %v", item, err)
	}
	job, err = r.Store.GetImplementationJob(t.Context(), monitor.Namespace, job.ID)
	if err != nil || job.Phase != repositoryMonitorIssuePhaseBlocked || job.CompletedAt == nil {
		t.Fatalf("failed paused job retained its active budget: %+v, %v", job, err)
	}
}

func TestRepositoryMonitorPauseRemovalReplaysResultBeforeNewCommand(t *testing.T) {
	r, monitor, issue := repositoryMonitorPauseTestReconciler(t, false)
	item := repositoryMonitorItemFromIssue(monitor, issue, nil)
	item.WorkflowPhase = repositoryMonitorIssuePhasePaused
	item.LabelsJSON = `["bug","orka:pause"]`
	item.LastActionTaskName = "completed-triage"
	item.LastActionKind = repositoryMonitorIssueActionTriage
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: item.LastActionTaskName, Namespace: monitor.Namespace}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded}}
	if err := r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	record := &store.ActionRecord{ID: repositoryMonitorIssueActionRecordID(task), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: issue.Number, ActionKind: item.LastActionKind, TaskName: task.Name, Verdict: "actionable", SnapshotDigest: item.SnapshotDigest}
	item.LastActionID = record.ID
	if err := r.Store.CreateActionRecord(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
	command := &store.CommandEvent{ID: "new-plan-command", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: issue.Number, Intent: "plan", Status: "accepted", CreatedAt: time.Now()}
	if err := r.Store.CreateCommandEvent(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if _, created, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "unpause-inventory", TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number, CommandEventID: command.ID}, "orka-agents", "orka"); err != nil || created != 1 {
		t.Fatalf("new command after pause removal: created=%d, err=%v", created, err)
	}
	item, err := r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
	if err != nil || item.WorkflowPhase != repositoryMonitorIssuePhasePlanQueued || item.LastActionKind != repositoryMonitorIssueActionPlan || item.LastActionTaskName == task.Name {
		t.Fatalf("fresh command could not advance resumed result: %+v, %v", item, err)
	}
}
