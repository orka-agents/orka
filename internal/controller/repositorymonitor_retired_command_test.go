package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

//nolint:gocyclo // Keep both retired intents and their persisted action states in one lifecycle fixture.
func TestRepositoryMonitorRetiresAcceptedCommands(t *testing.T) {
	for _, retired := range []struct {
		intent string
		kind   string
		action string
		reason string
	}{
		{repositoryMonitorRetiredAutomergeIntent, repositoryMonitorPullRequestKind, "automerge", repositoryMonitorAutomergeCommandRetiredReason},
		{repositoryMonitorRetiredApprovePlanIntent, repositoryMonitorIssueKind, "approve", repositoryMonitorApprovePlanRetiredReason},
	} {
		t.Run(retired.intent, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				actionStatus string
				runPhase     string
				wantStatus   string
			}{
				{"unqueued", repositoryMonitorWorkActionStatusQueued, "", repositoryMonitorWorkActionStatusFailed},
				{"running", repositoryMonitorWorkActionStatusRunning, repositoryMonitorRunPhaseRunning, repositoryMonitorWorkActionStatusFailed},
				{"retry_pending", store.RepositoryMonitorWorkActionStatusRetryPending, repositoryMonitorRunPhaseQueued, repositoryMonitorWorkActionStatusFailed},
				{"completed_run", repositoryMonitorWorkActionStatusQueued, repositoryMonitorRunPhaseSucceeded, repositoryMonitorWorkActionStatusFailed},
				{"cancelled", repositoryMonitorWorkActionStatusCancelled, repositoryMonitorRunPhaseQueued, repositoryMonitorWorkActionStatusCancelled},
				{"succeeded", repositoryMonitorWorkActionStatusSucceeded, repositoryMonitorRunPhaseSucceeded, repositoryMonitorWorkActionStatusSucceeded},
				{"missing_action", "", "", repositoryMonitorWorkActionStatusFailed},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					db := setupControllerSQLiteStore(t)
					monitor, _ := repositoryMonitorInventoryTestObjects("retired-automerge")
					command := &store.CommandEvent{ID: "legacy-" + retired.intent, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: retired.kind, Number: 1, HeadSHA: "head", Intent: retired.intent, Status: repositoryMonitorCommandAccepted}
					if err := db.CreateCommandEvent(ctx, command); err != nil {
						t.Fatal(err)
					}
					actionID := store.RepositoryMonitorWorkActionID(command.ID, retired.action)
					if tc.actionStatus != "" {
						if err := db.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, DesiredAction: retired.action, Status: tc.actionStatus}); err != nil {
							t.Fatal(err)
						}
					}
					if tc.runPhase != "" {
						if err := db.CreateMonitorRun(ctx, &store.MonitorRun{ID: repositoryMonitorCommandRunIDFromCommand(command.ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Phase: tc.runPhase, StartedAt: time.Now()}); err != nil {
							t.Fatal(err)
						}
					}
					otherAction := &store.WorkAction{ID: "other-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: 2, TargetSHA: "other-head", DesiredAction: "review", Status: repositoryMonitorWorkActionStatusQueued}
					if err := db.CreateWorkAction(ctx, otherAction); err != nil {
						t.Fatal(err)
					}
					r := &RepositoryMonitorReconciler{Store: db}
					for range 2 {
						queued, err := r.enqueueAcceptedRepositoryMonitorCommands(ctx, monitor)
						if err != nil || queued {
							t.Fatalf("retired command enqueued: queued=%v err=%v", queued, err)
						}
					}
					persisted, err := db.GetCommandEvent(ctx, monitor.Namespace, command.ID)
					if err != nil || persisted.Status != "processed" || persisted.ProcessedAt == nil || persisted.Error != retired.reason {
						t.Fatalf("retired command=%+v err=%v", persisted, err)
					}
					action, err := db.GetWorkAction(ctx, monitor.Namespace, actionID)
					if err != nil || action.Status != tc.wantStatus {
						t.Fatalf("retired action=%+v err=%v, want %s", action, err, tc.wantStatus)
					}
					if tc.wantStatus == repositoryMonitorWorkActionStatusFailed && (action.Error != retired.reason || action.CompletedAt == nil) {
						t.Fatalf("retired action lacks terminal reason: %+v", action)
					}
					other, err := db.GetWorkAction(ctx, monitor.Namespace, otherAction.ID)
					if err != nil || other.Status != repositoryMonitorWorkActionStatusQueued {
						t.Fatalf("retirement affected another action: %+v err=%v", other, err)
					}
					actions, _, err := db.ListWorkActions(ctx, store.WorkActionFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number})
					if err != nil || len(actions) != 1 || actions[0].ID != actionID {
						t.Fatalf("retirement created a different action identity: actions=%+v err=%v", actions, err)
					}
					runs, _, err := db.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
					wantRuns := 0
					if tc.runPhase != "" {
						wantRuns = 1
					}
					if err != nil || len(runs) != wantRuns {
						t.Fatalf("retirement created a run: runs=%+v err=%v", runs, err)
					}
					if retired.kind == repositoryMonitorPullRequestKind {
						pr := repositoryMonitorPullRequest{Number: command.Number, HeadSHA: command.HeadSHA}
						item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
						_, description, err := r.repositoryMonitorReadyOutcome(ctx, monitor, pr, item)
						if err != nil || description != "Waiting for a clean review of this commit." {
							t.Fatalf("retired command still blocks readiness: description=%q err=%v", description, err)
						}
					}
				})
			}
		})
	}
}

func TestRepositoryMonitorRetiresQueuedAutomergeRunWithoutClearingStop(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("retired-stopped-automerge")
	command := &store.CommandEvent{ID: "legacy-running-automerge", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "head", Intent: repositoryMonitorRetiredAutomergeIntent, Status: repositoryMonitorCommandAccepted}
	if err := db.CreateCommandEvent(ctx, command); err != nil {
		t.Fatal(err)
	}
	pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: "head"}
	item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
	item.SkipReason = repositoryMonitorIssueSkipStoppedByCommand
	item.RepairState = repositoryMonitorRepairPhaseFailed
	item.AutomergeState = repositoryMonitorAutomergeStateBlocked
	if err := db.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Store: db}
	run := &store.MonitorRun{ID: "legacy-running-run", CommandEventID: command.ID}
	handled, created, err := r.tryProcessPullRequestCommandRun(ctx, monitor, run, "orka-agents", "orka", pr, item)
	if err != nil || !handled || created != 0 {
		t.Fatalf("queued retired run: handled=%v created=%d err=%v", handled, created, err)
	}
	persisted, err := db.GetCommandEvent(ctx, monitor.Namespace, command.ID)
	if err != nil || persisted.Status != "processed" {
		t.Fatalf("queued retired command=%+v err=%v", persisted, err)
	}
	current, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, item.ItemKey)
	if err != nil || current.SkipReason != repositoryMonitorIssueSkipStoppedByCommand || current.RepairState != repositoryMonitorRepairPhaseFailed || current.AutomergeState != repositoryMonitorAutomergeStateBlocked {
		t.Fatalf("retirement cleared stop state: item=%+v err=%v", current, err)
	}
	action, err := db.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, command.Intent))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed || action.Error != repositoryMonitorAutomergeCommandRetiredReason {
		t.Fatalf("queued retired action=%+v err=%v", action, err)
	}
}

func TestRepositoryMonitorRetiresQueuedApprovePlanRunWithoutChangingIssue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopped bool
	}{{"planned", false}, {"stopped", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, _ := repositoryMonitorInventoryTestObjects("retired-plan-approval")
			command := &store.CommandEvent{ID: "legacy-plan-approval", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 1, IssueSnapshotDigest: "old-snapshot", Intent: repositoryMonitorRetiredApprovePlanIntent, Status: repositoryMonitorCommandAccepted}
			if err := db.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			actionID := store.RepositoryMonitorWorkActionID(command.ID, "approve")
			if err := db.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, DesiredAction: "approve", Status: repositoryMonitorWorkActionStatusQueued}); err != nil {
				t.Fatal(err)
			}
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "1", Number: 1, WorkflowPhase: repositoryMonitorIssuePhasePlanned, SnapshotDigest: "current-snapshot", LastCommandID: "implement-command", LastCommandIntent: repositoryMonitorCommandIntentImplement}
			if tc.stopped {
				item.WorkflowPhase, item.SkipReason = repositoryMonitorIssuePhaseBlocked, repositoryMonitorIssueSkipStoppedByCommand
			}
			if err := db.UpsertMonitorItem(ctx, item); err != nil {
				t.Fatal(err)
			}
			before := *item
			r := &RepositoryMonitorReconciler{Store: db}
			run := &store.MonitorRun{ID: "legacy-approval-run", CommandEventID: command.ID}
			if created, err := r.processIssueCommandRun(ctx, monitor, run, item, "orka-agents", "orka"); err != nil || created != 0 {
				t.Fatalf("queued retired issue run: created=%d err=%v", created, err)
			}
			persisted, err := db.GetCommandEvent(ctx, monitor.Namespace, command.ID)
			if err != nil || persisted.Status != repositoryMonitorCommandProcessed || persisted.Error != repositoryMonitorApprovePlanRetiredReason {
				t.Fatalf("queued retired command=%+v err=%v", persisted, err)
			}
			action, err := db.GetWorkAction(ctx, monitor.Namespace, actionID)
			if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed || action.Error != repositoryMonitorApprovePlanRetiredReason {
				t.Fatalf("queued historical approval action=%+v err=%v", action, err)
			}
			current, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorIssueKind, item.ItemKey)
			if err != nil || current.WorkflowPhase != before.WorkflowPhase || current.SkipReason != before.SkipReason || current.LastCommandID != before.LastCommandID || current.LastCommandIntent != before.LastCommandIntent || current.SnapshotDigest != before.SnapshotDigest {
				t.Fatalf("retirement changed issue workflow: item=%+v err=%v", current, err)
			}
		})
	}
}

func TestRepositoryMonitorInventoryRetiresCommandsBeforeTargetPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		intent       string
		kind         string
		policy       string
		actionStatus string
	}{
		{"approve_closed", repositoryMonitorRetiredApprovePlanIntent, repositoryMonitorIssueKind, "closed", repositoryMonitorWorkActionStatusSucceeded},
		{"approve_excluded", repositoryMonitorRetiredApprovePlanIntent, repositoryMonitorIssueKind, "excluded", repositoryMonitorWorkActionStatusCancelled},
		{"approve_disabled", repositoryMonitorRetiredApprovePlanIntent, repositoryMonitorIssueKind, "disabled", repositoryMonitorWorkActionStatusSucceeded},
		{"automerge_closed", repositoryMonitorRetiredAutomergeIntent, repositoryMonitorPullRequestKind, "closed", repositoryMonitorWorkActionStatusSucceeded},
		{"automerge_disabled", repositoryMonitorRetiredAutomergeIntent, repositoryMonitorPullRequestKind, "disabled", repositoryMonitorWorkActionStatusCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("retired-target-policy")
			enabled := tc.policy != "disabled"
			monitor.Spec.Targets.Issues.Enabled = enabled
			monitor.Spec.Targets.PullRequests.Enabled = &enabled
			monitor.Spec.Targets.Issues.ExcludeLabels = []string{"excluded"}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				state := repositoryMonitorItemStateOpen
				labels := []map[string]string{}
				if tc.policy == "closed" {
					state = "closed"
				}
				if tc.policy == "excluded" {
					labels = append(labels, map[string]string{"name": "excluded"})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": "Changed issue", "body": "New requirements", "state": state, "labels": labels, "updated_at": "2026-06-01T00:00:00Z", "head": map[string]string{"sha": "head"}, "base": map[string]string{"ref": "main"}})
			}))
			defer server.Close()
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db, GitHubAPIBaseURL: server.URL}
			command := &store.CommandEvent{ID: "legacy-policy-command", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: tc.kind, Number: 1, HeadSHA: "head", IssueSnapshotDigest: "old-snapshot", Intent: tc.intent, Status: repositoryMonitorCommandAccepted}
			if err := db.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			desiredAction := store.RepositoryMonitorDesiredActionForActionKind(repositoryMonitorCommandActionKind(command.Intent))
			completedAt := time.Now().Add(-time.Hour)
			action := &store.WorkAction{ID: store.RepositoryMonitorWorkActionID(command.ID, desiredAction), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, DesiredAction: desiredAction, Status: tc.actionStatus, CompletedAt: &completedAt, Error: "preserved-terminal-outcome"}
			if err := db.CreateWorkAction(ctx, action); err != nil {
				t.Fatal(err)
			}
			beforeAction, err := db.GetWorkAction(ctx, monitor.Namespace, action.ID)
			if err != nil {
				t.Fatal(err)
			}
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: tc.kind, ItemKey: "1", Number: 1, State: repositoryMonitorItemStateOpen, WorkflowPhase: repositoryMonitorIssuePhaseBlocked, SnapshotDigest: "current-snapshot", SkipReason: repositoryMonitorIssueSkipStoppedByCommand, LastCommandID: "stop-command", LastCommandIntent: repositoryMonitorCommandIntentStop}
			if err := db.UpsertMonitorItem(ctx, item); err != nil {
				t.Fatal(err)
			}
			beforeItem, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, item.Kind, item.ItemKey)
			if err != nil {
				t.Fatal(err)
			}
			run := &store.MonitorRun{ID: "legacy-policy-run", CommandEventID: command.ID, TargetKind: tc.kind, TargetNumber: command.Number}
			selected, created, skipped, err := r.processRepositoryMonitorInventoryRun(ctx, monitor, run, "orka-agents", "orka")
			if err != nil || selected != 0 || created != 0 || skipped != 0 || requests.Load() != 0 {
				t.Fatalf("retired inventory reached target policy: selected=%d created=%d skipped=%d requests=%d err=%v", selected, created, skipped, requests.Load(), err)
			}
			persisted, err := db.GetCommandEvent(ctx, monitor.Namespace, command.ID)
			if err != nil || persisted.Status != repositoryMonitorCommandProcessed || persisted.Error != repositoryMonitorRetiredCommandReason(command.Intent) {
				t.Fatalf("retired inventory command=%+v err=%v", persisted, err)
			}
			currentAction, err := db.GetWorkAction(ctx, monitor.Namespace, action.ID)
			if err != nil || !reflect.DeepEqual(currentAction, beforeAction) {
				t.Fatalf("retired inventory changed terminal action: before=%+v after=%+v err=%v", beforeAction, currentAction, err)
			}
			currentItem, err := db.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, item.Kind, item.ItemKey)
			if err != nil || !reflect.DeepEqual(currentItem, beforeItem) {
				t.Fatalf("retired inventory changed stopped item: before=%+v after=%+v err=%v", beforeItem, currentItem, err)
			}
		})
	}
}
