package controller

import (
	"bytes"
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
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
	for _, terminal := range []harnessv2.EventType{harnessv2.EventCompleted, harnessv2.EventCancelled} {
		t.Run(string(terminal), func(t *testing.T) {
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
			submitted := make(chan harnessv2.StartPromptRequest, 1)
			server := newDispatcherRuntimeServerWithOptions(t, plan.Profile, plan.Digest, dispatcherRuntimeServerOptions{
				terminalEvents: map[harnessv2.PromptID]harnessv2.EventType{harnessv2.PromptID("prompt-" + string(task.UID) + "-1"): terminal},
				onPrompt: func(request harnessv2.StartPromptRequest) {
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
			dispatchQueuedTask(ctx, t, dispatcher, queued)
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
	registered  chan runtimefeedback.Query
	query       runtimefeedback.Query
	completions []runtimefeedback.Query
	reasons     []string
}

func (s *dispatchFeedbackCapture) Register(_ context.Context, query runtimefeedback.Query) error {
	s.query = query
	s.registered <- query
	return nil
}
func (s *dispatchFeedbackCapture) Complete(_ context.Context, query runtimefeedback.Query, reason string) error {
	s.completions = append(s.completions, query)
	s.reasons = append(s.reasons, reason)
	return nil
}
func (*dispatchFeedbackCapture) Report(context.Context, runtimefeedback.Query) (runtimefeedback.Report, error) {
	return runtimefeedback.Report{}, store.ErrNotFound
}
