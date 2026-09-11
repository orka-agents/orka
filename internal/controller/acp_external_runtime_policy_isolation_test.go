package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPExternalConcurrentRegisteredPoliciesRemainIsolated(t *testing.T) {
	const readName, writeName = "isolated-read", "isolated-write"
	type observedCall struct {
		tool, operation, arguments string
	}
	entered := make(chan observedCall, 2)
	release := make(chan struct{})
	releaseCalls := sync.OnceFunc(func() { close(release) })
	var reads, writes atomic.Int32
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read synthetic Tool arguments: %v", err)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch name {
		case readName:
			reads.Add(1)
		case writeName:
			writes.Add(1)
		default:
			t.Errorf("unexpected Tool destination %q", name)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		entered <- observedCall{name, r.Header.Get("Idempotency-Key"), string(body)}
		select {
		case <-release:
			writeDispatcherJSON(w, map[string]string{"tool": name})
		case <-r.Context().Done():
		}
	}))
	defer gate.Close()
	defer releaseCalls()
	readTool := testExternalACPCustomTool(readName)
	readTool.Spec.HTTP.URL = gate.URL + "/" + readName
	writeTool := testExternalACPCustomTool(writeName)
	writeTool.Spec.BrokeredToolClass = corev1alpha1.AgentRuntimeBrokeredToolClassWrite
	writeTool.Spec.HTTP.URL = gate.URL + "/" + writeName
	readPolicy := testAgentRuntimeMCPPolicy()
	readPolicy.AllowedTools = []string{readName}
	reader := newExternalACPDispatchFixtureWithOptions(t, "reader-runtime", readPolicy,
		externalACPDispatchFixtureOptions{contextTimeout: time.Minute}, readTool, writeTool)
	writePolicy := testAgentRuntimeMCPPolicy()
	writePolicy.AllowedTools = []string{readName, writeName}
	writer := addExternalPolicyRuntime(t, reader, "writer-runtime", writePolicy)
	if reader.reconciler != writer.reconciler || reader.persistence != writer.persistence ||
		reader.dispatcher != writer.dispatcher || reader.epochs != writer.epochs {
		t.Fatal("runtime policies are not using one controller and durable store")
	}
	readerPrompt := holdExternalPolicyPrompt(t, reader)
	writerPrompt := holdExternalPolicyPrompt(t, writer)
	readTask, readDone := startExternalPolicyTask(t, reader, readerPrompt, "reader", true)
	writeTask, writeDone := startExternalPolicyTask(t, writer, writerPrompt, "writer", true)
	readPrompt := receiveAcceptedPolicyPrompt(t, reader, readTask, readerPrompt)
	writePrompt := receiveAcceptedPolicyPrompt(t, writer, writeTask, writerPrompt)
	assertDistinctExternalPolicyPrompts(t, readPrompt, writePrompt)
	for _, pair := range []struct {
		fixture *externalACPDispatchFixture
		prompt  harnessv2.StartPromptRequest
	}{{reader, readPrompt}, {writer, writePrompt}} {
		assertExternalPolicyConfiguration(t, pair.fixture, pair.prompt)
	}
	broker := newExternalPolicyBroker(t, reader)
	readCall := externalPolicyMCPCall(t, readPrompt, readName)
	writeCall := externalPolicyMCPCall(t, writePrompt, writeName)
	readResponse := startExternalPolicyMCPCall(t, reader, broker, readCall)
	writeResponse := startExternalPolicyMCPCall(t, writer, broker, writeCall)
	seen := map[string]observedCall{}
	for range 2 {
		select {
		case call := <-entered:
			seen[call.tool] = call
		case <-reader.ctx.Done():
			t.Fatal("both original Tool requests did not overlap")
		}
	}
	if len(seen) != 2 || reads.Load() != 1 || writes.Load() != 1 ||
		seen[readName].operation != string(readCall.Metadata.OperationID) ||
		seen[writeName].operation != string(writeCall.Metadata.OperationID) ||
		seen[readName].arguments != `{"probe":true}` || seen[writeName].arguments != `{"probe":true}` {
		t.Fatalf("overlapping Tool requests = %#v, reads=%d writes=%d", seen, reads.Load(), writes.Load())
	}
	// The two permitted Tool HTTP requests stay blocked while the same
	// reconciler rejects both a narrower and a broader external Task policy.
	for _, test := range []struct {
		name    string
		fixture *externalACPDispatchFixture
		allowed []string
	}{{"narrower-on-writer", writer, []string{readName}}, {"broader-on-reader", reader, []string{readName, writeName}}} {
		assertExternalPolicyAdmissionDenied(t, test.fixture, test.name, test.allowed)
	}
	if reader.createCalls.Load() != 1 || writer.createCalls.Load() != 1 ||
		readerPrompt.calls.Load() != 1 || writerPrompt.calls.Load() != 1 || reads.Load() != 1 || writes.Load() != 1 {
		t.Fatal("policy denial changed permitted demand or executed another Tool")
	}
	// Each prompt receives only its own real broker result. The runtime
	// fixture supplies deterministic terminal events, with no model involved.
	releaseCalls()
	for _, pair := range []struct {
		response <-chan *httptest.ResponseRecorder
		prompt   *heldExternalPolicyPrompt
		fixture  *externalACPDispatchFixture
		task     *corev1alpha1.Task
		done     <-chan struct{}
		tool     string
	}{{readResponse, readerPrompt, reader, readTask, readDone, readName}, {writeResponse, writerPrompt, writer, writeTask, writeDone, writeName}} {
		response := receiveExternalPolicyMCPResponse(t, reader.ctx, pair.response)
		want := fmt.Sprintf(`{"tool":%q}`, pair.tool)
		assertExternalPolicyMCPResult(t, response, want)
		finishExternalPolicyTask(t, pair.fixture, pair.task, pair.prompt, pair.done, want, false)
		assertExternalPolicyTranscript(t, reader, pair.task, want)
	}
}

func assertExternalPolicyTranscript(t *testing.T, f *externalACPDispatchFixture, task *corev1alpha1.Task, want string) {
	t.Helper()
	transcript, err := f.persistence.LoadTranscript(f.ctx, defaultNS, task.Spec.SessionRef.Name, 50)
	if err != nil || len(transcript) != 2 || transcript[1].Content != want {
		t.Fatalf("original Session transcript = %#v, err=%v", transcript, err)
	}
}

func assertDistinctExternalPolicyPrompts(t *testing.T, reader, writer harnessv2.StartPromptRequest) {
	t.Helper()
	if reader.Metadata.TaskUID == writer.Metadata.TaskUID ||
		reader.Metadata.PromptID == writer.Metadata.PromptID ||
		reader.Metadata.Fence.RuntimeSessionUID == writer.Metadata.Fence.RuntimeSessionUID ||
		reader.Metadata.Fence.RuntimeInstanceID == writer.Metadata.Fence.RuntimeInstanceID ||
		reader.Metadata.Fence.RuntimeProfileDigest == writer.Metadata.Fence.RuntimeProfileDigest ||
		reader.Metadata.Fence.ControllerEpoch != writer.Metadata.Fence.ControllerEpoch {
		t.Fatal("concurrent Tasks did not retain distinct session/runtime/profile identities at the same epoch")
	}
}

func assertExternalPolicyConfiguration(t *testing.T, f *externalACPDispatchFixture, prompt harnessv2.StartPromptRequest) {
	t.Helper()
	created := <-f.createRequests
	if created.AgentConfiguration != nil ||
		!slices.Equal(created.MCPConfiguration.ToolPolicy.AllowedToolNames, f.mcpPolicy.AllowedTools) ||
		!slices.Equal(prompt.MCPAuthorization.ToolPolicy.AllowedToolNames, f.mcpPolicy.AllowedTools) ||
		created.MCPConfiguration.ToolPolicyDigest != f.runtime.Spec.Capabilities.Profile.ToolPolicyDigest ||
		created.Metadata.Fence.RuntimeSessionUID != prompt.Metadata.Fence.RuntimeSessionUID {
		t.Fatal("registered policy or original Session was replaced during prompt admission")
	}
}

func assertExternalPolicyAdmissionDenied(t *testing.T, f *externalACPDispatchFixture, name string, allowed []string) {
	t.Helper()
	denied := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: name, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: f.agent.Name},
			Prompt: "must not reach a runtime", AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: allowed},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	if err := f.client.Create(f.ctx, denied); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reconciler.handlePending(f.ctx, denied); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(denied), denied); err != nil {
		t.Fatal(err)
	}
	if denied.Status.Phase != corev1alpha1.TaskPhaseFailed || denied.Status.Execution == nil ||
		denied.Status.Execution.Reason != corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile") ||
		denied.Status.Execution.RuntimeSessionUID != "" || denied.Status.AgentExecutionBinding != nil {
		t.Fatalf("mismatched external Task admitted: %#v", denied.Status)
	}
}

func assertExternalPolicyMCPResult(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	var result harnessv2.MCPBrokerCallResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.IsError || result.Replayed {
		t.Fatalf("permitted MCP call status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.TrimSpace(string(result.Result)) != want {
		t.Fatalf("result = %s, want %s", result.Result, want)
	}
}

// addExternalPolicyRuntime adds only a second runtime, Agent and auth fixture.
// Both fixtures retain the same Kubernetes client, epoch manager, reconciler,
// immutable snapshot store and dispatcher; no second controller is started.
func addExternalPolicyRuntime(t *testing.T, shared *externalACPDispatchFixture, name string, policy corev1alpha1.AgentRuntimeMCPPolicySpec) *externalACPDispatchFixture {
	t.Helper()
	profile, governance, limits := testAgentRuntimeProfileClaimsAndLimits()
	var err error
	profile.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedTools, policy.DisallowedTools, policy.AllowBash)
	if err != nil {
		t.Fatal(err)
	}
	profile.ApprovalPolicyDigest, err = harnessv2.CanonicalMCPApprovalPolicyDigest(agentRuntimeMCPApprovalPolicy(&policy))
	if err != nil {
		t.Fatal(err)
	}
	profile.MCPConfigurationDigest, err = harnessv2.CanonicalMCPConfigurationDigest(policy.AllowedTools)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	f := *shared
	f.createCalls, f.deleteCalls = &atomic.Int32{}, &atomic.Int32{}
	f.createRequests = make(chan harnessv2.CreateRuntimeSessionRequest, 8)
	f.deleteRequests = make(chan harnessv2.DeleteRuntimeSessionRequest, 8)
	f.mcpPolicy = policy
	poolUID, instanceID, bootID := name+"-pool", name+"-instance", name+"-boot"
	server := newDispatcherRuntimeServerForPoolWithOptions(t, profile, digest, poolUID,
		dispatcherRuntimeServerOptions{
			disableAgentSessionConfiguration: true, disablePermissions: true,
			onDelete: func(request harnessv2.DeleteRuntimeSessionRequest) { f.deleteCalls.Add(1); f.deleteRequests <- request },
		}, func(request harnessv2.CreateRuntimeSessionRequest) { f.createCalls.Add(1); f.createRequests <- request })
	t.Cleanup(server.Close)
	statusProxy := newExternalRuntimeStatusProxy(t, server.URL, func(status *harnessv2.StatusResponse) {
		status.Fence.RuntimeInstanceID = harnessv2.RuntimeInstanceID(instanceID)
		status.Fence.SupervisorBootID = harnessv2.SupervisorBootID(bootID)
	})
	f.runtime, _ = testAgentRuntimeAndSecret(t, statusProxy.URL, conformancetest.Config{
		ControllerBearerToken: strings.Repeat("w", 32), OperationCapabilitySecret: []byte(strings.Repeat("x", 32)),
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(instanceID), SupervisorBootID: harnessv2.SupervisorBootID(bootID),
		RuntimePoolUID: harnessv2.RuntimePoolUID(poolUID), Profile: profile, Limits: limits, SupportsDrain: true, WorkspaceGovernance: governance,
	})
	f.runtime.Name, f.runtime.UID = name, types.UID(name+"-uid")
	f.runtime.Spec.Capabilities.MCPPolicy = &policy
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: name + "-auth", UID: types.UID(name + "-auth-uid"),
			Labels:      map[string]string{agentRuntimeAuthUseLabel: "true", agentRuntimeAuthRefNameLabel: name},
			Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: statusProxy.URL}},
		Data: map[string][]byte{"controller-token": []byte(strings.Repeat("w", 32)), "capability-secret": []byte(strings.Repeat("x", 32))},
	}
	f.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name = secret.Name
	f.runtime.Spec.ClientAuth.OperationCapabilitySecretRef.Name = secret.Name
	f.agent = shared.agent.DeepCopy()
	f.agent.Name, f.agent.UID, f.agent.ResourceVersion = name+"-agent", types.UID(name+"-agent-uid"), ""
	f.agent.Spec.Runtime.RuntimeRef.Name = name
	for _, object := range []client.Object{f.runtime, secret, f.agent} {
		if err := shared.client.Create(shared.ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	f.runtime.Status = *shared.runtime.Status.DeepCopy()
	f.runtime.Status.ObservedGeneration = f.runtime.Generation
	f.runtime.Status.ObservedControllerAuthRefResourceVersion = secret.ResourceVersion
	f.runtime.Status.ObservedOperationCapabilityRefResourceVersion = secret.ResourceVersion
	observed := f.runtime.Status.ObservedCapabilities
	observed.RuntimeInstanceID, observed.SupervisorBootID, observed.RuntimePoolUID = instanceID, bootID, poolUID
	observed.RuntimeProfileDigest = string(digest)
	if err := shared.client.Status().Update(shared.ctx, f.runtime); err != nil {
		t.Fatal(err)
	}
	updateExternalACPObservedDescriptorDigest(t, &f)
	return &f
}

type externalPolicyPromptResult struct {
	text   string
	failed bool
}

type heldExternalPolicyPrompt struct {
	requests chan harnessv2.StartPromptRequest
	finish   chan externalPolicyPromptResult
	abort    chan struct{}
	calls    atomic.Int32
}

func holdExternalPolicyPrompt(t *testing.T, f *externalACPDispatchFixture) *heldExternalPolicyPrompt {
	t.Helper()
	held := &heldExternalPolicyPrompt{requests: make(chan harnessv2.StartPromptRequest, 2), finish: make(chan externalPolicyPromptResult, 1), abort: make(chan struct{})}
	target, err := url.Parse(f.runtime.Spec.Deployment.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/prompts/") {
			proxy.ServeHTTP(w, r)
			return
		}
		var request harnessv2.StartPromptRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode held prompt: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		held.calls.Add(1)
		w.Header().Set("Content-Type", harnessv2.NDJSONMediaType)
		encoder, err := harnessv2.NewEventEncoder(w, harnessv2.DefaultEventStreamLimits(), harnessv2.EventExpectationFromMetadata(request.Metadata))
		if err != nil {
			t.Errorf("held event encoder: %v", err)
			return
		}
		identity := func(sequence uint64) harnessv2.EventIdentity {
			return harnessv2.EventIdentity{
				RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID, SupervisorBootID: request.Metadata.Fence.SupervisorBootID,
				RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, RuntimeSessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration,
				TaskUID: request.Metadata.TaskUID, TaskAttempt: request.Metadata.TaskAttempt, PromptID: request.Metadata.PromptID,
				Sequence: sequence, RequestDigest: request.Metadata.RequestDigest, Timestamp: time.Now().UTC(),
			}
		}
		if err := encoder.Encode(harnessv2.Event{Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventAccepted, Identity: identity(1),
			Accepted: &harnessv2.AcceptedEvent{AcceptedAt: time.Now().UTC(), Lease: request.Lease, ACPVersion: harnessv2.ACPProfileV1}}); err != nil {
			t.Errorf("encode held acceptance: %v", err)
			return
		}
		w.(http.Flusher).Flush()
		held.requests <- request
		var result externalPolicyPromptResult
		select {
		case result = <-held.finish:
		case <-held.abort:
			return
		case <-r.Context().Done():
			return
		}
		terminal := harnessv2.Event{Protocol: harnessv2.ProtocolVersion, Identity: identity(2)}
		if result.failed {
			terminal.Type = harnessv2.EventFailed
			terminal.Failed = &harnessv2.FailedEvent{StopReason: harnessv2.ACPStopReasonRefusal, Code: "ToolCallFailed", Message: "synthetic Tool was refused"}
		} else {
			terminal.Type = harnessv2.EventCompleted
			terminal.Completed = &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
				Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: result.text}}}}
		}
		if err := encoder.Encode(terminal); err != nil {
			t.Errorf("encode held terminal: %v", err)
		}
		if err := encoder.Close(); err != nil {
			t.Errorf("close held stream: %v", err)
		}
	}))
	t.Cleanup(func() { close(held.abort); server.Close() })
	// Rebind only fake setup objects, before any Task snapshot is frozen.
	secret := &corev1.Secret{}
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: defaultNS, Name: f.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Annotations[agentRuntimeAuthEndpointAnnotation] = server.URL
	if err := f.client.Update(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	f.runtime.Spec.Deployment.Endpoint = server.URL
	f.runtime.Generation++
	if err := f.client.Update(f.ctx, f.runtime); err != nil {
		t.Fatal(err)
	}
	f.runtime.Status.ObservedGeneration = f.runtime.Generation
	f.runtime.Status.ObservedControllerAuthRefResourceVersion = secret.ResourceVersion
	f.runtime.Status.ObservedOperationCapabilityRefResourceVersion = secret.ResourceVersion
	if err := f.client.Status().Update(f.ctx, f.runtime); err != nil {
		t.Fatal(err)
	}
	return held
}

func startExternalPolicyTask(t *testing.T, f *externalACPDispatchFixture, held *heldExternalPolicyPrompt, name string, namedSession bool) (*corev1alpha1.Task, <-chan struct{}) {
	t.Helper()
	var session *corev1alpha1.SessionReference
	if namedSession {
		session = &corev1alpha1.SessionReference{Name: name + "-session", Create: true, Append: true, MaxMessages: 50}
	}
	queued := f.queueTask(t, name, types.UID(name+"-uid"), name+" prompt", session)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { defer close(done); dispatchQueuedTask(ctx, t, f.dispatcher, queued.DeepCopy()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			return
		default:
		}
		// Join dispatch even when an assertion fails while the prompt is held.
		// Otherwise the fixture database could close before terminal delivery.
		select {
		case held.finish <- externalPolicyPromptResult{failed: true}:
		default:
		}
		// The execution deadline is not a worker-completion signal. The
		// dispatcher can still settle state after its context is canceled.
		<-done
	})
	return queued, done
}

func receiveAcceptedPolicyPrompt(t *testing.T, f *externalACPDispatchFixture, task *corev1alpha1.Task, held *heldExternalPolicyPrompt) harnessv2.StartPromptRequest {
	t.Helper()
	var prompt harnessv2.StartPromptRequest
	select {
	case prompt = <-held.requests:
	case <-f.ctx.Done():
		t.Fatal("runtime did not receive original prompt")
	}
	id, err := promptAttemptIDFromTask(task)
	if err != nil {
		t.Fatal(err)
	}
	for {
		attempt, err := f.controlStore.GetPromptAttempt(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.ExecutionState == store.PromptExecutionAccepted || attempt.ExecutionState == store.PromptExecutionRunning {
			if attempt.Key.TaskUID != string(prompt.Metadata.TaskUID) || attempt.SessionUID != string(prompt.Metadata.Fence.RuntimeSessionUID) ||
				attempt.Key.PromptID != string(prompt.Metadata.PromptID) {
				t.Fatal("accepted prompt does not match durable original Task/Session")
			}
			return prompt
		}
		select {
		case <-time.After(time.Millisecond):
		case <-f.ctx.Done():
			t.Fatalf("prompt acceptance was not persisted: %s", attempt.ExecutionState)
		}
	}
}

func newExternalPolicyBroker(t *testing.T, f *externalACPDispatchFixture) *ACPMCPBroker {
	t.Helper()
	broker, err := NewProductionACPMCPBroker(ACPMCPBrokerDependencies{
		Reader: f.client, Epochs: f.epochs, ControlStore: f.controlStore, AgentExecutionSnapshots: f.persistence,
		KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func externalPolicyMCPCall(t *testing.T, prompt harnessv2.StartPromptRequest, tool string) harnessv2.MCPBrokerCallRequest {
	t.Helper()
	request := harnessv2.MCPBrokerCallRequest{Protocol: harnessv2.ProtocolVersion, Namespace: defaultNS,
		SessionState: harnessv2.RuntimeSessionStatePromptRunning, Metadata: prompt.Metadata, Lease: prompt.Lease,
		Authorization: prompt.MCPAuthorization, Call: harnessv2.MCPToolCall{CallID: string(prompt.Metadata.TaskUID) + "-call", ToolName: tool, Arguments: json.RawMessage(`{"probe":true}`)}}
	request.Metadata.OperationID = harnessv2.OperationID(string(prompt.Metadata.TaskUID) + "-tool-operation")
	var err error
	request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func startExternalPolicyMCPCall(t *testing.T, f *externalACPDispatchFixture, broker *ACPMCPBroker, request harnessv2.MCPBrokerCallRequest) <-chan *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: defaultNS, Name: f.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name}, secret); err != nil {
		t.Fatal(err)
	}
	capability, err := harnessv2.SignOperationCapability(secret.Data["capability-secret"], harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	incoming := httptest.NewRequestWithContext(ctx, http.MethodPost, harnessv2.MCPBrokerCallPath, bytes.NewReader(body))
	incoming.Header.Set("Content-Type", "application/json")
	incoming.Header.Set("Authorization", "Bearer "+string(secret.Data["controller-token"]))
	incoming.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	incoming.Header.Set(harnessv2.MCPBrokerPoolNamespaceHeader, request.Namespace)
	incoming.Header.Set(harnessv2.MCPBrokerPoolUIDHeader, string(request.Metadata.Fence.RuntimePoolUID))
	done := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response := httptest.NewRecorder()
		broker.ServeHTTP(response, incoming)
		done <- response
	}()
	t.Cleanup(func() {
		cancel()
		// Effect settlement can outlive request cancellation. Join it before
		// the shared store and epoch manager are torn down.
		<-finished
	})
	return done
}

func receiveExternalPolicyMCPResponse(t *testing.T, ctx context.Context, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		t.Fatal("MCP call did not finish")
		return nil
	}
}

func finishExternalPolicyTask(t *testing.T, f *externalACPDispatchFixture, task *corev1alpha1.Task, held *heldExternalPolicyPrompt, done <-chan struct{}, result string, failed bool) {
	t.Helper()
	held.finish <- externalPolicyPromptResult{text: result, failed: failed}
	select {
	case <-done:
	case <-f.ctx.Done():
		t.Fatal("original Task did not settle")
	}
	current := &corev1alpha1.Task{}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	wantPhase := corev1alpha1.TaskPhaseSucceeded
	if failed {
		wantPhase = corev1alpha1.TaskPhaseFailed
	}
	if current.UID != task.UID || current.Status.Phase != wantPhase || current.Status.Execution.PromptID != task.Status.Execution.PromptID {
		t.Fatalf("original Task terminal status = %#v", current.Status)
	}
	if !failed {
		stored, err := f.persistence.GetResult(f.ctx, task.Namespace, task.Name)
		if err != nil || string(stored) != result {
			t.Fatalf("original Task result = %q, err=%v", stored, err)
		}
	}
}
