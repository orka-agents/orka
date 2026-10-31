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
	run  func(t *testing.T, configure func(*ToolContext), args string) (string, client.Client)
}

// allowRuntimeClasses configures the operator RuntimeClass allowlist.
func allowRuntimeClasses(names ...string) func(*ToolContext) {
	return func(tc *ToolContext) { tc.ContainerTaskRuntimeClasses = names }
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
		{name: "chat", run: func(t *testing.T, configure func(*ToolContext), args string) (string, client.Client) {
			fc := newFakeClient()
			ctx := newCreateContainerTaskToolCtx(fc)
			if configure != nil {
				configure(GetToolContext(ctx))
			}
			return execute(ctx, &CreateContainerTaskTool{}, args), fc
		}},
		{name: "coordinator", run: func(t *testing.T, configure func(*ToolContext), args string) (string, client.Client) {
			t.Setenv(envOrkaTaskName, parentTaskName)
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)
			fc := newFakeClient(parentTask())
			tc := &ToolContext{Client: fc}
			if configure != nil {
				configure(tc)
			}
			return execute(WithToolContext(context.Background(), tc), NewCreateContainerTaskTool(fc), args), fc
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
			result, fc := path.run(t, allowRuntimeClasses("kata-qemu", "gvisor"), args)
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
	// Sixteen values under the per-value cap that together pass the total cap.
	largeEnv := make([]string, 16)
	for i := range largeEnv {
		largeEnv[i] = fmt.Sprintf(`{"name":"BIG_%d","value":"%s"}`, i, strings.Repeat("x", 20000))
	}
	// Secret-backed entries have no literal value; their names still count.
	// Fifteen literal values stay under the total on their own; seventeen
	// Secret-backed entries with names at the name cap push it over.
	largeSecretEnv := make([]string, 0, 32)
	for i := range 15 {
		largeSecretEnv = append(largeSecretEnv, fmt.Sprintf(`{"name":"LIT_%d","value":"%s"}`, i, strings.Repeat("x", 16384)))
	}
	for i := range 17 {
		name := strings.Repeat("A", maxContainerTaskEnvNameBytes-4) + fmt.Sprintf("_%02d", i)
		largeSecretEnv = append(largeSecretEnv, fmt.Sprintf(`{"name":"%s","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}`, name))
	}
	tests := []struct {
		name      string
		allowed   []string
		fragment  string
		errorType string
		mention   string
	}{
		{name: "env configMap source", fragment: `"env":[{"name":"MODE","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}]`, mention: "env[0].valueFrom.configMapKeyRef"},
		{name: "env value and valueFrom", fragment: `"env":[{"name":"TOKEN","value":"x","valueFrom":{"secretKeyRef":{"name":"s","key":"k"}}}]`, mention: "both value and valueFrom"},
		{name: "env secret without key", fragment: `"env":[{"name":"TOKEN","valueFrom":{"secretKeyRef":{"name":"s"}}}]`, mention: "secretKeyRef.key is required"},
		{name: "env invalid secret name", fragment: `"env":[{"name":"TOKEN","valueFrom":{"secretKeyRef":{"name":"Bad_Name","key":"k"}}}]`, mention: "not a valid Secret name"},
		{name: "env secret unknown field", fragment: `"env":[{"name":"TOKEN","valueFrom":{"secretKeyRef":{"name":"s","key":"k","optional":true}}}]`, mention: `"optional"`},
		{name: "secretRef namespace", fragment: `"secretRef":{"name":"s","namespace":"other-team"}`, mention: "secretRef.namespace"},
		{name: "secretRef without name", fragment: `"secretRef":{}`, mention: "secretRef.name is required"},
		{name: "env unknown field", fragment: `"env":[{"name":"A","value":"b","from":"c"}]`, mention: `"from"`},
		{name: "env missing name", fragment: `"env":[{"value":"b"}]`, mention: "env[0].name is required"},
		{name: "env invalid name", fragment: `"env":[{"name":"1BAD","value":"b"}]`, mention: "not a valid environment variable name"},
		{name: "env name too long", fragment: `"env":[{"name":"` + strings.Repeat("A", maxContainerTaskEnvNameBytes+1) + `","value":"b"}]`, mention: "env[0].name"},
		{name: "env reserved ORKA name", fragment: `"env":[{"name":"ORKA_RESULT_ENDPOINT","value":"http://evil"}]`, errorType: "reserved_env_name", mention: "ORKA_RESULT_ENDPOINT"},
		{name: "env reserved telemetry name", fragment: `"env":[{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://evil"}]`, errorType: "reserved_env_name", mention: "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{name: "env duplicate", fragment: `"env":[{"name":"A","value":"1"},{"name":"A","value":"2"}]`, mention: "duplicated"},
		{name: "env too many", fragment: `"env":[` + strings.Join(manyEnv, ",") + `]`, mention: "at most 64"},
		{name: "env value too long", fragment: `"env":[{"name":"A","value":"` + strings.Repeat("x", maxContainerTaskEnvValueBytes+1) + `"}]`, mention: "env[0].value"},
		{name: "env value with NUL", fragment: `"env":[{"name":"A","value":"before\u0000after"}]`, mention: "NUL"},
		{name: "env null item", fragment: `"env":[null]`, mention: "env[0]"},
		{name: "malformed quantity", fragment: `"resources":{"limits":{"cpu":"two"}}`, mention: "resources.limits.cpu"},
		{name: "empty quantity", fragment: `"resources":{"limits":{"cpu":"","memory":"2Gi"}}`, mention: "resources.limits.cpu"},
		{name: "unknown resources key", fragment: `"resources":{"limit":{"cpu":"1"}}`, mention: `"limit"`},
		{name: "env too large in total", fragment: `"env":[` + strings.Join(largeEnv, ",") + `]`, mention: "in total"},
		{name: "env too large in total with Secret-backed names", fragment: `"env":[` + strings.Join(largeSecretEnv, ",") + `]`, mention: "in total"},
		{name: "negative quantity", fragment: `"resources":{"requests":{"memory":"-1Gi"}}`, mention: "resources.requests.memory"},
		{name: "request above limit", fragment: `"resources":{"requests":{"cpu":"4"},"limits":{"cpu":"2"}}`, mention: "resources.requests.cpu"},
		{name: "too many retries", fragment: `"retryPolicy":{"maxRetries":6}`, mention: "maxRetries"},
		{name: "unknown retryPolicy key", fragment: `"retryPolicy":{"maxRetry":3}`, mention: `"maxRetry"`},
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
				result, fc := path.run(t, allowRuntimeClasses(tt.allowed...), `{`+containerSpecArgsBase+`,`+tt.fragment+`}`)
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

func TestCreateContainerTaskTool_AuthorizesSecretReferences(t *testing.T) {
	args := `{` + containerSpecArgsBase + `,"secretRef":{"name":"browser-login"},` +
		`"env":[{"name":"API_TOKEN","valueFrom":{"secretKeyRef":{"name":"api-token","key":"token"}}},` +
		`{"name":"OTHER_TOKEN","valueFrom":{"secretKeyRef":{"name":"api-token","key":"other"}}}]}`
	for _, path := range containerSpecArgsPaths() {
		t.Run(path.name+"/authorized", func(t *testing.T) {
			var checked []string
			result, fc := path.run(t, func(tc *ToolContext) {
				tc.RequireSecretReadAuthorization = true
				tc.AuthorizeSecretRead = func(_ context.Context, namespace, name string) *ChatToolError {
					checked = append(checked, namespace+"/"+name)
					return nil
				}
			}, args)
			expectContainerTaskSuccess(t, result)
			if want := []string{defaultNamespace + "/api-token", defaultNamespace + "/browser-login"}; fmt.Sprint(checked) != fmt.Sprint(want) {
				t.Fatalf("authorized Secrets = %v, want each name once: %v", checked, want)
			}
			created := createdContainerTasks(t, fc)
			if len(created) != 1 {
				t.Fatalf("created %d container tasks, want 1", len(created))
			}
			spec := created[0].Spec
			if spec.SecretRef == nil || spec.SecretRef.Name != "browser-login" || spec.SecretRef.Namespace != "" {
				t.Errorf("secretRef = %#v", spec.SecretRef)
			}
			if len(spec.Env) != 2 || spec.Env[0].ValueFrom == nil || spec.Env[0].ValueFrom.SecretKeyRef == nil ||
				spec.Env[0].ValueFrom.SecretKeyRef.Name != "api-token" || spec.Env[0].ValueFrom.SecretKeyRef.Key != "token" || spec.Env[0].Value != "" {
				t.Errorf("env = %#v", spec.Env)
			}
		})
		t.Run(path.name+"/denied", func(t *testing.T) {
			result, fc := path.run(t, func(tc *ToolContext) {
				tc.AuthorizeSecretRead = func(_ context.Context, _, name string) *ChatToolError {
					if name == "api-token" {
						return &ChatToolError{Type: "authorization_failed", Message: "missing credential scope", Suggestion: "Use an authorized token"}
					}
					return nil
				}
			}, args)
			r := parseContainerTaskToolResult(t, result)
			if r.Success || r.ErrorType != "authorization_failed" || !strings.Contains(r.Error, defaultNamespace+"/api-token") {
				t.Fatalf("result = %#v, want an authorization failure naming the Secret", r)
			}
			if created := createdContainerTasks(t, fc); len(created) != 0 {
				t.Fatalf("created %d container tasks for an unauthorized Secret", len(created))
			}
		})
		t.Run(path.name+"/fails closed without authorizer", func(t *testing.T) {
			result, fc := path.run(t, func(tc *ToolContext) { tc.RequireSecretReadAuthorization = true }, args)
			if r := parseContainerTaskToolResult(t, result); r.Success || r.ErrorType != "authorization_failed" {
				t.Fatalf("result = %#v, want a fail-closed authorization error", r)
			}
			if created := createdContainerTasks(t, fc); len(created) != 0 {
				t.Fatalf("created %d container tasks without Secret authorization", len(created))
			}
		})
	}
}
