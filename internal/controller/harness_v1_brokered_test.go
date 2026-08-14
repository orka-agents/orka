/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
)

func TestKubernetesHarnessV1BrokeredToolExecutorBindsTaskTransactionAuthority(t *testing.T) {
	const namespace = "default"
	const upstreamOKBody = `{"ok":true}`
	var lastTxnToken atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastTxnToken.Store(r.Header.Get("Txn-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamOKBody))
	}))
	defer upstream.Close()
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup", Namespace: namespace, UID: types.UID("lookup-uid"), Generation: 1},
		Spec: corev1alpha1.ToolSpec{
			Description: "look up a value", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			Parameters: &apiextensionsJSONForMCPTest,
			HTTP: &corev1alpha1.HTTPExecution{
				URL: upstream.URL, Method: http.MethodPost,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "tool-credential", Key: "token"},
			},
		},
	}
	request := harness.ToolCallRequest{
		Version: harness.ProtocolVersion, RuntimeSessionID: "runtime-session-a", TurnID: "turn-a",
		ToolCallID: "call-1", ToolName: tool.Name, Input: json.RawMessage(`{"query":"value"}`),
	}
	request.IdempotencyKey = harness.ToolRequestIdempotencyKey(
		request.RuntimeSessionID, request.TurnID, request.ToolCallID,
	)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	authenticated := harnessV1AuthenticatedTask{Name: "task", Namespace: namespace, UID: "task-uid"}
	newTask := func(scopes []string, constraint string, tokenSecret string) *corev1alpha1.Task {
		task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
			Name: authenticated.Name, Namespace: authenticated.Namespace, UID: types.UID(authenticated.UID),
		}}
		if tokenSecret != "" {
			task.Annotations = map[string]string{"orka.ai/transaction-token-secret": tokenSecret}
		}
		task.Spec.Transaction = &corev1alpha1.TaskTransaction{ID: "txn-1", Scopes: scopes}
		if constraint != "" {
			task.Spec.Transaction.Context = map[string]string{"secret": constraint}
		}
		return task
	}
	toolCredential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-credential", Namespace: namespace},
		Data:       map[string][]byte{"token": []byte("tool-token")},
	}
	newExecutor := func(enforce bool, objects ...client.Object) *KubernetesHarnessV1BrokeredToolExecutor {
		return &KubernetesHarnessV1BrokeredToolExecutor{
			Reader:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
			KubeClient: k8sfake.NewSimpleClientset(toolCredential.DeepCopy()), HTTPClient: upstream.Client(),
			EnforceTransactionCredentialAuth: enforce,
		}
	}
	authorizedTask := newTask([]string{"orka:secrets:credentials:read"}, "tool-credential", "")
	ctx := withHarnessV1AuthenticatedTask(context.Background(), authenticated)

	t.Run("enforcement off preserves current behavior without task authority", func(t *testing.T) {
		executor := newExecutor(false)
		result, err := executor.ExecuteHarnessV1BrokeredTool(context.Background(), namespace, tool.DeepCopy(), request)
		if err != nil {
			t.Fatalf("enforcement-off execution error = %v", err)
		}
		if string(result) != upstreamOKBody {
			t.Fatalf("enforcement-off result = %s", result)
		}
	})
	t.Run("missing authenticated task fails closed under enforcement", func(t *testing.T) {
		executor := newExecutor(true, authorizedTask.DeepCopy())
		if _, err := executor.ExecuteHarnessV1BrokeredTool(context.Background(), namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "task authority is unavailable") {
			t.Fatalf("missing authenticated task error = %v", err)
		}
	})
	t.Run("authenticated namespace mismatch fails closed under enforcement", func(t *testing.T) {
		executor := newExecutor(true, authorizedTask.DeepCopy())
		mismatched := withHarnessV1AuthenticatedTask(context.Background(), harnessV1AuthenticatedTask{
			Name: authenticated.Name, Namespace: "other-namespace", UID: authenticated.UID,
		})
		if _, err := executor.ExecuteHarnessV1BrokeredTool(mismatched, namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "task authority is unavailable") {
			t.Fatalf("namespace mismatch error = %v", err)
		}
	})
	t.Run("missing task object fails closed under enforcement", func(t *testing.T) {
		executor := newExecutor(true)
		if _, err := executor.ExecuteHarnessV1BrokeredTool(ctx, namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "load authenticated harness v1 brokered task authority") {
			t.Fatalf("missing task object error = %v", err)
		}
	})
	t.Run("task identity change fails closed under enforcement", func(t *testing.T) {
		replaced := authorizedTask.DeepCopy()
		replaced.UID = types.UID("replaced-task-uid")
		executor := newExecutor(true, replaced)
		if _, err := executor.ExecuteHarnessV1BrokeredTool(ctx, namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "task identity changed") {
			t.Fatalf("task identity change error = %v", err)
		}
	})
	t.Run("task without credential-read scope is refused", func(t *testing.T) {
		executor := newExecutor(true, newTask([]string{"reports.read"}, "", ""))
		if _, err := executor.ExecuteHarnessV1BrokeredTool(ctx, namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "not authorized by task transaction authority") {
			t.Fatalf("unauthorized scope error = %v", err)
		}
	})
	t.Run("task secret constraint must match the tool credential", func(t *testing.T) {
		executor := newExecutor(true, newTask([]string{"orka:secrets:credentials:read"}, "other-credential", ""))
		if _, err := executor.ExecuteHarnessV1BrokeredTool(ctx, namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "does not match task transaction authority") {
			t.Fatalf("secret constraint error = %v", err)
		}
	})
	t.Run("authorized task executes and attaches its owner-referenced token", func(t *testing.T) {
		tokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "task-txn-token", Namespace: namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
					Name: authenticated.Name, UID: types.UID(authenticated.UID),
				}},
			},
			Data: map[string][]byte{"token": []byte("task-scoped-token")},
		}
		executor := newExecutor(true,
			newTask([]string{"orka:secrets:credentials:read"}, "tool-credential", tokenSecret.Name), tokenSecret)
		result, err := executor.ExecuteHarnessV1BrokeredTool(ctx, namespace, tool.DeepCopy(), request)
		if err != nil {
			t.Fatalf("authorized execution error = %v", err)
		}
		if string(result) != upstreamOKBody {
			t.Fatalf("authorized result = %s", result)
		}
		if got, _ := lastTxnToken.Load().(string); got != "task-scoped-token" {
			t.Fatalf("Txn-Token header = %q, want the task's owner-referenced token", got)
		}
	})
	t.Run("token secret not owned by the task fails closed", func(t *testing.T) {
		unowned := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "task-txn-token", Namespace: namespace},
			Data:       map[string][]byte{"token": []byte("task-scoped-token")},
		}
		executor := newExecutor(true,
			newTask([]string{"orka:secrets:credentials:read"}, "tool-credential", unowned.Name), unowned)
		if _, err := executor.ExecuteHarnessV1BrokeredTool(ctx, namespace, tool.DeepCopy(), request); err == nil ||
			!strings.Contains(err.Error(), "not owned by the authenticated Task") {
			t.Fatalf("unowned token secret error = %v", err)
		}
	})
}

func TestContinueHarnessV1BrokeredToolCallStampsAuthenticatedTaskIdentity(t *testing.T) {
	fixture := newHarnessV1DispatcherStateFixture(t)
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixture.task.Namespace, Name: "lookup", UID: types.UID("lookup-tool-uid"), Generation: 1,
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "look up a value", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{URL: "https://tools.example.test/lookup", Method: http.MethodPost},
		},
	}
	if err := fixture.dispatcher.Client.Create(fixture.ctx, tool); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Tool{}
	if err := fixture.dispatcher.APIReader.Get(fixture.ctx, client.ObjectKeyFromObject(tool), current); err != nil {
		t.Fatal(err)
	}
	digest, err := harnessV1BrokeredToolDefinitionDigest(current)
	if err != nil {
		t.Fatal(err)
	}
	fixture.verified.body.HarnessV1.ToolExecutionMode = string(harness.ToolExecutionModeBrokered)
	fixture.verified.body.HarnessV1.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{
		corev1alpha1.AgentRuntimeBrokeredToolClassRead,
	}
	fixture.verified.body.HarnessV1.BrokeredTools = []agentExecutionSnapshotHarnessV1BrokeredTool{{
		Name: current.Name, Description: current.Spec.Description, BrokeredClass: current.Spec.BrokeredToolClass,
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		UID:        string(current.UID), Generation: current.Generation,
		DefinitionDigest: digest,
	}}
	call := harness.ToolCallRequest{
		Version: harness.ProtocolVersion, RuntimeSessionID: fixture.request.RuntimeSessionID,
		TurnID: fixture.request.TurnID, ToolCallID: "call-1", ToolName: current.Name,
		Input: json.RawMessage(`{"query":"value"}`),
	}
	call.IdempotencyKey = harness.ToolRequestIdempotencyKey(call.RuntimeSessionID, call.TurnID, call.ToolCallID)
	content, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	frame := harness.HarnessEventFrame{
		Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested,
		RuntimeSessionID: fixture.request.RuntimeSessionID, TurnID: fixture.request.TurnID,
		CorrelationID: fixture.request.CorrelationID, Seq: 2,
		ToolName: current.Name, ToolCallID: call.ToolCallID, Content: content,
	}
	fixture.dispatcher.ExternalEffects = fixture.durable
	stamped := 0
	fixture.dispatcher.BrokeredToolExecutor = HarnessV1BrokeredToolExecutorFunc(func(
		ctx context.Context,
		namespace string,
		_ *corev1alpha1.Tool,
		_ harness.ToolCallRequest,
	) (json.RawMessage, error) {
		authenticated, ok := harnessV1AuthenticatedTaskFromContext(ctx)
		if !ok || authenticated.Name != fixture.task.Name || authenticated.Namespace != namespace ||
			authenticated.Namespace != fixture.task.Namespace || authenticated.UID != string(fixture.task.UID) {
			t.Fatalf("authenticated task identity = %#v ok=%t, want the dispatched Task", authenticated, ok)
		}
		stamped++
		return json.RawMessage(`{"answer":42}`), nil
	})
	protocolClient := &recordingHarnessV1RecoveryClient{continueTurn: func(
		_ context.Context,
		request harness.ContinueTurnRequest,
	) (*harness.ContinueTurnResponse, error) {
		return &harness.ContinueTurnResponse{
			Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID,
			TurnID: request.TurnID, CorrelationID: request.CorrelationID,
		}, nil
	}}
	if err := fixture.dispatcher.continueHarnessV1BrokeredToolCall(
		fixture.ctx, fixture.task, fixture.verified, protocolClient, fixture.request,
		fixture.attempt, fixture.fence, frame,
	); err != nil {
		t.Fatalf("continue brokered call: %v", err)
	}
	if stamped != 1 {
		t.Fatalf("stamped executions = %d, want 1", stamped)
	}
}
