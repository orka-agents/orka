package controller

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type externalACPDispatchFixture struct {
	ctx            context.Context
	client         client.Client
	controlStore   store.DurableControlStore
	persistence    *sqlite.Store
	epochs         *ControllerEpochManager
	reconciler     *TaskReconciler
	dispatcher     *ACPDispatcher
	agent          *corev1alpha1.Agent
	runtime        *corev1alpha1.AgentRuntime
	createCalls    *atomic.Int32
	createRequests chan harnessv2.CreateRuntimeSessionRequest
}

func newExternalACPDispatchFixture(t *testing.T) *externalACPDispatchFixture {
	t.Helper()
	allowAgentRuntimeLoopback(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	profile, governance, limits := testAgentRuntimeProfileClaimsAndLimits()
	toolPolicyDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	profile.ToolPolicyDigest = toolPolicyDigest
	mcpConfigurationDigest, err := harnessv2.CanonicalMCPConfigurationDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	profile.MCPConfigurationDigest = mcpConfigurationDigest
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	createCalls := &atomic.Int32{}
	createRequests := make(chan harnessv2.CreateRuntimeSessionRequest, 8)
	server := newDispatcherRuntimeServer(t, profile, profileDigest, func(request harnessv2.CreateRuntimeSessionRequest) {
		createCalls.Add(1)
		createRequests <- request
	})
	t.Cleanup(server.Close)

	config := conformancetest.Config{
		ControllerBearerToken:     strings.Repeat("t", 32),
		OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID:         "pod-uid.boot-id",
		SupervisorBootID:          "boot-id",
		RuntimePoolUID:            "pool-uid",
		Profile:                   profile,
		Limits:                    limits,
		SupportsDrain:             true,
		WorkspaceGovernance:       governance,
	}
	externalRuntime, authSecret := testAgentRuntimeAndSecret(t, server.URL, config)
	externalRuntime.Name = "external-v2"
	externalRuntime.UID = types.UID("external-runtime-uid")
	authSecret.UID = types.UID("external-runtime-auth-uid")
	authSecret.ResourceVersion = "1"
	authSecret.Labels[agentRuntimeAuthRefNameLabel] = externalRuntime.Name
	limitsSpec := *externalRuntime.Spec.Capabilities.Limits
	governanceSpec := *externalRuntime.Spec.Capabilities.WorkspaceGovernance
	profileSpec := externalRuntime.Spec.Capabilities.Profile
	externalRuntime.Status = corev1alpha1.AgentRuntimeStatus{
		Ready:                                    true,
		ObservedGeneration:                       externalRuntime.Generation,
		ObservedControllerAuthRefResourceVersion: authSecret.ResourceVersion,
		ObservedOperationCapabilityRefResourceVersion: authSecret.ResourceVersion,
		ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
			ProtocolVersion:            harnessv2.ProtocolVersion,
			Transport:                  "http+ndjson",
			ACPVersion:                 harnessv2.ACPProfileV1,
			RuntimeInstanceID:          string(config.RuntimeInstanceID),
			SupervisorBootID:           string(config.SupervisorBootID),
			ControllerEpoch:            1,
			RuntimePoolUID:             string(config.RuntimePoolUID),
			RuntimePoolGeneration:      1,
			RuntimeProfileDigest:       profileSpec.Digest,
			ProfileDigestSchemaVersion: profileSpec.DigestSchemaVersion,
			AdapterName:                profileSpec.AdapterName,
			AdapterDigest:              profileSpec.AdapterDigest,
			ProviderKind:               profileSpec.ProviderKind,
			Model:                      profileSpec.Model,
			Limits:                     &limitsSpec,
			SupportsDrain:              true,
			WorkspaceGovernance:        &governanceSpec,
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaultNS, Name: "external-agent", UID: types.UID("external-agent-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: externalRuntime.Name},
		}},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNS, UID: types.UID("default-namespace-uid")}}
	scheme := newTestScheme()
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.Task{}, &corev1alpha1.AgentRuntime{}, &corev1alpha1.ControllerEpoch{},
			&corev1alpha1.PromptAttempt{}, &corev1alpha1.RuntimeSessionControl{},
			&corev1alpha1.BranchClaim{}, &corev1alpha1.Publication{}, &corev1alpha1.ExternalEffect{},
		).
		WithObjects(agent, externalRuntime, authSecret, namespace).Build()
	kubeClient = withControllerEpochLeaseUIDs(t, kubeClient)

	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "external-dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	persistence := sqlite.NewStore(db, "external-dispatch-test")
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x58}, sqlite.AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.SetAgentExecutionSnapshotCipher(cipher); err != nil {
		t.Fatal(err)
	}
	controlStore, err := storekube.NewComposite(kubeClient, defaultNS, persistence, storekube.WithAPIReader(kubeClient))
	if err != nil {
		t.Fatal(err)
	}
	epochs := NewControllerEpochManager(controlStore, "external-dispatch-controller").WithMirror(persistence)
	epochCtx, cancelEpoch := context.WithCancel(ctx)
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("stop external dispatch epoch manager: %v", err)
		}
	})
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fence.Epoch != externalRuntime.Status.ObservedCapabilities.ControllerEpoch {
		t.Fatalf("controller epoch = %d, fixture runtime epoch = %d", fence.Epoch, externalRuntime.Status.ObservedCapabilities.ControllerEpoch)
	}
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controlStore,
		Transcripts:     persistence,
		Publications:    controlStore,
		BranchClaims:    controlStore,
		Lineages:        persistence,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionManager := NewSessionManager(persistence)
	sessionManager.SetGatewayEventStore(persistence)
	reconciler := &TaskReconciler{
		Client: kubeClient, APIReader: kubeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(32),
		DurableControlStore: controlStore, ControllerEpochManager: epochs, AgentExecutionSnapshots: persistence,
		ResultStore: persistence, MessageStore: persistence, PlanStore: persistence, ExecutionEventStore: persistence,
		SessionManager: sessionManager, ACPRuntimeEnabled: true,
	}
	dispatcher := &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: persistence,
		EventStore: persistence, PlanStore: persistence, Snapshots: persistence, Epochs: epochs, Sessions: continuity,
	}
	return &externalACPDispatchFixture{
		ctx: ctx, client: kubeClient, controlStore: controlStore, persistence: persistence, epochs: epochs,
		reconciler: reconciler, dispatcher: dispatcher,
		agent: agent, runtime: externalRuntime, createCalls: createCalls, createRequests: createRequests,
	}
}

func (f *externalACPDispatchFixture) queueTask(
	t *testing.T,
	name string,
	uid types.UID,
	prompt string,
	sessionRef *corev1alpha1.SessionReference,
) *corev1alpha1.Task {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: defaultNS, Name: name, UID: uid, Generation: 1, Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: f.agent.Name},
			Prompt: prompt, SessionRef: sessionRef,
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	if err := f.client.Create(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	result, err := f.reconciler.handlePending(f.ctx, current)
	if err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if result.RequeueAfter != time.Second {
		_, candidateErr := f.reconciler.resolveAgentExecutionCandidate(f.ctx, current, f.agent)
		t.Fatalf("RequeueAfter = %v, want %v; candidate error: %v", result.RequeueAfter, time.Second, candidateErr)
	}
	queued := &corev1alpha1.Task{}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(task), queued); err != nil {
		t.Fatal(err)
	}
	if queued.Status.AgentExecutionBinding == nil ||
		queued.Status.AgentExecutionBinding.Backend != corev1alpha1.AgentExecutionBackendExternalEndpoint ||
		queued.Status.AgentExecutionBinding.RuntimeRef == nil ||
		queued.Status.AgentExecutionBinding.RuntimeRef.UID != f.runtime.UID {
		t.Fatalf("external execution binding = %#v; Task status = %#v", queued.Status.AgentExecutionBinding, queued.Status)
	}
	if queued.Status.Execution == nil || queued.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued ||
		queued.Status.Execution.AgentRuntimeName != f.runtime.Name ||
		queued.Status.Execution.AgentRuntimeUID != string(f.runtime.UID) ||
		queued.Status.Execution.RuntimePoolName != "" || queued.Status.Execution.RuntimePoolUID != "" {
		t.Fatalf("external queued execution = %#v", queued.Status.Execution)
	}
	if queued.Labels[acpExternalRuntimeTaskLabel] != f.runtime.Name || queued.Labels[acpRuntimeTaskPoolLabel] != "" {
		t.Fatalf("external queue labels = %#v", queued.Labels)
	}
	attemptID, err := promptAttemptIDFromTask(queued)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.controlStore.GetPromptAttempt(f.ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionQueued ||
		attempt.BindingDigest != queued.Status.AgentExecutionBinding.BindingDigest ||
		attempt.SnapshotDigest != queued.Status.AgentExecutionBinding.Snapshot.Digest ||
		attempt.RequestDigest != queued.Status.Execution.RequestDigest {
		t.Fatalf("external durable PromptAttempt = %#v", attempt)
	}
	var pools corev1alpha1.RuntimePoolList
	if err := f.client.List(f.ctx, &pools, client.InNamespace(defaultNS)); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("external queue created %d RuntimePools", len(pools.Items))
	}
	return queued
}

func (f *externalACPDispatchFixture) dispatch(t *testing.T, queued *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	dispatchQueuedTask(f.ctx, t, f.dispatcher, queued.DeepCopy())
	completed := &corev1alpha1.Task{}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(queued), completed); err != nil {
		t.Fatal(err)
	}
	return completed
}

func TestACPDispatcherExecutesExternalRuntimeTask(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	queued := fixture.queueTask(t, "external-task", types.UID("external-task-uid"), "do work", nil)
	completed := fixture.dispatch(t, queued)
	if completed.Status.Phase != corev1alpha1.TaskPhaseSucceeded || completed.Status.Execution == nil ||
		completed.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded {
		t.Fatalf("completed external Task status = %#v", completed.Status)
	}
	result, err := fixture.persistence.GetResult(fixture.ctx, completed.Namespace, completed.Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != "from runtime" {
		t.Fatalf("result = %q, want external runtime result", result)
	}
	if fixture.createCalls.Load() != 1 {
		t.Fatalf("external CreateRuntimeSession calls = %d, want 1", fixture.createCalls.Load())
	}
	select {
	case request := <-fixture.createRequests:
		if request.AgentConfiguration != nil {
			t.Fatalf("external runtime received controller-owned Agent configuration: %#v", request.AgentConfiguration)
		}
		if request.Metadata.Fence.RuntimeInstanceID != "pod-uid.boot-id" ||
			request.Metadata.Fence.RuntimePoolUID != "pool-uid" || request.Profile.Model != acpTestModel {
			t.Fatalf("external CreateRuntimeSession request = %#v", request)
		}
	default:
		t.Fatal("external runtime did not receive CreateRuntimeSession")
	}
}

func TestExternalRuntimeHTTPClientLeavesPromptStreamDeadlineToContext(t *testing.T) {
	transport := http.DefaultTransport
	httpClient := externalRuntimeHTTPClient(transport)
	if httpClient.Timeout != 0 {
		t.Fatalf("external runtime HTTP client timeout = %v, want no whole-stream timeout", httpClient.Timeout)
	}
	if httpClient.Transport != transport {
		t.Fatalf("external runtime HTTP transport = %T, want supplied dial-controlled transport", httpClient.Transport)
	}
	if httpClient.CheckRedirect == nil || httpClient.CheckRedirect(&http.Request{}, nil) != http.ErrUseLastResponse {
		t.Fatal("external runtime HTTP client did not reject redirects")
	}
}

func TestBuildPromptRequestHonorsExternalLeaseBounds(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	queued := fixture.queueTask(t, "external-lease-bounds", types.UID("external-lease-bounds-uid"), "bounded", nil)
	bound, err := fixture.reconciler.loadVerifiedBoundExecution(
		fixture.ctx, queued, queued.Status.AgentExecutionBinding,
	)
	if err != nil {
		t.Fatal(err)
	}
	observed := fixture.runtime.Status.ObservedCapabilities
	fence := harnessv2.Fence{
		RuntimeInstanceID:          harnessv2.RuntimeInstanceID(observed.RuntimeInstanceID),
		SupervisorBootID:           harnessv2.SupervisorBootID(observed.SupervisorBootID),
		ControllerEpoch:            uint64(observed.ControllerEpoch),
		RuntimePoolUID:             harnessv2.RuntimePoolUID(observed.RuntimePoolUID),
		RuntimePoolGeneration:      uint64(observed.RuntimePoolGeneration),
		RuntimeProfileDigest:       bound.plan.Digest,
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
		RuntimeSessionUID:          "external-lease-bounds-session",
		RuntimeSessionGeneration:   1,
	}
	tests := []struct {
		name     string
		minimum  int64
		maximum  int64
		expected time.Duration
	}{
		{name: "maximum below preferred duration", minimum: 5_000, maximum: 30_000, expected: 30 * time.Second},
		{name: "minimum above preferred duration", minimum: 100_000, maximum: 120_000, expected: 100 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := harnessv2.DefaultProtocolLimits()
			limits.MinPromptLeaseMillis = test.minimum
			limits.MaxPromptLeaseMillis = test.maximum
			request, err := fixture.dispatcher.buildPromptRequest(
				bound.frozenTask, fence, bound.plan.Profile, bound.mcpConfiguration, "", "bounded", limits, 0,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got := request.Lease.ExpiresAt.Sub(request.Lease.IssuedAt); got != test.expected {
				t.Fatalf("prompt lease duration = %s, want %s", got, test.expected)
			}
			if request.Metadata.ExpiresAt.After(request.Lease.ExpiresAt) ||
				request.MCPAuthorization.ExpiresAt.After(request.Lease.ExpiresAt) {
				t.Fatal("prompt request authority outlived the bounded lease")
			}
		})
	}
}

func TestValidateExternalRuntimeCapabilitiesAcceptsProviderAndModelSupersets(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	runtimeClient, err := harnessv2.NewClient(fixture.runtime.Spec.Deployment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := runtimeClient.Capabilities(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	capabilities.Provider.ProviderKinds = append(capabilities.Provider.ProviderKinds, "another-provider")
	capabilities.Provider.Models = append(capabilities.Provider.Models, "another-model")
	if _, _, err := validateExternalRuntimeCapabilities(fixture.runtime, capabilities); err != nil {
		t.Fatalf("provider/model capability supersets were rejected: %v", err)
	}
}

func TestACPDispatcherExternalRecoveryWaitsForObservedCapabilities(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	task := fixture.queueTask(t, "external-recovery", types.UID("external-recovery-task-uid"), "recover", nil)

	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	current.Status.Execution.RuntimeInstanceID = fixture.runtime.Status.ObservedCapabilities.RuntimeInstanceID
	current.Status.Execution.RuntimeSessionUID = "external-recovery-session-uid"
	current.Status.Execution.RuntimeSessionGeneration = 1
	if err := fixture.client.Status().Update(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}

	runtime := &corev1alpha1.AgentRuntime{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.runtime), runtime); err != nil {
		t.Fatal(err)
	}
	runtime.Status.Ready = false
	runtime.Status.ObservedCapabilities = nil
	if err := fixture.client.Status().Update(fixture.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}

	ready, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("external recovery reported cleanup complete without observed runtime capabilities")
	}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatalf("external recovery recorded cleanup digest %q without authenticated runtime identity", current.Status.Execution.RuntimeSessionCleanupDigest)
	}
}

func TestACPDispatcherExternalRecoveryRejectsFrozenRegistrationDrift(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	task := fixture.queueTask(t, "external-recovery-drift", types.UID("external-recovery-drift-uid"), "recover", nil)
	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	current.Status.Execution.RuntimeInstanceID = fixture.runtime.Status.ObservedCapabilities.RuntimeInstanceID
	current.Status.Execution.RuntimeSessionUID = "external-recovery-drift-session"
	current.Status.Execution.RuntimeSessionGeneration = 1
	if err := fixture.client.Status().Update(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}

	var replacementCalls atomic.Int32
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		replacementCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer replacement.Close()
	runtime := &corev1alpha1.AgentRuntime{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.runtime), runtime); err != nil {
		t.Fatal(err)
	}
	runtime.Spec.Deployment.Endpoint = replacement.URL
	if err := fixture.client.Update(fixture.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}

	ready, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, current)
	if err == nil || ready {
		t.Fatalf("recovery with drifted frozen registration = ready %t, error %v", ready, err)
	}
	if replacementCalls.Load() != 0 {
		t.Fatalf("recovery contacted replacement endpoint %d times before validating the snapshot", replacementCalls.Load())
	}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatalf("recovery recorded cleanup digest %q after registration drift", current.Status.Execution.RuntimeSessionCleanupDigest)
	}
}

func TestACPDispatcherExternalRecoveryHandlesReplacementWithoutObservedCapabilities(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	task := fixture.queueTask(t, "external-recovery-replacement", types.UID("external-recovery-replacement-uid"), "recover", nil)
	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	current.Status.Execution.RuntimeInstanceID = fixture.runtime.Status.ObservedCapabilities.RuntimeInstanceID
	current.Status.Execution.RuntimeSessionUID = "external-recovery-replacement-session"
	current.Status.Execution.RuntimeSessionGeneration = 1
	if err := fixture.client.Status().Update(fixture.ctx, current); err != nil {
		t.Fatal(err)
	}

	replacement := fixture.runtime.DeepCopy()
	if err := fixture.client.Delete(fixture.ctx, fixture.runtime); err != nil {
		t.Fatal(err)
	}
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("external-runtime-replacement-uid")
	replacement.Status = corev1alpha1.AgentRuntimeStatus{}
	if err := fixture.client.Create(fixture.ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}

	ready, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("replacement AgentRuntime without observed capabilities did not complete obsolete cleanup")
	}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Execution.RuntimeSessionCleanupDigest == "" {
		t.Fatal("replacement AgentRuntime cleanup did not record its completion receipt")
	}
}

func TestACPDispatcherExternalRuntimeDriftFailsBeforeRuntimeMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *externalACPDispatchFixture)
	}{
		{name: "runtime UID", mutate: func(t *testing.T, fixture *externalACPDispatchFixture) {
			current := &corev1alpha1.AgentRuntime{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.runtime), current); err != nil {
				t.Fatal(err)
			}
			current.UID = types.UID("replacement-runtime-uid")
			if err := fixture.client.Update(fixture.ctx, current); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "runtime generation", mutate: func(t *testing.T, fixture *externalACPDispatchFixture) {
			current := &corev1alpha1.AgentRuntime{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.runtime), current); err != nil {
				t.Fatal(err)
			}
			current.Generation++
			if err := fixture.client.Update(fixture.ctx, current); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "observed profile", mutate: func(t *testing.T, fixture *externalACPDispatchFixture) {
			current := &corev1alpha1.AgentRuntime{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.runtime), current); err != nil {
				t.Fatal(err)
			}
			current.Status.ObservedCapabilities.RuntimeProfileDigest = testControllerDigest("drifted-profile")
			if err := fixture.client.Status().Update(fixture.ctx, current); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "authentication", mutate: func(t *testing.T, fixture *externalACPDispatchFixture) {
			secret := &corev1.Secret{}
			key := client.ObjectKey{Namespace: defaultNS, Name: fixture.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name}
			if err := fixture.client.Get(fixture.ctx, key, secret); err != nil {
				t.Fatal(err)
			}
			secret.Data[fixture.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Key] = []byte(strings.Repeat("r", 32))
			if err := fixture.client.Update(fixture.ctx, secret); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "controller fence", mutate: func(t *testing.T, fixture *externalACPDispatchFixture) {
			current := &corev1alpha1.AgentRuntime{}
			if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.runtime), current); err != nil {
				t.Fatal(err)
			}
			current.Status.ObservedCapabilities.ControllerEpoch++
			if err := fixture.client.Status().Update(fixture.ctx, current); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExternalACPDispatchFixture(t)
			queued := fixture.queueTask(t, "external-drift", types.UID("external-drift-uid"), "do not mutate", nil)
			test.mutate(t, fixture)
			reserved, target, err := fixture.dispatcher.reserveTask(fixture.ctx, queued.DeepCopy())
			if err == nil && reserved != nil {
				_ = fixture.dispatcher.executeReservedTask(fixture.ctx, reserved, target)
			}
			if fixture.createCalls.Load() != 0 {
				t.Fatalf("external runtime received %d mutating requests after %s drift", fixture.createCalls.Load(), test.name)
			}
		})
	}
}

func TestACPDispatcherExternalSessionContinuationUsesRuntimeRefLineage(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	first := fixture.queueTask(t, "external-session-1", types.UID("external-session-task-1"), "first", &corev1alpha1.SessionReference{
		Name: "conversation", Create: true, Append: true,
	})
	firstCompleted := fixture.dispatch(t, first)
	second := fixture.queueTask(t, "external-session-2", types.UID("external-session-task-2"), "second", &corev1alpha1.SessionReference{
		Name: "conversation", Append: true,
	})
	secondCompleted := fixture.dispatch(t, second)
	if firstCompleted.Status.Execution == nil || secondCompleted.Status.Execution == nil ||
		firstCompleted.Status.Execution.RuntimeSessionUID == "" ||
		firstCompleted.Status.Execution.RuntimeSessionUID != secondCompleted.Status.Execution.RuntimeSessionUID ||
		firstCompleted.Status.Execution.RuntimeSessionGeneration != secondCompleted.Status.Execution.RuntimeSessionGeneration {
		t.Fatalf("external continuation did not reuse RuntimeSession: first=%#v second=%#v", firstCompleted.Status.Execution, secondCompleted.Status.Execution)
	}
	if fixture.createCalls.Load() != 1 {
		t.Fatalf("external continuation CreateRuntimeSession calls = %d, want 1", fixture.createCalls.Load())
	}
	control, err := fixture.controlStore.GetSessionControl(fixture.ctx, defaultNS, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	wantRuntimeIdentity := "runtimeRef:" + string(fixture.runtime.UID)
	if control.Lineage == nil || control.Lineage.ContractVersion != string(corev1alpha1.AgentRuntimeContractHarnessV2) ||
		control.Lineage.RuntimeIdentity != wantRuntimeIdentity {
		t.Fatalf("external Session lineage = %#v, want runtime identity %q", control.Lineage, wantRuntimeIdentity)
	}
}

func TestExternalRuntimeFrozenCapabilityEnvelopeRejectsEveryLiveDriftClass(t *testing.T) {
	profile, governance, limits := testAgentRuntimeProfileClaimsAndLimits()
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	base := func() harnessv2.CapabilitiesResponse {
		return harnessv2.CapabilitiesResponse{
			Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: harnessv2.ACPProfileV1,
			RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
			AdapterDigests: map[string]string{profile.ProviderKind: profile.AdapterDigests[profile.ProviderKind]},
			Limits:         limits,
			Provider: harnessv2.ProviderCapabilities{
				ProviderKinds: []string{profile.ProviderKind}, Models: []string{profile.Model},
				SupportsCancel: true, SupportsPermissions: true, SupportsTools: true,
			},
			WorkspaceGovernance:               governance,
			SupportsDrain:                     true,
			SupportsPublicationFinalization:   true,
			SupportsAgentSessionConfiguration: false,
		}
	}
	baseline := base()
	expected, err := harnessv2.CanonicalValue(&baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateFrozenExternalRuntimeCapabilities(expected, &baseline); err != nil {
		t.Fatalf("unchanged capability envelope rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*harnessv2.CapabilitiesResponse)
	}{
		{name: "adapter digests", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.AdapterDigests[profile.ProviderKind] = testControllerDigest("adapter-drift")
		}},
		{name: "limits", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.Limits.MaxTerminalResultBytes--
		}},
		{name: "provider", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.Provider.SupportsImages = true
		}},
		{name: "governance", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.WorkspaceGovernance.CancellationSettlement = false
		}},
		{name: "supports drain", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.SupportsDrain = false
		}},
		{name: "supports publication finalization", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.SupportsPublicationFinalization = false
		}},
		{name: "profile schema", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.ProfileDigestSchemaVersion++
		}},
		{name: "supports Agent session configuration", mutate: func(value *harnessv2.CapabilitiesResponse) {
			value.SupportsAgentSessionConfiguration = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := base()
			test.mutate(&current)
			if err := validateFrozenExternalRuntimeCapabilities(expected, &current); err == nil {
				t.Fatal("drifted capability envelope was accepted")
			}
		})
	}
}
