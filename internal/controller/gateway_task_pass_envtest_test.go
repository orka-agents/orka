package controller

import (
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Exercise server-assigned UIDs, generations, snapshot status CAS and reads,
// rather than accepting client-written identity fields from a fake API.
func gatewayPassAPIFixture(t *testing.T, endpoint string) (*TaskReconciler, *corev1alpha1.Task, *corev1alpha1.Agent) {
	t.Helper()
	api := newArchivedSessionAPIClient(t)
	require.NoError(t, gatewayv1alpha1.AddToScheme(api.Scheme()))
	ns := &corev1.Namespace{}
	require.NoError(t, api.Get(t.Context(), client.ObjectKey{Name: "default"}, ns))
	gateway := &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default"}, Spec: gatewayv1alpha1.GatewaySpec{GatewayClassName: "chat", Adapter: gatewayv1alpha1.GatewayAdapterLocation{Endpoint: endpoint}, InboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{Name: "inbound", Key: "token"}, OutboundAuthRef: gatewayv1alpha1.GatewayBearerAuthReference{Name: "delivery", Key: "token"}}}
	require.NoError(t, api.Create(t.Context(), gateway))
	_, task, agent := acpReplyPlanningFixture(t, true)
	agent.UID = ""
	agent.Generation = 0
	agent.ResourceVersion = ""
	require.NoError(t, api.Create(t.Context(), agent))
	task.Spec.Timeout = &metav1.Duration{Duration: 10 * time.Minute}
	task.UID = ""
	task.Generation = 0
	task.ResourceVersion = ""
	task.Spec.RequestedBy.Issuer = "gateway.orka.ai/default/" + string(ns.UID) + "/chat/" + string(gateway.UID)
	require.NoError(t, api.Create(t.Context(), task))
	r, db := newBindingTestReconciler(t)
	r.Client = api
	r.APIReader = api
	r.Scheme = api.Scheme()
	r.ACPRuntimeImages.Claude = r.ACPRuntimeImages.Codex
	now := time.Now().UTC()
	event := store.GatewayEvent{ID: "gev-test", Namespace: "default", NamespaceUID: string(ns.UID), GatewayName: "chat", GatewayUID: string(gateway.UID), GatewayGeneration: gateway.Generation, BindingName: "room", AgentName: agent.Name, ExternalEventID: "external", ProtocolVersion: "orka.gateway.v1", EventType: "delegatedText", Metadata: map[string]string{"gatewayGrantRef": "sender-generation-1"}, AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: task.Name, ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	_, _, err := db.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
	require.NoError(t, err)
	_, err = db.ClaimNextGatewayEvent(t.Context(), "default", "dispatch", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, db.MarkGatewayEventTaskCreated(t.Context(), "default", event.ID, task.Name, string(task.UID), "dispatch", now))
	r.GatewayService = gatewayruntime.NewService(api, db, db, db, gatewayruntime.DefaultConfig())
	return r, task, agent
}
