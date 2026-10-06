package controller

import (
	"context"
	"fmt"
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

func TestRepositoryMonitorWorkflowRequeueRequiresPullRequestTarget(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			monitor, _ := repositoryMonitorInventoryTestObjects("poll-target")
			monitor.Spec.Targets.PullRequests.Enabled = &enabled
			monitor.Spec.Targets.Issues.Enabled = true
			monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
			configureRepositoryMonitorTestWriteCredentials(monitor)
			monitor.Spec.GitSecretRef = nil
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(monitor).WithObjects(repositoryMonitorControllerObjects(monitor)...).Build()
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: setupControllerSQLiteStore(t)}
			key := types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(t.Context(), key, monitor); err != nil || monitor.Status.Phase != repositoryMonitorPhaseReady {
				t.Fatalf("monitor not ready: phase=%s conditions=%+v err=%v", monitor.Status.Phase, monitor.Status.Conditions, err)
			}
			want := time.Duration(0)
			if enabled {
				want = repositoryMonitorWorkflowPollInterval
			}
			if result.RequeueAfter != want {
				t.Fatalf("poll requeue=%v, want %v", result.RequeueAfter, want)
			}
		})
	}
}

func TestRepositoryMonitorPublishOnlyWorkflowPoll(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("publish-only")
	monitor.Spec.Review.Publish.Enabled = true
	r := &RepositoryMonitorReconciler{Store: db}
	if err := db.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, State: repositoryMonitorItemStateOpen}); err != nil {
		t.Fatal(err)
	}
	if err := r.queueRepositoryMonitorWorkflowPoll(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	runs, _, err := db.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
	if err != nil || len(runs) != 1 || runs[0].Trigger != "workflow" {
		t.Fatalf("publish-only poll runs=%+v err=%v", runs, err)
	}
}

func TestRepositoryMonitorRepairProjectionIgnoresQueuedOldHead(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("queued-old-head")
	r := &RepositoryMonitorReconciler{Store: db}
	if err := db.CreateRepairJob(ctx, &store.RepairJob{ID: "old-active", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, PRNumber: 1, HeadSHA: "old", Phase: repositoryMonitorRepairPhaseQueued, TaskName: "running-task"}); err != nil {
		t.Fatal(err)
	}
	state, err := r.repositoryMonitorRepairStateForHead(ctx, monitor, 1, "new")
	if err != nil || state != "" {
		t.Fatalf("old queued repair projected onto new head: %s %v", state, err)
	}
	state, err = r.repositoryMonitorRepairStateForHead(ctx, monitor, 1, "old")
	if err != nil || state != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("current queued repair lost: %s %v", state, err)
	}
}

func TestRepositoryMonitorUpdateBranchBudgetAndOutcome(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("update-budget")
	monitor.Spec.Repair.Enabled = true
	limit := int32(2)
	monitor.Spec.Repair.MaxRepairsPerPR, monitor.Spec.Repair.MaxRepairsPerHead = &limit, &limit
	r := &RepositoryMonitorReconciler{Store: db}
	pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: "head", HeadRepo: "orka-agents/orka", MergeableState: "dirty"}
	for i, phase := range []string{repositoryMonitorRunPhaseFailed, repositoryMonitorRunPhaseSucceeded} {
		if err := db.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: fmt.Sprint("update-", i), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 1, TargetSHA: "head", Status: phase, CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
		state, err := r.repositoryMonitorRepairStateForHead(ctx, monitor, 1, "head")
		if err != nil || state != phase {
			t.Fatalf("update outcome=%s err=%v, want %s", state, err, phase)
		}
		if i == 0 {
			// A terminal failed update must receive a new bounded command, not
			// resurrect its terminal action as permanently queued.
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "head", RepairState: state}
			if handled, err := r.tryRepositoryMonitorAutomaticRepair(ctx, monitor, &store.MonitorRun{}, "orka-agents", "orka", pr, item); err != nil || !handled {
				t.Fatalf("bounded retry handled=%v err=%v", handled, err)
			}
			commands, _, err := db.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
			if err != nil || len(commands) != 1 {
				t.Fatalf("retry commands=%+v err=%v", commands, err)
			}
			want := "policy-" + repositoryMonitorShortHash(fmt.Sprintf("%s|%d|%d|%s|%s|%d|%d", monitor.UID, monitor.Generation, pr.Number, pr.HeadSHA, repositoryMonitorCommandIntentUpdateBranch, 1, 1))
			if commands[0].ID != want {
				t.Fatalf("retry did not use consumed attempt counts: %s", commands[0].ID)
			}
		}
	}
	for _, head := range []string{"head", "new-head"} {
		pr.HeadSHA = head
		reason, countPR, countHead, err := r.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", pr, "", repositoryMonitorCommandIntentUpdateBranch)
		if err != nil || reason != repositoryMonitorRepairPRBudgetReason || countPR != 2 || (head == "head" && countHead != 2) || (head == "new-head" && countHead != 0) {
			t.Fatalf("head=%s budget=%s PR=%d head=%d err=%v", head, reason, countPR, countHead, err)
		}
	}
}

func TestRepositoryMonitorReadinessOwnershipSurvivesDeletion(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("deleted-owner")
	r := &RepositoryMonitorReconciler{Store: db}
	for _, record := range []*store.GitHubMutationRecord{
		{ID: "readiness", Operation: repositoryMonitorReadinessOperation, TargetSHA: "head", ExternalID: "123"},
		{ID: "unfinished-readiness", Operation: repositoryMonitorReadinessOperation, TargetSHA: "head"},
		{ID: "update", Operation: repositoryMonitorUpdateBranchOperation, TargetSHA: "head", ExternalID: "base"},
	} {
		if err := r.recordRepositoryMonitorGitHubMutation(ctx, monitor, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteRepositoryMonitor(ctx, monitor.Namespace, monitor.Name); err != nil {
		t.Fatal(err)
	}
	owned, err := r.repositoryMonitorOwnedReadinessStatuses(ctx, "head")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := owned[123]; !ok || len(owned) != 1 {
		t.Fatalf("lost remote status ownership: %v", owned)
	}
	records, _, err := db.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
	if err != nil || len(records) != 1 || records[0].ID != "readiness" {
		t.Fatalf("deletion retained more than recorded readiness identity: %+v %v", records, err)
	}
}
