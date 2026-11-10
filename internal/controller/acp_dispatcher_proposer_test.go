package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/protectedset"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	proposerTestRepository = "https://github.com/orka-agents/orka.git"
	proposerTestConfigMap  = "orka-protected-set"
	proposerTestNamespace  = "orka-system"
)

func loadSampleProtectedSetConfigMap(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "orka_protected_set.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var configMap corev1.ConfigMap
	if err := sigsyaml.UnmarshalStrict(data, &configMap); err != nil {
		t.Fatal(err)
	}
	configMap.Namespace = proposerTestNamespace
	configMap.Name = proposerTestConfigMap
	return &configMap
}

func sampleProtectedSet(t *testing.T) *protectedset.Set {
	t.Helper()
	set, err := protectedset.Parse([]byte(loadSampleProtectedSetConfigMap(t).Data[protectedset.ConfigMapKey]))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func proposerTestTask(set *protectedset.Set) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "orka-proposers", Name: "proposer",
			Annotations: map[string]string{labels.AnnotationImprovementRole: labels.ImprovementRoleProposer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "propose",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Read", "Glob", "Grep", "Edit", "Write", "Bash", "propose_memory"}},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: proposerTestRepository,
				PushBranch: "orka/issue-12-abcdef", DenyRepositoryControlPaths: true,
				DeniedPaths: set.DeniedPathsFor("github.com/orka-agents/orka"),
			},
		},
	}
}

func TestProposerConstraintViolation(t *testing.T) {
	set := sampleProtectedSet(t)
	effective := []string{"Bash", "Edit", "Glob", "Grep", "Read", "Write", "propose_memory"}
	tests := []struct {
		name      string
		mutate    func(*corev1alpha1.Task)
		set       *protectedset.Set
		effective []string
		external  bool
		want      string
	}{
		{name: "constrained write proposer runs"},
		{name: "external runtime endpoint", external: true, want: "built-in RuntimePool backend"},
		{name: "execution workspace class", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "sandbox"},
			}}
		}, want: "execution workspace class"},
		{name: "read proposer needs no path limits", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: proposerTestRepository}
		}},
		{name: "unknown role", mutate: func(task *corev1alpha1.Task) {
			task.Annotations[labels.AnnotationImprovementRole] = "evaluator"
		}, want: "is not supported"},
		{name: "blank role", mutate: func(task *corev1alpha1.Task) {
			task.Annotations[labels.AnnotationImprovementRole] = " "
		}, want: "is not supported"},
		{name: "no protected set", set: nil, want: "disabled"},
		{name: "container task", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Type = corev1alpha1.TaskTypeContainer
		}, want: "agent Task"},
		{name: "case-set namespace", mutate: func(task *corev1alpha1.Task) {
			task.Namespace = "orka-evals"
		}, want: "case set"},
		{name: "session continuation", mutate: func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "evaluated"}
		}, want: "Session"},
		{name: "forked transcript", mutate: func(task *corev1alpha1.Task) {
			task.Annotations[labels.AnnotationForkSourceTask] = "trial"
		}, want: "fork"},
		{name: "implicit tools", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = nil
		}, want: "explicit agentRuntime.allowedTools"},
		{name: "delegation tool", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "delegate_task")
		}, want: `"delegate_task"`},
		{name: "validation tool", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "run_validation")
		}, want: `"run_validation"`},
		{name: "brokered web tool", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "web_fetch")
		}, want: `"web_fetch"`},
		{name: "native web tool", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "WebSearch")
		}, want: `"WebSearch"`},
		{name: "memory recall", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "recall_memory")
		}, want: `"recall_memory"`},
		{name: "transcript search", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "search_transcript")
		}, want: `"search_transcript"`},
		{name: "connector tool", mutate: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime.AllowedTools = append(task.Spec.AgentRuntime.AllowedTools, "create_pull_request")
		}, want: `"create_pull_request"`},
		{name: "injected messaging tool in the frozen profile", effective: append(slices.Clone(effective), "send_message"), want: `"send_message"`},
		{name: "control paths allowed", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.DenyRepositoryControlPaths = false
		}, want: "denyRepositoryControlPaths"},
		{name: "denied paths missing", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.DeniedPaths = nil
		}, want: "deniedPaths does not cover"},
		{name: "denied paths stale against the protected set", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.DeniedPaths = slices.DeleteFunc(slices.Clone(task.Spec.Workspace.DeniedPaths), func(pattern string) bool {
				return pattern == "internal/admission/**"
			})
		}, want: `"internal/admission/**"`},
		{name: "fork publication target adds its paths", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.DeniedPaths = set.Paths
			task.Spec.Workspace.GitRepo = "https://github.com/example/service.git"
			task.Spec.Workspace.PublicationGitRepo = proposerTestRepository
		}, want: "deniedPaths does not cover"},
		{name: "no push branch", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.PushBranch = ""
		}, want: "pushBranch under orka/"},
		{name: "default branch", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.PushBranch = "refs/heads/main"
		}, want: "pushBranch under orka/"},
		{name: "bare prefix", mutate: func(task *corev1alpha1.Task) {
			task.Spec.Workspace.PushBranch = "orka/"
		}, want: "pushBranch under orka/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := proposerTestTask(set)
			if tt.mutate != nil {
				tt.mutate(task)
			}
			useSet := set
			if tt.name == "no protected set" {
				useSet = tt.set
			}
			tools := effective
			if tt.effective != nil {
				tools = tt.effective
			}
			got := proposerConstraintViolation(task, tools, tt.external, useSet)
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("proposerConstraintViolation() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckProposerConstraintsLoadsTheCurrentProtectedSet(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	set := sampleProtectedSet(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loadSampleProtectedSetConfigMap(t)).Build()
	configured := &TaskReconciler{ProtectedSet: protectedset.Source{Reader: reader, Namespace: proposerTestNamespace, Name: proposerTestConfigMap}}
	unconfigured := &TaskReconciler{}
	missing := &TaskReconciler{ProtectedSet: protectedset.Source{Reader: reader, Namespace: proposerTestNamespace, Name: "absent"}}
	effective := &verifiedAgentExecution{
		binding:          &corev1alpha1.AgentExecutionBinding{Backend: corev1alpha1.AgentExecutionBackendRuntimePool},
		mcpConfiguration: harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{"Read", "propose_memory"}}},
	}

	unmarked := proposerTestTask(set)
	delete(unmarked.Annotations, labels.AnnotationImprovementRole)
	unmarked.Spec.Workspace.DeniedPaths = nil
	if violation, err := unconfigured.checkProposerConstraints(ctx, unmarked, effective); violation != "" || err != nil {
		t.Fatalf("unmarked Task = (%q, %v), want it unaffected", violation, err)
	}
	for name, reconciler := range map[string]*TaskReconciler{"unconfigured": unconfigured, "missing ConfigMap": missing} {
		if violation, err := reconciler.checkProposerConstraints(ctx, proposerTestTask(set), effective); err != nil || !strings.Contains(violation, "proposer flows are disabled") {
			t.Fatalf("%s: checkProposerConstraints() = (%q, %v), want proposer flows disabled", name, violation, err)
		}
	}
	if violation, err := configured.checkProposerConstraints(ctx, proposerTestTask(set), effective); violation != "" || err != nil {
		t.Fatalf("constrained proposer = (%q, %v), want it admitted", violation, err)
	}
}

// TestQueueACPRuntimeTaskFailsUnconstrainedProposer covers the dispatch-time
// check on the real queue path: a proposer Task that is missing a constraint,
// or that runs without a protected set, fails before any runtime demand.
func TestQueueACPRuntimeTaskFailsUnconstrainedProposer(t *testing.T) {
	tests := []struct {
		name          string
		allowedTools  []string
		protectedSet  bool
		wantViolation string
	}{
		{name: "no protected set", allowedTools: []string{"Read", "Glob", "Grep"}, wantViolation: "proposer flows are disabled"},
		{name: "web tool", allowedTools: []string{"Read", "Glob", "Grep", "web_fetch"}, protectedSet: true, wantViolation: `"web_fetch"`},
		{name: "constrained", allowedTools: []string{"Read", "Glob", "Grep"}, protectedSet: true},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			uid := types.UID(strings.Repeat(string(rune('a'+index)), 8) + "-1111-1111-1111-111111111111")
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "proposer", UID: uid, Generation: 1,
					Annotations: map[string]string{labels.AnnotationImprovementRole: labels.ImprovementRoleProposer},
				},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAgent, Prompt: "propose",
					AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: tt.allowedTools},
					Workspace:    &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: proposerTestRepository},
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: types.UID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), Generation: 1},
				Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Name: acpTestModel}, Runtime: &corev1alpha1.AgentCLIRuntime{
					Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
				}},
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).
				WithObjects(task, loadSampleProtectedSetConfigMap(t)).Build()
			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			controlStore := sqlite.NewStore(db, "test")
			epochs := NewControllerEpochManager(controlStore, "controller-test")
			epochCtx, cancelEpoch := context.WithCancel(context.Background())
			epochDone := make(chan error, 1)
			go func() { epochDone <- epochs.Start(epochCtx) }()
			defer func() {
				cancelEpoch()
				if err := <-epochDone; err != nil {
					t.Fatal(err)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := epochs.CurrentFence(ctx); err != nil {
				t.Fatal(err)
			}
			reconciler := &TaskReconciler{
				Client: kubeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10), DurableControlStore: controlStore,
				ControllerEpochManager: epochs, ACPRuntimeNamespace: acpTestRuntimeNamespace,
				ACPRuntimeImages: ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)},
			}
			if tt.protectedSet {
				reconciler.ProtectedSet = protectedset.Source{Reader: kubeClient, Namespace: proposerTestNamespace, Name: proposerTestConfigMap}
			}
			bound := bindACPQueueTaskForTest(t, ctx, reconciler, task, agent)
			if _, err := reconciler.queueACPRuntimeTask(ctx, bound, agent); err != nil {
				t.Fatalf("queueACPRuntimeTask() error = %v", err)
			}
			var pools corev1alpha1.RuntimePoolList
			if err := kubeClient.List(ctx, &pools); err != nil {
				t.Fatal(err)
			}
			current := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
				t.Fatal(err)
			}
			if tt.wantViolation == "" {
				if current.Status.Execution != nil && current.Status.Execution.Reason == proposerConstraintViolationReason || len(pools.Items) != 1 {
					t.Fatalf("constrained proposer was not queued: execution=%#v pools=%d message=%q", current.Status.Execution, len(pools.Items), current.Status.Message)
				}
				return
			}
			if len(pools.Items) != 0 {
				t.Fatalf("unconstrained proposer created RuntimePools: %#v", pools.Items)
			}
			if current.Status.Execution == nil || current.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
				current.Status.Execution.Reason != proposerConstraintViolationReason || !strings.Contains(current.Status.Message, tt.wantViolation) {
				t.Fatalf("unconstrained proposer status: execution=%#v message=%q", current.Status.Execution, current.Status.Message)
			}
		})
	}
}

func TestACPMCPBrokerRefusesToolsOutsideTheProposerAllowlist(t *testing.T) {
	for _, role := range []string{labels.ImprovementRoleProposer, "invalid"} {
		t.Run(role, func(t *testing.T) {
			effects, fence := newMCPBrokerControlStore(t)
			request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
			bearer := strings.Repeat("b", 32)
			capability := []byte(strings.Repeat("c", 32))
			var executions atomic.Int32
			broker := &ACPMCPBroker{
				Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
					return ACPMCPBrokerCredentials{
						ControllerBearerToken: bearer, CapabilitySecret: capability,
						ExpectedFence: got.Metadata.Fence, RuntimeProfile: profile, ControllerFence: fence,
						Task: ACPMCPAuthenticatedTask{
							Name: acpDispatcherTestTaskName, Namespace: got.Namespace, UID: string(got.Metadata.TaskUID),
							ImprovementRole: role,
						},
					}, nil
				}),
				Prompts: ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
				Executor: ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
					executions.Add(1)
					return json.RawMessage(acpMCPTestOKBody), nil
				}),
				Effects: effects,
			}
			response := performMCPBrokerCall(t, broker, request, bearer, capability)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "improvement proposer") || executions.Load() != 0 {
				t.Fatalf("proposer call to %q = %d %s with %d executions, want 403 and no execution",
					request.Call.ToolName, response.Code, response.Body.String(), executions.Load())
			}
		})
	}
}

func TestProposerBrokeredToolAllowlist(t *testing.T) {
	for _, name := range []string{"propose_memory"} {
		if !proposerBrokeredToolAllowed(labels.ImprovementRoleProposer, name) {
			t.Errorf("proposer may not call %q", name)
		}
	}
	for _, name := range []string{
		"delegate_task", "wait_for_tasks", "run_validation", "web_search", "web_fetch", "recall_memory", "remember",
		"search_transcript", "send_message", "check_messages", "reply_in_conversation", "create_pull_request", "get_issue",
	} {
		if proposerBrokeredToolAllowed(labels.ImprovementRoleProposer, name) {
			t.Errorf("proposer may call %q", name)
		}
	}
	if !proposerBrokeredToolAllowed("", "web_fetch") {
		t.Error("an unmarked Task lost a brokered tool")
	}
	if proposerBrokeredToolAllowed("invalid", "propose_memory") {
		t.Error("an unknown improvement role may call a brokered tool")
	}
	if taskImprovementRole(nil) != "" || taskImprovementRole(map[string]string{labels.AnnotationImprovementRole: ""}) != "invalid" ||
		taskImprovementRole(map[string]string{labels.AnnotationImprovementRole: " proposer "}) != labels.ImprovementRoleProposer {
		t.Fatal("taskImprovementRole does not distinguish unmarked, blank and proposer Tasks")
	}
}

func TestDeniedPathsReachTheRuntimeAndThePublisher(t *testing.T) {
	workspace := &corev1alpha1.WorkspaceConfig{DeniedPaths: []string{"internal/admission/**", "AGENTS.md"}}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Workspace: workspace}}
	limits := acpWorkspaceDeltaLimits(task)
	published := publicationDeniedPaths(workspace)
	if !slices.Equal(limits.DeniedPaths, workspace.DeniedPaths) || !slices.Equal(published, workspace.DeniedPaths) {
		t.Fatalf("runtime limits %v and publisher patterns %v, want %v", limits.DeniedPaths, published, workspace.DeniedPaths)
	}
	limits.DeniedPaths[0], published[0] = "changed", "changed"
	if workspace.DeniedPaths[0] != "internal/admission/**" {
		t.Fatal("denied paths alias the Task workspace")
	}
	if publicationDeniedPaths(&corev1alpha1.WorkspaceConfig{}) != nil || publicationDeniedPaths(nil) != nil {
		t.Fatal("a Task without deniedPaths sent patterns to the publisher")
	}
}

func TestDeniedPathRejectionUsesPatchPathDenied(t *testing.T) {
	message := acpWorkspaceValidationFailureMessage(&harnessv2.ClientError{
		StatusCode: http.StatusConflict, Code: harnessv2.ErrorCodeSessionPoisoned, Message: harnessv2.WorkspaceDeltaDeniedPathMessage,
	})
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Delivery: &corev1alpha1.TaskDeliveryStatus{
		Reason: acpWorkspaceValidationFailedReason, Message: message,
	}}}
	if reason := repositoryMonitorWorkspacePathRejectionReason(task); reason != repositoryMonitorPatchPathDenied {
		t.Fatalf("denied-path rejection reason = %q, want %q", reason, repositoryMonitorPatchPathDenied)
	}
	withDenied := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{DeniedPaths: []string{"internal/admission/**"}}}}
	if !repositoryMonitorTaskPathDenied(withDenied, "internal/admission/task_provenance.go") ||
		repositoryMonitorTaskPathDenied(withDenied, "internal/api/server.go") || repositoryMonitorTaskPathDenied(&corev1alpha1.Task{}, "AGENTS.md") {
		t.Fatal("the controller re-check does not apply the Task's deniedPaths")
	}
}

func TestPublisherDeniedPathRejectionMatchesTheRuntimeRejection(t *testing.T) {
	err := classifyPublisherDeliveryError("PublicationPrepareFailed", &publisherservice.ClientError{
		StatusCode: http.StatusUnprocessableEntity,
		Response:   publisherservice.ErrorResponse{Code: publisherservice.ErrorCodeDeniedPath, Message: "workspace delta contains a denied path"},
	})
	deliveryErr, ok := errors.AsType[*acpDeliveryError](err)
	if !ok || deliveryErr.reason != acpWorkspaceValidationFailedReason || deliveryErr.message != harnessv2.WorkspaceDeltaDeniedPathMessage ||
		deliveryErr.state != corev1alpha1.TaskDeliveryStateDeliveryConflict || deliveryErr.outcome != corev1alpha1.TaskDeliveryOutcomeDeliveryConflict {
		t.Fatalf("publisher denied-path delivery error = %#v, want WorkspaceValidationFailed with the denied-path message", err)
	}
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Delivery: &corev1alpha1.TaskDeliveryStatus{
		Reason: deliveryErr.reason, Message: deliveryErr.message,
	}}}
	if reason := repositoryMonitorWorkspacePathRejectionReason(task); reason != repositoryMonitorPatchPathDenied {
		t.Fatalf("publisher denied-path rejection reason = %q, want %q", reason, repositoryMonitorPatchPathDenied)
	}

	unsafe, ok := errors.AsType[*acpDeliveryError](classifyPublisherDeliveryError("PublicationPrepareFailed", &publisherservice.ClientError{
		StatusCode: http.StatusUnprocessableEntity, Response: publisherservice.ErrorResponse{Code: "unsafe_delta"},
	}))
	if !ok || unsafe.reason != "PublicationPrepareFailed" {
		t.Fatalf("other unsafe delta delivery error = %#v, want PublicationPrepareFailed", unsafe)
	}
}

func TestWorkspacePreflightValidatesDeniedPaths(t *testing.T) {
	read := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: proposerTestRepository, DeniedPaths: []string{"AGENTS.md"},
	}}}
	if err := validateACPWorkspacePreflight(read); err == nil || !strings.Contains(err.Error(), "write workspace intent") {
		t.Fatalf("read-intent deniedPaths preflight error = %v, want write intent required", err)
	}
	invalid := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Workspace: &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: proposerTestRepository, DeniedPaths: []string{"../escape"},
	}}}
	if err := validateACPWorkspacePreflight(invalid); err == nil || !strings.Contains(err.Error(), "deniedPaths contains an invalid pattern") {
		t.Fatalf("invalid deniedPaths preflight error = %v", err)
	}
}

// A marked Task that does not take the ACP agent path is failed instead of
// running as a Job, so no execution path skips the proposer constraints.
func TestHandlePendingFailsMarkedContainerTask(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "marked-container", Namespace: "default", UID: "marked-container-uid", Generation: 1,
			Annotations: map[string]string{labels.AnnotationImprovementRole: labels.ImprovementRoleProposer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer, Image: "docker.io/library/busybox:1.36", Command: []string{"true"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(newTestScheme(), task)
	if _, err := r.handlePending(ctx, task); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(current.Status.Message, string(proposerConstraintViolationReason)) {
		t.Fatalf("marked container Task: phase=%s message=%q, want a proposer constraint failure", current.Status.Phase, current.Status.Message)
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("marked container Task created Jobs: %d, err=%v", len(jobs.Items), err)
	}
}

// A marked schedule is failed before it creates children, because scheduled
// children do not carry the improvement role.
func TestHandlePendingFailsMarkedScheduledTask(t *testing.T) {
	ctx := context.Background()
	for name, spec := range map[string]corev1alpha1.TaskSpec{
		"agent":     {Type: corev1alpha1.TaskTypeAgent, Prompt: "improve", Schedule: "*/5 * * * *"},
		"container": {Type: corev1alpha1.TaskTypeContainer, Image: "docker.io/library/busybox:1.36", Command: []string{"true"}, Schedule: "*/5 * * * *"},
	} {
		t.Run(name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "marked-scheduled-" + name, Namespace: "default", UID: types.UID("marked-scheduled-" + name), Generation: 1,
					Annotations: map[string]string{labels.AnnotationImprovementRole: labels.ImprovementRoleProposer},
				},
				Spec:   spec,
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}
			r := newUnitReconciler(newTestScheme(), task)
			if _, err := r.handlePending(ctx, task); err != nil {
				t.Fatal(err)
			}
			current := &corev1alpha1.Task{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(current.Status.Message, string(proposerConstraintViolationReason)) {
				t.Fatalf("marked scheduled Task: phase=%s message=%q, want a proposer constraint failure", current.Status.Phase, current.Status.Message)
			}
			var tasks corev1alpha1.TaskList
			if err := r.List(ctx, &tasks); err != nil || len(tasks.Items) != 1 {
				t.Fatalf("marked scheduled Task created children: %d Tasks, err=%v", len(tasks.Items), err)
			}
		})
	}
}

// Codex enforces only an unrestricted native policy under write intent, and
// that policy includes its native web tools, which a proposer must not hold.
// No tool list satisfies both, so a write-intent Codex proposer never runs.
func TestWriteIntentCodexProposerIsRefused(t *testing.T) {
	codex := string(corev1alpha1.AgentRuntimeCodex)
	unrestricted := acp.BuiltInRuntimeNativeToolNames(codex)
	if err := validateACPProviderNativePolicy(codex, corev1alpha1.WorkspaceIntentWrite, unrestricted, nil, true); err != nil {
		t.Fatalf("unrestricted codex write policy rejected: %v", err)
	}
	var withoutWeb []string
	for _, name := range unrestricted {
		if proposerAllowedToolName(name) {
			withoutWeb = append(withoutWeb, name)
		}
	}
	if len(withoutWeb) == len(unrestricted) {
		t.Fatal("the unrestricted codex tool list passed the proposer tool check")
	}
	if err := validateACPProviderNativePolicy(codex, corev1alpha1.WorkspaceIntentWrite, withoutWeb, nil, true); err == nil {
		t.Fatal("codex accepted a write policy without its native web tools; revisit the proposer web restriction")
	}
}

// TestDispatcherRechecksProposerConstraintsBeforeExecution covers a proposer
// that passed queue admission and then waited while the protected set changed
// or became unavailable: the dispatcher re-reads it before the reserved
// attempt starts, and fails closed without creating a Session or prompt.
func TestDispatcherRechecksProposerConstraintsBeforeExecution(t *testing.T) {
	tests := []struct {
		name          string
		configMap     func(*testing.T) *corev1.ConfigMap
		wantViolation string
	}{
		{name: "ConfigMap removed", wantViolation: "proposer flows are disabled"},
		{name: "case set added in the Task namespace", configMap: func(t *testing.T) *corev1.ConfigMap {
			configMap := loadSampleProtectedSetConfigMap(t)
			configMap.Data[protectedset.ConfigMapKey] = strings.Replace(configMap.Data[protectedset.ConfigMapKey],
				"{namespace: orka-evals, name: pr-review-train,", "{namespace: default, name: pr-review-train,", 1)
			return configMap
		}, wantViolation: "namespace that holds an evaluation case set"},
		{name: "unchanged", configMap: loadSampleProtectedSetConfigMap},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var creates, prompts atomic.Int32
			uid := types.UID(strings.Repeat(string(rune('a'+index)), 8) + "-9999-9999-9999-999999999999")
			fixture := newTaskScopedCreateConflictFixtureForRuntime(t, ctx, "proposer-recheck", uid, corev1alpha1.AgentRuntimeCodex,
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					options := dispatcherRuntimeServerOptions{onPrompt: func(harnessv2.StartPromptRequest) { prompts.Add(1) }}
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, options, func(harnessv2.CreateRuntimeSessionRequest) { creates.Add(1) })
				}, func(task *corev1alpha1.Task) {
					task.Annotations = map[string]string{labels.AnnotationImprovementRole: labels.ImprovementRoleProposer}
					task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Read", "Glob", "Grep"}}
				})
			defer fixture.stop()
			if tt.configMap != nil {
				if err := fixture.kubeClient.Create(ctx, tt.configMap(t)); err != nil {
					t.Fatal(err)
				}
			}
			fixture.dispatcher.ProtectedSet = protectedset.Source{
				Reader: fixture.kubeClient, Namespace: proposerTestNamespace, Name: proposerTestConfigMap,
			}

			reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
			if err != nil || reserved == nil {
				t.Fatalf("reserveTask() = %v, %v", reserved, err)
			}
			executeErr := fixture.dispatcher.executeReservedTask(ctx, reserved, target)
			current := fixture.currentTask(t, ctx)
			if tt.wantViolation == "" {
				// Only reaching the runtime matters here; this fixture's runtime
				// does not complete the rest of the turn.
				if current.Status.Execution.Reason == proposerConstraintViolationReason || creates.Load() == 0 {
					t.Fatalf("constrained proposer did not start: execution=%#v creates=%d err=%v", current.Status.Execution, creates.Load(), executeErr)
				}
				return
			}
			if executeErr != nil {
				t.Fatalf("executeReservedTask() error = %v", executeErr)
			}
			attempt, err := fixture.dispatcher.Store.GetPromptAttempt(ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.Execution.Reason != proposerConstraintViolationReason ||
				!strings.Contains(current.Status.Execution.Message, tt.wantViolation) {
				t.Fatalf("stale proposer status: phase=%s execution=%#v", current.Status.Phase, current.Status.Execution)
			}
			if attempt.ExecutionState != store.PromptExecutionFailed || attempt.TerminalReason != string(proposerConstraintViolationReason) {
				t.Fatalf("attempt = %s/%s, want Failed/%s", attempt.ExecutionState, attempt.TerminalReason, proposerConstraintViolationReason)
			}
			if creates.Load() != 0 || prompts.Load() != 0 {
				t.Fatalf("stale proposer reached the runtime: creates=%d prompts=%d", creates.Load(), prompts.Load())
			}
		})
	}
}

// A denied-path rejection that ends a publication during recovery classifies
// like a first-time rejection: the delivery status built from the stored
// publication carries WorkspaceValidationFailed and the denied-path message.
func TestRecoveredPublisherDeniedPathRejectionKeepsItsClassification(t *testing.T) {
	denied := &publisherservice.ClientError{
		StatusCode: http.StatusUnprocessableEntity,
		Response:   publisherservice.ErrorResponse{Code: publisherservice.ErrorCodeDeniedPath},
	}
	if got := publicationRecoveryPrepareFailureReason(denied); got != harnessv2.WorkspaceDeltaDeniedPathMessage {
		t.Fatalf("recovery reason = %q", got)
	}
	if got := publicationRecoveryPrepareFailureReason(errors.New("boom")); got != "publication preparation failed during recovery" {
		t.Fatalf("recovery reason for another error = %q", got)
	}
	publication := &store.Publication{State: store.PublicationPreparationFailed, TerminalReason: harnessv2.WorkspaceDeltaDeniedPathMessage}
	status := publicationTaskDeliveryStatus(&corev1alpha1.WorkspaceConfig{}, harnessv2.WorkspaceBaseline{},
		harnessv2.WorkspaceDeltaDescriptor{Artifact: &harnessv2.ArtifactReference{}}, publication, "")
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Delivery: &status}}
	if reason := repositoryMonitorWorkspacePathRejectionReason(task); reason != repositoryMonitorPatchPathDenied {
		t.Fatalf("recovered rejection reason = %q (delivery %+v), want %q", reason, status, repositoryMonitorPatchPathDenied)
	}
}
