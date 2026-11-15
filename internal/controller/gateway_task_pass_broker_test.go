package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestProductionBrokerGatewayTaskPass(t *testing.T) {
	for _, scenario := range []string{"root", "child", "child-cancel-during-issuance"} {
		t.Run(scenario, func(t *testing.T) { testProductionBrokerGatewayTaskPass(t, scenario) })
	}
}

func testProductionBrokerGatewayTaskPass(t *testing.T, scenario string) {
	child := scenario != "root"
	cancelAncestor := scenario == "child-cancel-during-issuance"
	var mu sync.Mutex
	var issued []childPassRequest
	var called, idempotency []string
	var afterIssue func()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/task-passes":
			var request childPassRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, "Bearer controller-target", r.Header.Get("Authorization"))
			mu.Lock()
			issued = append(issued, request)
			callback := afterIssue
			afterIssue = nil
			mu.Unlock()
			if callback != nil {
				callback()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pass": "fixture-pass-" + request.TaskUID, "expiresAt": time.Now().Add(time.Minute)})
		case "/v1/task-tools/lookup":
			mu.Lock()
			called = append(called, r.Header.Get("Authorization"))
			idempotency = append(idempotency, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	tool := testExternalACPCustomTool("lookup")
	tool.Spec.HTTP.URL = server.URL + "/v1/task-tools/lookup"
	tool.Spec.HTTP.GatewayTaskPass = true
	tool.Spec.BrokeredToolClass = corev1alpha1.AgentRuntimeBrokeredToolClassWrite
	invalid := tool.DeepCopy()
	invalid.Spec.Description = ""
	require.ErrorContains(t, (&ToolReconciler{}).validateTool(t.Context(), invalid), "description")
	policy := testAgentRuntimeMCPPolicy()
	policy.AllowedTools = []string{"lookup"}
	if child {
		policy.ApprovalRequiredTools = []string{"lookup"}
	}
	options := externalACPDispatchFixtureOptions{}
	if child {
		options.supportsBrokeredToolApprovals = true
		options.profileTransform = func(profile *harnessv2.RuntimeProfile) { profile.ProviderKind = "agentkit" }
	}
	f := newExternalACPDispatchFixtureWithOptions(t, "gateway-runtime", policy, options, tool)
	f.dispatcher.Sessions.gatewayEvents = f.persistence
	require.NoError(t, gatewayv1alpha1.AddToScheme(f.client.Scheme()))
	require.NoError(t, f.client.Create(f.ctx, &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid", Generation: 1}, Spec: gatewayv1alpha1.GatewaySpec{Adapter: gatewayv1alpha1.GatewayAdapterLocation{Endpoint: server.URL}}}))
	s := gatewayruntime.NewService(f.client, f.persistence, f.persistence, f.persistence, gatewayruntime.DefaultConfig())
	s.Config.AllowInsecureLoopback = true
	s.HTTPClient = server.Client()
	s.TaskPassCredentials = map[string]gatewayruntime.TaskPassCredential{"gateway-uid": {Endpoint: server.URL, Token: "controller-target"}}
	f.reconciler.GatewayService = s
	f.reconciler.TaskProvenanceProtected = true
	held := holdExternalPolicyPrompt(t, f)
	_, template, _ := acpReplyPlanningFixture(t, true)
	rootInputs := func(task *corev1alpha1.Task) {
		task.Spec.Prompt = ""
		task.Labels = template.Labels
		task.Annotations = template.Annotations
		task.Spec.SessionRef = template.Spec.SessionRef.DeepCopy()
		task.Spec.RequestedBy = template.Spec.RequestedBy.DeepCopy()
		task.Spec.RequestedBy.Issuer = "gateway.orka.ai/default/default-namespace-uid/chat/gateway-uid"
		now := time.Now().UTC()
		event := store.GatewayEvent{ID: "gev-test", Namespace: "default", NamespaceUID: "default-namespace-uid", GatewayName: "chat", GatewayUID: "gateway-uid", GatewayGeneration: 1, BindingName: "room", AgentName: f.agent.Name, ExternalEventID: "external", ProtocolVersion: "orka.gateway.v1", EventType: "delegatedText", Metadata: map[string]string{"gatewayGrantRef": "sender-generation-1"}, AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: task.Name, ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
		_, _, err := f.persistence.AdmitGatewayEvent(f.ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
		require.NoError(t, err)
		_, err = f.persistence.ClaimNextGatewayEvent(f.ctx, "default", "dispatch", now, time.Minute)
		require.NoError(t, err)
		require.NoError(t, f.persistence.MarkGatewayEventTaskCreated(f.ctx, "default", event.ID, task.Name, string(task.UID), "dispatch", now))
	}
	var parent *corev1alpha1.Task
	mutate := rootInputs
	if child {
		parent = f.queueTask(t, "gateway-root", "gateway-root-uid", "", nil, rootInputs)
		parent.Status.Phase = corev1alpha1.TaskPhaseRunning
		require.NoError(t, f.client.Status().Update(f.ctx, parent))
		mutate = func(task *corev1alpha1.Task) {
			task.Spec.RequestedBy = parent.Spec.RequestedBy.DeepCopy()
			task.Annotations = map[string]string{labels.AnnotationParentTaskName: parent.Name, labels.AnnotationParentTaskUID: string(parent.UID), labels.AnnotationParentTaskNamespace: parent.Namespace}
		}
	}
	task, taskDone := startExternalPolicyTask(t, f, held, "gateway-caller", false, mutate)
	prompt := receiveAcceptedPolicyPrompt(t, f, task, held)
	if child {
		require.NoError(t, f.client.Get(f.ctx, client.ObjectKeyFromObject(parent), parent))
		parent.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		parent.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
		parent.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeSucceeded
		parent.Status.Wait = &corev1alpha1.TaskWaitStatus{RequestedAt: metav1.Now(), State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindTasks, Attempt: parent.Status.Execution.Attempt, PromptID: parent.Status.Execution.PromptID, References: []string{task.Name}, ReferenceUIDs: []string{string(task.UID)}, Deadline: metav1.NewTime(time.Now().Add(2 * time.Minute))}
		require.NoError(t, f.client.Status().Update(f.ctx, parent))
		if cancelAncestor {
			afterIssue = func() { require.NoError(t, f.reconciler.requestDelegatedTaskCancellation(f.ctx, parent, "stop")) }
		}
	}
	request := externalPolicyMCPCall(t, prompt, "lookup")
	broker, err := NewProductionACPMCPBroker(ACPMCPBrokerDependencies{TaskProvenanceProtected: true, Reader: f.client, Epochs: f.epochs, ControlStore: f.controlStore, AgentExecutionSnapshots: f.persistence, ExecutionEvents: f.persistence.client, PromptLeases: f.dispatcher.PromptLeases, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: server.Client(), GatewayService: s})
	require.NoError(t, err)
	done := startExternalPolicyMCPCall(t, f, broker, request)
	if child {
		var pending approvals.Approval
		require.Eventually(t, func() bool {
			listed, err := approvals.ListEvents(f.ctx, f.persistence, task.Namespace, task.Name)
			require.NoError(t, err)
			for _, approval := range approvals.Derive(listed, time.Time{}) {
				if approval.Status == approvals.StatusPending {
					pending = approval
					return true
				}
			}
			return false
		}, 5*time.Second, time.Millisecond)
		mu.Lock()
		require.Empty(t, issued, "approval must precede all pass issuance")
		require.Empty(t, called)
		mu.Unlock()
		content, err := json.Marshal(map[string]string{"approvalID": pending.ID, "taskUID": string(task.UID), "actor": "reviewer"})
		require.NoError(t, err)
		_, err = f.persistence.AppendExecutionEvent(f.ctx, &store.ExecutionEvent{Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name, Type: events.ExecutionEventTypeApprovalApproved, ToolCallID: pending.ID, Summary: "reviewer decision", Content: content})
		require.NoError(t, err)
	}
	response := receiveExternalPolicyMCPResponse(t, f.ctx, done)
	require.Equal(t, 200, response.Code, response.Body.String())
	var result harnessv2.MCPBrokerCallResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.False(t, result.Replayed)
	wantIssued := 1
	wantEffect := store.ExternalEffectSucceeded
	if child && !cancelAncestor {
		wantIssued = 2
	}
	if cancelAncestor {
		require.True(t, result.IsError)
		require.Contains(t, string(result.Result), acpApprovalCodeNoEffect)
		wantEffect = store.ExternalEffectFailed
	} else {
		require.False(t, result.IsError)
	}
	assertExternalPolicyEffect(t, f, request, wantEffect)
	mu.Lock()
	require.Len(t, issued, wantIssued)
	if child {
		require.Equal(t, string(parent.UID), issued[0].TaskUID)
		require.Empty(t, issued[0].ParentTaskUID)
		if !cancelAncestor {
			require.Equal(t, string(task.UID), issued[1].TaskUID)
			require.Equal(t, string(parent.UID), issued[1].ParentTaskUID)
		}
	}
	if cancelAncestor {
		require.Empty(t, called)
		require.Empty(t, idempotency)
	} else {
		require.Equal(t, []string{"Bearer fixture-pass-" + string(task.UID)}, called, "only the leaf executes a tool")
		require.Equal(t, []string{string(request.Metadata.OperationID)}, idempotency, "idempotency belongs to the authenticated child operation")
	}
	mu.Unlock()
	replay := receiveExternalPolicyMCPResponse(t, f.ctx, startExternalPolicyMCPCall(t, f, broker, request))
	require.Equal(t, 200, replay.Code, replay.Body.String())
	var replayed harnessv2.MCPBrokerCallResponse
	require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &replayed))
	require.True(t, replayed.Replayed)
	require.JSONEq(t, string(result.Result), string(replayed.Result))
	assertExternalPolicyEffect(t, f, request, wantEffect)
	mu.Lock()
	require.Len(t, issued, wantIssued, "exact replay must not reissue any pass")
	if cancelAncestor {
		require.Empty(t, called)
	} else {
		require.Len(t, called, 1, "replay must not repeat the write")
	}
	mu.Unlock()
	finishExternalPolicyTask(t, f, task, held, taskDone, "safe result", false)
}
