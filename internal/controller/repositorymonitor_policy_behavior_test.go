package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/workers/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests pin how the repository monitor enforces its repair budgets,
// patch path limits and agent configuration today, and which records each
// refusal leaves behind.

func newRepositoryMonitorPolicyReconciler(t *testing.T, name string, githubAPI http.Handler, mutate func(*corev1alpha1.RepositoryMonitor)) (*corev1alpha1.RepositoryMonitor, *RepositoryMonitorReconciler, store.RepositoryMonitorStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects(name)
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	if mutate != nil {
		mutate(monitor)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	monitorStore := setupControllerSQLiteStore(t)
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore}
	if githubAPI == nil {
		// Paths that must not call GitHub fail on any request.
		githubAPI = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			t.Errorf("unexpected GitHub request: %s %s", req.Method, req.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		})
	}
	server := httptest.NewServer(githubAPI)
	t.Cleanup(server.Close)
	reconciler.GitHubAPIBaseURL = server.URL
	return monitor, reconciler, monitorStore
}

func repositoryMonitorTestTaskCount(t *testing.T, r *RepositoryMonitorReconciler) int {
	t.Helper()
	var tasks corev1alpha1.TaskList
	if err := r.List(context.Background(), &tasks); err != nil {
		t.Fatal(err)
	}
	return len(tasks.Items)
}

func repositoryMonitorTestEvents(t *testing.T, monitorStore store.RepositoryMonitorStore, monitor *corev1alpha1.RepositoryMonitor, eventType string) []store.MonitorEvent {
	t.Helper()
	events, _, err := monitorStore.ListMonitorEvents(context.Background(), store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, EventType: eventType, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func repositoryMonitorTestCommands(t *testing.T, monitorStore store.RepositoryMonitorStore, monitor *corev1alpha1.RepositoryMonitor) []store.CommandEvent {
	t.Helper()
	commands, _, err := monitorStore.ListCommandEvents(context.Background(), store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return commands
}

// An automatic repair refused by a spent budget starts no work and is kept as
// one repair_refused event per head and reason, however often the pull request
// is polled. A new head gets its own refusal or, when only the per-head budget
// was spent, a new repair.
func TestRepositoryMonitorRepairBudgetRefusalIsRecordedOncePerHead(t *testing.T) {
	tests := []struct {
		name string
		// spent lists the heads of earlier repairs that consume budget.
		spent   []string
		mutate  func(*corev1alpha1.RepositoryMonitor)
		wantWhy string
		// newHeadRepairs reports whether a new head is repaired again.
		newHeadRepairs bool
	}{
		{
			name:    "pull request budget",
			spent:   []string{"head-a", "head-b", "head-c"},
			mutate:  func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Repair.MaxRepairsPerPR = new(int32(3)) },
			wantWhy: repositoryMonitorRepairPRBudgetReason,
		},
		{
			name:           "head budget",
			spent:          []string{"head31"},
			mutate:         func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Repair.MaxRepairsPerHead = new(int32(1)) },
			wantWhy:        "repair_head_budget_exhausted",
			newHeadRepairs: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			github := &repositoryMonitorRepairLabelsGitHub{checks: map[string]map[string]string{
				"head31": {"tests": "failure"}, "head32": {"tests": "failure"}, "base31": {"tests": "success"},
			}}
			monitor, reconciler, monitorStore := newRepositoryMonitorPolicyReconciler(t, "budget-refusal", github, tt.mutate)
			for i, head := range tt.spent {
				job := &store.RepairJob{ID: "spent-" + head, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", PRNumber: 31, HeadSHA: head, Intent: repositoryMonitorCommandIntentFixCI, Phase: repositoryMonitorRepairPhaseFailed, CreatedAt: time.Now().Add(time.Duration(i-10) * time.Minute)}
				if err := monitorStore.CreateRepairJob(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			pr := repositoryMonitorPullRequest{Number: 31, State: "open", BaseBranch: "main", BaseSHA: "base31", HeadBranch: "feature", HeadRepo: "orka-agents/orka", HeadSHA: "head31", MergeableState: "clean"}
			item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
			pollSeq := 0
			nextPoll := func() *store.MonitorRun {
				pollSeq++
				return &store.MonitorRun{ID: fmt.Sprintf("poll-%d", pollSeq), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: repositoryMonitorRunTriggerWorkflow, StartedAt: time.Now()}
			}
			for range 3 {
				if _, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, nextPoll(), "orka-agents", "orka", pr, item); err != nil {
					t.Fatalf("tryRepositoryMonitorAutomaticRepair() error = %v", err)
				}
			}
			if item.SkipReason != tt.wantWhy || item.RepairState != repositoryMonitorRepairPhaseFailed {
				t.Fatalf("item skip=%q repair=%q, want refused with %s", item.SkipReason, item.RepairState, tt.wantWhy)
			}
			refusals := repositoryMonitorTestEvents(t, monitorStore, monitor, repositoryMonitorEventRepairRefused)
			if len(refusals) != 1 || refusals[0].ItemSHA != "head31" || !strings.Contains(refusals[0].Summary, tt.wantWhy) || !strings.Contains(refusals[0].MetadataJSON, `"reason":"`+tt.wantWhy+`"`) {
				t.Fatalf("refusals = %+v, want one for head31 naming %s", refusals, tt.wantWhy)
			}
			if commands := repositoryMonitorTestCommands(t, monitorStore, monitor); len(commands) != 0 {
				t.Fatalf("refused repair recorded commands %+v", commands)
			}
			if tasks := repositoryMonitorTestTaskCount(t, reconciler); tasks != 0 {
				t.Fatalf("refused repair created %d Tasks", tasks)
			}

			// A pushed head is a new attempt: it is evaluated against the budget again.
			pr.HeadSHA = "head32"
			item = repositoryMonitorItemFromPullRequest(monitor, pr, item)
			for range 2 {
				if _, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, nextPoll(), "orka-agents", "orka", pr, item); err != nil {
					t.Fatalf("tryRepositoryMonitorAutomaticRepair(new head) error = %v", err)
				}
			}
			refusals = repositoryMonitorTestEvents(t, monitorStore, monitor, repositoryMonitorEventRepairRefused)
			commands := repositoryMonitorTestCommands(t, monitorStore, monitor)
			if tt.newHeadRepairs {
				if len(refusals) != 1 || len(commands) != 1 || commands[0].Intent != repositoryMonitorCommandIntentFixCI || commands[0].HeadSHA != "head32" || commands[0].Source != repositoryMonitorControllerPolicySource {
					t.Fatalf("new head refusals=%+v commands=%+v, want one fix_ci command for head32", refusals, commands)
				}
				return
			}
			if len(commands) != 0 {
				t.Fatalf("new head recorded commands %+v under a spent pull request budget", commands)
			}
			heads := map[string]int{}
			for _, refusal := range refusals {
				if !strings.Contains(refusal.Summary, tt.wantWhy) || !strings.Contains(refusal.MetadataJSON, `"reason":"`+tt.wantWhy+`"`) {
					t.Fatalf("refusal = %+v, want reason %s", refusal, tt.wantWhy)
				}
				heads[refusal.ItemSHA]++
			}
			if len(refusals) != 2 || heads["head31"] != 1 || heads["head32"] != 1 {
				t.Fatalf("refusals = %+v, want one per head", refusals)
			}
		})
	}
}

// GitHub's update-branch is an automatic repair of a behind pull request. Its
// durable mutation consumes the same repair budget, so once the budget is
// spent a behind head is refused instead of updated again.
func TestRepositoryMonitorUpdateBranchConsumesRepairBudget(t *testing.T) {
	ctx := context.Background()
	monitor, reconciler, monitorStore := newRepositoryMonitorPolicyReconciler(t, "update-branch-budget", nil, func(m *corev1alpha1.RepositoryMonitor) {
		m.Spec.Repair.MaxRepairsPerPR = new(int32(1))
	})
	pr := repositoryMonitorPullRequest{Number: 31, State: "open", BaseBranch: "main", BaseSHA: "base31", HeadBranch: "feature", HeadRepo: "orka-agents/orka", HeadSHA: "head31", MergeableState: repositoryMonitorMergeableStateBehind}
	poll := &store.MonitorRun{ID: "poll", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: repositoryMonitorRunTriggerWorkflow, StartedAt: time.Now()}

	item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
	if handled, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, poll, "orka-agents", "orka", pr, item); err != nil || !handled {
		t.Fatalf("first behind poll handled=%t err=%v, want an update-branch command", handled, err)
	}
	commands := repositoryMonitorTestCommands(t, monitorStore, monitor)
	if len(commands) != 1 || commands[0].Intent != repositoryMonitorCommandIntentUpdateBranch {
		t.Fatalf("commands = %+v, want one update_branch command", commands)
	}

	// The update ran (the mutation is the durable record of the attempt) and
	// the base moved again, leaving a new head behind.
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: repositoryMonitorUpdateBranchMutationID(commands[0].ID), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: commands[0].ID, Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", Status: repositoryMonitorRunPhaseSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA = "head32"
	item = repositoryMonitorItemFromPullRequest(monitor, pr, nil)
	for i := range 3 {
		refusalPoll := &store.MonitorRun{ID: fmt.Sprintf("poll-refusal-%d", i+1), MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: repositoryMonitorRunTriggerWorkflow, StartedAt: time.Now()}
		if _, err := reconciler.tryRepositoryMonitorAutomaticRepair(ctx, monitor, refusalPoll, "orka-agents", "orka", pr, item); err != nil {
			t.Fatalf("tryRepositoryMonitorAutomaticRepair() error = %v", err)
		}
	}
	if item.SkipReason != repositoryMonitorRepairPRBudgetReason {
		t.Fatalf("item skip = %q, want %s", item.SkipReason, repositoryMonitorRepairPRBudgetReason)
	}
	if commands := repositoryMonitorTestCommands(t, monitorStore, monitor); len(commands) != 1 {
		t.Fatalf("commands = %+v, want no second update_branch command", commands)
	}
	refusals := repositoryMonitorTestEvents(t, monitorStore, monitor, repositoryMonitorEventRepairRefused)
	if len(refusals) != 1 || refusals[0].ItemSHA != "head32" || !strings.Contains(refusals[0].Summary, repositoryMonitorRepairPRBudgetReason) {
		t.Fatalf("refusals = %+v, want one for head32", refusals)
	}

	// Isolate the mutation's contribution to the budget: a fresh monitor with
	// only the succeeded update-branch mutation (and no prior CommandEvent)
	// still exhausts the PR repair budget.
	isoMonitor, isoReconciler, isoStore := newRepositoryMonitorPolicyReconciler(t, "update-branch-budget-mutation-only", nil, func(m *corev1alpha1.RepositoryMonitor) {
		m.Spec.Repair.MaxRepairsPerPR = new(int32(1))
	})
	if err := isoStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: repositoryMonitorUpdateBranchMutationID("cmd-prior"), MonitorNamespace: isoMonitor.Namespace, MonitorName: isoMonitor.Name, CommandEventID: "cmd-prior", Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", Status: repositoryMonitorRunPhaseSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	isoPoll := &store.MonitorRun{ID: "poll-iso", MonitorNamespace: isoMonitor.Namespace, MonitorName: isoMonitor.Name, Trigger: repositoryMonitorRunTriggerWorkflow, StartedAt: time.Now()}
	isoItem := repositoryMonitorItemFromPullRequest(isoMonitor, pr, nil)
	if _, err := isoReconciler.tryRepositoryMonitorAutomaticRepair(ctx, isoMonitor, isoPoll, "orka-agents", "orka", pr, isoItem); err != nil {
		t.Fatalf("mutation-only tryRepositoryMonitorAutomaticRepair() error = %v", err)
	}
	if isoItem.SkipReason != repositoryMonitorRepairPRBudgetReason {
		t.Fatalf("mutation-only item skip = %q, want %s", isoItem.SkipReason, repositoryMonitorRepairPRBudgetReason)
	}
	if cmds := repositoryMonitorTestCommands(t, isoStore, isoMonitor); len(cmds) != 0 {
		t.Fatalf("mutation-only commands = %+v, want none", cmds)
	}
}

// The controller validates an implementation patch against the same path
// policy the runtime applies to repairs: the repository control paths are
// always denied, and allowedPaths, when set, bounds everything else.
func TestRepositoryMonitorIssuePatchValidationAppliesAllowedPaths(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("patch-allowed-paths")
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"docs/**", ".github/workflows/**"}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore, ArtifactStore: monitorStore}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 56, SnapshotDigest: "sha256:allowed-paths"}
	tests := []struct {
		path string
		want string
	}{
		{path: ".github/workflows/ci.yml", want: repositoryMonitorPatchPathDenied},
		{path: "config/rbac/role.yaml", want: repositoryMonitorPatchPathDenied},
		{path: "charts/orka/templates/token-secret.yaml", want: repositoryMonitorPatchPathDenied},
		{path: "internal/x.go", want: repositoryMonitorPatchPathNotAllowed},
		{path: "docs/guide.md", want: ""},
	}
	for i, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			diff := "diff --git a/" + tt.path + " b/" + tt.path + "\n--- a/" + tt.path + "\n+++ b/" + tt.path + "\n@@ -1 +1 @@\n-old\n+new\n"
			record := &store.ActionRecord{ID: "act-allowed-" + string(rune('a'+i)), CommandEventID: "cmd-allowed"}
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-allowed-" + string(rune('a'+i)), Namespace: "default"}}
			got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, &common.StructuredResult{BaseSHA: "base", Diff: diff, Files: []string{tt.path}})
			if got != tt.want {
				t.Fatalf("patch reason for %s = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// A fix command on a monitor with no repairer is blocked at run time: the
// item and work action carry missing_repairer_agent, a repair_blocked event
// names it, and no Task is created.
func TestRepositoryMonitorFixCommandWithoutRepairerIsBlocked(t *testing.T) {
	ctx := context.Background()
	monitor, reconciler, monitorStore := newRepositoryMonitorPolicyReconciler(t, "missing-repairer-command", nil, func(m *corev1alpha1.RepositoryMonitor) {
		m.Spec.Agents.Repairer = nil
	})
	pr := repositoryMonitorPullRequest{Number: 31, State: "open", BaseBranch: "main", BaseSHA: "base31", HeadBranch: "feature", HeadRepo: "orka-agents/orka", HeadSHA: "head31", MergeableState: "clean"}
	item := repositoryMonitorItemFromPullRequest(monitor, pr, nil)
	command := &store.CommandEvent{ID: "cmd-fix-no-repairer", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 31, Source: "label", Intent: repositoryMonitorCommandIntentFix, HeadSHA: "head31", Status: repositoryMonitorCommandAccepted, CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatal(err)
	}
	run := &store.MonitorRun{ID: "run-fix-no-repairer", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "command", CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, StartedAt: time.Now()}
	handled, created, err := reconciler.tryProcessPullRequestCommandRun(ctx, monitor, run, "orka-agents", "orka", pr, item)
	if err != nil || !handled || created != 0 {
		t.Fatalf("tryProcessPullRequestCommandRun() handled=%t created=%d err=%v", handled, created, err)
	}
	if item.SkipReason != "missing_repairer_agent" || item.RepairState != repositoryMonitorRepairPhaseFailed {
		t.Fatalf("item skip=%q repair=%q, want missing_repairer_agent", item.SkipReason, item.RepairState)
	}
	actions, _, err := monitorStore.ListWorkActions(ctx, store.WorkActionFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, Limit: 5})
	if err != nil || len(actions) != 1 || actions[0].Status != repositoryMonitorWorkActionStatusBlocked || actions[0].BlockedReason != "missing_repairer_agent" {
		t.Fatalf("work actions = %+v err=%v, want one blocked with missing_repairer_agent", actions, err)
	}
	blocked := repositoryMonitorTestEvents(t, monitorStore, monitor, "repair_blocked")
	if len(blocked) != 1 || blocked[0].ItemSHA != "head31" || !strings.Contains(blocked[0].MetadataJSON, `"reason":"missing_repairer_agent"`) {
		t.Fatalf("repair_blocked events = %+v, want one naming missing_repairer_agent", blocked)
	}
	if tasks := repositoryMonitorTestTaskCount(t, reconciler); tasks != 0 {
		t.Fatalf("blocked fix command created %d Tasks", tasks)
	}
}

// Issue commands whose agent is not configured are blocked at run time and
// create no Task. A missing planner is also reported as an
// issue_action_blocked event; a missing implementer is recorded on the item
// and work action.
func TestRepositoryMonitorIssueCommandWithoutAgentIsBlocked(t *testing.T) {
	tests := []struct {
		name        string
		requirePlan bool
		wantAction  string
		wantSkip    string
		wantEvent   bool
	}{
		{name: "implement without implementer", requirePlan: false, wantAction: repositoryMonitorIssueActionImplementation, wantSkip: "missing_agent_" + repositoryMonitorIssueActionImplementation},
		{name: "implement without planner", requirePlan: true, wantAction: repositoryMonitorIssueActionPlan, wantSkip: "missing_agent_" + repositoryMonitorIssueActionPlan, wantEvent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitor, reconciler, monitorStore := newRepositoryMonitorPolicyReconciler(t, "missing-issue-agent", nil, func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.Repair.Enabled = false
				m.Spec.Agents.Repairer = nil
				m.Spec.IssueWorkflow.Implementation.RequirePlan = new(tt.requirePlan)
			})
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "42", Number: 42, SnapshotDigest: "sha256:missing-agent", WorkflowPhase: repositoryMonitorIssuePhaseDiscovered}
			command := &store.CommandEvent{ID: "cmd-implement-no-agent", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42, Source: "label", Intent: repositoryMonitorCommandIntentImplement, Status: repositoryMonitorCommandAccepted, CreatedAt: time.Now()}
			if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			run := &store.MonitorRun{ID: "run-implement-no-agent", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "command", CommandEventID: command.ID, TargetKind: repositoryMonitorIssueKind, TargetNumber: 42, StartedAt: time.Now()}
			created, err := reconciler.processIssueCommandRun(ctx, monitor, run, item, "orka-agents", "orka")
			if err != nil || created != 0 {
				t.Fatalf("processIssueCommandRun() created=%d err=%v", created, err)
			}
			if item.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || item.SkipReason != tt.wantSkip {
				t.Fatalf("item phase=%q skip=%q, want blocked with %s", item.WorkflowPhase, item.SkipReason, tt.wantSkip)
			}
			action, err := monitorStore.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForActionKind(tt.wantAction)))
			if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != tt.wantSkip {
				t.Fatalf("work action = %+v err=%v, want blocked with %s", action, err, tt.wantSkip)
			}
			blocked := repositoryMonitorTestEvents(t, monitorStore, monitor, "issue_action_blocked")
			if tt.wantEvent {
				if len(blocked) != 1 || !strings.Contains(blocked[0].Summary, "no agent configured") {
					t.Fatalf("issue_action_blocked events = %+v, want one naming the missing agent", blocked)
				}
			} else if len(blocked) != 0 {
				t.Fatalf("issue_action_blocked events = %+v, want none when implementer is missing", blocked)
			}
			if tasks := repositoryMonitorTestTaskCount(t, reconciler); tasks != 0 {
				t.Fatalf("blocked issue command created %d Tasks", tasks)
			}
		})
	}
}
