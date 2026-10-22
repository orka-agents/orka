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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const containerSpecArgsBase = `"image":"mcr.microsoft.com/playwright:v1.64.0-noble","args":["sh","-c","echo ok"]`

// containerSpecArgsPath runs create_container_task through the registry, as
// chat and worker calls do, against one of its two Task construction paths.
type containerSpecArgsPath struct {
	name string
	run  func(t *testing.T, allowed []string, args string) (string, client.Client)
}

func containerSpecArgsPaths() []containerSpecArgsPath {
	execute := func(ctx context.Context, tool Tool, args string) string {
		registry := NewRegistry()
		registry.Register(tool)
		result, err := registry.Execute(ctx, createContainerTaskToolName, json.RawMessage(args))
		if err != nil {
			return fmt.Sprintf(`{"success":false,"errorType":"argument_error","error":%q}`, err.Error())
		}
		return result
	}
	return []containerSpecArgsPath{
		{name: "chat", run: func(t *testing.T, allowed []string, args string) (string, client.Client) {
			fc := newFakeClient()
			ctx := newCreateContainerTaskToolCtx(fc)
			GetToolContext(ctx).ContainerTaskRuntimeClasses = allowed
			return execute(ctx, &CreateContainerTaskTool{}, args), fc
		}},
		{name: "coordinator", run: func(t *testing.T, allowed []string, args string) (string, client.Client) {
			t.Setenv(envOrkaTaskName, parentTaskName)
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)
			fc := newFakeClient(parentTask())
			ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, ContainerTaskRuntimeClasses: allowed})
			return execute(ctx, NewCreateContainerTaskTool(fc), args), fc
		}},
	}
}

func createdContainerTasks(t *testing.T, fc client.Client) []corev1alpha1.Task {
	t.Helper()
	list := &corev1alpha1.TaskList{}
	if err := fc.List(context.Background(), list); err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var created []corev1alpha1.Task
	for _, task := range list.Items {
		if task.Spec.Type == corev1alpha1.TaskTypeContainer {
			created = append(created, task)
		}
	}
	return created
}

func TestCreateContainerTaskTool_AppliesBrowserTaskSpecArgs(t *testing.T) {
	args := `{` + containerSpecArgsBase + `,
		"env":[{"name":"PLAYWRIGHT_BROWSERS_PATH","value":"/ms-playwright"},{"name":"PORT","value":8080},{"name":"EMPTY"}],
		"resources":{"requests":{"cpu":"500m","memory":"1Gi"},"limits":{"cpu":"2","memory":"2Gi"}},
		"retryPolicy":{"maxRetries":"2","backoffMultiplier":3,"initialDelay":"30s"},
		"execution":{"runtimeClassName":"gvisor"}}`
	for _, path := range containerSpecArgsPaths() {
		t.Run(path.name, func(t *testing.T) {
			result, fc := path.run(t, []string{"kata-qemu", "gvisor"}, args)
			expectContainerTaskSuccess(t, result)
			created := createdContainerTasks(t, fc)
			if len(created) != 1 {
				t.Fatalf("created %d container tasks, want 1", len(created))
			}
			spec := created[0].Spec
			wantEnv := []corev1.EnvVar{{Name: "PLAYWRIGHT_BROWSERS_PATH", Value: "/ms-playwright"}, {Name: "PORT", Value: "8080"}, {Name: "EMPTY"}}
			if fmt.Sprint(spec.Env) != fmt.Sprint(wantEnv) {
				t.Errorf("env = %v, want %v", spec.Env, wantEnv)
			}
			for name, want := range map[string]string{"requests.cpu": "500m", "requests.memory": "1Gi", "limits.cpu": "2", "limits.memory": "2Gi"} {
				list := spec.Resources.Requests
				if strings.HasPrefix(name, "limits.") {
					list = spec.Resources.Limits
				}
				got := list[corev1.ResourceName(name[strings.Index(name, ".")+1:])]
				if got.Cmp(resource.MustParse(want)) != 0 {
					t.Errorf("resources.%s = %s, want %s", name, got.String(), want)
				}
			}
			policy := spec.RetryPolicy
			if policy == nil || policy.MaxRetries != 2 || policy.BackoffMultiplier != 3 || policy.InitialDelay == nil || policy.InitialDelay.Duration != 30*time.Second {
				t.Errorf("retryPolicy = %#v", policy)
			}
			if spec.Execution == nil || spec.Execution.RuntimeClassName != "gvisor" || spec.Execution.NodeSelector != nil {
				t.Errorf("execution = %#v", spec.Execution)
			}
		})
	}
}

func TestCreateContainerTaskTool_RejectsInvalidSpecArgs(t *testing.T) {
	manyEnv := make([]string, maxContainerTaskEnvVars+1)
	for i := range manyEnv {
		manyEnv[i] = fmt.Sprintf(`{"name":"V%d","value":"x"}`, i)
	}
	tests := []struct {
		name      string
		allowed   []string
		fragment  string
		errorType string
		mention   string
	}{
		{name: "env valueFrom", fragment: `"env":[{"name":"TOKEN","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]`, mention: "env[0].valueFrom"},
		{name: "env unknown field", fragment: `"env":[{"name":"A","value":"b","from":"c"}]`, mention: `"from"`},
		{name: "env missing name", fragment: `"env":[{"value":"b"}]`, mention: "env[0].name is required"},
		{name: "env invalid name", fragment: `"env":[{"name":"1BAD","value":"b"}]`, mention: "not a valid environment variable name"},
		{name: "env reserved ORKA name", fragment: `"env":[{"name":"ORKA_RESULT_ENDPOINT","value":"http://evil"}]`, errorType: "reserved_env_name", mention: "ORKA_RESULT_ENDPOINT"},
		{name: "env reserved telemetry name", fragment: `"env":[{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://evil"}]`, errorType: "reserved_env_name", mention: "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{name: "env duplicate", fragment: `"env":[{"name":"A","value":"1"},{"name":"A","value":"2"}]`, mention: "duplicated"},
		{name: "env too many", fragment: `"env":[` + strings.Join(manyEnv, ",") + `]`, mention: "at most 64"},
		{name: "env value too long", fragment: `"env":[{"name":"A","value":"` + strings.Repeat("x", maxContainerTaskEnvValueBytes+1) + `"}]`, mention: "env[0].value"},
		{name: "env null item", fragment: `"env":[null]`, mention: "env[0]"},
		{name: "malformed quantity", fragment: `"resources":{"limits":{"cpu":"two"}}`, mention: "resources.limits.cpu"},
		{name: "negative quantity", fragment: `"resources":{"requests":{"memory":"-1Gi"}}`, mention: "resources.requests.memory"},
		{name: "request above limit", fragment: `"resources":{"requests":{"cpu":"4"},"limits":{"cpu":"2"}}`, mention: "resources.requests.cpu"},
		{name: "too many retries", fragment: `"retryPolicy":{"maxRetries":6}`, mention: "maxRetries"},
		{name: "negative retries", fragment: `"retryPolicy":{"maxRetries":-1}`, mention: "maxRetries"},
		{name: "fractional retries", fragment: `"retryPolicy":{"maxRetries":1.5}`, mention: "maxRetries"},
		{name: "low backoff", fragment: `"retryPolicy":{"backoffMultiplier":0.5}`, mention: "backoffMultiplier"},
		{name: "high backoff", fragment: `"retryPolicy":{"backoffMultiplier":11}`, mention: "backoffMultiplier"},
		{name: "malformed delay", fragment: `"retryPolicy":{"initialDelay":"soon"}`, mention: "initialDelay"},
		{name: "zero delay", fragment: `"retryPolicy":{"initialDelay":"0s"}`, mention: "initialDelay"},
		{name: "long delay", fragment: `"retryPolicy":{"initialDelay":"10m"}`, mention: "initialDelay"},
		{name: "runtime class without allowlist", fragment: `"execution":{"runtimeClassName":"gvisor"}`, errorType: "runtime_class_not_allowed", mention: "gvisor"},
		{name: "unlisted runtime class", allowed: []string{"kata-qemu"}, fragment: `"execution":{"runtimeClassName":"gvisor"}`, errorType: "runtime_class_not_allowed", mention: "kata-qemu"},
		{name: "node placement", allowed: []string{"gvisor"}, fragment: `"execution":{"runtimeClassName":"gvisor","nodeSelector":{"pool":"gpu"}}`, mention: "execution.nodeSelector"},
	}
	for _, tt := range tests {
		for _, path := range containerSpecArgsPaths() {
			t.Run(tt.name+"/"+path.name, func(t *testing.T) {
				result, fc := path.run(t, tt.allowed, `{`+containerSpecArgsBase+`,`+tt.fragment+`}`)
				r := parseContainerTaskToolResult(t, result)
				if r.Success {
					t.Fatalf("accepted %s", tt.fragment)
				}
				if tt.errorType != "" && r.ErrorType != tt.errorType {
					t.Errorf("errorType = %q, want %q", r.ErrorType, tt.errorType)
				}
				if text := r.Error + " " + r.Suggestion; !strings.Contains(text, tt.mention) {
					t.Errorf("error %q does not mention %q", text, tt.mention)
				}
				if created := createdContainerTasks(t, fc); len(created) != 0 {
					t.Fatalf("created %d container tasks for a rejected call", len(created))
				}
			})
		}
	}
}
