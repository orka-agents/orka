package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func repositoryMonitorTestEventMetadata(t *testing.T, event store.MonitorEvent) map[string]any {
	t.Helper()
	var metadata map[string]any
	if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
		t.Fatalf("event %s metadata %q: %v", event.ID, event.MetadataJSON, err)
	}
	return metadata
}

// repositoryMonitorTestOptionalString returns value as it appears in decoded
// event metadata: absent when empty.
func repositoryMonitorTestOptionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func repositoryMonitorTestEventsOfType(t *testing.T, monitorStore store.RepositoryMonitorStore, monitor *corev1alpha1.RepositoryMonitor, eventType string) []store.MonitorEvent {
	t.Helper()
	events, _, err := monitorStore.ListMonitorEvents(context.Background(), store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, EventType: eventType, Limit: 50})
	if err != nil {
		t.Fatalf("ListMonitorEvents(%s) error = %v", eventType, err)
	}
	return events
}

// A held pull request is recorded once per head and reason, not on every
// poll: each repeated poll of an unchanged draft writes nothing new, and a
// new head is held anew.
func TestRepositoryMonitorItemSkippedIsRecordedOncePerHeadAndReason(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	head := "draft-head-1"
	body := func() string {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Sprintf(`[{"number":2,"title":"Draft","state":"open","draft":true,"mergeable_state":"unknown","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"draft","sha":%q,"repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}]`, head)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if repositoryMonitorInventoryServerRoutesReviewContext(t, w, r, body()) {
			return
		}
		if r.URL.Path != "/repos/orka-agents/orka/pulls" {
			t.Fatalf("request path = %q, want pull inventory path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body()))
	}))
	t.Cleanup(server.Close)
	monitorStore := setupControllerSQLiteStore(t)
	monitor, secret := repositoryMonitorInventoryTestObjects("held-draft")
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	poll := func(runID string) {
		t.Helper()
		run := &store.MonitorRun{ID: runID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "manual", Phase: repositoryMonitorRunPhaseRunning, StartedAt: time.Now()}
		if _, _, skipped, err := reconciler.processRepositoryMonitorInventoryRun(ctx, monitor, run, "orka-agents", "orka"); err != nil || skipped != 1 {
			t.Fatalf("processRepositoryMonitorInventoryRun(%s) skipped=%d err=%v, want the draft skipped", runID, skipped, err)
		}
	}

	for i := range 3 {
		poll(fmt.Sprintf("run-%d", i))
	}
	held := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, "item_skipped")
	if len(held) != 1 || held[0].ItemSHA != "draft-head-1" || held[0].RunID != "run-0" {
		t.Fatalf("item_skipped events = %#v, want one for the first poll of the head", held)
	}
	metadata := repositoryMonitorTestEventMetadata(t, held[0])
	if metadata[store.RepositoryMonitorEventDecisionField] != store.RepositoryMonitorDecisionHeld || metadata[eventReasonField] != "draft" || metadata[store.RepositoryMonitorEventPolicyField] != "spec.targets.pullRequests.includeDrafts=false" {
		t.Fatalf("held metadata = %v, want a draft hold naming includeDrafts", metadata)
	}

	mu.Lock()
	head = "draft-head-2"
	mu.Unlock()
	poll("run-new-head")
	poll("run-new-head-again")
	held = repositoryMonitorTestEventsOfType(t, monitorStore, monitor, "item_skipped")
	if len(held) != 2 {
		t.Fatalf("item_skipped events after a new head = %#v, want one per head", held)
	}
}

// An implement command blocked before its Task exists records the block as an
// event naming the reason and the spec clause, alongside the work action.
func TestRepositoryMonitorBlockedImplementationRecordsDecisionEvent(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*corev1alpha1.RepositoryMonitor)
		wantReason string
		wantPolicy string
	}{
		{
			name:       "no implementer",
			configure:  func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Agents.Implementer = nil },
			wantReason: "missing_agent_" + repositoryMonitorIssueActionImplementation,
			wantPolicy: "spec.agents.implementer",
		},
		{
			name: "implementation disabled",
			configure: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.IssueWorkflow.Implementation.Enabled = new(false)
			},
			wantReason: "issue_workflow_phase_disabled",
			wantPolicy: "spec.issueWorkflow.implementation.enabled=false",
		},
		{
			name: "attempt budget spent",
			configure: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.IssueWorkflow.Implementation.MaxAttemptsPerIssue = new(int32(0))
			},
			wantReason: "implementation_attempt_budget_exhausted",
			wantPolicy: "spec.issueWorkflow.implementation.maxAttemptsPerIssue=0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			monitor := repositoryMonitorReviewIngestTestMonitor("blocked-implementation")
			monitor.Spec.Targets.Issues.Enabled = true
			monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
			monitor.Spec.IssueWorkflow.Implementation.RequirePlan = new(false)
			tt.configure(monitor)
			reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
			item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "42", Number: 42, SnapshotDigest: "sha256:blocked", WorkflowPhase: repositoryMonitorIssuePhaseDiscovered}
			command := &store.CommandEvent{ID: "cmd-blocked-implementation", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42, Source: "label", Intent: repositoryMonitorCommandIntentImplement, Status: repositoryMonitorCommandAccepted, CreatedAt: time.Now()}
			if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
				t.Fatal(err)
			}
			run := &store.MonitorRun{ID: "run-blocked-implementation", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "command", CommandEventID: command.ID, TargetKind: repositoryMonitorIssueKind, TargetNumber: 42, StartedAt: time.Now()}
			if created, err := reconciler.processIssueCommandRun(ctx, monitor, run, item, "orka-agents", "orka"); err != nil || created != 0 {
				t.Fatalf("processIssueCommandRun() created=%d err=%v", created, err)
			}
			if item.SkipReason != tt.wantReason {
				t.Fatalf("item skip = %q, want %q", item.SkipReason, tt.wantReason)
			}
			blocked := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, "issue_action_blocked")
			if len(blocked) != 1 || blocked[0].ItemSHA != item.SnapshotDigest || blocked[0].RunID != run.ID {
				t.Fatalf("issue_action_blocked events = %#v, want one for the command's run", blocked)
			}
			metadata := repositoryMonitorTestEventMetadata(t, blocked[0])
			if metadata[store.RepositoryMonitorEventDecisionField] != store.RepositoryMonitorDecisionBlocked || metadata[eventReasonField] != tt.wantReason ||
				metadata[store.RepositoryMonitorEventPolicyField] != tt.wantPolicy || metadata[commandEventIDField] != command.ID {
				t.Fatalf("blocked metadata = %v, want reason %s and policy %s for %s", metadata, tt.wantReason, tt.wantPolicy, command.ID)
			}
		})
	}
}

// A branch update that ends without updating the branch records a failed
// decision with a reason code; the free-text detail stays on the mutation.
func TestRepositoryMonitorFailedUpdateBranchRecordsDecisionEvent(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("failed-update-branch")
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	command := &store.CommandEvent{ID: "cmd-update-branch", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 9, Intent: repositoryMonitorCommandIntentUpdateBranch, HeadSHA: "head9"}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "9", Number: 9, HeadSHA: "head9"}
	mutation := &store.GitHubMutationRecord{ID: repositoryMonitorUpdateBranchMutationID(command.ID), CommandEventID: command.ID, Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 9, TargetSHA: "head9", Status: repositoryMonitorAutomergeStateStarted}
	if err := reconciler.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
		t.Fatal(err)
	}
	run := &store.MonitorRun{ID: "run-update-branch"}
	for range 2 {
		if err := reconciler.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, repositoryMonitorUpdateBranchTimeoutReason); err != nil {
			t.Fatalf("failRepositoryMonitorUpdateBranch() error = %v", err)
		}
	}
	failed := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, repositoryMonitorUpdateBranchFailure)
	if len(failed) != 1 || failed[0].ItemSHA != "head9" || failed[0].RunID != run.ID {
		t.Fatalf("update_branch_failed events = %#v, want one", failed)
	}
	metadata := repositoryMonitorTestEventMetadata(t, failed[0])
	if metadata[store.RepositoryMonitorEventDecisionField] != store.RepositoryMonitorDecisionFailed || metadata[eventReasonField] != "update_branch_timeout" ||
		metadata[repositoryMonitorEventMutationIDField] != mutation.ID || metadata[commandEventIDField] != command.ID {
		t.Fatalf("failed metadata = %v, want a timeout naming the mutation and command", metadata)
	}
}

// Every path that ends a branch update without updating the branch records
// the failure once, keyed by its mutation, whichever path ends it first.
func TestRepositoryMonitorUpdateBranchFailurePathsRecordOneEvent(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, name string) (*RepositoryMonitorReconciler, store.RepositoryMonitorStore, *corev1alpha1.RepositoryMonitor, *store.CommandEvent, *store.MonitorItem, *store.GitHubMutationRecord) {
		t.Helper()
		monitorStore := setupControllerSQLiteStore(t)
		monitor := repositoryMonitorReviewIngestTestMonitor(name)
		command := &store.CommandEvent{ID: "cmd-" + name, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 9, Intent: repositoryMonitorCommandIntentUpdateBranch, HeadSHA: "head9"}
		item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "9", Number: 9, HeadSHA: "head9"}
		if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
			t.Fatal(err)
		}
		mutation := &store.GitHubMutationRecord{ID: repositoryMonitorUpdateBranchMutationID(command.ID), CommandEventID: command.ID, Operation: repositoryMonitorUpdateBranchOperation, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 9, TargetSHA: "head9", Status: repositoryMonitorAutomergeStatePending}
		reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
		if err := reconciler.recordRepositoryMonitorGitHubMutation(ctx, monitor, mutation); err != nil {
			t.Fatal(err)
		}
		return reconciler, monitorStore, monitor, command, item, mutation
	}
	assertOneFailure := func(t *testing.T, monitorStore store.RepositoryMonitorStore, monitor *corev1alpha1.RepositoryMonitor, mutation *store.GitHubMutationRecord, wantRunID string) {
		t.Helper()
		failed := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, repositoryMonitorUpdateBranchFailure)
		if len(failed) != 1 || failed[0].ItemNumber != 9 || failed[0].ItemSHA != "head9" || failed[0].RunID != wantRunID {
			t.Fatalf("update_branch_failed events = %#v, want one for run %q", failed, wantRunID)
		}
		metadata := repositoryMonitorTestEventMetadata(t, failed[0])
		if metadata[store.RepositoryMonitorEventDecisionField] != store.RepositoryMonitorDecisionFailed || metadata[eventReasonField] != repositoryMonitorUpdateBranchFailure ||
			metadata[repositoryMonitorEventMutationIDField] != mutation.ID {
			t.Fatalf("failed metadata = %v, want a failure naming mutation %s", metadata, mutation.ID)
		}
	}
	run := &store.MonitorRun{ID: "run-update-branch"}

	t.Run("rejected submission", func(t *testing.T) {
		reconciler, monitorStore, monitor, command, item, mutation := setup(t, "rejected-submission")
		rejected := &repositoryMonitorGitHubAPIError{Operation: "update branch", StatusCode: http.StatusUnprocessableEntity, Body: "merge conflict"}
		if _, _, err := reconciler.recordRepositoryMonitorUpdateBranchSubmissionError(ctx, monitor, run, command, item, mutation, repositoryMonitorPullRequest{Number: 9, HeadSHA: "head9"}, rejected); !errors.Is(err, rejected) {
			t.Fatalf("recordRepositoryMonitorUpdateBranchSubmissionError() error = %v, want the rejection", err)
		}
		assertOneFailure(t, monitorStore, monitor, mutation, run.ID)
	})

	t.Run("terminalized command", func(t *testing.T) {
		reconciler, monitorStore, monitor, command, _, mutation := setup(t, "terminalized-command")
		for range 2 {
			if preserve, err := reconciler.terminalizeRepositoryMonitorUpdateBranch(ctx, monitor, run, *command, "stale command"); err != nil || preserve {
				t.Fatalf("terminalizeRepositoryMonitorUpdateBranch() = preserve %v, err %v", preserve, err)
			}
		}
		assertOneFailure(t, monitorStore, monitor, mutation, run.ID)
	})

	t.Run("terminalized after failure", func(t *testing.T) {
		reconciler, monitorStore, monitor, command, item, mutation := setup(t, "terminalized-after-failure")
		if err := reconciler.failRepositoryMonitorUpdateBranch(ctx, monitor, run, command, item, mutation, "pull request closed before the branch update completed"); err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.terminalizeRepositoryMonitorUpdateBranch(ctx, monitor, nil, *command, "stale command"); err != nil {
			t.Fatal(err)
		}
		assertOneFailure(t, monitorStore, monitor, mutation, run.ID)
	})
}

// A retried run starts under the same ID; each attempt keeps its own
// run_started and run_failed, which the retry budget counts.
func TestRepositoryMonitorRunAttemptsKeepTheirOwnEvents(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("run-attempts")
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	started := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for attempt := range 2 {
		run := &store.MonitorRun{ID: "run-retried", StartedAt: started.Add(time.Duration(attempt) * time.Minute)}
		metadata := map[string]any{stateField: "github_timeout", repositoryMonitorRunAttemptField: repositoryMonitorRunAttempt(run)}
		for range 2 {
			if err := reconciler.createMonitorEvent(ctx, monitor, run.ID, "", 0, "", repositoryMonitorRunFailurePermanent, "repository monitor run failed; retry scheduled", metadata); err != nil {
				t.Fatal(err)
			}
		}
	}
	if failed := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, repositoryMonitorRunFailurePermanent); len(failed) != 2 {
		t.Fatalf("run_failed events = %d, want one per attempt", len(failed))
	}
}

func TestRepositoryMonitorEventPolicyNamesSpecClauses(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	monitor.Spec.Repair.MaxRepairsPerPR = new(int32(3))
	monitor.Spec.Policy.ProtectedLabels = []string{"security-sensitive"}
	monitor.Spec.Review.RequireGreenCI = true
	monitor.Spec.Review.MaxRounds = new(int32(4))
	tests := []struct {
		name     string
		itemKind string
		metadata map[string]any
		want     string
	}{
		{name: "PR budget", metadata: map[string]any{eventReasonField: repositoryMonitorRepairPRBudgetReason}, want: "spec.repair.maxRepairsPerPR=3"},
		{name: "head budget default", metadata: map[string]any{eventReasonField: "repair_head_budget_exhausted"}, want: "spec.repair.maxRepairsPerHead=2"},
		{name: "repair disabled", metadata: map[string]any{eventReasonField: "repair_disabled"}, want: "spec.repair.enabled=false"},
		{name: "fork not allowed", metadata: map[string]any{eventReasonField: "fork_pr_repair_not_writable"}, want: "spec.repair.allowForkPullRequests=false"},
		{name: "fork edits are GitHub state", metadata: map[string]any{eventReasonField: "fork_pr_maintainer_edits_disabled"}},
		{name: "label actor", metadata: map[string]any{eventReasonField: repositoryMonitorRepairSkipLabelActorNotPermitted}, want: "spec.triggers.github.labels.requireActorPermission=write"},
		{name: "protected label", metadata: map[string]any{eventReasonField: repositoryMonitorSkipReasonBlockedLabel, objectLabelsField: []any{"Security-Sensitive"}}, want: "spec.policy.protectedLabels"},
		{name: "pause label", metadata: map[string]any{eventReasonField: repositoryMonitorSkipReasonBlockedLabel, objectLabelsField: []string{"orka:pause"}}, want: "spec.policy.pauseLabels"},
		{name: "issue over limit", itemKind: repositoryMonitorIssueKind, metadata: map[string]any{eventReasonField: repositoryMonitorSkipReasonOverLimit}, want: "spec.targets.issues.maxPerRun=10"},
		{name: "PR over limit", itemKind: repositoryMonitorPullRequestKind, metadata: map[string]any{eventReasonField: repositoryMonitorSkipReasonOverLimit}, want: "spec.targets.pullRequests.maxPerRun=20"},
		{name: "CI gate", metadata: map[string]any{eventReasonField: repositoryMonitorCINotGreen, "ciState": repositoryMonitorCINotGreen}, want: "spec.review.requireGreenCI=true"},
		{name: "missing planner", metadata: map[string]any{eventReasonField: "missing_agent_" + repositoryMonitorIssueActionPlan}, want: "spec.agents.planner"},
		{name: "research disabled", metadata: map[string]any{eventReasonField: "issue_workflow_phase_disabled", repositoryMonitorEventActionKindKey: repositoryMonitorIssueActionResearch}, want: "spec.issueWorkflow.research.enabled=false"},
		{name: "max rounds", metadata: map[string]any{eventReasonField: repositoryMonitorReviewMaxRoundsReason}, want: "spec.review.maxRounds=4"},
		{name: "control path", metadata: map[string]any{eventReasonField: repositoryMonitorPatchPathDenied}, want: repositoryMonitorRepositoryControlPathsPolicy},
		{name: "publishing disabled", metadata: map[string]any{eventReasonField: repositoryMonitorPublishSkipDisabled}, want: "spec.review.publish.enabled=false"},
		{name: "passed not posted", metadata: map[string]any{eventReasonField: repositoryMonitorPublishSkipVerdictNotConfigured, eventVerdictField: repositoryMonitorReviewVerdictPassed}, want: "spec.review.publish.postPassed=false"},
		{name: "needs changes not posted", metadata: map[string]any{eventReasonField: repositoryMonitorPublishSkipVerdictNotConfigured, eventVerdictField: repositoryMonitorReviewVerdictNeedsChanges}, want: "spec.review.publish.postNeedsChanges=false"},
		{name: "needs human not posted", metadata: map[string]any{eventReasonField: repositoryMonitorPublishSkipVerdictNotConfigured, eventVerdictField: repositoryMonitorReviewVerdictNeedsHuman}, want: "spec.review.publish.postNeedsHuman=false"},
		{name: "security sensitive not posted", metadata: map[string]any{eventReasonField: repositoryMonitorPublishSkipSecuritySensitiveNotPublic}, want: "spec.review.publish.postSecuritySensitive=false"},
		{name: "runtime failure", metadata: map[string]any{eventReasonField: repositoryMonitorRepairTaskNotSucceededReason}},
		{name: "no reason", metadata: map[string]any{}},
	}
	for _, tt := range tests {
		if got := repositoryMonitorEventPolicy(monitor, tt.itemKind, tt.metadata); got != tt.want {
			t.Errorf("%s: policy = %q, want %q", tt.name, got, tt.want)
		}
	}
}
