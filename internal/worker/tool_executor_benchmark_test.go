/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func BenchmarkToolExecutorTelemetryDisabled(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "bench_http"},
		Spec:       corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: server.URL}},
	}
	args := json.RawMessage(`{"x":1}`)
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		if _, err := executor.Execute(ctx, tool, args); err != nil {
			b.Fatalf("Execute() error = %v", err)
		}
	}
}
