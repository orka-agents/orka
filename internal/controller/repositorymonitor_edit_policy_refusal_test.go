package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const repositoryMonitorEditPolicyTestAllowedPaths = "spec.issueWorkflow.implementation.allowedPaths"

func repositoryMonitorIssuePromptInput(t *testing.T, prompt string) map[string]json.RawMessage {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "\n\nInput:\n")
	if !ok {
		t.Fatalf("prompt has no input: %q", prompt)
	}
	payload, _, ok := strings.Cut(rest, "\n\nReturn exactly one JSON object")
	if !ok {
		t.Fatalf("prompt has no schema after its input: %q", prompt)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &input); err != nil {
		t.Fatalf("prompt input %q: %v", payload, err)
	}
	return input
}

func repositoryMonitorPromptEditPolicy(t *testing.T, input map[string]json.RawMessage) repositoryMonitorEditPolicy {
	t.Helper()
	raw, ok := input[repositoryMonitorEditPolicyField]
	if !ok {
		t.Fatalf("input %v has no editPolicy", input)
	}
	var policy repositoryMonitorEditPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

// The plan sees the paths implementation may change, so its allowedFiles can
// stay inside them, and the implement Task sees which named files it may
// change and which are context only.
func TestRepositoryMonitorIssueActionPromptsCarryEditPolicy(t *testing.T) {
	monitor := repositoryMonitorReviewIngestTestMonitor("edit-policy-prompts")
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"README.md", "docs/**"}
	item := &store.MonitorItem{Number: 7, Title: "Clarify the install section", Body: "The install steps in README.md are stale.", SnapshotDigest: "sha256:edit"}
	plan := store.ActionRecord{ActionKind: repositoryMonitorIssueActionPlan, PayloadJSON: `{"allowedFiles":[".github/ISSUE_TEMPLATE/bug_report.yml","website/docs/contributing.md"]}`}

	planPrompt := buildRepositoryMonitorIssueActionPrompt(monitor, "orka-agents", "orka", item, repositoryMonitorIssueActionPlan, repositoryMonitorIssuePhasePlanQueued, nil)
	if !strings.Contains(planPrompt, repositoryMonitorPlanEditPolicyInstruction) {
		t.Fatal("plan prompt lacks the edit policy instruction")
	}
	planPolicy := repositoryMonitorPromptEditPolicy(t, repositoryMonitorIssuePromptInput(t, planPrompt))
	if !slices.Equal(planPolicy.AllowedPaths, []string{"README.md", "docs/**"}) || !planPolicy.DeniesRepositoryControlPaths || len(planPolicy.EditableFiles)+len(planPolicy.ReadOnlyFiles) != 0 {
		t.Fatalf("plan edit policy = %+v, want the allowed paths only", planPolicy)
	}

	implementPrompt := buildRepositoryMonitorIssueActionPrompt(monitor, "orka-agents", "orka", item, repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseImplementationQueued, []store.ActionRecord{plan})
	if !strings.Contains(implementPrompt, repositoryMonitorEditPolicyInstruction) {
		t.Fatal("implement prompt lacks the edit policy instruction")
	}
	implementPolicy := repositoryMonitorPromptEditPolicy(t, repositoryMonitorIssuePromptInput(t, implementPrompt))
	if !slices.Equal(implementPolicy.EditableFiles, []string{"README.md"}) ||
		!slices.Equal(implementPolicy.ReadOnlyFiles, []string{".github/ISSUE_TEMPLATE/bug_report.yml", "website/docs/contributing.md"}) {
		t.Fatalf("implement edit policy = %+v, want README.md editable and the plan's files read-only", implementPolicy)
	}

	triagePrompt := buildRepositoryMonitorIssueActionPrompt(monitor, "orka-agents", "orka", item, repositoryMonitorIssueActionTriage, repositoryMonitorIssuePhaseTriageQueued, nil)
	if _, ok := repositoryMonitorIssuePromptInput(t, triagePrompt)[repositoryMonitorEditPolicyField]; ok {
		t.Fatal("a read-only triage prompt carries an edit policy")
	}
}

// An implement command whose issue and plan name only files the monitor may
// not change is refused before a Task exists, with a decision event and the
// reason on the issue's status comment.
func TestRepositoryMonitorImplementationRefusesWhenNothingNamedIsEditable(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commentBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/orka-agents/orka/issues/42/comments" {
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		commentBodies = append(commentBodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":4242,"html_url":"https://github.com/orka-agents/orka/issues/42#issuecomment-4242"}`))
	}))
	t.Cleanup(server.Close)

	monitor := repositoryMonitorReviewIngestTestMonitor("no-editable-target")
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	monitor.Spec.IssueWorkflow.Implementation.RequirePlan = new(false)
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"README.md"}
	monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: repositoryMonitorTestForgeCredential}
	forgeSecret := repositoryMonitorControllerTestSecret(repositoryMonitorTestForgeCredential, map[string][]byte{repositoryMonitorTokenKey: []byte("forge-token")})
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(forgeSecret).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}

	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "42", Number: 42, State: "open",
		Title: "Contributing guide is out of date", Body: "Update `website/docs/contributing.md` to match the new process.", SnapshotDigest: "sha256:no-editable", WorkflowPhase: repositoryMonitorIssuePhaseDiscovered}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := monitorStore.CreateActionRecord(ctx, &store.ActionRecord{ID: "plan-42", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42,
		ActionKind: repositoryMonitorIssueActionPlan, Verdict: "ready", PayloadJSON: `{"allowedFiles":[".github/ISSUE_TEMPLATE/bug_report.yml"]}`, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	command := &store.CommandEvent{ID: "cmd-no-editable-target", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42, Source: "label", Intent: repositoryMonitorCommandIntentImplement, Status: repositoryMonitorCommandAccepted, CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatal(err)
	}
	run := &store.MonitorRun{ID: "run-no-editable-target", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "command", CommandEventID: command.ID, TargetKind: repositoryMonitorIssueKind, TargetNumber: 42, StartedAt: time.Now()}
	for range 2 {
		if created, err := reconciler.processIssueCommandRun(ctx, monitor, run, item, "orka-agents", "orka"); err != nil || created != 0 {
			t.Fatalf("processIssueCommandRun() created=%d err=%v", created, err)
		}
	}

	if item.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || item.SkipReason != repositoryMonitorNoEditableTargetReason {
		t.Fatalf("item phase=%q skip=%q, want blocked with %s", item.WorkflowPhase, item.SkipReason, repositoryMonitorNoEditableTargetReason)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 {
		t.Fatalf("tasks = %d (err %v), want none before the model run", len(tasks.Items), err)
	}
	refused := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, repositoryMonitorIssueActionRefused)
	if len(refused) != 1 || refused[0].RunID != run.ID || refused[0].ItemSHA != item.SnapshotDigest {
		t.Fatalf("issue_action_refused events = %#v, want one for the command's run", refused)
	}
	assertRepositoryMonitorNoEditableTargetEvent(t, refused[0], command.ID, "website/docs/contributing.md", ".github/ISSUE_TEMPLATE/bug_report.yml")
	action, err := monitorStore.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForActionKind(repositoryMonitorIssueActionImplementation)))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked {
		t.Fatalf("work action = %#v err=%v, want blocked", action, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(commentBodies) != 1 {
		t.Fatalf("status comment posts = %d, want one across the replay", len(commentBodies))
	}
	var comment struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(commentBodies[0]), &comment); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**State:** blocked", repositoryMonitorNoEditableTargetReason, "website/docs/contributing.md", ".github/ISSUE_TEMPLATE/bug_report.yml"} {
		if !strings.Contains(comment.Body, want) {
			t.Errorf("status comment %q lacks %q", comment.Body, want)
		}
	}
}

// assertRepositoryMonitorNoEditableTargetEvent checks a refusal event's
// decision, reason, policy, command, and read-only files.
func assertRepositoryMonitorNoEditableTargetEvent(t *testing.T, event store.MonitorEvent, commandID string, wantReadOnly ...string) {
	t.Helper()
	metadata := repositoryMonitorTestEventMetadata(t, event)
	if metadata[store.RepositoryMonitorEventDecisionField] != store.RepositoryMonitorDecisionRefused || metadata[eventReasonField] != repositoryMonitorNoEditableTargetReason ||
		metadata[store.RepositoryMonitorEventPolicyField] != repositoryMonitorEditPolicyTestAllowedPaths || metadata[commandEventIDField] != commandID {
		t.Fatalf("refused metadata = %v", metadata)
	}
	readOnly, _ := metadata[repositoryMonitorReadOnlyFilesField].([]any)
	got := make([]string, 0, len(readOnly))
	for _, file := range readOnly {
		value, _ := file.(string)
		got = append(got, value)
	}
	if !slices.Equal(got, wantReadOnly) {
		t.Fatalf("readOnlyFiles = %q, want %q", got, wantReadOnly)
	}
}

// A findings repair whose open findings all name files outside the allowed
// paths is refused before its repair job or Task exists.
func TestRepositoryMonitorFindingsRepairRefusesWhenNoFindingIsEditable(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, _ := repositoryMonitorInventoryTestObjects("findings-no-editable")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"internal/**"}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(monitor, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: monitor.Namespace, UID: "namespace-uid"}}).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", BaseSHA: "base31", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	findings := `[{"priority":"P1","file":"website/docs/setup.md","line":3,"title":"Stale command"},{"priority":"P2","file":".github/workflows/ci.yml","line":9,"title":"Pin the action"}]`
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "review-31", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 31, HeadSHA: pr.HeadSHA,
		Verdict: repositoryMonitorReviewVerdictNeedsChanges, FindingsJSON: findings, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: pr.HeadSHA, LastReviewID: "review-31"}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	command := &store.CommandEvent{ID: "cmd-findings-no-editable", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 31, Source: "api", Intent: repositoryMonitorCommandIntentFix, HeadSHA: pr.HeadSHA, Status: repositoryMonitorCommandAccepted, CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatal(err)
	}
	run := &store.MonitorRun{ID: "run-findings-no-editable", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "command", CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, StartedAt: time.Now()}

	handled, created, err := reconciler.tryProcessPullRequestCommandRun(ctx, monitor, run, "orka-agents", "orka", pr, item)
	if err != nil || !handled || created != 0 {
		t.Fatalf("tryProcessPullRequestCommandRun() = (%v, %d, %v), want handled without a Task", handled, created, err)
	}
	if item.SkipReason != repositoryMonitorNoEditableTargetReason {
		t.Fatalf("item skip = %q, want %s", item.SkipReason, repositoryMonitorNoEditableTargetReason)
	}
	if _, err := monitorStore.GetRepairJob(ctx, monitor.Namespace, "repair-"+repositoryMonitorShortHash(command.ID)); !errorsIsStoreNotFound(err) {
		t.Fatalf("repair job lookup err = %v, want none created", err)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 0 {
		t.Fatalf("tasks = %d (err %v), want none", len(tasks.Items), err)
	}
	refused := repositoryMonitorTestEventsOfType(t, monitorStore, monitor, repositoryMonitorEventRepairRefused)
	if len(refused) != 1 {
		t.Fatalf("repair_refused events = %#v, want one", refused)
	}
	assertRepositoryMonitorNoEditableTargetEvent(t, refused[0], command.ID, "website/docs/setup.md", ".github/workflows/ci.yml")
}

// A findings repair with an editable finding starts and carries the split in
// its prompt; CI and branch-update repairs never refuse.
func TestRepositoryMonitorRepairEditPolicy(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("findings-editable")
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"internal/**"}
	pr := repositoryMonitorPullRequest{Number: 32, HeadSHA: "head32"}
	findings := `[{"priority":"P1","file":"internal/a.go","line":3,"title":"Nil check"},{"priority":"P2","file":"website/docs/setup.md","line":9,"title":"Stale"},{"priority":"P2","file":"internal/b.go","status":"fixed","title":"Fixed"}]`
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "review-32", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 32, HeadSHA: pr.HeadSHA,
		Verdict: repositoryMonitorReviewVerdictNeedsChanges, FindingsJSON: findings, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	item := &store.MonitorItem{Number: 32, HeadSHA: pr.HeadSHA, LastReviewID: "review-32"}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	prompt, policy, err := reconciler.buildRepositoryMonitorRepairPromptAndPolicy(ctx, monitor, repositoryMonitorCommandIntentFix, "orka-agents/orka", pr, item, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(policy.EditableFiles, []string{"internal/a.go"}) || !slices.Equal(policy.ReadOnlyFiles, []string{"website/docs/setup.md"}) {
		t.Fatalf("repair edit policy = %+v, want open findings split", policy)
	}
	if repositoryMonitorRepairRefusesNoEditableTarget(repositoryMonitorCommandIntentFix, policy) {
		t.Fatal("a findings repair with an editable finding was refused")
	}
	if !strings.Contains(prompt, repositoryMonitorEditPolicyInstruction) || !repositoryMonitorRepairPromptInstructionsMatch(prompt, repositoryMonitorCommandIntentFix) {
		t.Fatal("repair prompt lacks matching edit policy instructions")
	}
	input, ok := repositoryMonitorRepairPromptInput(prompt)
	if !ok {
		t.Fatal("repair prompt input does not decode")
	}
	if _, ok := input[repositoryMonitorEditPolicyField]; !ok {
		t.Fatal("repair prompt input lacks editPolicy")
	}

	readOnly := newRepositoryMonitorEditPolicy(monitor, []string{"website/docs/setup.md"})
	for intent, want := range map[string]bool{
		repositoryMonitorCommandIntentFix:          true,
		repositoryMonitorCommandIntentFixCI:        false,
		repositoryMonitorCommandIntentUpdateBranch: false,
	} {
		if got := repositoryMonitorRepairRefusesNoEditableTarget(intent, readOnly); got != want {
			t.Errorf("%s refuses = %v, want %v", intent, got, want)
		}
	}
}

// Prompts written before repair inputs carried an edit policy still match
// their own instructions; the edit policy instruction needs the editPolicy
// section and the reverse.
func TestRepositoryMonitorRepairPromptEditPolicyInstructionsMatch(t *testing.T) {
	withPolicy := "{\n  \"schemaVersion\": \"orka.prRepair.input.v1\",\n  \"editPolicy\": {\"deniesRepositoryControlPaths\": true}\n}\n"
	withoutPolicy := "{\n  \"schemaVersion\": \"orka.prRepair.input.v1\"\n}\n"
	current := repositoryMonitorRepairPromptPreambleWithExternal(repositoryMonitorCommandIntentFix, false, false, false, true)
	previous := repositoryMonitorRepairPromptPreamble(repositoryMonitorCommandIntentFix, false, false)
	if !repositoryMonitorRepairPromptInstructionsMatch(current+repositoryMonitorRepairPromptInputMarker+withPolicy, repositoryMonitorCommandIntentFix) {
		t.Fatal("current instructions with an edit policy must match")
	}
	if !repositoryMonitorRepairPromptInstructionsMatch(previous+repositoryMonitorRepairPromptInputMarker+withoutPolicy, repositoryMonitorCommandIntentFix) {
		t.Fatal("instructions from before edit policies must match")
	}
	if repositoryMonitorRepairPromptInstructionsMatch(current+repositoryMonitorRepairPromptInputMarker+withoutPolicy, repositoryMonitorCommandIntentFix) {
		t.Fatal("edit policy instructions matched an input without an edit policy")
	}
	if repositoryMonitorRepairPromptInstructionsMatch(previous+repositoryMonitorRepairPromptInputMarker+withPolicy, repositoryMonitorCommandIntentFix) {
		t.Fatal("instructions without the edit policy matched an input that carries one")
	}
}
