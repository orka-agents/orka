package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/test/fixtures/compatmodel"
)

type compatRouterIntegration struct {
	url         string
	tokens      map[string]string
	clients     map[string]client.Client
	backends    map[string]string
	modelCalls  atomic.Int32
	created     atomic.Int32
	permissions atomic.Int32
}

func newCompatRouterIntegration(t *testing.T) *compatRouterIntegration {
	t.Helper()
	f := &compatRouterIntegration{tokens: map[string]string{}, clients: map[string]client.Client{}, backends: map[string]string{}}
	identities := map[string]string{}
	for _, namespace := range []string{"team-a", "team-b"} {
		for _, role := range []string{"editor", "chat-only", "denied"} {
			key := namespace + "/" + role
			f.tokens[key] = t.Name() + "/" + key
			identities[f.tokens[key]] = "system:serviceaccount:" + namespace + ":" + role
		}
	}
	authClient := compatRouterTokenClient(t, identities)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.modelCalls.Add(1)
		compatmodel.Handler(w, r)
	}))
	t.Cleanup(model.Close)
	for _, namespace := range []string{"team-a", "team-b"} {
		scheme := runtime.NewScheme()
		for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, authenticationv1.AddToScheme} {
			require.NoError(t, add(scheme))
		}
		db, err := sqlite.NewDB(":memory:")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		results := sqlite.NewStore(db, ":memory:")
		require.NoError(t, results.SaveResult(t.Context(), namespace, "same-task", []byte("RESULT:"+namespace)))
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(
			&corev1alpha1.Provider{
				ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: namespace},
				Spec: corev1alpha1.ProviderSpec{Type: corev1alpha1.ProviderTypeOpenAI, BaseURL: model.URL + "/v1", DefaultModel: namespace + "-catalog",
					SecretRef: corev1alpha1.ProviderSecretRef{Name: "same-secret", Key: "api-key"}},
			},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "same-secret", Namespace: namespace}, Data: map[string][]byte{"api-key": []byte("fixture-" + namespace)}},
			&corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "same-agent", Namespace: namespace}, Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: namespace + "-agent"}}},
			&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "same-task", Namespace: namespace}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded, ResultRef: &corev1alpha1.ResultReference{Available: true}}},
		).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*authenticationv1.TokenReview); ok {
					return authClient.Create(ctx, obj, opts...)
				}
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				if task, ok := obj.(*corev1alpha1.Task); ok {
					f.created.Add(1)
					// Complete the fixture immediately; deployed tests separately
					// require a real controller, worker Pod and persisted result.
					task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
					task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
					if err := c.Status().Update(ctx, task); err != nil {
						return err
					}
					return results.SaveResult(ctx, task.Namespace, task.Name, []byte("RESULT:"+namespace))
				}
				return nil
			},
		}).Build()
		f.clients[namespace] = kube
		clientset := kubefake.NewClientset()
		clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
			f.permissions.Add(1)
			a := review.Spec.ResourceAttributes
			correctIdentity := parseServiceAccountNamespace(review.Spec.User) == namespace && review.Spec.UID == "fixture-uid"
			review.Status.Allowed = correctIdentity && a.Namespace == namespace && a.Group == corev1alpha1.GroupVersion.Group &&
				(strings.HasSuffix(review.Spec.User, ":editor") || (strings.HasSuffix(review.Spec.User, ":chat-only") && (a.Resource == "chats" || a.Resource == "providers")))
			return true, review, nil
		})
		server := NewServer(kube, nil, ServerConfig{
			WatchNamespace: namespace, EnforceNamespaceIsolation: true, Clientset: clientset, ResultStore: results,
			Chat: ChatConfig{Enabled: true, MaxDuration: 30 * time.Second, ToolTimeout: 5 * time.Second, MaxIterations: 8},
		})
		f.backends[namespace] = serveCompatTestApp(t, server.app)
	}
	router, err := NewCompatRouter(authClient, f.backends)
	require.NoError(t, err)
	t.Cleanup(router.Close)
	entry := httptest.NewServer(router)
	t.Cleanup(entry.Close)
	f.url = entry.URL
	return f
}

func TestCompatRouterInstallationMisconfiguredRoute(t *testing.T) {
	f := newCompatRouterIntegration(t)
	authClient := compatRouterTokenClient(t, map[string]string{f.tokens["team-a/editor"]: "system:serviceaccount:team-a:editor"})
	router, err := NewCompatRouter(authClient, map[string]string{"team-a": f.backends["team-b"]})
	require.NoError(t, err)
	t.Cleanup(router.Close)
	entry := httptest.NewServer(router)
	t.Cleanup(entry.Close)
	f.url = entry.URL
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages", "/openai/v1/models", "/anthropic/v1/models"} {
		status, _ := f.request(t, "team-a", "editor", path, "create", false, false)
		require.Equal(t, http.StatusForbidden, status)
	}
	require.Zero(t, f.modelCalls.Load(), "the wrong installation must reject the request before Provider access")
	require.Zero(t, f.created.Load())
}

func serveCompatTestApp(t *testing.T, app *fiber.App) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		require.NoError(t, app.Shutdown())
		require.NoError(t, <-done)
	})
	return "http://" + listener.Addr().String()
}

func (f *compatRouterIntegration) request(t *testing.T, namespace, role, path, prompt string, stream, disabled bool) (int, string) {
	t.Helper()
	method := http.MethodPost
	if strings.Contains(path, "/models") {
		method = http.MethodGet
	}
	data, err := json.Marshal(map[string]any{
		"model": "shared/model", "messages": []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 1024, "stream": stream, "user": "metadata-must-not-select-a-namespace",
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), method, f.url+path, strings.NewReader(string(data)))
	require.NoError(t, err)
	if strings.HasPrefix(path, "/anthropic/") {
		req.Header.Set(XAPIKeyHeader, f.tokens[namespace+"/"+role])
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		req.Header.Set(AuthHeader, BearerPrefix+f.tokens[namespace+"/"+role])
	}
	req.Header.Set("Content-Type", "application/json")
	if disabled {
		req.Header.Set("X-Orka-Tools", "disabled")
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if stream && resp.StatusCode == http.StatusOK {
		require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
		if strings.HasPrefix(path, "/anthropic/") {
			require.Contains(t, string(body), "event: message_stop")
		} else {
			require.Contains(t, string(body), "data: [DONE]")
		}
	}
	return resp.StatusCode, string(body)
}

func TestCompatRouterInstallationIsolation(t *testing.T) {
	f := newCompatRouterIntegration(t)
	t.Run("concurrent namespaces and APIs", func(t *testing.T) {
		for _, namespace := range []string{"team-a", "team-b"} {
			for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s%s/stream=%t", namespace, path, stream), func(t *testing.T) {
						t.Parallel()
						other := "team-b"
						if namespace == "team-b" {
							other = "team-a"
						}
						for _, prompt := range []string{"plain", "read", "agents", "create"} {
							status, body := f.request(t, namespace, "editor", path, prompt, stream, false)
							require.Equal(t, http.StatusOK, status, body)
							require.Contains(t, body, "OUTPUT:"+namespace)
							require.NotContains(t, body, "OUTPUT:"+other)
							require.NotContains(t, body, "RESULT:"+other)
							require.NotContains(t, body, "fixture-team-", "Provider credentials must not reach the response")
							if prompt == "read" || prompt == "create" {
								require.Contains(t, body, "RESULT:"+namespace)
							}
							if prompt == "agents" {
								require.Contains(t, body, namespace+"-agent")
								require.NotContains(t, body, other+"-agent")
							}
						}
						status, body := f.request(t, namespace, "editor", path, "create", stream, true)
						require.Equal(t, http.StatusOK, status, body)
						require.Contains(t, body, "OUTPUT:"+namespace)
						require.Contains(t, body, "tools disabled")
						for _, prompt := range []string{"read " + other, "create " + other, "agents " + other} {
							status, body := f.request(t, namespace, "editor", path, prompt, stream, false)
							require.Equal(t, http.StatusOK, status, body)
							require.Contains(t, body, "success\\\":false")
							require.NotContains(t, body, "RESULT:"+other)
							require.NotContains(t, body, other+"-agent")
						}
					})
				}
			}
		}
	})
	require.EqualValues(t, 8, f.created.Load())
	require.Positive(t, f.permissions.Load())
	for _, namespace := range []string{"team-a", "team-b"} {
		for _, path := range []string{"/openai/v1/models", "/anthropic/v1/models"} {
			status, body := f.request(t, namespace, "editor", path, "", false, false)
			require.Equal(t, http.StatusOK, status, body)
			require.Contains(t, body, "shared/"+namespace+"-catalog")
			var models OAIModelList
			require.NoError(t, json.Unmarshal([]byte(body), &models))
			require.Equal(t, "list", models.Object)
			require.Len(t, models.Data, 2)
		}
	}
}

func TestCompatRouterInstallationRBACAndMissingProvider(t *testing.T) {
	f := newCompatRouterIntegration(t)
	for _, namespace := range []string{"team-a", "team-b"} {
		for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages", "/openai/v1/models", "/anthropic/v1/models"} {
			before := f.modelCalls.Load()
			status, _ := f.request(t, namespace, "denied", path, "create", false, false)
			require.Equal(t, http.StatusForbidden, status)
			require.Equal(t, before, f.modelCalls.Load(), "route RBAC must run before Provider access")
			status, _ = f.request(t, namespace, "editor", path+"?namespace=outside", "create", false, false)
			require.Equal(t, http.StatusForbidden, status)
			require.Equal(t, before, f.modelCalls.Load())
		}
		for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
			status, body := f.request(t, namespace, "chat-only", path, "create", false, false)
			require.Equal(t, http.StatusOK, status, body)
			require.Contains(t, body, "success\\\":false")
			require.Zero(t, f.created.Load(), "chat permission must not confer Task creation permission")
		}
	}
	// The other installation still has the same Provider and Secret names.
	require.NoError(t, f.clients["team-a"].Delete(t.Context(), &corev1alpha1.Provider{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"}}))
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/v1/messages"} {
		before := f.modelCalls.Load()
		status, body := f.request(t, "team-a", "editor", path, "plain", false, true)
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, body, "provider")
		require.Equal(t, before, f.modelCalls.Load(), "missing Provider must not fall back to another installation")
		status, body = f.request(t, "team-b", "editor", path, "plain", false, true)
		require.Equal(t, http.StatusOK, status, body)
		require.Contains(t, body, "OUTPUT:team-b")
	}
}
