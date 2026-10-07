package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

//nolint:gocyclo // Exercise retry identity and both configured bounds in one durable lifecycle.
func TestRepositoryMonitorAutomaticRepairRecoversExhaustedPreTaskCommands(t *testing.T) {
	for _, bound := range []string{"head", "pull_request"} {
		t.Run(bound, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, secret := repositoryMonitorInventoryTestObjects("orphan-policy")
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			small, large := int32(2), int32(10)
			monitor.Spec.Repair.MaxRepairsPerPR, monitor.Spec.Repair.MaxRepairsPerHead = &large, &small
			wantReason := "repair_head_budget_exhausted"
			if bound == "pull_request" {
				monitor.Spec.Repair.MaxRepairsPerPR, monitor.Spec.Repair.MaxRepairsPerHead = &small, &large
				wantReason = repositoryMonitorRepairPRBudgetReason
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Store: db}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/check-runs") {
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":1,"name":"tests","status":"completed","conclusion":"failure"}]}`))
			}))
			defer server.Close()
			r.GitHubAPIBaseURL = server.URL
			pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head", State: "open", HeadRepo: "orka-agents/orka"}
			item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
			var previousID string
			for attempt := range 2 {
				if handled, err := r.tryRepositoryMonitorAutomaticRepair(ctx, monitor, &store.MonitorRun{}, "orka-agents", "orka", pr, item); err != nil || !handled {
					t.Fatalf("attempt %d not queued: handled=%v err=%v", attempt, handled, err)
				}
				commands, _, err := db.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Status: repositoryMonitorCommandAccepted})
				if err != nil || len(commands) != 1 || commands[0].ID == previousID {
					t.Fatalf("attempt %d reused terminal command: commands=%+v err=%v", attempt, commands, err)
				}
				command := commands[0]
				previousID = command.ID
				job := &store.RepairJob{ID: "repair-" + repositoryMonitorShortHash(command.ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: pr.Number, HeadSHA: pr.HeadSHA, Intent: command.Intent, Source: command.Source, Phase: repositoryMonitorRepairPhaseQueued, TaskName: "missing-" + command.ID, LastError: repositoryMonitorRepairTaskCreateError}
				if err := db.CreateRepairJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				// An accepted command with no Task retains its canonical retries.
				item.RepairState = ""
				if handled, err := r.tryRepositoryMonitorAutomaticRepair(ctx, monitor, &store.MonitorRun{}, "orka-agents", "orka", pr, item); err != nil || !handled {
					t.Fatalf("accepted command recovery: handled=%v err=%v", handled, err)
				}
				all, _, err := db.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
				if err != nil || len(all) != attempt+1 {
					t.Fatalf("accepted command spawned another attempt: commands=%+v err=%v", all, err)
				}
				// Persist the state left after pre-Task run retries are exhausted.
				command.Status = "processed"
				now := time.Now()
				command.ProcessedAt, command.Error = &now, "retry_attempts_exhausted"
				if err := db.UpdateCommandEvent(ctx, &command); err != nil {
					t.Fatal(err)
				}
				action, err := db.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForIntent(command.Intent)))
				if err != nil {
					t.Fatal(err)
				}
				action.Status, action.Error, action.CompletedAt = repositoryMonitorWorkActionStatusFailed, command.Error, &now
				if err := db.UpdateWorkAction(ctx, action); err != nil {
					t.Fatal(err)
				}
				run, err := db.GetMonitorRun(ctx, monitor.Namespace, repositoryMonitorCommandRunIDFromCommand(command.ID))
				if err != nil {
					t.Fatal(err)
				}
				run.Phase, run.Error, run.CompletedAt = repositoryMonitorRunPhaseFailed, "[run_failed] retry_attempts_exhausted", &now
				if err := db.UpdateMonitorRun(ctx, run); err != nil {
					t.Fatal(err)
				}
				item.RepairState, err = r.repositoryMonitorRepairStateForHead(ctx, monitor, pr.Number, pr.HeadSHA)
				if err != nil || item.RepairState != repositoryMonitorRepairPhaseFailed {
					t.Fatalf("exhausted pre-Task failure was lost: state=%q err=%v", item.RepairState, err)
				}
			}
			if handled, err := r.tryRepositoryMonitorAutomaticRepair(ctx, monitor, &store.MonitorRun{}, "orka-agents", "orka", pr, item); err != nil || !handled || item.SkipReason != wantReason {
				t.Fatalf("attempt bound not enforced: handled=%v reason=%q err=%v", handled, item.SkipReason, err)
			}
			commands, _, err := db.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
			if err != nil || len(commands) != 2 {
				t.Fatalf("bounded recovery commands=%+v err=%v", commands, err)
			}
		})
	}
}

func TestRepositoryMonitorRepairPolicyCountsTerminalCommandsOnce(t *testing.T) {
	for _, evidence := range []string{"none", "repair_job", "branch_update"} {
		t.Run(evidence, func(t *testing.T) {
			ctx := context.Background()
			db := setupControllerSQLiteStore(t)
			monitor, _ := repositoryMonitorInventoryTestObjects("terminal-policy")
			monitor.Spec.Repair.Enabled = true
			monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
			command := &store.CommandEvent{ID: "policy-command", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "head", Source: "controller_policy", Intent: repositoryMonitorCommandIntentFixCI, Status: "processed"}
			if evidence == "branch_update" {
				command.Intent = repositoryMonitorCommandIntentUpdateBranch
			}
			if err := db.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			switch evidence {
			case "repair_job":
				if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "repair-" + repositoryMonitorShortHash(command.ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: 1, HeadSHA: "head", Phase: repositoryMonitorRepairPhaseFailed}); err != nil {
					t.Fatal(err)
				}
			case "branch_update":
				if err := db.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: repositoryMonitorUpdateBranchMutationID(command.ID), CommandEventID: command.ID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 1, TargetSHA: "head", Status: repositoryMonitorRunPhaseFailed}); err != nil {
					t.Fatal(err)
				}
			}
			r := &RepositoryMonitorReconciler{Store: db}
			pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: "head", HeadRepo: "orka-agents/orka"}
			reason, countPR, countHead, err := r.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", pr, "", repositoryMonitorCommandIntentFixCI)
			if err != nil || reason != "" || countPR != 1 || countHead != 1 {
				t.Fatalf("terminal command budget: reason=%q PR=%d head=%d err=%v", reason, countPR, countHead, err)
			}
		})
	}
}
