package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestProductionBrokerGatewayTaskPass(t *testing.T) {
	issued, called := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/task-passes":
			issued++
			require.Equal(t, "Bearer controller-target", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"pass": "ephemeral-pass", "expiresAt": time.Now().Add(time.Minute)})
		case "/v1/task-tools/lookup":
			called++
			require.Equal(t, "Bearer ephemeral-pass", r.Header.Get("Authorization"))
			require.NotEmpty(t, r.Header.Get("Idempotency-Key"))
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
	f := newExternalACPDispatchFixtureWithPolicy(t, "gateway-runtime", policy, tool)
	f.dispatcher.Sessions.gatewayEvents = f.persistence
	require.NoError(t, gatewayv1alpha1.AddToScheme(f.client.Scheme()))
	require.NoError(t, f.client.Create(f.ctx, &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid", Generation: 1}, Spec: gatewayv1alpha1.GatewaySpec{Adapter: gatewayv1alpha1.GatewayAdapterLocation{Endpoint: server.URL}}}))
	s := gatewayruntime.NewService(f.client, f.persistence, f.persistence, f.persistence, gatewayruntime.DefaultConfig())
	s.Config.AllowInsecureLoopback = true
	s.HTTPClient = server.Client()
	s.TaskPassCredentials = map[string]gatewayruntime.TaskPassCredential{"gateway-uid": {Endpoint: server.URL, Token: "controller-target"}}
	f.reconciler.GatewayService = s
	held := holdExternalPolicyPrompt(t, f)
	_, template, _ := acpReplyPlanningFixture(t, true)
	task, taskDone := startExternalPolicyTask(t, f, held, "gateway-root", false, func(task *corev1alpha1.Task) {
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
	})
	prompt := receiveAcceptedPolicyPrompt(t, f, task, held)
	request := externalPolicyMCPCall(t, prompt, "lookup")
	broker, err := NewProductionACPMCPBroker(ACPMCPBrokerDependencies{Reader: f.client, Epochs: f.epochs, ControlStore: f.controlStore, AgentExecutionSnapshots: f.persistence, ExecutionEvents: f.persistence, PromptLeases: f.dispatcher.PromptLeases, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: server.Client(), GatewayService: s})
	require.NoError(t, err)
	response := receiveExternalPolicyMCPResponse(t, f.ctx, startExternalPolicyMCPCall(t, f, broker, request))
	require.Equal(t, 200, response.Code, response.Body.String())
	require.Equal(t, 1, issued)
	require.Equal(t, 1, called)
	assertExternalPolicyEffect(t, f, request, store.ExternalEffectSucceeded)
	replay := receiveExternalPolicyMCPResponse(t, f.ctx, startExternalPolicyMCPCall(t, f, broker, request))
	require.Equal(t, 200, replay.Code)
	require.Equal(t, 1, issued)
	require.Equal(t, 1, called)
	finishExternalPolicyTask(t, f, task, held, taskDone, "safe result", false)
}
