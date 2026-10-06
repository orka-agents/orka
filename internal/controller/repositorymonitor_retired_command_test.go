package controller

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorRetiresAcceptedAutomergeCommands(t *testing.T) {
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
			command := &store.CommandEvent{ID: "legacy-automerge", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "head", Intent: repositoryMonitorRetiredAutomergeIntent, Status: repositoryMonitorCommandAccepted}
			if err := db.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			actionID := store.RepositoryMonitorWorkActionID(command.ID, command.Intent)
			if tc.actionStatus != "" {
				if err := db.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, DesiredAction: command.Intent, Status: tc.actionStatus}); err != nil {
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
			if err != nil || persisted.Status != "processed" || persisted.ProcessedAt == nil || persisted.Error != repositoryMonitorAutomergeCommandRetiredReason {
				t.Fatalf("retired command=%+v err=%v", persisted, err)
			}
			action, err := db.GetWorkAction(ctx, monitor.Namespace, actionID)
			if err != nil || action.Status != tc.wantStatus {
				t.Fatalf("retired action=%+v err=%v, want %s", action, err, tc.wantStatus)
			}
			if tc.wantStatus == repositoryMonitorWorkActionStatusFailed && (action.Error != repositoryMonitorAutomergeCommandRetiredReason || action.CompletedAt == nil) {
				t.Fatalf("retired action lacks terminal reason: %+v", action)
			}
			other, err := db.GetWorkAction(ctx, monitor.Namespace, otherAction.ID)
			if err != nil || other.Status != repositoryMonitorWorkActionStatusQueued {
				t.Fatalf("retirement affected another action: %+v err=%v", other, err)
			}
			runs, _, err := db.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
			wantRuns := 0
			if tc.runPhase != "" {
				wantRuns = 1
			}
			if err != nil || len(runs) != wantRuns {
				t.Fatalf("retirement created a run: runs=%+v err=%v", runs, err)
			}
			pr := repositoryMonitorPullRequest{Number: command.Number, HeadSHA: command.HeadSHA}
			item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
			_, description, err := r.repositoryMonitorReadyOutcome(ctx, monitor, pr, item)
			if err != nil || description != "Waiting for a clean review of this commit." {
				t.Fatalf("retired command still blocks readiness: description=%q err=%v", description, err)
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
