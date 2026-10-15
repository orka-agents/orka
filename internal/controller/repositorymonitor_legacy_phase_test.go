package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

//nolint:gocyclo // Keep persisted-state normalization and command progression in one regression fixture.
func TestRepositoryMonitorLegacyIssuePhasesReconcileAndAcceptCommands(t *testing.T) {
	for _, tc := range []struct {
		legacyPhase string
		wantPhase   string
		wantCommand string
	}{
		{legacyPhase: "approved", wantPhase: repositoryMonitorIssuePhasePlanned, wantCommand: repositoryMonitorIssuePhaseImplementationQueued},
		{legacyPhase: "approval_required", wantPhase: repositoryMonitorIssuePhaseBlocked, wantCommand: repositoryMonitorIssuePhasePlanQueued},
	} {
		t.Run(tc.legacyPhase, func(t *testing.T) {
			r, monitor, issue := repositoryMonitorLegacyPhaseTestReconciler(t)
			item := repositoryMonitorItemFromIssue(monitor, issue, nil)
			item.WorkflowPhase = tc.legacyPhase
			item.LastActionID = "retained-plan"
			item.LastActionKind = repositoryMonitorIssueActionPlan
			item.LastActionTaskName = "retained-planner-task"
			if tc.legacyPhase == "approval_required" {
				item.SkipReason = "plan_requires_human_approval"
				item.LastVerdict = repositoryMonitorReviewVerdictNeedsHuman
			}
			if err := r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			plan := &store.ActionRecord{ID: item.LastActionID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: issue.Number, ActionKind: item.LastActionKind, TaskName: item.LastActionTaskName, SnapshotDigest: item.SnapshotDigest, Verdict: repositoryMonitorIssueVerdictReady, PayloadJSON: `{}`, CreatedAt: time.Now()}
			if tc.legacyPhase == "approval_required" {
				plan.Verdict = repositoryMonitorReviewVerdictNeedsHuman
			}
			if err := r.Store.CreateActionRecord(t.Context(), plan); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, created, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "legacy-inventory", TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number}, "orka-agents", "orka"); err != nil || created != 0 {
					t.Fatalf("background inventory created=%d, err=%v", created, err)
				}
			}
			stored, err := r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
			if err != nil {
				t.Fatal(err)
			}
			if stored.WorkflowPhase != tc.wantPhase || stored.SnapshotDigest != item.SnapshotDigest || stored.LastActionID != plan.ID || stored.LastActionTaskName != plan.TaskName {
				t.Fatalf("unchanged legacy snapshot did not recover: %+v", stored)
			}
			if tc.legacyPhase == "approval_required" && stored.SkipReason != repositoryMonitorLegacyPlanRequiresReplanning {
				t.Fatalf("unapproved plan lost its hold: %+v", stored)
			}
			var tasks corev1alpha1.TaskList
			if err := r.List(t.Context(), &tasks); err != nil || len(tasks.Items) != 0 {
				t.Fatalf("normalization started work without a command: tasks=%d, err=%v", len(tasks.Items), err)
			}
			command := &store.CommandEvent{ID: "fresh-implement", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: issue.Number, Intent: repositoryMonitorCommandIntentImplement, IssueSnapshotDigest: stored.SnapshotDigest, Status: "accepted", CreatedAt: time.Now()}
			if err := r.Store.CreateCommandEvent(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if _, created, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "fresh-implement-run", TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number, CommandEventID: command.ID}, "orka-agents", "orka"); err != nil || created != 1 {
				t.Fatalf("fresh implement created=%d, err=%v", created, err)
			}
			stored, err = r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
			if err != nil || stored.WorkflowPhase != tc.wantCommand || stored.LastActionTaskName == plan.TaskName {
				t.Fatalf("fresh command did not advance legacy workflow: %+v, err=%v", stored, err)
			}
		})
	}
}

func repositoryMonitorLegacyPhaseTestReconciler(t *testing.T) (*RepositoryMonitorReconciler, *corev1alpha1.RepositoryMonitor, repositoryMonitorIssue) {
	t.Helper()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("legacy-issue-phase")
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/repos/orka-agents/orka/issues/42" {
			t.Errorf("unexpected GitHub request: %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42,"title":"Fix issue","body":"Keep scope small","state":"open","labels":[]}`))
	}))
	t.Cleanup(server.Close)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	issue := repositoryMonitorIssue{Number: 42, Title: "Fix issue", Body: "Keep scope small", State: "open", Labels: []string{}}
	return &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, GitHubAPIBaseURL: server.URL}, monitor, issue
}

func TestRepositoryMonitorLegacyIssuePhasesPreserveStop(t *testing.T) {
	for _, phase := range []string{"approved", "approval_required"} {
		t.Run(phase, func(t *testing.T) {
			r, monitor, issue := repositoryMonitorLegacyPhaseTestReconciler(t)
			item := repositoryMonitorItemFromIssue(monitor, issue, nil)
			item.WorkflowPhase = phase
			item.SkipReason = repositoryMonitorIssueSkipStoppedByCommand
			if err := r.Store.UpsertMonitorItem(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			command := &store.CommandEvent{ID: "implement-stopped-issue", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: issue.Number, Intent: repositoryMonitorCommandIntentImplement, IssueSnapshotDigest: item.SnapshotDigest, Status: "accepted", CreatedAt: time.Now()}
			if err := r.Store.CreateCommandEvent(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			for _, commandID := range []string{"", command.ID} {
				if _, created, _, err := r.processIssueInventoryRun(t.Context(), monitor, &store.MonitorRun{ID: "stopped-legacy-run" + commandID, TargetKind: repositoryMonitorIssueKind, TargetNumber: issue.Number, CommandEventID: commandID}, "orka-agents", "orka"); err != nil || created != 0 {
					t.Fatalf("stopped legacy inventory created=%d, err=%v", created, err)
				}
				stored, err := r.Store.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, "42")
				if err != nil || stored.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || stored.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
					t.Fatalf("legacy normalization lost explicit stop: %+v, err=%v", stored, err)
				}
			}
			var tasks corev1alpha1.TaskList
			if err := r.List(t.Context(), &tasks); err != nil || len(tasks.Items) != 0 {
				t.Fatalf("stopped legacy issue started work: tasks=%d, err=%v", len(tasks.Items), err)
			}
		})
	}
}
