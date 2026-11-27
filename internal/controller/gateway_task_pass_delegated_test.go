package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGatewayTaskPassDelegatedTools(t *testing.T) {
	g := &childPassGateway{bindings: map[string]childPassRequest{}}
	server := httptest.NewTLSServer(http.HandlerFunc(g.serve))
	defer server.Close()
	r, parent, agent := gatewayPassAPIFixture(t, server.URL)
	r.TaskProvenanceProtected = true
	r.GatewayService.HTTPClient = server.Client()
	r.GatewayService.Config.AllowInsecureLoopback = true
	rootGrant, err := r.GatewayService.ResolveTaskGrant(t.Context(), parent)
	require.NoError(t, err)
	r.GatewayService.TaskPassCredentials = map[string]gatewayruntime.TaskPassCredential{rootGrant.GatewayUID: {Endpoint: server.URL, Token: "fixture-controller"}}
	for _, name := range []string{"lookup", "forbidden"} {
		require.NoError(t, r.Create(t.Context(), &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: parent.Namespace}, Spec: corev1alpha1.ToolSpec{Description: name, BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{URL: server.URL + "/v1/task-tools/" + name, GatewayTaskPass: true}}}))
	}
	require.NoError(t, r.Create(t.Context(), &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: parent.Namespace}, Spec: corev1alpha1.ToolSpec{Description: "plain", HTTP: &corev1alpha1.HTTPExecution{URL: "https://example.com/plain"}}}))

	disabled := agent.DeepCopy()
	disabled.ObjectMeta = metav1.ObjectMeta{Name: "disabled-delegation", Namespace: agent.Namespace}
	disabled.Spec.Coordination = &corev1alpha1.CoordinationConfig{DelegatedGatewayTools: []string{"lookup"}}
	err = r.Create(t.Context(), disabled)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.ErrorContains(t, err, "delegatedGatewayTools requires coordination to be enabled")

	childAgent := agent.DeepCopy()
	childAgent.Spec.Runtime.DefaultAllowedTools = []string{"lookup", "forbidden"}
	agent.Spec.Runtime.DefaultAllowedTools = nil
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true, DelegatedGatewayTools: []string{"lookup"}}
	bindPassTask(t, r, parent, agent)
	parentBody := gatewayBoundBody(t, r, parent)
	require.Equal(t, []string{"lookup"}, parentBody.GatewayGrant.Tools)
	_, offered := parentBody.MCPConfiguration.ToolPolicy.Descriptor("lookup")
	require.False(t, offered, "a delegated tool must stay out of the parent's tool policy")
	call := harnessv2.MCPToolCall{CallID: "parent-call", ToolName: "lookup", Arguments: json.RawMessage(`{}`)}
	_, err = call.ValidateAt(harnessv2.PromptMCPAuthorization{ToolPolicy: parentBody.MCPConfiguration.ToolPolicy}, time.Now())
	require.ErrorContains(t, err, `MCP tool "lookup" is not an allowed brokered tool`)

	for _, name := range []string{"plain", "missing"} {
		bad := agent.DeepCopy()
		bad.Spec.Coordination.DelegatedGatewayTools = []string{name}
		_, err = r.freezeGatewayTaskGrant(t.Context(), parent, bad, *parentBody.MCPConfiguration)
		require.True(t, isPermanentACPAgentConfigurationError(err), "%s: %v", name, err)
	}
	plainParent := agent.DeepCopy()
	plainParent.Spec.Coordination.DelegatedGatewayTools = nil
	grant, err := r.freezeGatewayTaskGrant(t.Context(), parent, plainParent, *parentBody.MCPConfiguration)
	require.NoError(t, err)
	require.Empty(t, grant.Tools)
	manual := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "manual-parent", Namespace: parent.Namespace}, Spec: *parent.Spec.DeepCopy()}
	manual.Spec.RequestedBy = nil
	require.NoError(t, r.Create(t.Context(), manual))
	grant, err = r.freezeGatewayTaskGrant(t.Context(), manual, agent, *parentBody.MCPConfiguration)
	require.NoError(t, err)
	require.Nil(t, grant)

	newChild := func(name string, allowed ...string) *corev1alpha1.Task {
		child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: parent.Namespace, Finalizers: []string{labels.TaskFinalizer}, Annotations: map[string]string{
			labels.AnnotationParentTaskName: parent.Name, labels.AnnotationParentTaskNamespace: parent.Namespace, labels.AnnotationParentTaskUID: string(parent.UID),
		}}, Spec: *parent.Spec.DeepCopy()}
		child.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
		child.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: allowed}
		require.NoError(t, r.Create(t.Context(), child))
		bindPassTask(t, r, child, childAgent)
		return child
	}
	lookup := &corev1alpha1.Tool{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: parent.Namespace, Name: "lookup"}, lookup))
	descriptor, err := customACPMCPToolDescriptor(lookup)
	require.NoError(t, err)
	callLookup := func(child *corev1alpha1.Task) error {
		request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
		request.Namespace = child.Namespace
		request.Metadata.TaskUID = harnessv2.TaskUID(child.UID)
		request.Metadata.OperationID = harnessv2.OperationID(child.Name + "-operation")
		request.Call.ToolName = "lookup"
		request.Call.Arguments = json.RawMessage(`{}`)
		ctx := withACPMCPAuthenticatedTask(t.Context(), ACPMCPAuthenticatedTask{Name: child.Name, Namespace: child.Namespace, UID: string(child.UID)})
		executor := RegistryACPMCPToolExecutor{TaskProvenanceProtected: true, Reader: r.Client, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: server.Client(), AgentExecutionSnapshots: r.AgentExecutionSnapshots, GatewayService: r.GatewayService}
		_, err := executor.ExecuteACPMCPTool(ctx, request, descriptor)
		return err
	}

	with := newChild("delegated-child", "lookup", "forbidden")
	require.Equal(t, []string{"lookup"}, gatewayBoundBody(t, r, with).GatewayGrant.Tools)
	require.NoError(t, callLookup(with))
	g.Lock()
	require.Equal(t, []string{string(parent.UID), string(with.UID)}, g.issued)
	require.Equal(t, []string{"lookup"}, g.bindings[string(parent.UID)].Tools)
	require.Equal(t, []string{string(with.UID)}, g.called)
	g.Unlock()

	without := newChild("undelegated-child", "forbidden")
	require.Empty(t, gatewayBoundBody(t, r, without).GatewayGrant.Tools)
	require.Error(t, callLookup(without))
	g.Lock()
	require.Len(t, g.issued, 2)
	require.Len(t, g.called, 1)
	g.Unlock()

	childOf := func(name string, of *corev1alpha1.Task) *corev1alpha1.Task {
		child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: of.Namespace, Annotations: map[string]string{
			labels.AnnotationParentTaskName: of.Name, labels.AnnotationParentTaskNamespace: of.Namespace, labels.AnnotationParentTaskUID: string(of.UID),
		}}, Spec: *parent.Spec.DeepCopy()}
		require.NoError(t, r.Create(t.Context(), child))
		return child
	}
	router := childOf("router-child", parent)
	grant, err = r.freezeGatewayTaskGrant(t.Context(), router, agent, *parentBody.MCPConfiguration)
	require.NoError(t, err)
	require.Equal(t, []string{"lookup"}, grant.Tools)
	require.Equal(t, string(parent.UID), grant.Parent.UID)
	grant, err = r.freezeGatewayTaskGrant(t.Context(), parent, agent, *gatewayBoundBody(t, r, with).MCPConfiguration)
	require.NoError(t, err)
	require.Equal(t, []string{"forbidden", "lookup"}, grant.Tools)
	native := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "native-parent", Namespace: parent.Namespace}, Spec: *parent.Spec.DeepCopy()}
	require.NoError(t, r.Create(t.Context(), native))
	nativeRouter := childOf("native-router-child", native)
	for _, protected := range []bool{true, false} {
		r.TaskProvenanceProtected = protected
		grant, err = r.freezeGatewayTaskGrant(t.Context(), nativeRouter, agent, *parentBody.MCPConfiguration)
		require.NoError(t, err, "provenance protected: %v", protected)
		require.Nil(t, grant)
	}
	r.TaskProvenanceProtected = true
	_, err = r.freezeGatewayTaskGrant(t.Context(), nativeRouter, childAgent, *gatewayBoundBody(t, r, with).MCPConfiguration)
	require.ErrorContains(t, err, "parent has no frozen gateway grant")
	require.True(t, isPermanentACPAgentConfigurationError(err))
}
