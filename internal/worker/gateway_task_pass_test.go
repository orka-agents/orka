package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestGatewayTaskPassInjectionAndRefusal(t *testing.T) {
	calls, issued := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "Bearer ephemeral-pass", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Txn-Token"))
		require.Equal(t, "operation-1", r.Header.Get("Idempotency-Key"))
		if calls == 1 {
			_, _ = w.Write([]byte(`{"answer":"ephemeral-pass"}`))
			return
		}
		w.Header().Set("Location", "/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write([]byte("gateway-response-secret ephemeral-pass"))
	}))
	defer server.Close()
	tool := &corev1alpha1.Tool{}
	tool.Name = "lookup"
	require.NoError(t, json.Unmarshal([]byte(`{"gatewayTaskPass":true,"url":"`+server.URL+`/v1/task-tools/lookup"}`), &tool.Spec.HTTP))
	executor := NewToolExecutorForNamespace("default", nil, server.Client())
	executor.SetGatewayTaskPass(func(context.Context, *corev1alpha1.Tool) (string, error) { issued++; return "ephemeral-pass", nil }, server.Client())
	ctx := WithToolIdempotencyKey(t.Context(), "operation-1")
	result, err := executor.Execute(ctx, tool, json.RawMessage(`{"value":"x"}`))
	require.NoError(t, err)
	require.NotContains(t, result, "ephemeral-pass")
	_, err = executor.Execute(ctx, tool, json.RawMessage(`{}`))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "gateway-response-secret")
	require.Equal(t, 2, calls)
	for _, mutate := range []func(*corev1alpha1.Tool){
		func(t *corev1alpha1.Tool) {
			t.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "forbidden"}
		},
		func(t *corev1alpha1.Tool) {
			t.Spec.HTTP.OutboundAccessPolicyRef = &corev1alpha1.LocalObjectReference{Name: "forbidden"}
		},
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.Headers = map[string]string{"X": "y"} },
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.AuthInject = "body" },
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.AuthBodyKey = "token" },
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.Forward = &corev1alpha1.HTTPForward{} },
		func(t *corev1alpha1.Tool) { t.Spec.MCP = &corev1alpha1.MCPToolServer{} },
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.Method = "GET" },
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.URL += "?identity=other" },
		func(t *corev1alpha1.Tool) { t.Spec.HTTP.URL = strings.ReplaceAll(t.Spec.HTTP.URL, "lookup", "{value}") },
	} {
		changed := tool.DeepCopy()
		mutate(changed)
		_, err := executor.Execute(ctx, changed, json.RawMessage(`{}`))
		require.Error(t, err)
	}
	require.Equal(t, 2, issued, "invalid configuration must not acquire authority or read credentials")
	executor.SetGatewayTaskPass(func(context.Context, *corev1alpha1.Tool) (string, error) {
		return "", errors.New("gateway-response-secret")
	}, server.Client())
	_, err = executor.Execute(ctx, tool, json.RawMessage(`{}`))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "gateway-response-secret")
}

func TestGatewayTaskPassRefusesWorkerBeforeOutboundOrSecrets(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	var tool corev1alpha1.Tool
	require.NoError(t, json.Unmarshal([]byte(`{"metadata":{"name":"lookup"},"spec":{"http":{"gatewayTaskPass":true,"url":"`+server.URL+`/v1/task-tools/lookup"}}}`), &tool))
	executor := NewToolExecutorForNamespace("default", nil, server.Client())
	_, err := executor.Execute(t.Context(), &tool, json.RawMessage(`{}`))
	require.ErrorContains(t, err, "gateway task pass")
	require.Zero(t, calls)
}
