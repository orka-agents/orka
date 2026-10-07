package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

//nolint:gocyclo // Keep exhausted command recovery and its public readiness outcome in one fixture.
func TestRepositoryMonitorInventoryPreservesPreTaskRepairFailure(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("terminal-pre-task")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Review.Publish.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
	command := &store.CommandEvent{ID: "policy-pre-task", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "head", Source: "controller_policy", Intent: repositoryMonitorCommandIntentFixCI, Status: repositoryMonitorCommandAccepted}
	if err := db.CreateCommandEvent(ctx, command); err != nil {
		t.Fatal(err)
	}
	runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
	if err := db.CreateMonitorRun(ctx, &store.MonitorRun{ID: runID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Phase: repositoryMonitorRunPhaseFailed, Error: "[retry_scheduled] task creation failed"}); err != nil {
		t.Fatal(err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent))
	if err := db.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, DesiredAction: command.Intent, Status: repositoryMonitorWorkActionStatusQueued}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "repair-" + repositoryMonitorShortHash(command.ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: command.Number, HeadSHA: command.HeadSHA, Phase: repositoryMonitorRepairPhaseQueued, TaskName: "missing-task", LastError: repositoryMonitorRepairTaskCreateError}); err != nil {
		t.Fatal(err)
	}
	for attempt := range repositoryMonitorCommandMaxRetries {
		if err := db.CreateMonitorEvent(ctx, &store.MonitorEvent{ID: fmt.Sprint("failed-attempt-", attempt), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, RunID: runID, EventType: repositoryMonitorRunFailurePermanent}); err != nil {
			t.Fatal(err)
		}
	}
	// Terminalization first fails the action. Command processing follows on the
	// next reconciliation, and an inventory can run between those transitions.
	if queued, err := r.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor); err != nil || queued {
		t.Fatalf("exhausted command recovery: queued=%v err=%v", queued, err)
	}
	reviewID := seedRepositoryMonitorAutomergeReview(t, ctx, db, monitor.Name, command.Number, command.HeadSHA)
	if err := db.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: command.Kind, ItemKey: "1", Number: command.Number, HeadSHA: command.HeadSHA, State: repositoryMonitorItemStateOpen, RepairState: repositoryMonitorRepairPhaseFailed, LastReviewID: reviewID, LastReviewedHeadSHA: command.HeadSHA, LastVerdict: repositoryMonitorReviewVerdictPassed}); err != nil {
		t.Fatal(err)
	}
	var statusID atomic.Int64
	statusWrites := make(chan repositoryMonitorCommitStatus, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls/1"):
			_, _ = w.Write([]byte(`{"number":1,"state":"open","mergeable_state":"clean","head":{"sha":"head","repo":{"full_name":"orka-agents/orka"}},"base":{"ref":"main"},"labels":[]}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[]`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":1,"name":"tests","status":"completed","conclusion":"success"}]}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"total_count":0,"statuses":[]}`))
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/"):
			var status repositoryMonitorCommitStatus
			if err := json.NewDecoder(req.Body).Decode(&status); err != nil {
				t.Error(err)
			}
			status.ID = statusID.Add(1)
			statusWrites <- status
			_ = json.NewEncoder(w).Encode(status)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	r.GitHubAPIBaseURL = server.URL
	latest := ""
	for poll := range 2 {
		run := &store.MonitorRun{ID: fmt.Sprint("inventory-", poll), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: command.Number}
		if _, _, _, err := r.processRepositoryMonitorInventoryRun(ctx, monitor, run, "orka-agents", "orka"); err != nil {
			t.Fatal(err)
		}
		for len(statusWrites) > 0 {
			status := <-statusWrites
			if status.State == repositoryMonitorStatusSuccess {
				t.Fatal("terminal pre-Task failure published readiness success")
			}
			latest = status.State
		}
		item, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
		if err != nil || item.RepairState != repositoryMonitorRepairPhaseFailed || latest != repositoryMonitorStatusFailure {
			t.Fatalf("poll %d lost terminal failure: item=%+v readiness=%q err=%v", poll, item, latest, err)
		}
		if _, err := r.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepositoryMonitorTerminalRepairProjectionUsesCurrentHeadEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		head          string
		source        string
		commandStatus string
		intent        string
		actionStatus  string
		jobPhase      string
		mutationPhase string
		want          string
	}{
		{name: "failed", actionStatus: repositoryMonitorWorkActionStatusFailed, want: repositoryMonitorRepairPhaseFailed},
		{name: "budget_blocked", actionStatus: repositoryMonitorWorkActionStatusBlocked, want: repositoryMonitorRepairPhaseFailed},
		{name: "accepted_terminal_failure", commandStatus: repositoryMonitorCommandAccepted, actionStatus: repositoryMonitorWorkActionStatusFailed, want: repositoryMonitorRepairPhaseFailed},
		{name: "accepted_active", commandStatus: repositoryMonitorCommandAccepted, actionStatus: repositoryMonitorWorkActionStatusQueued},
		{name: "cancelled", actionStatus: repositoryMonitorWorkActionStatusCancelled},
		{name: "succeeded", actionStatus: repositoryMonitorWorkActionStatusSucceeded},
		{name: "old_head", head: "old-head", actionStatus: repositoryMonitorWorkActionStatusFailed},
		{name: "manual", source: "api", actionStatus: repositoryMonitorWorkActionStatusFailed},
		{name: "review", intent: repositoryMonitorCommandIntentReview, actionStatus: repositoryMonitorWorkActionStatusFailed},
		{name: "orphan_job", actionStatus: repositoryMonitorWorkActionStatusFailed, jobPhase: repositoryMonitorRepairPhaseQueued, want: repositoryMonitorRepairPhaseFailed},
		{name: "terminal_job", actionStatus: repositoryMonitorWorkActionStatusFailed, jobPhase: repositoryMonitorRepairPhaseSucceeded, want: repositoryMonitorRepairPhaseSucceeded},
		{name: "terminal_mutation", intent: repositoryMonitorCommandIntentUpdateBranch, actionStatus: repositoryMonitorWorkActionStatusFailed, mutationPhase: repositoryMonitorRunPhaseSucceeded, want: repositoryMonitorRepairPhaseSucceeded},
		{name: "pending_mutation", intent: repositoryMonitorCommandIntentUpdateBranch, actionStatus: repositoryMonitorWorkActionStatusFailed, mutationPhase: repositoryMonitorAutomergeStatePending, want: repositoryMonitorRepairPhaseQueued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, _ := repositoryMonitorInventoryTestObjects("terminal-head-projection")
			command := &store.CommandEvent{ID: "policy-projection", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: firstNonEmptyString(tc.head, "head"), Source: firstNonEmptyString(tc.source, "controller_policy"), Intent: firstNonEmptyString(tc.intent, repositoryMonitorCommandIntentFixCI), Status: firstNonEmptyString(tc.commandStatus, repositoryMonitorCommandProcessed), Error: "a terminal reason", CreatedAt: time.Now().Add(-time.Hour)}
			if err := db.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			desiredAction := store.RepositoryMonitorDesiredActionForIntent(command.Intent)
			action := &store.WorkAction{ID: store.RepositoryMonitorWorkActionID(command.ID, desiredAction), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, DesiredAction: desiredAction, Status: tc.actionStatus, Error: command.Error}
			if action.Status == repositoryMonitorWorkActionStatusBlocked {
				action.Error, action.BlockedReason = "", "repair_head_budget_exhausted"
			}
			if err := db.CreateWorkAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			// Give the command a later timestamp than its authoritative evidence
			// to prove that duplicate command projection cannot replace success.
			if tc.jobPhase != "" {
				if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "repair-" + repositoryMonitorShortHash(command.ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: command.Number, HeadSHA: command.HeadSHA, Phase: tc.jobPhase, CreatedAt: command.CreatedAt.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mutationPhase != "" {
				if err := db.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: repositoryMonitorUpdateBranchMutationID(command.ID), CommandEventID: command.ID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Operation: repositoryMonitorUpdateBranchOperation, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Status: tc.mutationPhase, CreatedAt: command.CreatedAt.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			r := &RepositoryMonitorReconciler{Store: db}
			state, err := r.repositoryMonitorRepairStateForHead(ctx, monitor, command.Number, "head")
			if err != nil || state != tc.want {
				t.Fatalf("terminal projection=%q err=%v, want %q", state, err, tc.want)
			}
		})
	}
}
