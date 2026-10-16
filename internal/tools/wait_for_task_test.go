/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestWaitForTaskTool_Name(t *testing.T) {
	tool := &WaitForTaskTool{}
	if got := tool.Name(); got != waitForTaskToolName {
		t.Errorf("Name() = %q, want %q", got, waitForTaskToolName)
	}
}

func TestWaitForTaskTool_Description(t *testing.T) {
	tool := &WaitForTaskTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestWaitForTaskTool_Parameters(t *testing.T) {
	tool := &WaitForTaskTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props[nameField]; !ok {
		t.Error("missing name property")
	}
	timeout, ok := props[timeoutField].(map[string]any)
	if !ok {
		t.Fatal("missing timeout property")
	}
	if got := timeout[jsonSchemaMinimumField]; got != float64(1) {
		t.Errorf("timeout minimum = %v, want 1", got)
	}
	if got := timeout[jsonSchemaMaximumField]; got != float64(60) {
		t.Errorf("timeout maximum = %v, want 60", got)
	}
}

func TestWaitForTaskTool_Execute(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name       string
		args       string
		task       *corev1alpha1.Task
		wantPhase  string
		wantErrStr string
	}{
		{
			name: "task already succeeded",
			args: `{"name": "done-task"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "done-task",
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:          corev1alpha1.TaskPhaseSucceeded,
					StartTime:      &now,
					CompletionTime: &now,
				},
			},
			wantPhase: taskPhaseSucceededString,
		},
		{
			name: "task already failed",
			args: `{"name": "failed-task"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-task",
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:          corev1alpha1.TaskPhaseFailed,
					Message:        "something went wrong",
					StartTime:      &now,
					CompletionTime: &now,
				},
			},
			wantPhase: taskPhaseFailedString,
		},
		{
			name: "task still running — returns timeout result",
			args: `{"name": "running-task", "timeout": 1}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testRunningTaskName,
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:     corev1alpha1.TaskPhaseRunning,
					StartTime: &now,
				},
			},
			wantPhase: taskPhaseRunningString,
		},
		{
			name: testCustomNamespaceCaseName,
			args: `{"name": "ns-task", "namespace": "prod"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ns-task",
					Namespace: testProdNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseSucceeded,
				},
			},
			wantPhase: taskPhaseSucceededString,
		},
		{
			name:       "negative timeout",
			args:       `{"name": "running-task", "timeout": -1}`,
			wantErrStr: "timeout must be at least 1 second",
		},
		{
			name:       "zero timeout",
			args:       `{"name": "running-task", "timeout": 0}`,
			wantErrStr: "timeout must be at least 1 second",
		},
		{
			name:       "missing name argument",
			args:       `{}`,
			wantErrStr: "name is required",
		},
		{
			name:       invalidJSONArgsCaseName,
			args:       `{bad json}`,
			wantErrStr: failedToParseArgumentsMessage,
		},
		{
			name:       taskNotFoundCaseName,
			args:       `{"name": "nonexistent"}`,
			wantErrStr: errTypeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &WaitForTaskTool{}

			fc := newFakeClient()
			if tt.task != nil {
				fc = newFakeClient(tt.task)
			}

			tc := &ToolContext{
				Client:    fc,
				Namespace: defaultNamespace,
			}
			ctx := WithToolContext(context.Background(), tc)

			result, err := tool.Execute(ctx, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantErrStr != "" {
				if !strings.Contains(result, tt.wantErrStr) {
					t.Errorf("result = %q, want to contain %q", result, tt.wantErrStr)
				}
				return
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Success {
				t.Errorf("expected success=true, got false; result: %s", result)
			}

			data, ok := res.Data.(map[string]any)
			if !ok {
				t.Fatalf("expected data map, got %T", res.Data)
			}
			if phase, _ := data[phaseField].(string); phase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", phase, tt.wantPhase)
			}
		})
	}
}

func TestWaitForTaskTool_Execute_MissingToolContext(t *testing.T) {
	tool := &WaitForTaskTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name": "test"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "missing tool context") {
		t.Errorf("expected missing tool context error, got %q", result)
	}
}

func TestWaitForTaskTool_Execute_ContextCancelled(t *testing.T) {
	now := metav1.Now()
	tool := &WaitForTaskTool{}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRunningTaskName,
			Namespace: defaultNamespace,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			StartTime: &now,
		},
	}

	tc := &ToolContext{
		Client:    newFakeClient(task),
		Namespace: defaultNamespace,
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithToolContext(ctx, tc)
	cancel() // cancel immediately

	result, err := tool.Execute(ctx, json.RawMessage(`{"name": "running-task", "timeout": 60}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return timeout/still-running result
	if !strings.Contains(result, "still running") {
		t.Errorf("expected still running message, got %q", result)
	}
}

func TestWaitForTaskTool_RegistryTimeout(t *testing.T) {
	tests := []struct {
		name        string
		args        string
		wantInvalid bool
	}{
		{name: "negative number", args: `{"name":"running-task","timeout":-1}`, wantInvalid: true},
		{name: "negative numeric string", args: `{"name":"running-task","timeout":"-1"}`, wantInvalid: true},
		{name: "zero number", args: `{"name":"running-task","timeout":0}`, wantInvalid: true},
		{name: "zero numeric string", args: `{"name":"running-task","timeout":"0"}`, wantInvalid: true},
		{name: "minimum int64 number", args: `{"name":"running-task","timeout":-9223372036854775808}`, wantInvalid: true},
		{name: "minimum int64 string", args: `{"name":"running-task","timeout":"-9223372036854775808"}`, wantInvalid: true},
		{name: "omitted timeout", args: `{"name":"running-task"}`},
		{name: "null timeout is omitted", args: `{"name":"running-task","timeout":null}`},
		{name: "minimum number", args: `{"name":"running-task","timeout":1}`},
		{name: "minimum numeric string", args: `{"name":"running-task","timeout":"1"}`},
		{name: "default number", args: `{"name":"running-task","timeout":30}`},
		{name: "default numeric string", args: `{"name":"running-task","timeout":"30"}`},
		{name: "maximum number", args: `{"name":"running-task","timeout":60}`},
		{name: "maximum numeric string", args: `{"name":"running-task","timeout":"60"}`},
		{name: "capped number", args: `{"name":"running-task","timeout":61}`},
		{name: "capped numeric string", args: `{"name":"running-task","timeout":"61"}`},
		{name: "maximum int64 number", args: `{"name":"running-task","timeout":9223372036854775807}`},
		{name: "maximum int64 string", args: `{"name":"running-task","timeout":"9223372036854775807"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: testRunningTaskName, Namespace: defaultNamespace},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			reads := 0
			fc := newFakeClientWithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					err := c.Get(ctx, key, obj, opts...)
					// Cancel after the first read so valid controls never wait for a poll.
					cancel()
					return err
				},
			}, task)
			ctx = WithToolContext(ctx, &ToolContext{Client: fc, Namespace: defaultNamespace})
			registry := NewRegistry()
			registry.Register(&WaitForTaskTool{})

			result, err := registry.Execute(ctx, waitForTaskToolName, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("Registry.Execute() error = %v", err)
			}
			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if tt.wantInvalid {
				if res.Success || res.ErrorType != invalidArgumentsErrorType || res.Error != "timeout must be at least 1 second" {
					t.Errorf("result = %s, want invalid_arguments for nonpositive timeout", result)
				}
				if reads != 0 {
					t.Errorf("Task reads = %d, want no initial read or polling", reads)
				}
				return
			}
			if !res.Success {
				t.Fatalf("result = %s, want successful continuation", result)
			}
			data, ok := res.Data.(map[string]any)
			message, _ := data[messageField].(string)
			if !ok || data[phaseField] != taskPhaseRunningString || !strings.Contains(message, "Call wait_for_task again") {
				t.Errorf("result = %s, want running Task with continuation message", result)
			}
			if reads != 1 {
				t.Errorf("Task reads = %d, want only the initial read", reads)
			}
		})
	}
}

func TestWaitForTaskTool_Execute_TimeoutClampsToMax(t *testing.T) {
	for _, timeout := range []int64{60, 61, 9223372036854775807} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				task := &corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{Name: testRunningTaskName, Namespace: defaultNamespace},
					Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
				}
				ctx := WithToolContext(context.Background(), &ToolContext{Client: newFakeClient(task), Namespace: defaultNamespace})
				registry := NewRegistry()
				registry.Register(&WaitForTaskTool{})
				started := time.Now()
				result, err := registry.Execute(ctx, waitForTaskToolName,
					json.RawMessage(fmt.Sprintf(`{"name":"running-task","timeout":%d}`, timeout)))
				if err != nil || !strings.Contains(result, "still running") {
					t.Fatalf("wait = %q, %v; want running-task continuation", result, err)
				}
				// Polling can add one two-second tick after the capped deadline.
				if elapsed := time.Since(started); elapsed < 60*time.Second || elapsed > 62*time.Second {
					t.Fatalf("timeout %d waited %s, want the 60-second cap plus at most one poll", timeout, elapsed)
				}
			})
		})
	}
}
