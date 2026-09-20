package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/runtimefeedback"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/tools"
)

type feedbackTestService struct {
	registrations []runtimefeedback.Query
	completions   []runtimefeedback.Query
	reasons       []string
	report        func(context.Context, runtimefeedback.Query) (runtimefeedback.Report, error)
}

func (s *feedbackTestService) Register(_ context.Context, query runtimefeedback.Query) error {
	s.registrations = append(s.registrations, query)
	return nil
}
func (s *feedbackTestService) Complete(_ context.Context, query runtimefeedback.Query, reason string) error {
	s.completions = append(s.completions, query)
	s.reasons = append(s.reasons, reason)
	return nil
}
func (s *feedbackTestService) Report(ctx context.Context, query runtimefeedback.Query) (runtimefeedback.Report, error) {
	return s.report(ctx, query)
}

func TestRuntimeFeedbackPlanningKeepsPoolsExclusiveAcrossTasksAndRotation(t *testing.T) {
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{UID: "agent-uid", Generation: 1}, Spec: corev1alpha1.AgentSpec{
		Model: testOpenCodeModelConfig(), Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode,
			ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2), DefaultAllowedTools: []string{RuntimeFeedbackToolName}},
	}}
	first := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{UID: "first-task"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	second := first.DeepCopy()
	second.UID = "second-task"
	images := ACPRuntimeImages{Opencode: "example.test/opencode@sha256:" + strings.Repeat("a", 64)}
	plan, err := PlanACPRuntime(first, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	other, err := PlanACPRuntime(second, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Digest != other.Digest || plan.PoolName == other.PoolName || plan.RuntimeFeedbackTaskUID != string(first.UID) {
		t.Fatal("feedback pools do not isolate the immutable Task identity")
	}
	images.Opencode = "example.test/opencode@sha256:" + strings.Repeat("b", 64)
	rotated, err := currentACPRuntimeDeliveryPlan(plan, images)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.plan.PoolName == plan.PoolName || rotated.plan.RuntimeFeedbackTaskUID != string(first.UID) {
		t.Fatal("image rotation lost Task isolation")
	}
	agent.Spec.Runtime.DefaultAllowedTools = []string{"Read"}
	plain, err := PlanACPRuntime(first, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	plainOther, err := PlanACPRuntime(second, agent, images)
	if err != nil {
		t.Fatal(err)
	}
	if plain.PoolName != plainOther.PoolName || plain.RuntimeFeedbackTaskUID != "" {
		t.Fatal("ordinary pools changed")
	}
}

type feedbackFixture struct {
	reader        client.Client
	task          *corev1alpha1.Task
	pool          *corev1alpha1.RuntimePool
	pod           *corev1.Pod
	request       harnessv2.MCPBrokerCallRequest
	profile       harnessv2.RuntimeProfile
	status        atomic.Pointer[harnessv2.StatusResponse]
	runtimeClient *harnessv2.Client
	ctx           context.Context
}

func newFeedbackFixture(t *testing.T) *feedbackFixture {
	t.Helper()
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	request.Metadata.Fence.RuntimeInstanceID = harnessv2.RuntimeInstanceID(runtimePoolRuntimeInstanceID(types.UID("pod-uid"), request.Metadata.Fence.SupervisorBootID))
	profile.ProviderKind = "opencode"
	profile.Model = "openai/gpt-test"
	profile.ModelLimits = &harnessv2.ModelTokenLimits{Context: 10000, Output: 1000}
	descriptor := harnessv2.MCPToolDescriptor{Name: RuntimeFeedbackToolName, Description: (&runtimeFeedbackTool{}).Description(), InputSchema: (&runtimeFeedbackTool{}).Parameters(), Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectReadOnly}
	request.Call.ToolName = RuntimeFeedbackToolName
	request.Call.Arguments = json.RawMessage(`{}`)
	request.Authorization.ToolPolicy.AllowedToolNames = []string{RuntimeFeedbackToolName}
	request.Authorization.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{descriptor}
	request.Authorization.ToolPolicy.DescriptorDigest, _ = harnessv2.CanonicalMCPToolDescriptorDigest(request.Authorization.ToolPolicy.Tools)
	profile.ToolPolicyDigest, _ = harnessv2.CanonicalRuntimeToolPolicyDigest([]string{RuntimeFeedbackToolName}, nil, true)
	profile.MCPConfigurationDigest, _ = harnessv2.CanonicalMCPConfigurationDigest([]string{RuntimeFeedbackToolName})
	request.Authorization.ToolPolicyDigest = profile.ToolPolicyDigest
	request.Authorization.MCPConfigurationDigest = profile.MCPConfigurationDigest
	request.Metadata.Fence.RuntimeProfileDigest, _ = harnessv2.CanonicalProfileDigest(profile)
	request.Metadata.RequestDigest, _ = harnessv2.CanonicalRequestDigest(request)
	f := &feedbackFixture{request: request, profile: profile}
	f.pool = &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: request.Namespace, UID: types.UID(request.Metadata.Fence.RuntimePoolUID), Generation: int64(request.Metadata.Fence.RuntimePoolGeneration), Labels: map[string]string{runtimeFeedbackTaskLabel: string(request.Metadata.TaskUID)}}, Spec: corev1alpha1.RuntimePoolSpec{
		Capacity: &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 1, MaxRunningPrompts: 1}, Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Image: "example.test/runtime@sha256:" + strings.Repeat("a", 64), Profile: corev1alpha1.RuntimePoolProfileSpec{ProviderKind: "opencode", Digest: string(request.Metadata.Fence.RuntimeProfileDigest)}},
	}}
	digest, err := runtimePoolIdentityDigest(f.pool.Spec.Runtime.Profile.Digest, f.pool.Spec.Runtime.Image, string(request.Metadata.TaskUID))
	if err != nil {
		t.Fatal(err)
	}
	f.pool.Name = acpRuntimePoolName("opencode", harnessv2.ProfileDigest(digest))
	f.pool.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "workers", PodName: "runtime-pod", PodUID: "pod-uid", RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID), BootID: string(request.Metadata.Fence.SupervisorBootID), ControllerEpoch: 1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(request.Metadata.Fence.RuntimeProfileDigest)}
	f.task = &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: request.Namespace, Name: "task", UID: types.UID(request.Metadata.TaskUID)}, Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: string(request.Metadata.PromptID), RuntimePoolName: f.pool.Name, RuntimePoolUID: string(f.pool.UID), RuntimeSessionUID: string(request.Metadata.Fence.RuntimeSessionUID), RuntimeSessionGeneration: int64(request.Metadata.Fence.RuntimeSessionGeneration), RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID), ControllerEpoch: 1}}}
	f.pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "workers", Name: "runtime-pod", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "runtime"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "runtime", Ready: true, ContainerID: "containerd://" + strings.Repeat("a", 64), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}}}
	status := &harnessv2.StatusResponse{Protocol: harnessv2.ProtocolVersion, Fence: request.Metadata.Fence, Lifecycle: harnessv2.SupervisorLifecycleReady, Timestamp: time.Now().UTC(), Sessions: []harnessv2.RuntimeSessionStatus{{RuntimeSessionID: "session", RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, Generation: request.Metadata.Fence.RuntimeSessionGeneration, State: harnessv2.RuntimeSessionStatePromptRunning, ActivePromptID: request.Metadata.PromptID, LastTransitionAt: time.Now().UTC()}}, ActivePrompts: []harnessv2.ActivePromptStatus{{RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, SessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration, TaskUID: request.Metadata.TaskUID, TaskAttempt: request.Metadata.TaskAttempt, PromptID: request.Metadata.PromptID, LeaseExpiresAt: time.Now().Add(time.Minute), FrameSequence: 1, StartedAt: time.Now().Add(-time.Second)}}, Pressure: harnessv2.PressureMetadata{ResidentSessions: 1, ActivePrompts: 1}}
	status.Fence.RuntimeSessionUID = ""
	status.Fence.RuntimeSessionGeneration = 0
	f.status.Store(status)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != harnessv2.StatusPath || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 32) || r.Header.Get(harnessv2.OperationCapabilityHeader) == "" {
			t.Error("supervisor status did not use existing controller authorization")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.status.Load())
	}))
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	f.pool.Status.ActiveInstance.PodAddress = endpoint.Host
	f.runtimeClient, err = harnessv2.NewClient(server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("b", 32)), harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("c", 32))), harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID}))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "workers", Name: "pool-auth", Labels: map[string]string{runtimePoolUIDLabel: string(f.pool.UID), runtimePoolCredentialEpochLabel: "1", runtimePoolAuthLabel: booleanTrueValue}}, Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("b", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("c", 32))}}
	f.reader = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(f.task, f.pool, f.pod).WithObjects(f.task, f.pool, f.pod, secret).Build()
	f.ctx = withACPMCPAuthenticatedTask(context.Background(), ACPMCPAuthenticatedTask{Name: f.task.Name, Namespace: f.task.Namespace, UID: string(f.task.UID)})
	f.ctx = context.WithValue(f.ctx, acpMCPExecutionContextKey{}, request)
	f.ctx = context.WithValue(f.ctx, acpMCPTaskDataGuardContextKey{}, ACPMCPTaskDataGuard(func(ctx context.Context, access func(context.Context) error) error { return access(ctx) }))
	return f
}

func feedbackReport(query runtimefeedback.Query) runtimefeedback.Report {
	now := time.Now().UTC()
	return runtimefeedback.Report{APIVersion: runtimefeedback.APIVersion, Kind: runtimefeedback.ReportKind, RunID: query.RunID, Workload: query.Workload, Status: "Collecting", SampledAt: now, Capture: &runtimefeedback.Capture{StartedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9 * time.Minute)}, Source: runtimefeedback.Source{Name: "gkr-runtime-observer", InstanceID: "gkr-boot"}, AttributionScope: "Container", Completeness: "Partial", Events: []runtimefeedback.Event{{Timestamp: now.Add(-time.Second), Decision: "deny", KernelEnforced: true, DestinationAddress: "203.0.113.9", DestinationPort: 443, Protocol: "TCP"}}}
}

func TestRuntimeFeedbackToolDoesNotAcceptAgentSelectedIdentity(t *testing.T) {
	tool := &runtimeFeedbackTool{}
	for _, args := range []string{`{"taskUID":"other"}`, `{"runID":"other"}`, `{"containerID":"other"}`, `{"url":"https://attacker.invalid"}`, `null`, `[]`, `{} {}`} {
		if _, err := tool.Execute(context.Background(), json.RawMessage(args)); err == nil {
			t.Fatalf("accepted untrusted arguments: %s", args)
		}
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted unauthenticated execution")
	}
}

func TestRuntimeFeedbackRejectsReplacedOrSharedWorkload(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*feedbackFixture)
	}{
		{"shared pool", func(f *feedbackFixture) { f.pool.Spec.Capacity.MaxResidentSessions = 2 }},
		{"different Task", func(f *feedbackFixture) { f.pool.Labels[runtimeFeedbackTaskLabel] = "other" }},
		{"replaced Pod", func(f *feedbackFixture) { f.pool.Status.ActiveInstance.PodUID = "old-pod" }},
		{"sidecar", func(f *feedbackFixture) {
			f.pod.Spec.Containers = append(f.pod.Spec.Containers, corev1.Container{Name: "sidecar"})
		}},
		{"unknown container", func(f *feedbackFixture) { f.pod.Status.ContainerStatuses[0].ContainerID = "" }},
		{"supervisor restart", func(f *feedbackFixture) { f.pool.Status.ActiveInstance.BootID = "new-boot" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFeedbackFixture(t)
			test.mutate(f)
			poolStatus, podStatus := f.pool.Status, f.pod.Status
			if err := f.reader.Update(context.Background(), f.pool); err != nil {
				t.Fatal(err)
			}
			f.pool.Status = poolStatus
			if err := f.reader.Status().Update(context.Background(), f.pool); err != nil {
				t.Fatal(err)
			}
			if err := f.reader.Update(context.Background(), f.pod); err != nil {
				t.Fatal(err)
			}
			f.pod.Status = podStatus
			if err := f.reader.Status().Update(context.Background(), f.pod); err != nil {
				t.Fatal(err)
			}
			if _, _, err := resolveRuntimeFeedbackExecution(context.Background(), f.reader, f.task, f.request.Metadata.Fence); err == nil {
				t.Fatal("accepted stale/shared workload")
			}
		})
	}
}

func TestRuntimeFeedbackRegistersBeforePromptAndCompletesExactRun(t *testing.T) {
	f := newFeedbackFixture(t)
	idle := *f.status.Load()
	idle.Sessions = append([]harnessv2.RuntimeSessionStatus(nil), idle.Sessions...)
	idle.Sessions[0].State = harnessv2.RuntimeSessionStateIdle
	idle.Sessions[0].ActivePromptID = ""
	idle.ActivePrompts = nil
	idle.Pressure.ActivePrompts = 0
	f.status.Store(&idle)
	service := &feedbackTestService{}
	d := &ACPDispatcher{Client: f.reader, APIReader: f.reader, RuntimeFeedback: service}
	policy := harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{RuntimeFeedbackToolName}}}
	complete, err := d.startRuntimeFeedback(context.Background(), f.task, f.request.Metadata.Fence, f.runtimeClient, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(service.registrations) != 1 {
		t.Fatal("capture was not registered before prompt")
	}
	complete("Cancelled")
	if len(service.completions) != 1 || service.registrations[0] != service.completions[0] || service.reasons[0] != "Cancelled" {
		t.Fatal("completion lost exact execution binding")
	}
}

func TestRuntimeFeedbackToolReturnsBoundEvidenceAndRechecksContainer(t *testing.T) {
	for _, scenario := range []string{"evidence", "unavailable", "wrong report", "container restarted", "prompt cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFeedbackFixture(t)
			service := &feedbackTestService{report: func(_ context.Context, q runtimefeedback.Query) (runtimefeedback.Report, error) {
				r := feedbackReport(q)
				switch scenario {
				case "unavailable":
					return runtimefeedback.Report{}, errors.New("provider capability URL must never escape")
				case "wrong report":
					r.Workload.PodUID = "other"
				case "container restarted":
					f.pod.Status.ContainerStatuses[0].RestartCount++
					f.pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("b", 64)
					if err := f.reader.Status().Update(context.Background(), f.pod); err != nil {
						t.Fatal(err)
					}
				case "prompt cancelled":
					ended := *f.status.Load()
					ended.Sessions = append([]harnessv2.RuntimeSessionStatus(nil), ended.Sessions...)
					ended.Sessions[0].State = harnessv2.RuntimeSessionStateIdle
					ended.Sessions[0].ActivePromptID = ""
					ended.ActivePrompts = nil
					ended.Pressure.ActivePrompts = 0
					f.status.Store(&ended)
				}
				return r, nil
			}}
			tool := &runtimeFeedbackTool{reader: f.reader, service: service}
			result, err := tool.Execute(f.ctx, json.RawMessage(`{}`))
			if scenario == "container restarted" || scenario == "prompt cancelled" {
				if err == nil || result != "" {
					t.Fatal("released evidence after execution changed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "evidence" {
				if !strings.Contains(result, "203.0.113.9") || !strings.Contains(result, string(f.task.UID)) {
					t.Fatal("trusted evidence missing")
				}
			} else if !strings.Contains(result, `"status":"Unavailable"`) || strings.Contains(result, "203.0.113.9") || strings.Contains(result, "capability URL") {
				t.Fatal("unavailable report disclosed untrusted evidence")
			}
			if len(service.registrations) != 0 || len(service.completions) != 0 {
				t.Fatal("read-only tool mutated capture lifecycle")
			}
		})
	}
}

func TestRuntimeFeedbackToolRegistrationIsExplicitAndReadOnly(t *testing.T) {
	f := newFeedbackFixture(t)
	registry := tools.NewRegistry()
	if _, ok := registry.Get(RuntimeFeedbackToolName); ok {
		t.Fatal("tool is implicit")
	}
	if err := RegisterRuntimeFeedbackTool(registry, f.reader, &feedbackTestService{}); err != nil {
		t.Fatal(err)
	}
	descriptors, err := buildCanonicalMCPToolDescriptors(context.Background(), f.reader, "default", "opencode", []string{RuntimeFeedbackToolName}, nil, false, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 1 || descriptors[0].Source != harnessv2.MCPToolSourceBrokeredBuiltin || descriptors[0].Effect != harnessv2.MCPToolEffectReadOnly {
		t.Fatal("tool is not brokered read-only")
	}
}

func TestRuntimeFeedbackBrokerUsesAuthenticatedAttemptAndEpochGuard(t *testing.T) {
	f := newFeedbackFixture(t)
	scheme := bindingTestScheme(t)
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	guardClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.ControllerEpoch{}).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("test-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	controls, err := storekube.New(guardClient, "orka-system", storekube.WithAPIReader(guardClient))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := controls.CompareAndSwapControllerEpoch(context.Background(), store.ControllerEpochCAS{NewEpoch: 1, HolderID: "controller", RequestDigest: store.CanonicalBytesDigest([]byte("epoch"))})
	if err != nil {
		t.Fatal(err)
	}
	epoch := store.ControllerEpochFence{Name: owner.Name, Epoch: owner.Epoch, HolderID: owner.HolderID}
	registry := tools.NewRegistry()
	var reports atomic.Int32
	var preAdmissionStatus string
	service := &feedbackTestService{report: func(_ context.Context, q runtimefeedback.Query) (runtimefeedback.Report, error) {
		reports.Add(1)
		report := feedbackReport(q)
		if preAdmissionStatus != "" {
			endedAt := f.status.Load().ActivePrompts[0].StartedAt.Add(-time.Second)
			report.Status = preAdmissionStatus
			report.Capture.EndedAt = &endedAt
			report.Capture.ExpiresAt = endedAt
			report.Events[0].Timestamp = endedAt.Add(-time.Second)
		}
		return report, nil
	}}
	if err := RegisterRuntimeFeedbackTool(registry, f.reader, service); err != nil {
		t.Fatal(err)
	}
	attempts := &liveMCPPromptAttemptStore{}
	attempt := &store.PromptAttempt{Key: store.PromptAttemptKey{Namespace: f.request.Namespace, TaskUID: string(f.request.Metadata.TaskUID), Attempt: int64(f.request.Metadata.TaskAttempt), PromptID: string(f.request.Metadata.PromptID)}, SessionUID: string(f.request.Metadata.Fence.RuntimeSessionUID), RuntimeInstanceID: string(f.request.Metadata.Fence.RuntimeInstanceID), ControllerEpoch: int64(f.request.Metadata.Fence.ControllerEpoch), ExecutionState: store.PromptExecutionRunning}
	attempt.ID, _ = attempt.Key.CanonicalID()
	attempts.current.Store(attempt)
	broker := &ACPMCPBroker{Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
		if request.Metadata.TaskUID != f.request.Metadata.TaskUID || request.Metadata.TaskAttempt != f.request.Metadata.TaskAttempt || request.Metadata.PromptID != f.request.Metadata.PromptID {
			return ACPMCPBrokerCredentials{}, errors.New("execution mismatch")
		}
		return ACPMCPBrokerCredentials{ControllerBearerToken: strings.Repeat("b", 32), CapabilitySecret: []byte(strings.Repeat("c", 32)), ExpectedFence: f.request.Metadata.Fence, RuntimeProfile: f.profile, ControllerFence: epoch, Task: ACPMCPAuthenticatedTask{Name: f.task.Name, Namespace: f.task.Namespace, UID: string(f.task.UID)}}, nil
	}), Prompts: DurableACPMCPPromptAuthorizer{Attempts: attempts}, Executor: RegistryACPMCPToolExecutor{Registry: registry}, Effects: controls, EpochMutations: controls}
	for _, scenario := range []string{"valid", "expired before admission", "finalized before admission", "wrong bearer", "wrong capability", "wrong attempt", "ended prompt"} {
		t.Run(scenario, func(t *testing.T) {
			preAdmissionStatus = ""
			request := f.request
			bearer := strings.Repeat("b", 32)
			capability := []byte(strings.Repeat("c", 32))
			before := reports.Load()
			switch scenario {
			case "expired before admission":
				preAdmissionStatus = runtimefeedback.Expired
			case "finalized before admission":
				preAdmissionStatus = runtimefeedback.Finalized
			case "wrong bearer":
				bearer = strings.Repeat("z", 32)
			case "wrong capability":
				capability = []byte(strings.Repeat("z", 32))
			case "wrong attempt":
				request.Metadata.TaskAttempt++
			case "ended prompt":
				ended := *attempt
				ended.ExecutionState = store.PromptExecutionCancelled
				attempts.current.Store(&ended)
			}
			request.Metadata.RequestDigest, _ = harnessv2.CanonicalRequestDigest(request)
			response := performMCPBrokerCall(t, broker, request, bearer, capability)
			if scenario == "valid" {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "203.0.113.9") || reports.Load() != before+1 {
					t.Fatalf("valid authenticated diagnosis failed: status %d", response.Code)
				}
			} else if preAdmissionStatus != "" {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Unavailable") ||
					strings.Contains(response.Body.String(), "203.0.113.9") || reports.Load() != before+1 {
					t.Fatal("authenticated broker released pre-admission evidence instead of unavailable diagnostics")
				}
			} else if response.Code == http.StatusOK || reports.Load() != before {
				t.Fatal("unauthorized or ended execution reached feedback service")
			}
		})
	}
}

func TestRuntimeFeedbackSnapshotAndMaterializedPoolPreserveTaskIsolation(t *testing.T) {
	task := bindingTestTask()
	agent := bindingTestAgent()
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeOpencode
	agent.Spec.SystemPrompt = nil
	agent.Spec.Model = testOpenCodeModelConfig()
	agent.Spec.Runtime.DefaultAllowedTools = []string{RuntimeFeedbackToolName}
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	reconciler.ACPRuntimeImages.Opencode = "example.test/opencode@sha256:" + strings.Repeat("a", 64)
	reconciler.MCPRegistry = tools.NewRegistry()
	if err := RegisterRuntimeFeedbackTool(reconciler.MCPRegistry, reconciler.Client, &feedbackTestService{}); err != nil {
		t.Fatal(err)
	}
	candidate, err := reconciler.resolveAgentExecutionCandidate(context.Background(), task, agent)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeAgentExecutionSnapshot(candidate.snapshotBody)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &store.AgentExecutionSnapshot{TaskUID: string(task.UID), Digest: candidate.binding.Snapshot.Digest, SchemaVersion: candidate.binding.Snapshot.SchemaVersion, Body: candidate.snapshotBody}
	plan, _, _, err := validateAgentExecutionSnapshot(&candidate.binding, snapshot, body)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RuntimeFeedbackTaskUID != string(task.UID) {
		t.Fatal("snapshot lost Task isolation")
	}
	// Changing only the binding's Task identity cannot authorize the original pool.
	other := candidate.binding.DeepCopy()
	other.Task.UID = "other-task"
	if _, _, _, err := validateAgentExecutionSnapshot(other, snapshot, body); err == nil {
		t.Fatal("snapshot rebound to another Task")
	}
	pool, _, err := reconciler.ensureACPRuntimePool(context.Background(), task.Namespace, plan, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !runtimeFeedbackPoolMatches(pool, string(task.UID)) {
		t.Fatal("materialized pool lost single-Task capacity")
	}
}

func TestRuntimeFeedbackOptOutPreservesExecutionModesAndSnapshot(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeOpencode,
		corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot,
	} {
		for _, mode := range []string{"fresh", "session", "workspace"} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				task := bindingTestTask()
				switch mode {
				case "session":
					task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "continued-session"}
				case "workspace":
					task = workspaceBindingTestTask(nil)
				}
				task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{RuntimeFeedbackToolName}}
				agent := bindingTestAgent()
				agent.Spec.Runtime.Type = provider
				agent.Spec.SystemPrompt = nil
				agent.Spec.Runtime.DefaultAllowBash = new(false)
				agent.Spec.Runtime.DefaultAllowedTools = []string{"Glob", "Grep", "Read", RuntimeFeedbackToolName}
				if provider == corev1alpha1.AgentRuntimeOpencode {
					agent.Spec.Model = testOpenCodeModelConfig()
				}
				reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
				image := "example.test/runtime@sha256:" + strings.Repeat("a", 64)
				reconciler.ACPRuntimeImages = ACPRuntimeImages{Codex: image, Opencode: image, Claude: image, Copilot: image}
				// An explicitly denied tool must not need the feedback service configured.
				reconciler.MCPRegistry = tools.NewRegistry()
				candidate, err := reconciler.resolveAgentExecutionCandidate(context.Background(), task, agent)
				if err != nil {
					t.Fatalf("resolve candidate with feedback denied: %v", err)
				}
				body, err := decodeAgentExecutionSnapshot(candidate.snapshotBody)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := &store.AgentExecutionSnapshot{TaskUID: string(task.UID), Digest: candidate.binding.Snapshot.Digest, SchemaVersion: candidate.binding.Snapshot.SchemaVersion, Body: candidate.snapshotBody}
				plan, _, policy, err := validateAgentExecutionSnapshot(&candidate.binding, snapshot, body)
				if err != nil {
					t.Fatalf("restore snapshot with feedback denied: %v", err)
				}
				if plan.RuntimeFeedbackTaskUID != "" || policy.ToolPolicy.Allows(RuntimeFeedbackToolName) {
					t.Fatal("explicit denial enabled runtime feedback")
				}
				for _, descriptor := range policy.ToolPolicy.Tools {
					if descriptor.Name == RuntimeFeedbackToolName {
						t.Fatal("explicitly denied feedback was exposed as an MCP tool")
					}
				}
				if (plan.Workspace != nil) != (mode == "workspace") {
					t.Fatal("feedback denial changed the execution workspace binding")
				}
				pool, _, err := reconciler.ensureACPRuntimePool(context.Background(), task.Namespace, plan, "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				if pool.Labels[runtimeFeedbackTaskLabel] != "" {
					t.Fatal("feedback denial added a dedicated feedback pool label")
				}
				if mode != "workspace" {
					if pool.Spec.Capacity.MaxResidentSessions != corev1alpha1.DefaultRuntimePoolMaxResidentSessions ||
						pool.Spec.Capacity.MaxRunningPrompts != corev1alpha1.DefaultRuntimePoolMaxRunningPrompts {
						t.Fatal("feedback denial changed ordinary pool capacity")
					}
					other := task.DeepCopy()
					other.UID = "other-task"
					otherPlan, err := PlanACPRuntime(other, agent, reconciler.ACPRuntimeImages)
					if err != nil || otherPlan.PoolName != plan.PoolName {
						t.Fatalf("feedback denial made the pool Task-specific: %v", err)
					}
				}
				// Removing the denial must still apply feedback's supported-mode restrictions.
				enabled := task.DeepCopy()
				enabled.Spec.AgentRuntime.DisallowedTools = nil
				_, err = PlanACPRuntime(enabled, agent, reconciler.ACPRuntimeImages)
				wantRejection := mode != "fresh" || (provider != corev1alpha1.AgentRuntimeCodex && provider != corev1alpha1.AgentRuntimeOpencode)
				if (err != nil) != wantRejection {
					t.Fatalf("enabled feedback planning error = %v, want rejection = %v", err, wantRejection)
				}
			})
		}
	}
}

func TestRuntimeFeedbackCodexKeepsExistingNativeReadPolicy(t *testing.T) {
	task := bindingTestTask()
	agent := bindingTestAgent()
	agent.Spec.Runtime.DefaultAllowBash = new(true)
	agent.Spec.Runtime.DefaultAllowedTools = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "WebSearch", "WebFetch", RuntimeFeedbackToolName}
	plan, err := PlanACPRuntime(task, agent, ACPRuntimeImages{Codex: "example.test/codex@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.RuntimeFeedbackTaskUID != string(task.UID) || plan.Profile.WorkspaceIntent != harnessv2.WorkspaceIntentRead || !strings.HasPrefix(plan.PoolName, "acp-codex-") {
		t.Fatal("native Codex feedback changed workspace intent or Task isolation")
	}
	registry := tools.NewRegistry()
	if err := RegisterRuntimeFeedbackTool(registry, fake.NewClientBuilder().WithScheme(bindingTestScheme(t)).Build(), &feedbackTestService{}); err != nil {
		t.Fatal(err)
	}
	configuration, err := buildRuntimeSessionMCPConfigurationWithRegistry(context.Background(), nil, task, agent, plan.Profile, registry)
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.ToolPolicy.AllowBash || !configuration.ToolPolicy.Allows(RuntimeFeedbackToolName) {
		t.Fatal("native Codex policy was weakened or lost")
	}
}

func TestRuntimeFeedbackCannotFallbackToCustomToolWhenDisabled(t *testing.T) {
	f := newFeedbackFixture(t)
	custom := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: f.task.Namespace, Name: RuntimeFeedbackToolName}, Spec: corev1alpha1.ToolSpec{BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead}}
	if err := f.reader.Create(context.Background(), custom); err != nil {
		t.Fatal(err)
	}
	if _, err := buildCanonicalMCPToolDescriptors(context.Background(), f.reader, f.task.Namespace, "codex", []string{RuntimeFeedbackToolName}, nil, true, tools.NewRegistry()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatal("disabled feedback fell back to custom Tool")
	}
}

func TestRuntimeFeedbackCodexPermissionUsesFrozenDescriptor(t *testing.T) {
	f := newFeedbackFixture(t)
	registry := tools.NewRegistry()
	if err := RegisterRuntimeFeedbackTool(registry, f.reader, &feedbackTestService{}); err != nil {
		t.Fatal(err)
	}
	descriptors, err := buildCanonicalMCPToolDescriptors(t.Context(), f.reader, "default", "codex", []string{RuntimeFeedbackToolName}, nil, false, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 1 || descriptors[0].Source != harnessv2.MCPToolSourceBrokeredBuiltin || descriptors[0].Effect != harnessv2.MCPToolEffectReadOnly {
		t.Fatal("runtime_feedback descriptor is not frozen read-only")
	}
	for _, tt := range []struct {
		name, tool           string
		required, disallowed bool
		want                 string
	}{
		{name: "correlated granted feedback", tool: RuntimeFeedbackToolName, want: "allow_once"},
		{name: "missing identity", want: "decline"},
		{name: "display name is not canonical identity", tool: "mcp.orka.runtime_feedback", want: "decline"},
		{name: "ungranted mutating tool", tool: "mutate", want: "decline"},
		{name: "disallowed feedback", tool: RuntimeFeedbackToolName, disallowed: true, want: "decline"},
		{name: "human-required feedback", tool: RuntimeFeedbackToolName, required: true, want: "decline"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configuration := harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{RuntimeFeedbackToolName}, Tools: descriptors}}
			if tt.required {
				configuration.ApprovalPolicy.RequiredTools = []string{RuntimeFeedbackToolName}
			}
			if tt.disallowed {
				configuration.ToolPolicy.DisallowedToolNames = []string{RuntimeFeedbackToolName}
			}
			permission := &harnessv2.PermissionRequestedEvent{
				ToolName: tt.tool, ToolCallID: "tool-call-v1-sha256-fixture", Title: "mcp.orka.runtime_feedback",
				Options: []harnessv2.PermissionOption{
					{OptionID: "allow_once", Kind: harnessv2.PermissionOptionAllowOnce},
					{OptionID: "decline", Kind: harnessv2.PermissionOptionRejectOnce},
				},
			}
			got := frozenMCPPermissionDecision(configuration, "codex", permission)
			if got.Outcome != harnessv2.PermissionDecisionSelected || got.OptionID != tt.want {
				t.Fatalf("frozen Codex permission = %#v", got)
			}
		})
	}
}
