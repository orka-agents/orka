package controller

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/runtimefeedback"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tools"
)

// This enters planning, frozen binding, queue reservation and actual dispatch.
// Kubernetes storage and the HTTP supervisor are fixtures. It proves capture
// ordering and exact identity on the real dispatcher, not native enforcement.
func TestRuntimeFeedbackDispatchRegistersBeforeSubmissionAndCompletes(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal harnessv2.EventType
		fault    string
	}{
		{name: "completed", terminal: harnessv2.EventCompleted},
		{name: "cancelled", terminal: harnessv2.EventCancelled},
		{name: "Pod API failure recovers before submission", terminal: harnessv2.EventCompleted, fault: "pod-read"},
		{name: "supervisor status failure recovers before submission", terminal: harnessv2.EventCompleted, fault: "status"},
		{name: "interrupted status body recovers before submission", terminal: harnessv2.EventCompleted, fault: "status-body"},
		{name: "interrupted status error body recovers before submission", terminal: harnessv2.EventCompleted, fault: "status-error-body"},
		{name: "empty status fails unsent attempt", terminal: harnessv2.EventCompleted, fault: "status-empty"},
		{name: "malformed status fails unsent attempt", terminal: harnessv2.EventCompleted, fault: "status-malformed"},
		{name: "invalid Pod identity fails unsent attempt", terminal: harnessv2.EventCompleted, fault: "identity"},
		{name: "missing configuration fails unsent attempt", terminal: harnessv2.EventCompleted, fault: "configuration"},
		{name: "GKR registration unavailable still submits", terminal: harnessv2.EventCompleted, fault: "register"},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal := test.terminal
			scheme := bindingTestScheme(t)
			if err := coordinationv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "feedback-task", UID: "feedback-task-uid"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "inspect denied operation", AgentRef: &corev1alpha1.AgentReference{Name: "agent"}, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{}}}
			agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: "agent-uid", Generation: 1}, Spec: corev1alpha1.AgentSpec{Model: testOpenCodeModelConfig(), Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2), DefaultAllowedTools: []string{RuntimeFeedbackToolName}}}}
			images := ACPRuntimeImages{Opencode: "example.test/opencode@sha256:" + strings.Repeat("a", 64)}
			plan := frozenACPDispatcherPlanForTest(t, task, agent, images)
			registered := make(chan runtimefeedback.Query, 1)
			service := &dispatchFeedbackCapture{registered: registered}
			if test.fault == "register" {
				service.registerErr = errors.New("capture unavailable")
			}
			var statusUnavailable atomic.Bool
			statusUnavailable.Store(strings.HasPrefix(test.fault, "status"))
			submitted := make(chan harnessv2.StartPromptRequest, 1)
			server := newDispatcherRuntimeServerWithOptions(t, plan.Profile, plan.Digest, dispatcherRuntimeServerOptions{
				statusBody: func(response harnessv2.StatusResponse) (string, int, int) {
					return feedbackDispatchStatusBody(response, &statusUnavailable, test.fault)
				},
				statusError: func(response harnessv2.StatusResponse) *harnessv2.ErrorResponse {
					return feedbackDispatchStatusError(response, &statusUnavailable, test.fault)
				},
				terminalEvents: map[harnessv2.PromptID]harnessv2.EventType{harnessv2.PromptID("prompt-" + string(task.UID) + "-1"): terminal},
				onPrompt: func(request harnessv2.StartPromptRequest) {
					verifyFeedbackRegistrationForPrompt(t, registered, request)
					submitted <- request
				},
			})
			defer server.Close()
			endpoint, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: plan.PoolName, UID: acpDispatcherTestPoolUID, Generation: 1, Labels: map[string]string{runtimeFeedbackTaskLabel: string(task.UID)}}, Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan)}, DesiredReplicas: 1, Capacity: &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 1, MaxRunningPrompts: 1}}, Status: corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting, ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: endpoint.Host, PodUID: "pod-uid", BootID: "boot-id", RuntimeInstanceID: "pod-uid.boot-id", ControllerEpoch: 1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(plan.Digest), ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10)}}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth-e1", Labels: map[string]string{runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID)}}, Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "runtime-pod", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{Name: "runtime"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "runtime", Ready: true, ContainerID: "containerd://" + strings.Repeat("a", 64), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}}}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).WithObjects(task, agent, pool, secret, pod, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}).Build()
			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "dispatch.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			persistence := sqlite.NewStore(db, "feedback-dispatch")
			cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x61}, sqlite.AgentExecutionSnapshotKeyBytes))
			if err != nil {
				t.Fatal(err)
			}
			if err := persistence.SetAgentExecutionSnapshotCipher(cipher); err != nil {
				t.Fatal(err)
			}
			epochs := NewControllerEpochManager(persistence, "feedback-controller")
			epochCtx, stopEpoch := context.WithCancel(t.Context())
			epochDone := make(chan error, 1)
			go func() { epochDone <- epochs.Start(epochCtx) }()
			defer func() { stopEpoch(); <-epochDone }()
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			if _, err := epochs.CurrentFence(ctx); err != nil {
				t.Fatal(err)
			}
			registry := tools.NewRegistry()
			if err := RegisterRuntimeFeedbackTool(registry, kube, service); err != nil {
				t.Fatal(err)
			}
			binder := &TaskReconciler{Client: kube, APIReader: kube, Scheme: scheme, DurableControlStore: persistence, AgentExecutionSnapshots: persistence, ControllerEpochManager: epochs, ACPRuntimeEnabled: true, ACPRuntimeNamespace: "orka-runtimes", ACPRuntimeImages: images, MCPRegistry: registry}
			bound := bindACPQueueTaskForTest(t, ctx, binder, task, agent)
			if _, err := binder.queueACPRuntimeTask(ctx, bound, agent); err != nil {
				t.Fatal(err)
			}
			queued := &corev1alpha1.Task{}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(task), queued); err != nil {
				t.Fatal(err)
			}
			dispatcher := &ACPDispatcher{Client: kube, APIReader: kube, Store: persistence, ResultStore: persistence, Snapshots: persistence, Epochs: epochs, RuntimeFeedback: service, MCPRegistry: registry}
			if test.fault == "pod-read" {
				dispatcher.APIReader = &feedbackPodReadFailure{Reader: kube}
			}
			if test.fault == "configuration" {
				dispatcher.RuntimeFeedback = nil
			}
			if test.fault == "identity" {
				if err := kube.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
					t.Fatal(err)
				}
				pool.Status.ActiveInstance.PodUID = "different-pod-uid"
				if err := kube.Status().Update(ctx, pool); err != nil {
					t.Fatal(err)
				}
			}
			dispatchQueuedTask(ctx, t, dispatcher, queued)
			if verifyFeedbackPreparationAndRetry(t, ctx, dispatcher, task, service, submitted, &statusUnavailable, test.fault) {
				return
			}
			select {
			case request := <-submitted:
				if !request.MCPAuthorization.ToolPolicy.Allows(RuntimeFeedbackToolName) {
					t.Fatal("dispatch dropped the frozen feedback grant")
				}
			default:
				t.Fatal("dispatcher never submitted a prompt")
			}
			reason := runtimefeedback.Completed
			if terminal == harnessv2.EventCancelled {
				reason = runtimefeedback.Cancelled
			}
			if len(service.completions) != 1 || service.completions[0] != service.query || len(service.reasons) != 1 || service.reasons[0] != reason {
				t.Fatal("dispatch did not complete the exact registered capture with its terminal outcome")
			}
			current := &corev1alpha1.Task{}
			if err := kube.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			want := corev1alpha1.TaskPhaseSucceeded
			if terminal == harnessv2.EventCancelled {
				want = corev1alpha1.TaskPhaseCancelled
			}
			if current.Status.Phase != want {
				t.Fatalf("terminal task phase=%s want=%s", current.Status.Phase, want)
			}
		})
	}
}

type dispatchFeedbackCapture struct {
	registerErr error
	registered  chan runtimefeedback.Query
	query       runtimefeedback.Query
	completions []runtimefeedback.Query
	reasons     []string
}

func (s *dispatchFeedbackCapture) Register(_ context.Context, query runtimefeedback.Query) error {
	s.query = query
	s.registered <- query
	return s.registerErr
}
func (s *dispatchFeedbackCapture) Complete(_ context.Context, query runtimefeedback.Query, reason string) error {
	s.completions = append(s.completions, query)
	s.reasons = append(s.reasons, reason)
	return nil
}
func (*dispatchFeedbackCapture) Report(context.Context, runtimefeedback.Query) (runtimefeedback.Report, error) {
	return runtimefeedback.Report{}, store.ErrNotFound
}

type feedbackPodReadFailure struct{ client.Reader }

func (r *feedbackPodReadFailure) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Pod); ok {
		return errors.New("temporary API failure")
	}
	return r.Reader.Get(ctx, key, object, options...)
}

// Assert both durable/public states before retrying through the real dispatcher.
func verifyFeedbackPreparationAndRetry(t *testing.T, ctx context.Context, dispatcher *ACPDispatcher, task *corev1alpha1.Task, service *dispatchFeedbackCapture, submitted chan harnessv2.StartPromptRequest, statusUnavailable *atomic.Bool, fault string) bool {
	t.Helper()
	if fault != "pod-read" && !strings.HasPrefix(fault, "status") && fault != "identity" && fault != "configuration" {
		return false
	}
	kube := dispatcher.Client
	current := &corev1alpha1.Task{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	attemptID, err := promptAttemptIDFromTask(current)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := dispatcher.Store.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(submitted) != 0 || len(service.registered) != 0 || len(service.completions) != 0 {
		t.Fatal("failed preparation submitted or registered a prompt")
	}
	if fault == "identity" || fault == "configuration" || fault == "status-malformed" || fault == "status-empty" {
		if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.Execution.Reason != "RuntimeFeedbackPreparationFailed" || attempt.ExecutionState != store.PromptExecutionFailed {
			t.Fatal("definitive feedback preparation failure did not settle the unsent attempt")
		}
		return true
	}
	if current.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved || attempt.ExecutionState != store.PromptExecutionReserved {
		t.Fatalf("transient preparation failure stranded the unsent durable attempt: Task=%s, attempt=%s", current.Status.Execution.State, attempt.ExecutionState)
	}
	dispatcher.APIReader = kube
	statusUnavailable.Store(false)
	dispatchQueuedTask(ctx, t, dispatcher, current)
	return false
}

func verifyFeedbackRegistrationForPrompt(t *testing.T, registered chan runtimefeedback.Query, request harnessv2.StartPromptRequest) {
	t.Helper()
	select {
	case query := <-registered:
		execution := runtimeFeedbackExecution{TaskUID: string(request.Metadata.TaskUID), TaskAttempt: request.Metadata.TaskAttempt, PromptID: request.Metadata.PromptID, Fence: request.Metadata.Fence, Workload: runtimefeedback.Workload{Namespace: "orka-runtimes", PodName: "runtime-pod", PodUID: "pod-uid", ContainerName: "runtime", ContainerID: "containerd://" + strings.Repeat("a", 64), Node: "node"}}
		expected, err := execution.query()
		if err != nil || query != expected {
			t.Error("dispatcher registration did not retain the submitted prompt and workload identity")
		}
	default:
		t.Error("prompt submitted before capture registration")
	}
}

func feedbackDispatchStatusBody(response harnessv2.StatusResponse, unavailable *atomic.Bool, fault string) (string, int, int) {
	if len(response.Sessions) != 1 || !unavailable.Load() {
		return "", 0, 0
	}
	switch fault {
	case "status-body":
		return "{", 1024, 200 // Real HTTP read ends with unexpected EOF.
	case "status-error-body":
		return "{", 1024, 429
	case "status-empty":
		return "", 0, 200
	case "status-malformed":
		return "{", 1, 200 // Complete body, invalid JSON.
	default:
		return "", 0, 0
	}
}
func feedbackDispatchStatusError(response harnessv2.StatusResponse, unavailable *atomic.Bool, fault string) *harnessv2.ErrorResponse {
	if len(response.Sessions) == 1 && unavailable.Load() && fault == "status" {
		return &harnessv2.ErrorResponse{Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeRateLimited, Message: "temporarily unavailable", Retryable: true}
	}
	return nil
}
