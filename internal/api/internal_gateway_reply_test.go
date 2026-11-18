package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/workerclient"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func budgetRequest(t *testing.T, f *gatewayMessageAPIFixture, id string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/tasks/default/"+f.task.Name+"/gateway-messages/budget?requestID="+id, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp.StatusCode, body
}

func TestGatewayReplyExcludedFromCompatibilityAdvertisement(t *testing.T) {
	request := &llm.CompletionRequest{}
	injectOrkaTools(request)
	require.NotEmpty(t, request.Tools)
	for _, tool := range request.Tools {
		require.NotEqual(t, "reply_in_conversation", tool.Name)
	}
}

func TestInternalGatewayMessageBudgetRevocationAndReceipts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *gatewayMessageAPIFixture)
		status int
	}{
		{"receipt at cap", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.service.Config.InterimMessagesPerTask = 1
			withdrawGatewayMessageCapability(t, f)
		}, 200},
		{"forged pod", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.user.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"forged"}
		}, 403},
		{"forged origin", func(t *testing.T, f *gatewayMessageAPIFixture) {
			f.task.Spec.RequestedBy = nil
			require.NoError(t, f.h.k8sClient.Update(t.Context(), f.task))
		}, 403},
		{"revoked", func(t *testing.T, f *gatewayMessageAPIFixture) {
			require.NoError(t, f.db.RevokeTaskJob(t.Context(), store.TaskJobIdentity{Namespace: f.task.Namespace, TaskUID: string(f.task.UID), JobUID: f.task.Status.JobUID}))
		}, 403},
		{"nil service", func(t *testing.T, f *gatewayMessageAPIFixture) { f.h.gatewayService = nil }, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGatewayMessageAPIFixture(t)
			status, _ := f.request(t, []byte(`{"requestID":"existing","content":"private text"}`))
			require.Equal(t, 202, status)
			test.mutate(t, f)
			status, b := budgetRequest(t, f, "existing")
			require.Equal(t, test.status, status)
			if status == 200 {
				require.Equal(t, map[string]any{"accepted": float64(1), "limit": float64(1), "requestExists": true}, b)
			}
			rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: "default"})
			require.NoError(t, err)
			require.Len(t, rows, 1)
		})
	}
	f := newGatewayMessageAPIFixture(t)
	withdrawGatewayMessageCapability(t, f)
	status, b := budgetRequest(t, f, "new")
	require.Equal(t, 409, status)
	require.Equal(t, "interim_delivery_unsupported", b["error"].(map[string]any)["code"])
}

func TestInternalGatewayMessageBudgetWriterRevocation(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	status, _ := f.request(t, []byte(`{"requestID":"existing","content":"working"}`))
	require.Equal(t, 202, status)
	revoked := false
	fresh := interceptor.NewClient(f.h.k8sClient.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		err := c.Get(ctx, key, obj, opts...)
		if _, ok := obj.(*gatewayv1alpha1.Gateway); ok && !revoked {
			require.NoError(t, f.db.RevokeTaskJob(ctx, store.TaskJobIdentity{Namespace: f.task.Namespace, TaskUID: string(f.task.UID), JobUID: f.task.Status.JobUID}))
			revoked = true
		}
		return err
	}})
	f.h.apiReader = fresh
	f.service.APIReader = fresh
	status, _ = budgetRequest(t, f, "existing")
	require.True(t, revoked)
	require.Equal(t, 403, status)
}

func TestNativeGatewayReplyToolAPIAndSQLite(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	f.service.Config.InterimMessagesPerTask = 2
	sessionBefore, err := f.db.GetSession(t.Context(), f.event.Namespace, f.event.SessionName)
	require.NoError(t, err)
	var dropFirstPost atomic.Bool
	dropFirstPost.Store(true)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := f.app.Test(r, fiber.TestConfig{Timeout: 0})
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer func() { require.NoError(t, response.Body.Close()) }()
		if r.Method == http.MethodPost {
			posts.Add(1)
			if dropFirstPost.Swap(false) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(f.token), 0600))
	sender, err := workerclient.New(workerclient.Config{ControllerURL: server.URL, Namespace: f.task.Namespace, TaskName: f.task.Name, TaskUID: string(f.task.UID), TokenFile: path})
	require.NoError(t, err)
	tc := &tools.ToolContext{Namespace: f.task.Namespace, TaskID: f.task.Name, TaskUID: string(f.task.UID), OperationID: "trusted-execution/call-1", GatewayReplySender: sender}
	tool := tools.NewReplyInConversationTool()
	args := json.RawMessage(`{"content":"working privately"}`)
	result, err := tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.Error(t, err)
	require.Empty(t, result)
	result, err = tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.NoError(t, err)
	require.Contains(t, result, `"created":false`)
	require.NotContains(t, result, "working privately")
	first := result
	tc.OperationID = "trusted-execution/call-2"
	result, err = tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.NoError(t, err)
	require.NotEqual(t, first, result)
	tc.OperationID = "trusted-execution/call-3"
	_, err = tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.ErrorContains(t, err, "limit")
	require.Equal(t, int32(3), posts.Load(), "Execute must deny before POST, independently of atomic admission")
	_, err = sender.Enqueue(t.Context(), "bypass-preflight", "working privately")
	require.ErrorContains(t, err, "lifetime message limit")
	tc.OperationID = "trusted-execution/call-1"
	_, err = tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.NoError(t, err)
	_, err = tool.Execute(tools.WithToolContext(t.Context(), tc), json.RawMessage(`{"content":"changed"}`))
	require.ErrorContains(t, err, "conflicts")
	require.NotContains(t, err.Error(), "unknown")
	withdrawGatewayMessageCapability(t, f)
	tc.OperationID = "trusted-execution/unsupported-call"
	_, err = tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.ErrorContains(t, err, "does not support interim delivery")
	require.NotContains(t, err.Error(), "budget")
	tc.OperationID = ""
	before := posts.Load()
	_, err = tool.Execute(tools.WithToolContext(t.Context(), tc), args)
	require.Error(t, err)
	require.Equal(t, before, posts.Load())
	rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.NotEqual(t, rows[0].ID, rows[1].ID)
	current := &corev1alpha1.Task{}
	require.NoError(t, f.h.k8sClient.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
	require.Equal(t, f.task, current)
	sessionAfter, err := f.db.GetSession(t.Context(), f.event.Namespace, f.event.SessionName)
	require.NoError(t, err)
	require.Equal(t, sessionBefore, sessionAfter)
	eventAfter, err := f.db.GetGatewayEvent(t.Context(), f.event.Namespace, f.event.ID)
	require.NoError(t, err)
	require.Equal(t, f.event, eventAfter)
}
