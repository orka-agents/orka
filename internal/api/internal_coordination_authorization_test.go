package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestInternalCoordinationRequiresProvenanceAdmission(t *testing.T) {
	h, _, dataStore := setupTestInternalHandlers()
	h.taskProvenanceProtected = false
	app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
	for _, session := range []string{"my-session", "prior"} {
		require.NoError(t, dataStore.CreateSession(t.Context(), &store.SessionRecord{
			Namespace: "default", Name: session, SessionType: "task",
		}))
		require.NoError(t, dataStore.AppendMessages(t.Context(), "default", session, []store.SessionMessage{{
			Role: "user", Content: "history from " + session,
		}}))
	}

	for _, query := range []string{"", "&sessionName=my-session"} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=history"+query, nil))
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var results []store.TranscriptSearchResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&results))
		require.Len(t, results, 1)
		require.Equal(t, "my-session", results[0].SessionName)
	}

	for _, request := range []taskScopedRequest{
		{method: http.MethodGet, path: "/internal/v1/sessions/default/search?query=history&sessionName=prior"},
		{method: http.MethodGet, path: "/internal/v1/messages/default/my-task?parentTask=coordinator"},
		{method: http.MethodGet, path: "/internal/v1/messages/default/my-task?parentTask=my-task"},
		{method: http.MethodPost, path: "/internal/v1/messages/default", body: map[string]string{
			"fromTask": "my-task", "toTask": "worker-b", "parentTask": "coordinator", "content": "update",
		}},
		{method: http.MethodPost, path: "/internal/v1/messages/default", body: map[string]string{
			"fromTask": "my-task", "toTask": "*", "parentTask": "my-task", "content": "update",
		}},
	} {
		t.Run(request.method+" "+request.path, func(t *testing.T) {
			resp := doTaskScopedInternalRequest(t, app, request)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
		})
	}
}

func TestInternalCoordinationLookupErrors(t *testing.T) {
	for _, lookup := range []struct {
		name string
		task string
		path string
		body any
	}{
		{name: "message target", task: "worker-b", path: "/internal/v1/messages/default", body: map[string]string{
			"fromTask": "my-task", "toTask": "worker-b", "parentTask": "coordinator", "content": "update",
		}},
		{name: "message parent", task: "coordinator", path: "/internal/v1/messages/default", body: map[string]string{
			"fromTask": "my-task", "toTask": "worker-b", "parentTask": "coordinator", "content": "update",
		}},
		{name: "inbox parent", task: "coordinator", path: "/internal/v1/messages/default/my-task?parentTask=coordinator&markRead=false"},
	} {
		for _, failure := range []struct {
			name   string
			err    error
			status int
		}{
			{name: "missing", err: apierrors.NewNotFound(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"}, lookup.task), status: http.StatusForbidden},
			{name: "timeout", err: apierrors.NewTimeoutError("backend unavailable", 1), status: http.StatusInternalServerError},
		} {
			t.Run(lookup.name+"/"+failure.name, func(t *testing.T) {
				h, _, _ := setupTestInternalHandlers()
				baseClient, ok := h.k8sClient.(client.WithWatch)
				require.True(t, ok)
				h.apiReader = interceptor.NewClient(baseClient, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1alpha1.Task); ok && key.Name == lookup.task {
							return failure.err
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
				app := newTaskScopedInternalApp(h, internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid"))
				method := http.MethodPost
				if lookup.body == nil {
					method = http.MethodGet
				}
				resp := doTaskScopedInternalRequest(t, app, taskScopedRequest{method: method, path: lookup.path, body: lookup.body})
				require.Equal(t, failure.status, resp.StatusCode)
			})
		}
	}
}
