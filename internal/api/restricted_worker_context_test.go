package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tools"
)

// The chart forces enforce mode. These exercise the existing controller deputy
// with that mode: a context token is a separate grant, not the worker's RBAC.
func TestRestrictedWorkerContextGrantHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, scope, namespace, taskType string
		want                             int
	}{
		{"read-only", ContextTokenScopeTaskGet, "default", "container", http.StatusForbidden},
		{"wrong namespace", ContextTokenScopeTaskCreate, "other", "container", http.StatusForbidden},
		{"wrong task type", ContextTokenScopeTaskCreate, "default", "ai", http.StatusForbidden},
		{"explicit create grant", ContextTokenScopeTaskCreate, "default", "container", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestOIDCProvider(t)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			backend := fake.NewClientBuilder().WithScheme(scheme).Build()
			app := setupTestCreateTaskHandlerWithAuthzReaders(t, testContextTokenConfig(t, provider, ""),
				ContextTokenAuthorizationModeEnforce, backend, backend)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": tc.scope,
				"tctx":  map[string]any{"namespace": tc.namespace, "taskType": tc.taskType},
			})
			req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(
				`{"name":"attempt","namespace":"default","type":"container","image":"busybox"}`))
			req.Header.Set(TransactionTokenHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			require.Equal(t, tc.want, resp.StatusCode)
			tasks := &corev1alpha1.TaskList{}
			require.NoError(t, backend.List(t.Context(), tasks))
			if tc.want == http.StatusCreated {
				require.Len(t, tasks.Items, 1, "a matching issuer grant is separate authority")
			} else {
				require.Empty(t, tasks.Items, "denied request must not persist a Task")
			}
		})
	}
}

func TestRestrictedWorkerContextGrantProxyTool(t *testing.T) {
	for _, profile := range []compatProxyToolContextProfile{openAICompatProxyToolContextProfile, anthropicCompatProxyToolContextProfile} {
		for _, tc := range []struct {
			name, scope, taskType string
			allowed               bool
		}{
			{"read-only", ContextTokenScopeTaskGet, "container", false},
			{"wrong task type", ContextTokenScopeTaskCreate, "ai", false},
			{"explicit create grant", ContextTokenScopeTaskCreate, "container", true},
		} {
			t.Run(profile.SourceLabel+"/"+tc.name, func(t *testing.T) {
				scheme := runtime.NewScheme()
				require.NoError(t, corev1alpha1.AddToScheme(scheme))
				backend := fake.NewClientBuilder().WithScheme(scheme).Build()
				token := &ContextToken{Scopes: []string{tc.scope}, TransactionContext: map[string]any{
					"namespace": "default", "taskType": tc.taskType,
				}}
				toolContext := newCompatProxyToolContext(compatProxyToolContextConfig{
					Client: backend, Namespace: "default", Profile: profile,
					AuthContext: token, UserInfo: contextTokenToUserInfo(token),
					AuthorizationConfig: enforceContextTokenAuthorizationConfig(),
					GenerateTaskName:    func() string { return "attempt" },
				})
				ctx := tools.WithToolContext(context.Background(), toolContext)
				result, err := tools.NewCreateContainerTaskTool(backend).Execute(ctx, json.RawMessage(`{"image":"busybox"}`))
				require.NoError(t, err)
				tasks := &corev1alpha1.TaskList{}
				require.NoError(t, backend.List(t.Context(), tasks))
				if tc.allowed {
					require.Len(t, tasks.Items, 1, result)
				} else {
					require.Empty(t, tasks.Items, "proxy tool must not use controller authority outside the token grant")
					require.Contains(t, result, "authorization_failed")
				}
			})
		}
	}
}
