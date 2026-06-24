/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestCoordinatorReadDeleteToolsEnforceNamespaceScope(t *testing.T) {
	victimTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-task", Namespace: "victim-ns"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	victimAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-agent", Namespace: "victim-ns"},
	}

	tests := []struct {
		name    string
		tool    Tool
		args    map[string]any
		objects []client.Object
	}{
		{name: "check task progress", tool: &CheckTaskProgressTool{}, args: map[string]any{nameField: "victim-task", namespaceField: "victim-ns"}, objects: []client.Object{victimTask}},
		{name: "fetch task output", tool: &FetchTaskOutputTool{}, args: map[string]any{nameField: "victim-task", namespaceField: "victim-ns"}, objects: []client.Object{victimTask}},
		{name: "wait for task", tool: &WaitForTaskTool{}, args: map[string]any{nameField: "victim-task", namespaceField: "victim-ns", timeoutField: 1}, objects: []client.Object{victimTask}},
		{name: "cancel task", tool: &ChatCancelTaskTool{}, args: map[string]any{nameField: "victim-task", namespaceField: "victim-ns"}, objects: []client.Object{victimTask}},
		{name: "list tasks", tool: &ListTasksTool{}, args: map[string]any{namespaceField: "victim-ns"}, objects: []client.Object{victimTask}},
		{name: "list agents", tool: &ListAgentsTool{}, args: map[string]any{namespaceField: "victim-ns"}, objects: []client.Object{victimAgent}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient(tt.objects...)
			ctx := WithToolContext(context.Background(), &ToolContext{
				Client:                    fc,
				Namespace:                 defaultNamespace,
				WatchNamespace:            defaultNamespace,
				EnforceNamespaceIsolation: true,
				ResultStore:               &fakeResultStore{data: map[string][]byte{"victim-ns/victim-task": []byte("secret output")}},
			})

			argsJSON, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("failed to marshal args: %v", err)
			}
			result, err := tt.tool.Execute(ctx, argsJSON)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if res.Success {
				t.Fatalf("expected namespace permission error, got success: %s", result)
			}
			if res.ErrorType != "permission_denied" {
				t.Fatalf("errorType = %q, want permission_denied; result=%s", res.ErrorType, result)
			}
		})
	}
}
