package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
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

func TestValidateGatewayTaskPassToolPermitsOnlyReviewedFields(t *testing.T) {
	valid := func() *corev1alpha1.Tool {
		tool := &corev1alpha1.Tool{}
		tool.Name = "lookup"
		tool.Spec = corev1alpha1.ToolSpec{
			Description:       "Look up a record",
			Parameters:        &apiextensionsv1.JSON{Raw: []byte(`{"type":"object"}`)},
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP: &corev1alpha1.HTTPExecution{
				GatewayTaskPass: true, URL: "https://gateway.example/v1/task-tools/lookup",
				Method: http.MethodPost, AuthInject: "header", Headers: map[string]string{},
				Timeout: &metav1.Duration{Duration: 10 * time.Second},
			},
		}
		return tool
	}
	require.NoError(t, ValidateGatewayTaskPassTool(valid()))

	// Every field outside the reviewed set, including fields added later, must
	// be refused once populated.
	permitted := map[string]map[string]bool{
		"ToolSpec":      {"Description": true, "Parameters": true, "BrokeredToolClass": true, "HTTP": true},
		"HTTPExecution": {"GatewayTaskPass": true, "URL": true, "Method": true, "Timeout": true, "AuthInject": true},
	}
	for _, target := range []struct {
		name   string
		object func(*corev1alpha1.Tool) reflect.Value
	}{
		{"ToolSpec", func(tool *corev1alpha1.Tool) reflect.Value { return reflect.ValueOf(&tool.Spec).Elem() }},
		{"HTTPExecution", func(tool *corev1alpha1.Tool) reflect.Value { return reflect.ValueOf(tool.Spec.HTTP).Elem() }},
	} {
		fields := target.object(valid()).Type()
		for i := range fields.NumField() {
			field := fields.Field(i)
			if permitted[target.name][field.Name] {
				continue
			}
			t.Run(target.name+"."+field.Name, func(t *testing.T) {
				tool := valid()
				setNonZero(t, target.object(tool).Field(i))
				require.Error(t, ValidateGatewayTaskPassTool(tool))
			})
		}
	}
}

func setNonZero(t *testing.T, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
	case reflect.Map:
		entries := reflect.MakeMap(value.Type())
		entries.SetMapIndex(reflect.Zero(value.Type().Key()), reflect.Zero(value.Type().Elem()))
		value.Set(entries)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	default:
		t.Fatalf("add a non-zero value for field kind %s", value.Kind())
	}
}
