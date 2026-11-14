package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGatewayTaskPassRootExecution(t *testing.T) {
	issued, called := 0, 0
	mode := "ok"
	var boundUID, gatewayUID string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/task-passes":
			issued++
			require.Equal(t, "Bearer target-controller-credential", r.Header.Get("Authorization"))
			var req struct {
				ProtocolVersion, GrantRef, NamespaceUID, GatewayUID, TaskUID string
				Tools                                                        []string
				Deadline                                                     time.Time
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, "sender-generation-1", req.GrantRef)
			require.Equal(t, gatewayUID, req.GatewayUID)
			require.Equal(t, []string{"lookup"}, req.Tools)
			require.NotEmpty(t, req.NamespaceUID)
			if boundUID == "" {
				boundUID = req.TaskUID
			}
			require.Equal(t, boundUID, req.TaskUID)
			expires := time.Now().Add(time.Minute)
			switch mode {
			case "five-minutes":
				expires = time.Now().Add(5 * time.Minute)
			case "oversized":
				_, _ = w.Write(make([]byte, 16385))
				return
			case "malformed":
				_, _ = w.Write([]byte("{"))
				return
			case "empty":
				_ = json.NewEncoder(w).Encode(map[string]any{"pass": "", "expiresAt": expires})
				return
			case "expired":
				expires = time.Now().Add(-time.Minute)
			case "overlong":
				expires = time.Now().Add(6 * time.Minute)
			case "deadline":
				expires = req.Deadline.Add(time.Second)
			case "redirect":
				w.Header().Set("Location", "/stolen")
				w.WriteHeader(307)
				return
			case "relinked":
				w.WriteHeader(409)
				_, _ = w.Write([]byte("gateway-response-secret"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pass": "ephemeral-pass", "expiresAt": expires})
		case "/v1/task-tools/lookup":
			called++
			require.Equal(t, "Bearer ephemeral-pass", r.Header.Get("Authorization"))
			require.Equal(t, "root-operation", r.Header.Get("Idempotency-Key"))
			require.Empty(t, r.Header.Get("Txn-Token"))
			_, _ = w.Write([]byte(`{"answer":"safe"}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	r, task, agent := gatewayPassAPIFixture(t, server.URL)
	gateway := &gatewayv1alpha1.Gateway{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "chat"}, gateway))
	gatewayUID = string(gateway.UID)
	gateway.Spec.Adapter.Endpoint = server.URL
	require.NoError(t, r.Update(t.Context(), gateway))
	r.GatewayService.HTTPClient = server.Client()
	r.GatewayService.Config.AllowInsecureLoopback = true
	r.GatewayService.TaskPassCredentials = map[string]gatewayruntime.TaskPassCredential{gatewayUID: {Endpoint: server.URL, Token: "target-controller-credential"}}
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "lookup", Namespace: "default"}, Spec: corev1alpha1.ToolSpec{Description: "lookup", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{URL: server.URL + "/v1/task-tools/lookup", GatewayTaskPass: true}}}
	require.NoError(t, r.Create(t.Context(), tool))
	agent.Spec.Runtime.DefaultAllowedTools = []string{"lookup"}
	_, planErr := r.resolveAgentExecutionCandidate(t.Context(), task, agent)
	require.NoError(t, planErr)
	_, err, handled := r.ensureAgentExecutionBinding(t.Context(), task, agent)
	require.NoError(t, err)
	require.False(t, handled, "status: %+v", task.Status)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	require.NoError(t, r.Status().Update(t.Context(), task))
	snapshot, err := r.AgentExecutionSnapshots.GetAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{TaskUID: string(task.UID), Digest: task.Status.AgentExecutionBinding.Snapshot.Digest})
	require.NoError(t, err)
	require.NotContains(t, string(snapshot.Body), "target-controller-credential")
	require.NotContains(t, string(snapshot.Body), "ephemeral-pass")
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	require.NoError(t, err)
	require.Equal(t, []string{"lookup"}, body.GatewayGrant.Tools)
	request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	request.Namespace = task.Namespace
	request.Metadata.TaskUID = harnessv2.TaskUID(task.UID)
	request.Metadata.OperationID = "root-operation"
	request.Call.ToolName = "lookup"
	request.Call.Arguments = json.RawMessage(`{"value":"reviewed"}`)
	descriptor, err := customACPMCPToolDescriptor(tool)
	require.NoError(t, err)
	ctx := withACPMCPAuthenticatedTask(t.Context(), ACPMCPAuthenticatedTask{Name: task.Name, Namespace: task.Namespace, UID: string(task.UID)})
	executor := RegistryACPMCPToolExecutor{Reader: r.Client, KubeClient: k8sfake.NewSimpleClientset(), HTTPClient: server.Client(), AgentExecutionSnapshots: r.AgentExecutionSnapshots, GatewayService: r.GatewayService}
	for _, happyMode := range []string{"ok", "five-minutes"} {
		mode = happyMode
		result, err := executor.ExecuteACPMCPTool(ctx, request, descriptor)
		require.NoError(t, err)
		require.Contains(t, string(result), "safe")
	}
	require.Equal(t, 2, issued, "restart/reconstruction reacquires the same durable binding per call")
	require.Equal(t, 2, called)
	// The live Gateway and frozen snapshot can agree on an attacker origin;
	// only operator-owned configuration may choose where credentials are sent.
	config, err := json.Marshal(map[string]any{gatewayUID: map[string]string{"endpoint": "https://operator-pinned.invalid", "token": "target-controller-credential"}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "credentials.json")
	require.NoError(t, os.WriteFile(path, config, 0600))
	r.GatewayService.TaskPassCredentials, err = gatewayruntime.LoadTaskPassCredentials(path)
	require.NoError(t, err)
	_, err = executor.ExecuteACPMCPTool(ctx, request, descriptor)
	require.ErrorContains(t, err, "gateway task pass is unavailable")
	require.Equal(t, 2, issued, "mismatched operator origin must receive no credential")
	require.Equal(t, 2, called)
	r.GatewayService.TaskPassCredentials = map[string]gatewayruntime.TaskPassCredential{gatewayUID: {Endpoint: server.URL, Token: "target-controller-credential"}}
	for _, failure := range []string{"expired", "overlong", "deadline", "redirect", "relinked", "oversized", "malformed", "empty"} {
		mode = failure
		_, err := executor.ExecuteACPMCPTool(ctx, request, descriptor)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "gateway-response-secret")
	}
	require.Equal(t, 2, called)
	mode = "ok"
	delete(r.GatewayService.TaskPassCredentials, gatewayUID)
	before := issued
	_, err = executor.ExecuteACPMCPTool(ctx, request, descriptor)
	require.Error(t, err)
	require.Equal(t, before, issued)
	r.GatewayService.TaskPassCredentials[gatewayUID] = gatewayruntime.TaskPassCredential{Endpoint: server.URL, Token: "target-controller-credential"}
	task.Labels["orka.ai/parent-task"] = "parent"
	require.NoError(t, r.Update(t.Context(), task))
	_, err = executor.ExecuteACPMCPTool(ctx, request, descriptor)
	require.Error(t, err)
	require.Equal(t, before, issued)
	delete(task.Labels, "orka.ai/parent-task")
	require.NoError(t, r.Update(t.Context(), task))
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	require.NoError(t, r.Status().Update(t.Context(), task))
	_, err = executor.ExecuteACPMCPTool(ctx, request, descriptor)
	require.Error(t, err)
	require.Equal(t, before, issued)
}
