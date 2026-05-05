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

func TestCreateContainerTaskTool_Name(t *testing.T) {
	tool := &CreateContainerTaskTool{}
	if got := tool.Name(); got != "create_container_task" {
		t.Errorf("Name() = %v, want %v", got, "create_container_task")
	}
}

func TestCreateContainerTaskTool_Description(t *testing.T) {
	tool := &CreateContainerTaskTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCreateContainerTaskTool_Parameters(t *testing.T) {
	tool := &CreateContainerTaskTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{"name", "image", "command", "args", "workspace", "prior_task", "namespace", "timeout", "priority", "schedule"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func newCreateContainerTaskToolCtx(fc client.Client) context.Context {
	taskCounter := 0
	tc := &ToolContext{
		Client:    fc,
		Namespace: "default",
		GenerateTaskName: func() string {
			taskCounter++
			return "container-task-1"
		},
		TaskLabels: func() map[string]string {
			return map[string]string{"orka.ai/managed": "true"}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() { taskCounter++ },
	}
	return WithToolContext(context.Background(), tc)
}

func parseContainerTaskToolResult(t *testing.T, result string) ChatToolResult {
	t.Helper()

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	return r
}

func expectContainerTaskSuccess(t *testing.T, result string) {
	t.Helper()

	r := parseContainerTaskToolResult(t, result)
	if !r.Success {
		t.Errorf("expected success, got error: %s", r.Error)
	}
}

func expectContainerTaskCreated(t *testing.T, result string) {
	t.Helper()

	r := parseContainerTaskToolResult(t, result)
	if !r.Success {
		t.Errorf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["name"] != "container-task-1" {
		t.Errorf("name = %v, want container-task-1", data["name"])
	}
	if data["namespace"] != "default" {
		t.Errorf("namespace = %v, want default", data["namespace"])
	}
	if data["phase"] != "Pending" {
		t.Errorf("phase = %v, want Pending", data["phase"])
	}
}

func expectContainerTaskScheduled(t *testing.T, result string) {
	t.Helper()

	r := parseContainerTaskToolResult(t, result)
	if !r.Success {
		t.Errorf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	msg := data["message"].(string)
	if msg == "Task created" {
		t.Error("expected scheduled message, got one-time message")
	}
}

func expectContainerTaskError(t *testing.T, result, errorType string) {
	t.Helper()

	r := parseContainerTaskToolResult(t, result)
	if r.Success {
		t.Fatalf("expected failure with %s", errorType)
	}
	if r.ErrorType != errorType {
		t.Errorf("errorType = %v, want %s", r.ErrorType, errorType)
	}
}

func expectContainerTaskWorkspace(t *testing.T, fc client.Client) {
	t.Helper()

	task := &corev1alpha1.Task{}
	key := client.ObjectKey{Name: "container-task-1", Namespace: "default"}
	if err := fc.Get(context.Background(), key, task); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}
	if task.Spec.Workspace == nil {
		t.Fatal("expected workspace")
	}
	if task.Spec.Workspace.GitRepo != "https://github.com/example/repo.git" {
		t.Errorf("gitRepo = %q", task.Spec.Workspace.GitRepo)
	}
	if task.Spec.Workspace.Ref != "abc123" {
		t.Errorf("ref = %q", task.Spec.Workspace.Ref)
	}
	if task.Spec.Workspace.GitSecretRef == nil || task.Spec.Workspace.GitSecretRef.Name != testGitCredentialsSecret {
		t.Fatalf("gitSecretRef = %#v", task.Spec.Workspace.GitSecretRef)
	}
	if task.Spec.PriorTaskRef == nil || task.Spec.PriorTaskRef.Name != "coder-task" {
		t.Fatalf("priorTaskRef = %#v", task.Spec.PriorTaskRef)
	}
}

func TestCreateContainerTaskTool_Execute(t *testing.T) {
	tests := []struct {
		name        string
		args        json.RawMessage
		objects     []client.Object
		checkResult func(t *testing.T, result string)
		checkClient func(t *testing.T, fc client.Client)
	}{
		{
			name:        "happy path with image and command",
			args:        json.RawMessage(`{"name":"my-task","image":"alpine:latest","command":["sh","-c"],"args":["echo hello"]}`),
			checkResult: expectContainerTaskCreated,
		},
		{
			name:        "minimal args - name only",
			args:        json.RawMessage(`{"name":"min-task"}`),
			checkResult: expectContainerTaskSuccess,
		},
		{
			name:        "with priority and timeout",
			args:        json.RawMessage(`{"name":"my-task","image":"busybox","timeout":"5m","priority":100}`),
			checkResult: expectContainerTaskSuccess,
		},
		{
			name:        "with schedule",
			args:        json.RawMessage(`{"name":"cron-task","image":"busybox","schedule":"*/5 * * * *"}`),
			checkResult: expectContainerTaskScheduled,
		},
		{
			name: "with workspace",
			args: json.RawMessage(`{"name":"validation","image":"golang:1.26","command":["sh","-lc"],"args":["go test ./..."],"workspace":{"gitRepo":"https://github.com/example/repo.git","branch":"feature","ref":"abc123","gitSecretRef":"git-credentials","subPath":"src"},"prior_task":"coder-task"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{Name: "coder-task", Namespace: "default"},
					Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				},
			},
			checkResult: expectContainerTaskSuccess,
			checkClient: expectContainerTaskWorkspace,
		},
		{
			name: "repo validation without workspace fails",
			args: json.RawMessage(`{"name":"validation","image":"golang:1.26","command":["sh","-lc"],"args":["go test ./..."]}`),
			checkResult: func(t *testing.T, result string) {
				expectContainerTaskError(t, result, "missing_workspace")
			},
		},
		{
			name: "invalid JSON args",
			args: json.RawMessage(`{bad json`),
			checkResult: func(t *testing.T, result string) {
				expectContainerTaskError(t, result, errTypeInvalidArgs)
			},
		},
		{
			name: "invalid timeout",
			args: json.RawMessage(`{"name":"t","timeout":"xyz"}`),
			checkResult: func(t *testing.T, result string) {
				expectContainerTaskError(t, result, errTypeInvalidArgs)
			},
		},
		{
			name: "k8s already exists error",
			args: json.RawMessage(`{"name":"existing"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "container-task-1",
						Namespace: "default",
					},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
				},
			},
			checkResult: func(t *testing.T, result string) {
				expectContainerTaskError(t, result, "already_exists")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient(tt.objects...)
			ctx := newCreateContainerTaskToolCtx(fc)
			tool := &CreateContainerTaskTool{}

			result, err := tool.Execute(ctx, tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.checkResult != nil {
				tt.checkResult(t, result)
			}
			if tt.checkClient != nil {
				tt.checkClient(t, fc)
			}
		})
	}
}

func TestCreateContainerTaskTool_Execute_MissingContext(t *testing.T) {
	tool := &CreateContainerTaskTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"t"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Error("expected failure for missing context")
	}
	if r.ErrorType != "internal_error" {
		t.Errorf("errorType = %v, want internal_error", r.ErrorType)
	}
}
