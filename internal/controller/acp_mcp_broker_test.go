package controller

import (
	"k8s.io/utils/ptr"

	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storesqlite "github.com/orka-agents/orka/internal/store/sqlite"
)

var apiextensionsJSONForMCPTest = apiextensionsv1.JSON{Raw: json.RawMessage(`{"type":"object"}`)}

func TestRegistryACPMCPToolExecutorReusesCustomToolExecutorWithIdempotency(t *testing.T) {
	request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectConsequential)
	request.Call.ToolName = "custom_tool"
	request.Call.Arguments = json.RawMessage(`{"value":"x"}`)
	request.Metadata.OperationID = "mcp-custom-operation"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != string(request.Metadata.OperationID) {
			t.Fatalf("Idempotency-Key = %q", r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["value"] != "x" {
			t.Fatalf("custom tool body = %#v err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "custom_tool", Namespace: request.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description: "custom write", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			Parameters: &apiextensionsJSONForMCPTest,
			HTTP:       &corev1alpha1.HTTPExecution{URL: upstream.URL, Method: http.MethodPost},
		},
	}
	descriptor, err := customACPMCPToolDescriptor(tool)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool).Build()
	executor := RegistryACPMCPToolExecutor{
		Reader: reader, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: upstream.Client(),
	}
	result, err := executor.ExecuteACPMCPTool(context.Background(), request, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("custom tool result = %s", result)
	}

	changed := tool.DeepCopy()
	changed.Spec.Description = "changed after authorization"
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(changed).Build()
	executor.Reader = reader
	if _, err := executor.ExecuteACPMCPTool(context.Background(), request, descriptor); err == nil ||
		!strings.Contains(err.Error(), "changed after prompt authorization") {
		t.Fatalf("descriptor drift error = %v", err)
	}
}

func TestACPMCPBrokerConsequentialCallUsesDurableReplay(t *testing.T) {
	effects, fence := newMCPBrokerControlStore(t)
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectConsequential)
	bearer := strings.Repeat("b", 32)
	capability := []byte(strings.Repeat("c", 32))
	var executions atomic.Int32
	var authorizations atomic.Int32
	broker := &ACPMCPBroker{
		Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
			return ACPMCPBrokerCredentials{
				ControllerBearerToken: bearer, CapabilitySecret: capability,
				ExpectedFence: got.Metadata.Fence, RuntimeProfile: profile, ControllerFence: fence,
			}, nil
		}),
		Prompts: ACPMCPPromptAuthorizerFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) error {
			authorizations.Add(1)
			if got.Metadata.TaskUID != request.Metadata.TaskUID || got.Metadata.PromptID != request.Metadata.PromptID ||
				got.Lease.Generation != request.Lease.Generation {
				t.Fatalf("prompt authorizer received wrong identity: %#v", got)
			}
			return nil
		}),
		Executor: ACPMCPToolExecutorFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			executions.Add(1)
			if descriptor.Name != "mutate" || got.Call.CallID != "call-1" {
				t.Fatalf("executor received wrong call: descriptor=%#v call=%#v", descriptor, got.Call)
			}
			return json.RawMessage(`{"changed":true}`), nil
		}),
		Effects: effects,
	}

	first := performMCPBrokerCall(t, broker, request, bearer, capability)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}
	second := performMCPBrokerCall(t, broker, request, bearer, capability)
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d body=%s", second.Code, second.Body.String())
	}
	if executions.Load() != 1 {
		t.Fatalf("executor calls = %d, want one durable consequential execution", executions.Load())
	}
	if authorizations.Load() != 3 {
		t.Fatalf("prompt authorizer calls = %d, want pre-call plus post-execution authorization", authorizations.Load())
	}
	var response harnessv2.MCPBrokerCallResponse
	if err := json.Unmarshal(second.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if string(response.Result) != `{"changed":true}` || !response.Replayed {
		t.Fatalf("replayed response = %#v", response)
	}
}

func TestACPMCPBrokerConsequentialConcurrentDuplicateDoesNotDoubleExecute(t *testing.T) {
	effects, fence := newMCPBrokerControlStore(t)
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectConsequential)
	bearer := strings.Repeat("b", 32)
	capability := []byte(strings.Repeat("c", 32))
	entered := make(chan struct{})
	release := make(chan struct{})
	var executions atomic.Int32
	broker := &ACPMCPBroker{
		Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
			return ACPMCPBrokerCredentials{
				ControllerBearerToken: bearer, CapabilitySecret: capability,
				ExpectedFence: got.Metadata.Fence, RuntimeProfile: profile, ControllerFence: fence,
			}, nil
		}),
		Prompts: ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
		Executor: ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			if executions.Add(1) == 1 {
				close(entered)
			}
			<-release
			return json.RawMessage(`{"changed":true}`), nil
		}),
		Effects: effects,
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- performMCPBrokerCall(t, broker, request, bearer, capability) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first consequential execution did not start")
	}
	duplicate := performMCPBrokerCall(t, broker, request, bearer, capability)
	if duplicate.Code == http.StatusOK {
		t.Fatalf("concurrent duplicate unexpectedly succeeded: %s", duplicate.Body.String())
	}
	if executions.Load() != 1 {
		t.Fatalf("concurrent executor calls = %d, want 1", executions.Load())
	}
	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first consequential status = %d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first consequential execution did not finish")
	}
	replay := performMCPBrokerCall(t, broker, request, bearer, capability)
	if replay.Code != http.StatusOK || executions.Load() != 1 {
		t.Fatalf("committed replay status=%d calls=%d body=%s", replay.Code, executions.Load(), replay.Body.String())
	}
}

func TestACPMCPBrokerReadOnlyCallExecutesWithoutLedgerReplay(t *testing.T) {
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
			}, nil
		}),
		Prompts: ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
		Executor: ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			executions.Add(1)
			return json.RawMessage(`{"value":"fresh"}`), nil
		}),
		Effects: effects,
	}
	for range 2 {
		response := performMCPBrokerCall(t, broker, request, bearer, capability)
		if response.Code != http.StatusOK {
			t.Fatalf("read-only status = %d body=%s", response.Code, response.Body.String())
		}
	}
	if executions.Load() != 2 {
		t.Fatalf("read-only executor calls = %d, want 2", executions.Load())
	}
}

func TestACPMCPBrokerRejectsAuthFenceProfileAndInactivePrompt(t *testing.T) {
	effects, fence := newMCPBrokerControlStore(t)
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	bearer := strings.Repeat("b", 32)
	capability := []byte(strings.Repeat("c", 32))
	promptAllowed := true
	broker := &ACPMCPBroker{
		Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
			expected := got.Metadata.Fence
			return ACPMCPBrokerCredentials{
				ControllerBearerToken: bearer, CapabilitySecret: capability,
				ExpectedFence: expected, RuntimeProfile: profile, ControllerFence: fence,
			}, nil
		}),
		Prompts: ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error {
			if !promptAllowed {
				return store.ErrConflict
			}
			return nil
		}),
		Executor: ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		}),
		Effects: effects,
	}

	wrongBearer := performMCPBrokerCall(t, broker, request, strings.Repeat("x", 32), capability)
	if wrongBearer.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer status = %d", wrongBearer.Code)
	}
	wrongCapability := performMCPBrokerCall(t, broker, request, bearer, []byte(strings.Repeat("x", 32)))
	if wrongCapability.Code != http.StatusForbidden {
		t.Fatalf("wrong capability status = %d", wrongCapability.Code)
	}
	promptAllowed = false
	inactive := performMCPBrokerCall(t, broker, request, bearer, capability)
	if inactive.Code != http.StatusForbidden {
		t.Fatalf("inactive prompt status = %d", inactive.Code)
	}

	promptAllowed = true
	staleProfile := profile
	staleProfile.ToolPolicyDigest = testControllerMCPDigest("stale")
	broker.Credentials = ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
		return ACPMCPBrokerCredentials{
			ControllerBearerToken: bearer, CapabilitySecret: capability,
			ExpectedFence: got.Metadata.Fence, RuntimeProfile: staleProfile, ControllerFence: fence,
		}, nil
	})
	profileResponse := performMCPBrokerCall(t, broker, request, bearer, capability)
	if profileResponse.Code != http.StatusGone {
		t.Fatalf("stale profile status = %d", profileResponse.Code)
	}
}

func TestKubernetesACPMCPBrokerCredentialResolverChecksTaskSessionGeneration(t *testing.T) {
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: request.Namespace, UID: "pool-uid", Generation: 2},
		Spec: corev1alpha1.RuntimePoolSpec{
			RuntimeNamespace: "runtime-system",
			Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
				Profile: RuntimePoolProfileFromPlan(ACPRuntimePlan{Profile: profile, Digest: request.Metadata.Fence.RuntimeProfileDigest}),
			},
		},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "runtime-system", PodName: "runtime-pod", PodAddress: "10.0.0.2", PodUID: "pod-uid",
			BootID: string(request.Metadata.Fence.SupervisorBootID), RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID),
			ControllerEpoch: 1, ProfileDigest: string(request.Metadata.Fence.RuntimeProfileDigest),
			ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10), ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2,
		}},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: request.Namespace, UID: "task-uid"},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-1",
			RuntimePoolName: pool.Name, RuntimePoolUID: string(pool.UID),
			RuntimeInstanceID:        string(request.Metadata.Fence.RuntimeInstanceID),
			RuntimeSessionUID:        string(request.Authorization.RuntimeSessionUID),
			RuntimeSessionGeneration: int64(request.Authorization.SessionGeneration), ControllerEpoch: 1,
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-auth", Namespace: "runtime-system", Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{
			runtimePoolControllerTokenKey:  []byte(strings.Repeat("b", 32)),
			runtimePoolCapabilitySecretKey: []byte(strings.Repeat("c", 32)),
		},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, task, secret).Build()
	epochs := NewControllerEpochManager(nil, "controller-a")
	epochs.current = &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller-a"}
	close(epochs.ready)
	resolver := KubernetesACPMCPBrokerCredentialResolver{Reader: reader, Epochs: epochs}
	credentials, err := resolver.ResolveACPMCPBrokerCredentials(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.ExpectedFence.RuntimeSessionGeneration != request.Authorization.SessionGeneration ||
		credentials.ExpectedFence.RuntimeSessionUID != request.Authorization.RuntimeSessionUID {
		t.Fatalf("resolved fence = %#v", credentials.ExpectedFence)
	}

	task.Status.Execution.RuntimeSessionGeneration++
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, task, secret).Build()
	resolver.Reader = reader
	if _, err := resolver.ResolveACPMCPBrokerCredentials(context.Background(), request); err == nil {
		t.Fatal("mismatched Task RuntimeSession generation remained authorized")
	}
}

func TestKubernetesACPMCPBrokerCredentialResolverSupportsExternalRuntime(t *testing.T) {
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	request.Metadata.Fence.RuntimeInstanceID = "external-instance"
	request.Metadata.Fence.SupervisorBootID = "external-boot"
	request.Metadata.Fence.RuntimePoolUID = "external-pool"
	request.Metadata.Fence.RuntimePoolGeneration = 5
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.Fence.RuntimeProfileDigest = profileDigest
	request.Authorization.RuntimeSessionUID = request.Metadata.Fence.RuntimeSessionUID
	request.Authorization.SessionGeneration = request.Metadata.Fence.RuntimeSessionGeneration
	digest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.RequestDigest = digest
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	profileSpec := corev1alpha1.AgentRuntimeProfileSpec{
		Digest: string(request.Metadata.Fence.RuntimeProfileDigest), DigestSchemaVersion: int32(harnessv2.ProfileDigestSchemaVersion),
		ACPProfile: profile.ACPProfile, AdapterName: "adapter", AdapterDigest: profile.AdapterDigests["adapter"],
		ProviderKind: profile.ProviderKind, Model: profile.Model,
		AgentConfigurationDigest: profile.AgentConfigurationDigest, ToolPolicyDigest: profile.ToolPolicyDigest,
		ApprovalPolicyDigest: profile.ApprovalPolicyDigest, MCPConfigurationDigest: profile.MCPConfigurationDigest,
		WorkspaceIntent:     corev1alpha1.WorkspaceIntent(profile.WorkspaceIntent),
		ProxyCredentialRole: profile.ProxyCredentialRole, ProxyCredentialScope: profile.ProxyCredentialScope,
		ResourceClass: profile.ResourceClass,
	}
	governance := corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities{
		Mode:                     corev1alpha1.AgentRuntimeWorkspaceGovernanceStrict,
		OrkaOwnedWorkspaceDeltas: true, PromptScopedBrokerAuthorization: true, NoDirectSCMPublication: true,
		OrkaOwnedCleanRoomPublication: true, ExactInstanceFencing: true,
		DuplicateSafeMutations: true, CancellationSettlement: true,
	}
	external := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: request.Namespace, UID: "external-uid", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV2),
			Deployment:      corev1alpha1.AgentRuntimeDeploymentSpec{Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: "https://runtime.example.invalid"},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
				ControllerBearerTokenSecretRef: &corev1alpha1.AgentRuntimeSecretKeyReference{Name: "external-bearer", Key: "token"},
				OperationCapabilitySecretRef:   &corev1alpha1.AgentRuntimeSecretKeyReference{Name: "external-capability", Key: "secret"},
			},
			Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
				RuntimeInstanceID: "external-instance", Profile: &profileSpec, SupportsDrain: true,
				WorkspaceGovernance: &governance,
			},
		},
		Status: corev1alpha1.AgentRuntimeStatus{
			Ready: true, ObservedGeneration: 1,
			ObservedControllerAuthRefResourceVersion:      "rv-bearer",
			ObservedOperationCapabilityRefResourceVersion: "rv-capability",
			ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ProtocolVersion: string(corev1alpha1.RuntimePoolProtocolHarnessV2), RuntimeInstanceID: "external-instance",
				SupervisorBootID: "external-boot", ControllerEpoch: 1,
				RuntimePoolUID: "external-pool", RuntimePoolGeneration: 5,
				RuntimeProfileDigest: profileSpec.Digest, ProfileDigestSchemaVersion: int32(harnessv2.ProfileDigestSchemaVersion),
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: request.Namespace, UID: "task-uid"},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-1",
			AgentRuntimeName: external.Name, AgentRuntimeUID: string(external.UID),
			RuntimeInstanceID: "external-instance", RuntimeSessionUID: string(request.Authorization.RuntimeSessionUID),
			RuntimeSessionGeneration: int64(request.Authorization.SessionGeneration), ControllerEpoch: 1,
		}},
	}
	bearer := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-bearer", Namespace: request.Namespace, ResourceVersion: "rv-bearer"},
		Data:       map[string][]byte{"token": []byte(strings.Repeat("b", 32))},
	}
	capability := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-capability", Namespace: request.Namespace, ResourceVersion: "rv-capability"},
		Data:       map[string][]byte{"secret": []byte(strings.Repeat("c", 32))},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(external, task, bearer, capability).Build()
	epochs := NewControllerEpochManager(nil, "controller-a")
	epochs.current = &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller-a"}
	close(epochs.ready)
	resolver := KubernetesACPMCPBrokerCredentialResolver{Reader: reader, Epochs: epochs}
	credentials, err := resolver.ResolveACPMCPBrokerCredentials(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.ExpectedFence.RuntimeInstanceID != "external-instance" ||
		credentials.ExpectedFence.RuntimePoolGeneration != 5 || credentials.RuntimeProfile.Model != profile.Model {
		t.Fatalf("external credentials = %#v", credentials)
	}
}

func TestDurableACPMCPPromptAuthorizerRequiresActiveExactAttempt(t *testing.T) {
	request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	attempt := &store.PromptAttempt{
		Key: store.PromptAttemptKey{
			Namespace: request.Namespace, TaskUID: string(request.Metadata.TaskUID),
			Attempt: int64(request.Metadata.TaskAttempt), PromptID: string(request.Metadata.PromptID),
		},
		SessionUID: string(request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID),
		ControllerEpoch: int64(request.Metadata.Fence.ControllerEpoch), ExecutionState: store.PromptExecutionRunning,
	}
	attempt.ID, _ = attempt.Key.CanonicalID()
	authorizer := DurableACPMCPPromptAuthorizer{Attempts: staticPromptAttemptStore{attempt: attempt}}
	if err := authorizer.AuthorizeACPMCPPrompt(context.Background(), request); err != nil {
		t.Fatalf("AuthorizeACPMCPPrompt() error = %v", err)
	}
	settling := *attempt
	settling.ExecutionState = store.PromptExecutionSettling
	authorizer.Attempts = staticPromptAttemptStore{attempt: &settling}
	if err := authorizer.AuthorizeACPMCPPrompt(context.Background(), request); err == nil {
		t.Fatal("settling prompt remained authorized")
	}
	wrongSession := *attempt
	wrongSession.SessionUID = "other-session"
	authorizer.Attempts = staticPromptAttemptStore{attempt: &wrongSession}
	if err := authorizer.AuthorizeACPMCPPrompt(context.Background(), request); err == nil {
		t.Fatal("mismatched RuntimeSession remained authorized")
	}
}

type staticPromptAttemptStore struct {
	attempt *store.PromptAttempt
}

func (s staticPromptAttemptStore) CreatePromptAttempt(context.Context, *store.PromptAttempt, store.ControllerEpochFence) (*store.PromptAttempt, error) {
	return nil, store.ErrConflict
}
func (s staticPromptAttemptStore) GetPromptAttempt(context.Context, string) (*store.PromptAttempt, error) {
	if s.attempt == nil {
		return nil, store.ErrNotFound
	}
	copy := *s.attempt
	return &copy, nil
}
func (s staticPromptAttemptStore) TransitionPromptAttemptExecution(context.Context, store.PromptAttemptExecutionTransition) (*store.PromptAttempt, error) {
	return nil, store.ErrConflict
}
func (s staticPromptAttemptStore) RecoverPromptAttemptPreSubmission(context.Context, store.PromptAttemptPreSubmissionRecovery) (*store.PromptAttempt, error) {
	return nil, store.ErrConflict
}
func (s staticPromptAttemptStore) TransitionPromptAttemptDelivery(context.Context, store.PromptAttemptDeliveryTransition) (*store.PromptAttempt, error) {
	return nil, store.ErrConflict
}

func testMCPBrokerRequest(t *testing.T, effect harnessv2.MCPToolEffect) (harnessv2.MCPBrokerCallRequest, harnessv2.RuntimeProfile) {
	t.Helper()
	now := time.Now().UTC()
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot", ControllerEpoch: 1,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 2,
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(testControllerMCPDigest("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	descriptor := harnessv2.MCPToolDescriptor{
		Name: "mutate", Description: "test broker tool", InputSchema: json.RawMessage(`{"type":"object"}`),
		Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: effect,
	}
	descriptorDigest, err := harnessv2.CanonicalMCPToolDescriptorDigest([]harnessv2.MCPToolDescriptor{descriptor})
	if err != nil {
		t.Fatal(err)
	}
	toolPolicy := harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{"mutate"}, AllowBash: true,
		Tools: []harnessv2.MCPToolDescriptor{descriptor}, DescriptorDigest: descriptorDigest,
	}
	approvalPolicy := harnessv2.MCPApprovalPolicy{}
	toolDigest, _ := harnessv2.CanonicalRuntimeToolPolicyDigest(toolPolicy.AllowedToolNames, toolPolicy.DisallowedToolNames, toolPolicy.AllowBash)
	approvalDigest, _ := harnessv2.CanonicalMCPApprovalPolicyDigest(approvalPolicy)
	mcpDigest, _ := harnessv2.CanonicalMCPConfigurationDigest(toolPolicy.AllowedToolNames)
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{"adapter": testControllerMCPDigest("adapter")},
		ProviderKind: "codex", Model: "model", AgentConfigurationDigest: testControllerMCPDigest("agent"),
		ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest,
		MCPConfigurationDigest: mcpDigest, WorkspaceIntent: harnessv2.WorkspaceIntentRead,
		ProxyCredentialRole: "provider", ProxyCredentialScope: "model:model", ResourceClass: "standard",
	}
	lease := harnessv2.PromptLease{Generation: 4, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(2 * time.Minute)}
	metadata := harnessv2.MutationMetadata{
		Fence: fence, TaskUID: "task-uid", TaskAttempt: 1, PromptID: "prompt-1", OperationID: "mcp-operation-1",
		RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion, ExpiresAt: now.Add(30 * time.Second),
	}
	authorization := harnessv2.PromptMCPAuthorization{
		RuntimeSessionUID: fence.RuntimeSessionUID, SessionGeneration: fence.RuntimeSessionGeneration,
		TaskUID: metadata.TaskUID, TaskAttempt: metadata.TaskAttempt, PromptID: metadata.PromptID,
		LeaseGeneration: lease.Generation, ToolPolicyDigest: profile.ToolPolicyDigest,
		ApprovalPolicyDigest: profile.ApprovalPolicyDigest, MCPConfigurationDigest: profile.MCPConfigurationDigest,
		ToolPolicy: toolPolicy, ApprovalPolicy: approvalPolicy, ExpiresAt: now.Add(time.Minute),
	}
	request := harnessv2.MCPBrokerCallRequest{
		Protocol: harnessv2.ProtocolVersion, Namespace: "default", SessionState: harnessv2.RuntimeSessionStatePromptRunning,
		Metadata: metadata, Lease: lease, Authorization: authorization,
		Call: harnessv2.MCPToolCall{CallID: "call-1", ToolName: "mutate", Arguments: json.RawMessage(`{"value":"x"}`)},
	}
	digest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.RequestDigest = digest
	return request, profile
}

func performMCPBrokerCall(
	t *testing.T,
	broker http.Handler,
	request harnessv2.MCPBrokerCallRequest,
	bearer string,
	capabilitySecret []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := harnessv2.SignOperationCapability(capabilitySecret, harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, harnessv2.MCPBrokerCallPath, bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+bearer)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	response := httptest.NewRecorder()
	broker.ServeHTTP(response, httpRequest)
	return response
}

func newMCPBrokerControlStore(t *testing.T) (*storesqlite.Store, store.ControllerEpochFence) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := storesqlite.NewDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	control := storesqlite.NewStore(db, path)
	epoch, err := control.CompareAndSwapControllerEpoch(context.Background(), store.ControllerEpochCAS{
		ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1, HolderID: "controller-a",
		RequestDigest: testControllerMCPDigest("epoch"), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return control, store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
}

func testControllerMCPDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
