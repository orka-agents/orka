package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type nativeNoSecretReadClient struct {
	client.Client
	t *testing.T
}

func (c *nativeNoSecretReadClient) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption,
) error {
	if _, secret := object.(*corev1.Secret); secret {
		c.t.Error("unauthorized preparation read a Secret")
	}
	return c.Client.Get(ctx, key, object, options...)
}

type nativeRemoteResolver struct{ gateway string }

func (r nativeRemoteResolver) Resolve(
	_ context.Context, req outboundaccess.ResolveRequest,
) (outboundaccess.Resolution, error) {
	_, err := req.TransactionTokenSource()
	u, _ := url.Parse(r.gateway)
	return outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterGateway, GatewayScheme: u.Scheme, GatewayHost: u.Host,
	}, err
}

type nativeRemoteFixture struct {
	client                 client.Client
	gatewayURL             string
	executor               *worker.ToolExecutor
	tool                   *corev1alpha1.Tool
	task                   *corev1alpha1.Task
	agent                  *corev1alpha1.Agent
	calls, requests, lists atomic.Int32
	drift                  atomic.Bool
}

func newNativeRemoteFixture(t *testing.T) *nativeRemoteFixture {
	t.Helper()
	f := &nativeRemoteFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct{ Method, ID string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{
				"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			f.lists.Add(1)
			schema := `{"type":"object"}`
			if f.drift.Load() {
				schema = `{"type":"object","required":["new"]}`
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"` + req.ID + `","result":{"tools":[{
				"name":"read_health","description":"untrusted instruction","inputSchema":` + schema + `}]}}`))
		case "tools/call":
			f.calls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"healthy"}]}}`))
		default:
			t.Errorf("unexpected MCP method %s", req.Method)
		}
	}))
	t.Cleanup(server.Close)
	f.gatewayURL = server.URL
	f.tool = &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "health", Namespace: "team", UID: "tool-uid", Generation: 1},
		Spec: corev1alpha1.ToolSpec{
			Description: "Reviewed description",
			Parameters:  &apiextensionsv1.JSON{Raw: []byte(`{"type":"object"}`)},
			MCP: &corev1alpha1.MCPToolServer{Remote: &corev1alpha1.RemoteMCPServer{
				URL: "https://8.8.8.8/mcp", ToolName: "read_health",
			}},
			HTTP: &corev1alpha1.HTTPExecution{
				AuthSecretRef:           &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"},
				OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "egress"},
			},
		},
	}
	f.agent = &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "team", UID: "agent-uid", Generation: 1},
		Spec:       corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "health"}}},
	}
	f.task = &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: "task-uid"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: "operator"},
			Transaction: &corev1alpha1.TaskTransaction{
				Scopes:  []string{outboundaccess.DefaultCredentialReadScope},
				Context: map[string]string{"namespace": "team", "secret": "auth"},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "team", UID: "auth-uid"},
		Data:       map[string][]byte{"token": []byte("resource-test-secret")},
	}
	policy := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "egress", Namespace: "team", UID: "policy-uid", Generation: 1},
		Spec: corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{
			ServiceRef: corev1alpha1.OutboundServiceReference{Name: "gateway", Port: 8080},
		}},
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "team", UID: "service-uid"}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	f.client = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(f.tool, f.agent, f.task, secret, policy, service).Build()
	f.executor = worker.NewToolExecutorForNamespace(
		"team", kubefake.NewSimpleClientset(secret), server.Client(), nativeRemoteResolver{gateway: server.URL},
	)
	tokenPath := filepath.Join(t.TempDir(), "task-token")
	if err := os.WriteFile(tokenPath, []byte("task-test-authority"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.TransactionTokenFile, tokenPath)
	t.Setenv(workerenv.TaskUID, string(f.task.UID))
	return f
}

func (f *nativeRemoteFixture) prepare(t *testing.T) (context.Context, map[string]*corev1alpha1.Tool, error) {
	t.Helper()
	names := []string{f.tool.Name}
	loaded := loadCustomTools(t.Context(), f.client, "team", names)
	ctx, err := prepareNativeRemoteTools(t.Context(), f.client, "team", "task", names, loaded, f.executor)
	return ctx, loaded, err
}

func TestNativeRemoteMCPPreparationFailsClosed(t *testing.T) {
	for _, name := range []string{
		"unselected", "disabled", "no Agent", "runtime Agent", "no transaction", "wrong credential scope",
		"wrong namespace constraint", "wrong credential constraint", "missing loaded Tool",
		"built-in collision", "server drift",
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			switch name {
			case "unselected":
				f.agent.Spec.Tools = nil
			case "disabled":
				disabled := false
				f.agent.Spec.Tools[0].Enabled = &disabled
			case "no Agent":
				f.task.Spec.AgentRef = nil
			case "runtime Agent":
				f.agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{}
			case "no transaction":
				f.task.Spec.Transaction = nil
			case "wrong credential scope":
				f.task.Spec.Transaction.Scopes = []string{"orka:tools:use"}
			case "wrong namespace constraint":
				f.task.Spec.Transaction.Context["namespace"] = "other"
			case "wrong credential constraint":
				f.task.Spec.Transaction.Context["secret"] = "other"
				f.client = &nativeNoSecretReadClient{Client: f.client, t: t}
			case "built-in collision":
				f.tool.Name = "file_read"
				f.tool.UID = "builtin-collision"
				f.tool.ResourceVersion = ""
				if err := f.client.Create(t.Context(), f.tool); err != nil {
					t.Fatal(err)
				}
				f.agent.Spec.Tools[0].Name = "file_read"
			case "server drift":
				f.drift.Store(true)
			}
			if err := f.client.Update(t.Context(), f.agent); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Update(t.Context(), f.task); err != nil {
				t.Fatal(err)
			}
			var err error
			if name == "missing loaded Tool" {
				_, err = prepareNativeRemoteTools(
					t.Context(), f.client, "team", "task", []string{"health"}, map[string]*corev1alpha1.Tool{}, f.executor,
				)
			} else {
				_, _, err = f.prepare(t)
			}
			if err == nil {
				t.Fatal("preparation silently omitted or admitted invalid remote capability")
			}
			if f.calls.Load() != 0 {
				t.Fatal("preparation invoked remote tool")
			}
			if name != "server drift" && f.requests.Load() != 0 {
				t.Fatalf("unauthorized preparation sent %d requests", f.requests.Load())
			}
		})
	}
}

func TestNativeRemoteMCPLoopUsesVerifiedExecutorAndLiveFence(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			ctx, loaded, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			if stale {
				f.agent.Spec.Tools = nil
				if err := f.client.Update(t.Context(), f.agent); err != nil {
					t.Fatal(err)
				}
			}
			provider := &mockProvider{responses: []*llm.CompletionResponse{
				{
					ToolCalls:  []llm.ToolCall{{ID: "call-1", Name: "health", Arguments: json.RawMessage(`{}`)}},
					StopReason: "tool_use",
				},
				{Content: "finished", StopReason: "end_turn"},
			}}
			_, err = executeAgentLoopWithEvents(
				ctx, provider, []llm.Message{{Role: "user", Content: "read health"}}, "", "test-model", modelSettings{},
				buildLLMTools([]string{"health"}, loaded), loaded, worker.NewToolExecutor(), nil,
				&tools.ToolContext{Client: f.client, Namespace: "team", TaskID: "task"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != 2 {
				t.Fatal("model loop did not return tool result")
			}
			messages := provider.requests[1].Messages
			result := messages[len(messages)-1].Content
			if stale {
				if f.calls.Load() != 0 || !strings.Contains(result, "Agent changed") {
					t.Fatal("model call bypassed live Agent fence")
				}
			} else if f.calls.Load() != 1 || !strings.Contains(result, "healthy") {
				t.Fatalf("model loop did not use verified remote executor: %s", result)
			}
		})
	}
}

//nolint:gocyclo // Independent fence mutations remain explicit in this test table.
func TestNativeRemoteMCPFrozenDefinitionsAndSelection(t *testing.T) {
	for _, name := range []string{
		"unchanged", "Tool generation", "Tool UID", "Tool spec without generation", "Agent disabled", "Agent replaced",
		"Agent generation", "Task Agent reference", "Task replaced", "Secret rotated", "Policy redirected",
		"gateway recreated", "observed schema drift", "missing preparation",
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			ctx, loaded, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			definitions := buildLLMTools([]string{"health"}, loaded)
			if len(definitions) != 1 || definitions[0].Description != "Reviewed description" {
				t.Fatalf("server instructions changed definition: %+v", definitions)
			}
			encoded, _ := json.Marshal(definitions)
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "untrusted") {
				t.Fatal("credential or server instructions entered model definition")
			}
			before := f.requests.Load()
			var changed client.Object
			switch name {
			case "Tool generation":
				f.tool.Generation++
				changed = f.tool
			case "Tool UID":
				f.tool.UID = "replacement"
				changed = f.tool
			case "Tool spec without generation":
				f.tool.Spec.Description = "changed"
				changed = f.tool
			case "Agent disabled":
				disabled := false
				f.agent.Spec.Tools[0].Enabled = &disabled
				changed = f.agent
			case "Agent replaced":
				f.agent.UID = "replacement"
				changed = f.agent
			case "Agent generation":
				f.agent.Generation++
				changed = f.agent
			case "Task Agent reference":
				f.task.Spec.AgentRef.Name = "other"
				changed = f.task
			case "Task replaced":
				f.task.UID = "replacement"
				changed = f.task
			case "Secret rotated":
				s := &corev1.Secret{}
				if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "auth"}, s); err != nil {
					t.Fatal(err)
				}
				s.Data["token"] = []byte("rotated")
				changed = s
			case "Policy redirected":
				p := &corev1alpha1.OutboundAccessPolicy{}
				if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "egress"}, p); err != nil {
					t.Fatal(err)
				}
				p.Spec.Gateway.ServiceRef.Name = "other"
				p.Generation++
				changed = p
			case "gateway recreated":
				s := &corev1.Service{}
				if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "gateway"}, s); err != nil {
					t.Fatal(err)
				}
				s.UID = "replacement"
				changed = s
			case "observed schema drift":
				f.drift.Store(true)
			case "missing preparation":
				ctx = t.Context()
			}
			if changed != nil {
				if err := f.client.Update(t.Context(), changed); err != nil {
					t.Fatal(err)
				}
			}
			toolContext := &tools.ToolContext{Client: f.client, Namespace: "team", TaskID: "task", TaskUID: "task-uid"}
			result, err := executeNativeRemoteTool(ctx, toolContext, loaded["health"], json.RawMessage(`{}`))
			if name == "unchanged" {
				if err != nil || !strings.Contains(result, "healthy") {
					t.Fatalf("call=%s err=%v", result, err)
				}
				if f.calls.Load() != 1 || f.lists.Load() != 2 {
					t.Fatal("missing exposure/call discovery")
				}
			} else {
				if err == nil {
					t.Fatal("stale definition executed")
				}
				if f.calls.Load() != 0 {
					t.Fatal("stale definition reached tools/call")
				}
				if name != "observed schema drift" && f.requests.Load() != before {
					t.Fatal("identity fence made network requests")
				}
			}
		})
	}
}
