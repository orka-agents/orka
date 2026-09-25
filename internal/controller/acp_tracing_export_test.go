package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/workers/acp/supervisor"
)

// RunACPTraceBoundary is test-only glue for the external test package: API imports
// controller, so task creation cannot be imported into controller's own tests.
// It uses the existing production reserve/execute sequence and real supervisor.
func RunACPTraceBoundary(t *testing.T, tasks []*corev1alpha1.Task, agent *corev1alpha1.Agent, tracer trace.Tracer, barrierURL string) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	images := ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)}
	for i, task := range tasks {
		task.UID = types.UID(fmt.Sprintf("trace-task-%d", i))
		task.ResourceVersion = ""
		task.Generation = 1
		task.Spec.SessionRef = nil
		task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: int32(i + 1), Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateQueued, Attempt: int32(i + 1), PromptID: fmt.Sprintf("prompt-%s-%d", task.UID, i+1),
			RuntimePoolUID: "pool-uid", RequestDigest: testControlDigestForDispatcher(task.Name),
		}}
	}
	plan := frozenACPDispatcherPlanForTest(t, tasks[0], agent, images)
	for _, task := range tasks {
		if task.Labels == nil {
			task.Labels = map[string]string{}
		}
		task.Labels[acpRuntimeTaskPoolLabel] = plan.PoolName
		task.Status.Execution.RuntimePoolName = plan.PoolName
	}
	base := t.TempDir()
	for _, dir := range []string{base, filepath.Dir(base)} {
		if err := os.Chmod(dir, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	allocator, err := acp.NewUIDAllocator(65534, 65550, 65534)
	if err != nil {
		t.Fatal(err)
	}
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var adapterName, adapterDigest string
	for name, digest := range plan.Profile.AdapterDigests {
		adapterName, adapterDigest = name, digest
		break
	}
	cfg := supervisor.Config{
		Tracer: tracer, ListenAddress: ":0",
		Fence: harnessv2.Fence{RuntimeInstanceID: "pod-uid.boot-id", SupervisorBootID: "boot-id", ControllerEpoch: 1,
			RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: plan.Digest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion},
		Capabilities: harnessv2.CapabilitiesResponse{Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: plan.Profile.ACPProfile,
			RuntimeProfileDigest: plan.Digest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
			AdapterDigests: plan.Profile.AdapterDigests, Limits: harnessv2.DefaultProtocolLimits(),
			Provider:            harnessv2.ProviderCapabilities{ProviderKinds: []string{"codex"}, Models: []string{plan.Profile.Model}, SupportsPermissions: true, SupportsCancel: true},
			WorkspaceGovernance: harnessv2.StrictWorkspaceGovernanceCapabilities(), SupportsDrain: true,
			SupportsPublicationFinalization: true, SupportsAgentSessionConfiguration: true},
		Provider: supervisor.ProviderProfile{Kind: "codex", Model: plan.Profile.Model, Command: command,
			Args: []string{"-test.run=^TestACPTraceProviderFixture$"}, AdapterName: adapterName, AdapterDigest: adapterDigest,
			Environment: map[string]string{"ORKA_TRACE_FIXTURE": "true", "ORKA_TRACE_FIXTURE_BARRIER": barrierURL}},
		ControllerBearerToken: strings.Repeat("t", 32), CapabilitySecret: []byte(strings.Repeat("s", 32)), RequireCapabilities: true,
		SessionBaseDir: filepath.Join(base, "sessions"), UIDAllocator: allocator,
		ProviderProxy: supervisor.ProviderProxyConfig{UpstreamBaseURL: "http://127.0.0.1:1", UpstreamBearerToken: strings.Repeat("p", 32), ProviderKind: "codex", Model: plan.Profile.Model},
		MCPBroker: supervisor.MCPBrokerFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
			return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: json.RawMessage(`{"ok":true}`)}, nil
		}),
		WorkspaceMaterializer: supervisor.EmptyWorkspaceMaterializer(), InitializeTimeout: 5 * time.Second, CancelGrace: time.Second,
	}
	cfg.Capabilities.Limits.MaxResidentSessions = 4
	cfg.Capabilities.Limits.MaxConcurrentPrompts = 2
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sup.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	server := httptest.NewServer(sup.Handler())
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: plan.PoolName, UID: "pool-uid", Generation: 1},
		Spec:       corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan)}, DesiredReplicas: 1},
		Status: corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
			ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Host, PodUID: "pod-uid", BootID: "boot-id", RuntimeInstanceID: "pod-uid.boot-id", ControllerEpoch: 1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(plan.Digest), ProfileDigestSchemaVersion: "1"}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth-e1", Labels: map[string]string{runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID)}}, Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(cfg.ControllerBearerToken), runtimePoolCapabilitySecretKey: cfg.CapabilitySecret}}
	objects := make([]client.Object, 0, 4+len(tasks))
	objects = append(objects, pool, secret, agent, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}})
	for _, task := range tasks {
		objects = append(objects, task)
	}
	kc := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{}, &corev1alpha1.RuntimeSessionControl{}, &corev1alpha1.BranchClaim{}, &corev1alpha1.Publication{}, &corev1alpha1.ExternalEffect{}).WithObjects(objects...).Build()
	kc = withControllerEpochLeaseUIDs(t, kc)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	persistence := sqlite.NewStore(db, "test")
	control, err := kubestore.NewComposite(kc, "orka-system", persistence, kubestore.WithAPIReader(kc))
	if err != nil {
		t.Fatal(err)
	}
	epochs := NewControllerEpochManager(control, "trace-test").WithMirror(persistence)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	epochCtx, cancelEpoch := context.WithCancel(ctx)
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Error(err)
		}
	}()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range tasks {
		binder := &TaskReconciler{Client: kc, APIReader: kc, Scheme: scheme, ACPRuntimeEnabled: true, ACPRuntimeImages: images, AgentExecutionSnapshots: persistence}
		if _, err := binder.resolveAgentExecutionCandidate(ctx, task, agent); err != nil {
			t.Fatalf("trace fixture candidate for %s: %v", task.Name, err)
		}
		task = prepareBoundACPDispatcherTaskWithStoresForTest(t, ctx, kc, scheme, control, persistence, task, agent, images, epochs)
		task.Status.Execution.ControllerEpoch = fence.Epoch
		if err := kc.Status().Update(ctx, task); err != nil {
			t.Fatal(err)
		}
		tasks[i] = task
		key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: int64(task.Status.Execution.Attempt), PromptID: task.Status.Execution.PromptID}
		id, err := key.CanonicalID()
		if err != nil {
			t.Fatal(err)
		}
		_, err = control.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{ID: id, Key: key, RequestDigest: task.Status.Execution.RequestDigest, BindingDigest: task.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: task.Status.AgentExecutionBinding.Snapshot.Digest, ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested}), fence)
		if err != nil {
			t.Fatal(err)
		}
	}
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{SessionControls: control, Transcripts: persistence, Publications: control, BranchClaims: control, Lineages: persistence})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &ACPDispatcher{Client: kc, APIReader: kc, Store: control, ResultStore: persistence, EventStore: persistence, PlanStore: persistence, Snapshots: persistence, Epochs: epochs, Sessions: continuity}
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Go(func() { dispatchQueuedTask(ctx, t, dispatcher, task.DeepCopy()) })
	}
	wg.Wait()
	for _, task := range tasks {
		completed := &corev1alpha1.Task{}
		if err := kc.Get(ctx, client.ObjectKeyFromObject(task), completed); err != nil {
			t.Fatal(err)
		}
		if completed.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
			t.Fatalf("task %s phase = %s: %s", task.Name, completed.Status.Phase, completed.Status.Message)
		}
	}
}
