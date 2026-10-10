package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/workerenv"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/workers/common"
	"github.com/stretchr/testify/require"
)

type nativeReplyRecorder struct{ requests []string }

func (s *nativeReplyRecorder) Budget(context.Context, string) (tools.GatewayReplyBudget, error) {
	return tools.GatewayReplyBudget{Limit: 10}, nil
}
func (s *nativeReplyRecorder) Enqueue(_ context.Context, id, _ string) (tools.GatewayReplyReceipt, error) {
	s.requests = append(s.requests, id)
	return tools.GatewayReplyReceipt{DeliveryID: "gdm-receipt", Status: "Pending", Created: true}, nil
}

func TestNativeGatewayReplyAdvertisementAndNonterminalLoop(t *testing.T) {
	tools.RegisterBuiltinTools()
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable hallucination", true: "bound reply"}[available], func(t *testing.T) {
			sender := &nativeReplyRecorder{}
			tc := &tools.ToolContext{Namespace: "default", TaskID: "task", TaskUID: "task-uid"}
			if available {
				tc.GatewayReplySender = sender
			}
			definitions := buildLLMTools([]string{"reply_in_conversation"}, nil, tc)
			if available {
				require.Len(t, definitions, 1)
			} else {
				require.Empty(t, definitions)
			}
			call := llm.ToolCall{ID: "call", Name: "reply_in_conversation", Arguments: json.RawMessage(`{"content":"working"}`)}
			provider := &mockProvider{responses: []*llm.CompletionResponse{
				{ToolCalls: []llm.ToolCall{call}, StopReason: "tool_use"},
				{ToolCalls: []llm.ToolCall{call}, StopReason: "tool_use"},
				{Content: "normal final answer", StopReason: "end_turn"},
			}}
			result, err := executeAgentLoopWithEvents(
				t.Context(), provider, []llm.Message{{Role: "user", Content: "work"}}, "", "test-model",
				modelSettings{maxTokens: 4096}, definitions, nil, nil, common.NewFakeEventRecorder(), tc,
			)
			require.NoError(t, err)
			require.Equal(t, "normal final answer", result)
			require.Len(t, provider.requests, 3)
			if available {
				require.Len(t, sender.requests, 2)
				require.NotEqual(t, sender.requests[0], sender.requests[1],
					"regenerated calls in different turns must not dedupe by text or reused provider ID")
				require.Contains(t, provider.requests[1].Messages[len(provider.requests[1].Messages)-1].Content, `"created":true`)
			} else {
				require.Empty(t, sender.requests)
				require.Contains(t, provider.requests[1].Messages[len(provider.requests[1].Messages)-1].Content, "not enabled")
			}
		})
	}
}

func TestNativeGatewayReplyBindingAndSpoofExclusions(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		edit   func(*workerenv.AIWorkerEnv, *corev1alpha1.Task)
		want   bool
	}{
		{"bound", 200, func(*workerenv.AIWorkerEnv, *corev1alpha1.Task) {}, true},
		{"no enable flag", 200, func(e *workerenv.AIWorkerEnv, _ *corev1alpha1.Task) {
			e.GatewayReplyEnabled = false
		}, false},
		{"not selected", 200, func(e *workerenv.AIWorkerEnv, _ *corev1alpha1.Task) { e.Tools = nil }, false},
		{"forged env ordinary Task denied by durable API", 403, func(*workerenv.AIWorkerEnv, *corev1alpha1.Task) {}, false},
		{"revoked worker", 403, func(*workerenv.AIWorkerEnv, *corev1alpha1.Task) {}, false},
		{"wrong task UID", 200, func(e *workerenv.AIWorkerEnv, _ *corev1alpha1.Task) { e.TaskUID = "spoof" }, false},
		{"container", 200, func(_ *workerenv.AIWorkerEnv, t *corev1alpha1.Task) {
			t.Spec.Type = corev1alpha1.TaskTypeContainer
		}, false},
		{"ACP", 200, func(_ *workerenv.AIWorkerEnv, t *corev1alpha1.Task) {
			t.Spec.Type = corev1alpha1.TaskTypeAgent
		}, false},
		{"delegated", 200, func(_ *workerenv.AIWorkerEnv, t *corev1alpha1.Task) {
			t.Labels = map[string]string{labels.LabelParentTask: "parent"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer projected-token", r.Header.Get("Authorization"))
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, `{"taskUID":"uid"}`)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "projected")
			require.NoError(t, os.WriteFile(path, []byte("projected-token"), 0600))
			t.Setenv(workerenv.ServiceAccountToken, "spoofed-token")
			env := workerenv.AIWorkerEnv{
				BaseEnv: workerenv.BaseEnv{
					ControllerURL: server.URL, TaskName: "task", TaskNamespace: "default", TaskUID: "uid",
				},
				Tools: []string{"reply_in_conversation"}, GatewayReplyEnabled: true,
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: "uid"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			test.edit(&env, task)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
			sender, err := newNativeGatewayReplySender(
				t.Context(), func() (client.Reader, error) { return kube, nil }, env, path, nativeGatewayReplyBootstrapTimeout,
			)
			if test.want {
				require.NoError(t, err)
				require.NotNil(t, sender)
			} else {
				require.Nil(t, sender)
				if test.name == "no enable flag" || test.name == "not selected" {
					require.NoError(t, err)
				} else {
					require.Error(t, err, "identity denials must remain fatal")
				}
			}
			require.Empty(t, logs(), "neither successful binding, explicit denial, nor policy exclusion is degradation")
			tc := &tools.ToolContext{
				Namespace: env.TaskNamespace, TaskID: env.TaskName, TaskUID: env.TaskUID, GatewayReplySender: sender,
			}
			definitions := buildLLMTools(env.Tools, nil, tc)
			require.Equal(t, test.want, len(definitions) == 1)
		})
	}
}

func TestNativeGatewayReplyBootstrapAvailabilityAndRecovery(t *testing.T) {
	tools.RegisterBuiltinTools()
	for _, test := range []struct {
		name             string
		originStatus     int
		budgetStatus     int
		publicationDelay bool
		wantSender       bool
		wantError        bool
	}{
		{name: "readiness lost after planning", originStatus: 200, budgetStatus: 503, wantSender: true},
		{name: "capability lost after planning", originStatus: 200, budgetStatus: 409, wantSender: true},
		{name: "Job publication delayed", originStatus: 200, publicationDelay: true, wantSender: true},
		{name: "origin service unavailable", originStatus: 503},
		{name: "origin service failure", originStatus: 500},
		{name: "forged or revoked", originStatus: 403, wantError: true},
		{name: "inactive identity", originStatus: 409, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			var originCalls atomic.Int32
			var recovered atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/v1/tasks/default/task/gateway-messages/origin":
					n := originCalls.Add(1)
					status := test.originStatus
					if test.publicationDelay && n == 1 {
						status = 503
					}
					w.WriteHeader(status)
					if status != http.StatusOK {
						_, _ = fmt.Fprint(w, "opaque upstream diagnostic https://private.invalid?token=secret Task text")
						return
					}
					_, _ = fmt.Fprint(w, `{"taskUID":"uid"}`)
				case "/internal/v1/tasks/default/task/gateway-messages/budget":
					if test.budgetStatus != 0 && !recovered.Load() {
						w.WriteHeader(test.budgetStatus)
						return
					}
					_, _ = fmt.Fprint(w, `{"accepted":0,"limit":10,"requestExists":false}`)
				default:
					t.Errorf("unexpected bootstrap route: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(path, []byte("projected-token"), 0600))
			env := workerenv.AIWorkerEnv{
				BaseEnv: workerenv.BaseEnv{
					ControllerURL: server.URL, TaskNamespace: "default", TaskName: "task", TaskUID: "uid",
				},
				Tools: []string{"reply_in_conversation"}, GatewayReplyEnabled: true,
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: "uid"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
			sender, err := newNativeGatewayReplySender(
				t.Context(), func() (client.Reader, error) { return kube, nil }, env, path, nativeGatewayReplyBootstrapTimeout,
			)
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err, "optional service availability must not terminate Task startup")
			}
			require.Equal(t, test.wantSender, sender != nil)
			if !test.wantSender && !test.wantError {
				require.Equal(t, "warning: reply_in_conversation omitted reason=origin_unavailable\n", logs(),
					"exhausted retries must warn only once, without upstream diagnostics")
			} else {
				require.Empty(t, logs())
			}
			tc := &tools.ToolContext{Namespace: "default", TaskID: "task", TaskUID: "uid", GatewayReplySender: sender}
			definitions := buildLLMTools(env.Tools, nil, tc)
			require.Equal(t, test.wantSender, len(definitions) == 1)
			if test.publicationDelay {
				require.GreaterOrEqual(t, originCalls.Load(), int32(2))
			}
			if test.originStatus == 503 {
				require.Greater(t, originCalls.Load(), int32(1))
				require.LessOrEqual(t, originCalls.Load(), int32(5))
			}
			if test.budgetStatus != 0 {
				_, err = sender.Budget(t.Context(), "new")
				require.Error(t, err)
				recovered.Store(true)
				_, err = sender.Budget(t.Context(), "new")
				require.NoError(t, err, "same bootstrapped sender must recover without restarting or re-advertising")
			}
			if !test.wantError {
				provider := &mockProvider{responses: []*llm.CompletionResponse{
					{Content: "normal final answer", StopReason: "end_turn"},
				}}
				result, err := executeAgentLoopWithEvents(
					t.Context(), provider, []llm.Message{{Role: "user", Content: "work"}}, "", "test-model",
					modelSettings{maxTokens: 4096}, definitions, nil, nil, common.NewFakeEventRecorder(), tc,
				)
				require.NoError(t, err)
				require.Equal(t, "normal final answer", result)
			}
		})
	}
}

func TestNativeGatewayReplyUnavailableDependenciesDenyToolOnly(t *testing.T) {
	for _, dependency := range []string{"task read", "transport", "response transport", "projected token"} {
		t.Run(dependency, func(t *testing.T) {
			logs := captureNativeReplyStderr(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if dependency == "response transport" {
					w.Header().Set("Content-Length", "1000")
					_, _ = fmt.Fprint(w, `{"taskUID":`)
					return
				}
				t.Error("unavailable dependency must not grant origin")
			}))
			defer server.Close()
			if dependency != "response transport" {
				server.Close()
			}
			path := filepath.Join(t.TempDir(), "token")
			if dependency != "projected token" {
				require.NoError(t, os.WriteFile(path, []byte("projected-token"), 0600))
			}
			env := workerenv.AIWorkerEnv{
				BaseEnv: workerenv.BaseEnv{
					ControllerURL: server.URL, TaskNamespace: "default", TaskName: "task", TaskUID: "uid",
				},
				Tools: []string{"reply_in_conversation"}, GatewayReplyEnabled: true,
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: "uid"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if dependency == "task read" {
						return errors.New("temporary connection failure")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			sender, err := newNativeGatewayReplySender(
				t.Context(), func() (client.Reader, error) { return kube, nil }, env, path, nativeGatewayReplyBootstrapTimeout,
			)
			require.NoError(t, err)
			require.Nil(t, sender)
			reason := "origin_unavailable"
			if dependency == "task read" {
				reason = "task_read_unavailable"
			}
			require.Equal(t, "warning: reply_in_conversation omitted reason="+reason+"\n", logs())
		})
	}
}

func TestNativeGatewayReplyTokenReviewBackendUnavailable(t *testing.T) {
	tools.RegisterBuiltinTools()
	for _, failure := range []string{"create error", "status error", "authenticated status error", "invalid token"} {
		t.Run(failure, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: "uid"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, authenticationv1.AddToScheme(scheme))
			var reviews, handled atomic.Int32
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.CreateOption) error {
					if review, ok := obj.(*authenticationv1.TokenReview); ok {
						reviews.Add(1)
						if failure == "create error" {
							return errors.New("TokenReview backend connection failed")
						}
						if failure != "invalid token" {
							review.Status.Error = "private TokenReview authenticator diagnostic"
						}
						review.Status.Authenticated = failure == "authenticated status error"
						return nil
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()
			app := fiber.New()
			app.Get("/internal/v1/tasks/:namespace/:taskName/gateway-messages/origin",
				api.NewAuthMiddleware(kube, api.AuthConfig{ReportTokenReviewUnavailable: true}),
				func(c fiber.Ctx) error { handled.Add(1); return c.JSON(fiber.Map{"taskUID": "uid"}) })
			server := httptest.NewServer(adaptor.FiberApp(app))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "token")
			// A unique never-authenticated token forces real TokenReview on every attempt.
			require.NoError(t, os.WriteFile(path, []byte("uncached-"+server.URL), 0600))
			env := workerenv.AIWorkerEnv{
				BaseEnv: workerenv.BaseEnv{
					ControllerURL: server.URL, TaskNamespace: "default", TaskName: "task", TaskUID: "uid",
				},
				Tools: []string{"reply_in_conversation"}, GatewayReplyEnabled: true,
			}
			sender, err := newNativeGatewayReplySender(
				t.Context(), func() (client.Reader, error) { return kube, nil }, env, path, nativeGatewayReplyBootstrapTimeout,
			)
			require.Nil(t, sender)
			require.Zero(t, handled.Load(), "failed auth must never grant origin or reach its handler")
			tc := &tools.ToolContext{Namespace: "default", TaskID: "task", TaskUID: "uid", GatewayReplySender: sender}
			definitions := buildLLMTools(env.Tools, nil, tc)
			require.Empty(t, definitions)
			if failure == "invalid token" {
				require.Error(t, err, "a genuinely invalid token must remain fatal")
				require.Equal(t, int32(1), reviews.Load(), "invalid tokens must not retry")
				return
			}
			require.NoError(t, err, "backend outage must deny the optional tool, not fail the Task")
			require.Equal(t, int32(5), reviews.Load(), "backend failures must retry within the existing bound")
			provider := &mockProvider{responses: []*llm.CompletionResponse{
				{Content: "normal final answer", StopReason: "end_turn"},
			}}
			result, err := executeAgentLoopWithEvents(
				t.Context(), provider, []llm.Message{{Role: "user", Content: "work"}}, "", "test-model",
				modelSettings{maxTokens: 4096}, definitions, nil, nil, common.NewFakeEventRecorder(), tc,
			)
			require.NoError(t, err)
			require.Equal(t, "normal final answer", result)
		})
	}
}

func TestNativeGatewayReplyMissingCallIdentityFailsClosed(t *testing.T) {
	tools.RegisterBuiltinTools()
	sender := &nativeReplyRecorder{}
	tc := &tools.ToolContext{Namespace: "default", TaskID: "task", TaskUID: "uid", GatewayReplySender: sender}
	call := llm.ToolCall{Name: "reply_in_conversation", Arguments: json.RawMessage(`{"content":"working"}`)}
	provider := &mockProvider{responses: []*llm.CompletionResponse{
		{ToolCalls: []llm.ToolCall{call}, StopReason: "tool_use"},
		{Content: "done", StopReason: "end_turn"},
	}}
	_, err := executeAgentLoopWithEvents(
		t.Context(), provider, []llm.Message{{Role: "user", Content: "work"}}, "", "test-model",
		modelSettings{maxTokens: 4096},
		buildLLMTools([]string{"reply_in_conversation"}, nil, tc), nil, nil, common.NewFakeEventRecorder(), tc,
	)
	require.NoError(t, err)
	require.Empty(t, sender.requests)
}
