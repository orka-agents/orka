package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRemoteMCPControllerDoesNotHostOrProbe(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	r, tool, statusWrites := newRemoteMCPControllerFixture(t, server.URL)
	r.HTTPClient = server.Client()
	c := r.Client
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tool)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(tool), tool); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("controller issued %d ambient credential probes", calls.Load())
	}
	accepted := meta.FindStatusCondition(tool.Status.Conditions, "Accepted")
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatalf("remote not accepted: %+v", tool.Status)
	}
	available := meta.FindStatusCondition(tool.Status.Conditions, "Available")
	if tool.Status.Available || available == nil || available.Status != metav1.ConditionUnknown {
		t.Fatalf("controller claimed authenticated readiness: %+v", tool.Status)
	}
	if tool.Status.Actor != nil || tool.Status.Workspace != nil || len(tool.Finalizers) > 0 {
		t.Fatalf("remote gained hosting state: %+v", tool)
	}
	assertRemoteMCPStatusStable(t, r, tool, statusWrites)
	// Schemaless admission stays compatible; controller rejection must revoke acceptance.
	tool.Spec.Parameters = nil
	if err := c.Update(t.Context(), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tool)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(tool), tool); err != nil {
		t.Fatal(err)
	}
	accepted = meta.FindStatusCondition(tool.Status.Conditions, "Accepted")
	if accepted == nil || accepted.Status != metav1.ConditionFalse || tool.Status.Error == "" || tool.Status.LastCheck != nil {
		t.Fatalf("invalid remote configuration retained acceptance or fabricated a health check: %+v", tool.Status)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid remote configuration probed endpoint")
	}
	if *statusWrites < 2 {
		t.Fatal("changed remote status was not persisted")
	}
	assertRemoteMCPStatusStable(t, r, tool, statusWrites)
	before := *statusWrites
	tool.Generation++
	if err := c.Update(t.Context(), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tool)}); err != nil {
		t.Fatal(err)
	}
	if *statusWrites != before+1 {
		t.Fatal("generation change did not update observed conditions")
	}
	assertRemoteMCPStatusStable(t, r, tool, statusWrites)
}

func newRemoteMCPControllerFixture(t *testing.T, endpoint string) (*ToolReconciler, *corev1alpha1.Tool, *int) {
	t.Helper()
	var tool corev1alpha1.Tool
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"remote","namespace":"default"},"spec":{"description":"reviewed","parameters":{"type":"object"},"http":{"authSecretRef":{"name":"auth","key":"token"},"outboundAccessPolicyRef":{"name":"egress"}},"mcp":{"remote":{"url":"`+endpoint+`/mcp","toolName":"read"}}}}`), &tool); err != nil {
		t.Fatal(err)
	}
	scheme := newToolScheme()
	policy := &corev1alpha1.OutboundAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "egress", Namespace: "default", Generation: 1}}
	if err := json.Unmarshal([]byte(`{"gateway":{"serviceRef":{"name":"gateway","port":8080}}}`), &policy.Spec); err != nil {
		t.Fatal(err)
	}
	policy.Status.ObservedGeneration = 1
	for _, typ := range []string{corev1alpha1.OutboundAccessPolicyConditionAccepted, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs} {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: 1})
	}
	statusWrites := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&tool).WithObjects(&tool, policy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-credential")}}).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subresource == "status" {
				statusWrites++
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	}).Build()
	r := &ToolReconciler{Client: c, Scheme: scheme, SkipSSRFValidation: true}
	return r, &tool, &statusWrites
}

func assertRemoteMCPStatusStable(t *testing.T, r *ToolReconciler, tool *corev1alpha1.Tool, statusWrites *int) {
	t.Helper()
	before := *statusWrites
	for range 2 {
		result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tool)})
		if err != nil || result.RequeueAfter != toolHealthCheckInterval {
			t.Fatalf("stable reconciliation lost periodic recheck: %v", err)
		}
	}
	if *statusWrites != before {
		t.Error("unchanged remote status caused additional writes")
	}
}

func TestRemoteMCPBackendTransitionClearsAccepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	for _, initial := range []string{"accepted", "rejected", "nonremote"} {
		for _, backend := range []struct {
			name      string
			http      *corev1alpha1.HTTPExecution
			mcp       *corev1alpha1.MCPToolServer
			available bool
			errMsg    string
		}{
			{name: "http", http: &corev1alpha1.HTTPExecution{URL: server.URL}, available: true},
			{name: "invalid-http", http: &corev1alpha1.HTTPExecution{}, errMsg: "http.url is required"},
			{name: "workspace", mcp: &corev1alpha1.MCPToolServer{Workspace: &corev1alpha1.MCPWorkspace{
				ClassRef: corev1alpha1.WorkspaceClassReference{Name: "service-v1"}, Port: 8080,
			}}, errMsg: "mcp.workspace requires controller-first Tool workspace integration"},
			{name: "actor", mcp: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{
				TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
			}}, available: true},
		} {
			t.Run(initial+"/"+backend.name, func(t *testing.T) {
				r, tool, writes := newRemoteMCPControllerFixture(t, server.URL)
				if initial != "nonremote" {
					if initial == "rejected" {
						tool.Spec.Parameters = nil
					}
					if err := r.Update(t.Context(), tool); err != nil {
						t.Fatal(err)
					}
					reconcileAndGetRemoteMCPTool(t, r, tool)
					accepted := meta.FindStatusCondition(tool.Status.Conditions, "Accepted")
					if accepted == nil || (accepted.Status == metav1.ConditionTrue) != (initial == "accepted") {
						t.Fatalf("unexpected initial remote status: %+v", tool.Status)
					}
				}
				unrelated := metav1.Condition{Type: "Reviewed", Status: metav1.ConditionTrue, Reason: "Approved", LastTransitionTime: metav1.NewTime(time.Unix(1, 0))}
				meta.SetStatusCondition(&tool.Status.Conditions, unrelated)
				if err := r.Status().Update(t.Context(), tool); err != nil {
					t.Fatal(err)
				}
				tool.Spec = corev1alpha1.ToolSpec{Description: "nonremote", HTTP: backend.http, MCP: backend.mcp}
				tool.Generation++
				if err := r.Update(t.Context(), tool); err != nil {
					t.Fatal(err)
				}
				r.WorkspaceProviderAPIEnabled, r.SubstrateEnabled = true, true
				r.SubstrateConfig = SubstrateConfig{RouterURL: server.URL, ActorDNSSuffix: "actors.example.com"}
				r.SubstrateTemplateValidator = func(context.Context, *ExecutionWorkspaceRequest) error { return nil }
				r.SubstrateExecutorFactory = func(SubstrateConfig) (workspace.WorkspaceExecutor, error) {
					return &recordingToolWorkspaceExecutor{}, nil
				}
				reconcileAndGetRemoteMCPTool(t, r, tool)
				if meta.FindStatusCondition(tool.Status.Conditions, "Accepted") != nil {
					t.Fatalf("nonremote Tool retained remote Accepted: %+v", tool.Status.Conditions)
				}
				if got := meta.FindStatusCondition(tool.Status.Conditions, unrelated.Type); got == nil || !reflect.DeepEqual(*got, unrelated) {
					t.Fatalf("unrelated condition changed: %+v", got)
				}
				// Actor ownership is recorded before the first health/status update.
				if backend.name == "actor" {
					reconcileAndGetRemoteMCPTool(t, r, tool)
				}
				assertNonRemoteToolAvailability(t, tool, backend.available, backend.errMsg)
				for range 2 {
					before := *writes
					reconcileAndGetRemoteMCPTool(t, r, tool)
					if *writes != before+1 {
						t.Fatalf("ordinary nonremote reconcile made %d status writes, want only its existing health write", *writes-before)
					}
					assertNonRemoteToolAvailability(t, tool, backend.available, backend.errMsg)
				}
			})
		}
	}
}

func reconcileAndGetRemoteMCPTool(t *testing.T, r *ToolReconciler, tool *corev1alpha1.Tool) {
	t.Helper()
	key := client.ObjectKeyFromObject(tool)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), key, tool); err != nil {
		t.Fatal(err)
	}
}

func assertNonRemoteToolAvailability(t *testing.T, tool *corev1alpha1.Tool, available bool, errMsg string) {
	t.Helper()
	status, reason := metav1.ConditionFalse, "EndpointUnreachable"
	if available {
		status, reason = metav1.ConditionTrue, "EndpointReachable"
	}
	condition := meta.FindStatusCondition(tool.Status.Conditions, "Available")
	if tool.Status.Available != available || tool.Status.Error != errMsg || tool.Status.LastCheck == nil ||
		condition == nil || condition.Status != status || condition.Reason != reason || condition.ObservedGeneration != tool.Generation {
		t.Fatalf("ordinary availability status changed: %+v", tool.Status)
	}
}

func TestRemoteMCPBackendTransitionStatusError(t *testing.T) {
	r, tool, _ := newRemoteMCPControllerFixture(t, "https://example.com")
	reconcileAndGetRemoteMCPTool(t, r, tool)
	tool.Spec.MCP = nil
	tool.Spec.HTTP = &corev1alpha1.HTTPExecution{}
	if err := r.Update(t.Context(), tool); err != nil {
		t.Fatal(err)
	}
	before := tool.Status.DeepCopy()
	wantErr := errors.New("status write failed")
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return wantErr
		},
	})
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tool)})
	if !errors.Is(err, wantErr) || result != (ctrl.Result{}) {
		t.Fatalf("status failure = (%+v, %v), want empty result and original error", result, err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(tool), tool); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*before, tool.Status) {
		t.Fatalf("failed status update changed persisted status: %+v", tool.Status)
	}
}

func TestRemoteMCPBackendTransitionPreservesFinalization(t *testing.T) {
	for _, phase := range []string{"backend-cleanup", "deletion"} {
		t.Run(phase, func(t *testing.T) {
			r, tool, writes := newRemoteMCPControllerFixture(t, "https://example.com")
			reconcileAndGetRemoteMCPTool(t, r, tool)
			tool.Spec.MCP = nil
			tool.Spec.HTTP = &corev1alpha1.HTTPExecution{}
			tool.Finalizers = []string{substrateMCPToolActorFinalizer}
			if err := r.Update(t.Context(), tool); err != nil {
				t.Fatal(err)
			}
			if phase == "deletion" {
				if err := r.Delete(t.Context(), tool); err != nil {
					t.Fatal(err)
				}
			}
			before := *writes
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tool)})
			if err != nil || result != (ctrl.Result{}) || *writes != before {
				t.Fatalf("finalization changed: result=%+v err=%v extra status writes=%d", result, err, *writes-before)
			}
		})
	}
}

func TestRemoteMCPExcludedFromACP(t *testing.T) {
	var tool corev1alpha1.Tool
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"remote"},"spec":{"description":"reviewed","brokeredToolClass":"read","parameters":{"type":"object"},"mcp":{"remote":{"url":"https://example.com/mcp","toolName":"read"}}}}`), &tool); err != nil {
		t.Fatal(err)
	}
	if _, err := customACPMCPToolDescriptor(&tool); err == nil {
		t.Fatal("remote tool exposed to ACP")
	}
	tool.Namespace, tool.UID, tool.Generation = "default", "remote-uid", 1
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&tool).Build()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{AgentRuntime: &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{tool.Name}}},
	}
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{}}}
	target := resolvedHarnessV1Target{
		runtimeRef: &corev1alpha1.AgentRuntime{}, backend: corev1alpha1.AgentExecutionBackendExternalEndpoint,
		supportsContinuation: true, brokeredToolClasses: []corev1alpha1.AgentRuntimeBrokeredToolClass{corev1alpha1.AgentRuntimeBrokeredToolClassRead},
	}
	if _, err := resolveHarnessV1BrokeredTools(t.Context(), reader, task, agent, target); err == nil {
		t.Fatal("remote tool exposed through legacy harness v1 broker")
	}
}
