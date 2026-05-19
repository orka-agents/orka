/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	sandboxextv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

const (
	staleResourceLabelKey   = "stale"
	staleResourceLabelValue = scheduledRunLabelValue
)

// newTestScheme creates a scheme with all types needed for unit tests.
func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
	_ = sandboxextv1alpha1.AddToScheme(s)
	return s
}

// newUnitReconciler builds a TaskReconciler backed by a fake client.
func newUnitReconciler(scheme *runtime.Scheme, objs ...client.Object) *TaskReconciler {
	fb := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{})
	if len(objs) > 0 {
		fb = fb.WithObjects(objs...)
	}
	fc := fb.Build()

	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		panic(err)
	}
	ss := sqlite.NewStore(db, ":memory:")
	return &TaskReconciler{
		Client:         fc,
		Scheme:         scheme,
		JobBuilder:     NewJobBuilder(fc),
		SessionManager: NewSessionManager(ss),
		Recorder:       record.NewFakeRecorder(100),
		ResultStore:    ss,
		PlanStore:      ss,
	}
}

// ---------------------------------------------------------------------------
// isAutonomousTask
// ---------------------------------------------------------------------------

func TestIsAutonomousTask_NoAgentRef(t *testing.T) {
	r := newUnitReconciler(newTestScheme())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	if r.isAutonomousTask(context.Background(), task) {
		t.Error("expected false when agentRef is nil")
	}
}

func TestIsAutonomousTask_AgentNotFound(t *testing.T) {
	r := newUnitReconciler(newTestScheme())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "missing"},
		},
	}
	if r.isAutonomousTask(context.Background(), task) {
		t.Error("expected false when agent does not exist")
	}
}

func TestIsAutonomousTask_CoordinationNil(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "default"},
	}
	r := newUnitReconciler(scheme, agent)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a1"},
		},
	}
	if r.isAutonomousTask(context.Background(), task) {
		t.Error("expected false when coordination is nil")
	}
}

func TestIsAutonomousTask_AutonomousTrue(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous: true,
			},
		},
	}
	r := newUnitReconciler(scheme, agent)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a1"},
		},
	}
	if !r.isAutonomousTask(context.Background(), task) {
		t.Error("expected true when autonomous is enabled")
	}
}

func TestIsAutonomousTask_CrossNamespace(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "other"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous: true,
			},
		},
	}
	r := newUnitReconciler(scheme, agent)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a1", Namespace: "other"},
		},
	}
	if !r.isAutonomousTask(context.Background(), task) {
		t.Error("expected true when cross-namespace agent has autonomous enabled")
	}
}

// ---------------------------------------------------------------------------
// resolveAgent
// ---------------------------------------------------------------------------

func TestResolveAgent_NilRef(t *testing.T) {
	r := newUnitReconciler(newTestScheme())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	agent, err := r.resolveAgent(context.Background(), task)
	if err != nil || agent != nil {
		t.Errorf("expected (nil, nil), got (%v, %v)", agent, err)
	}
}

func TestResolveAgent_Found(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "default"},
	}
	r := newUnitReconciler(scheme, agent)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a1"},
		},
	}
	got, err := r.resolveAgent(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "a1" {
		t.Errorf("expected agent name a1, got %s", got.Name)
	}
}

func TestResolveAgent_NotFound(t *testing.T) {
	r := newUnitReconciler(newTestScheme())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "missing"},
		},
	}
	_, err := r.resolveAgent(context.Background(), task)
	if err == nil {
		t.Error("expected error when agent not found")
	}
}

func TestResolveAgent_NamespaceIsolation(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "other"},
	}
	r := newUnitReconciler(scheme, agent)
	r.EnforceNamespaceIsolation = true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a1", Namespace: "other"},
		},
	}
	_, err := r.resolveAgent(context.Background(), task)
	if err == nil {
		t.Error("expected error for cross-namespace agent with isolation enforced")
	}
}

func TestResolveAgent_CrossNamespaceAllowed(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1", Namespace: "other"},
	}
	r := newUnitReconciler(scheme, agent)
	r.EnforceNamespaceIsolation = false
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a1", Namespace: "other"},
		},
	}
	got, err := r.resolveAgent(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "a1" {
		t.Errorf("expected a1, got %s", got.Name)
	}
}

// ---------------------------------------------------------------------------
// resolveProviderRef (pure logic, no client needed)
// ---------------------------------------------------------------------------

func TestResolveProviderRef_AgentTask(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	if ref := r.resolveProviderRef(task, nil); ref != nil {
		t.Error("expected nil for agent tasks")
	}
}

func TestResolveProviderRef_TaskAI(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "task-provider"},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "agent-provider"},
		},
	}
	ref := r.resolveProviderRef(task, agent)
	if ref == nil || ref.Name != "task-provider" {
		t.Errorf("expected task-level provider, got %v", ref)
	}
}

func TestResolveProviderRef_AgentFallback(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "agent-provider"},
		},
	}
	ref := r.resolveProviderRef(task, agent)
	if ref == nil || ref.Name != "agent-provider" {
		t.Errorf("expected agent-level provider, got %v", ref)
	}
}

func TestResolveProviderRef_NilEverything(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	if ref := r.resolveProviderRef(task, nil); ref != nil {
		t.Errorf("expected nil, got %v", ref)
	}
}

// ---------------------------------------------------------------------------
// validateTaskAgentCompatibility (pure logic)
// ---------------------------------------------------------------------------

func TestValidateTaskAgentCompatibility_AgentTaskNoAgent(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "do stuff"},
	}
	if err := r.validateTaskAgentCompatibility(task, nil); err == nil {
		t.Error("expected error for agent task without agentRef")
	}
}

func TestValidateTaskAgentCompatibility_AgentTaskNoRuntime(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "do stuff"},
	}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a1"}}
	if err := r.validateTaskAgentCompatibility(task, agent); err == nil {
		t.Error("expected error when agent has no runtime")
	}
}

func TestValidateTaskAgentCompatibility_AgentTaskRuntimeAndProvider(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "do stuff"},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1"},
		Spec: corev1alpha1.AgentSpec{
			Runtime:     &corev1alpha1.AgentCLIRuntime{Type: "copilot"},
			ProviderRef: &corev1alpha1.ProviderReference{Name: "p1"},
		},
	}
	if err := r.validateTaskAgentCompatibility(task, agent); err == nil {
		t.Error("expected error when runtime and providerRef are both set")
	}
}

func TestValidateTaskAgentCompatibility_AgentTaskAgentExecutionWorkspace(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "do stuff"},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: "copilot"},
			Execution: &corev1alpha1.ExecutionSpec{
				Workspace: &corev1alpha1.ExecutionWorkspaceSpec{Enabled: true},
			},
		},
	}
	err := r.validateTaskAgentCompatibility(task, agent)
	if err == nil {
		t.Fatal("expected error when agent execution workspace is enabled")
	}
	if !strings.Contains(err.Error(), "Task.spec.execution.workspace") {
		t.Fatalf("expected Task.spec.execution.workspace guidance, got %q", err.Error())
	}
}

func TestValidateTaskAgentCompatibility_AgentTaskRuntimeAndModelProvider(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "do stuff"},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: "copilot"},
			Model:   &corev1alpha1.ModelConfig{Provider: "openai"},
		},
	}
	if err := r.validateTaskAgentCompatibility(task, agent); err == nil {
		t.Error("expected error when runtime and model.provider are both set")
	}
}

func TestValidateTaskAgentCompatibility_AgentTaskNoPrompt(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: "copilot"},
		},
	}
	if err := r.validateTaskAgentCompatibility(task, agent); err == nil {
		t.Error("expected error when prompt is empty for agent task")
	}
}

func TestValidateTaskAgentCompatibility_AgentTaskValid(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "do it"},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: "copilot"},
		},
	}
	if err := r.validateTaskAgentCompatibility(task, agent); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestValidateTaskAgentCompatibility_AITaskWithRuntime(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "a1"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: "copilot"},
		},
	}
	if err := r.validateTaskAgentCompatibility(task, agent); err == nil {
		t.Error("expected error for AI task referencing agent with runtime")
	}
}

func TestValidateTaskAgentCompatibility_ContainerTask(t *testing.T) {
	r := &TaskReconciler{}
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	if err := r.validateTaskAgentCompatibility(task, nil); err != nil {
		t.Errorf("expected no error for container task, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// validateExecutionWorkspace (pure logic)
// ---------------------------------------------------------------------------

func TestResolveExecutionWorkspaceRequestValidatesSandboxTemplateExists(t *testing.T) {
	scheme := newTestScheme()

	workspace := func(name string, namespace string) *corev1alpha1.ExecutionWorkspaceSpec {
		ws := &corev1alpha1.ExecutionWorkspaceSpec{
			Enabled: true,
			TemplateRef: &corev1alpha1.WorkspaceTemplateReference{
				Name: name,
			},
		}
		if namespace != "" {
			ws.TemplateRef.Namespace = namespace
		}
		return ws
	}

	task := func(name string, ws *corev1alpha1.ExecutionWorkspaceSpec) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: defaultNS},
			Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: ws,
				},
			},
		}
	}

	t.Run("existing template in task namespace is accepted", func(t *testing.T) {
		template := &sandboxextv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "task-template", Namespace: defaultNS},
		}
		r := newUnitReconciler(scheme, template)
		r.AgentSandboxEnabled = true

		request, err := r.resolveExecutionWorkspaceRequest(context.Background(), task("task-ok", workspace("task-template", "")))
		if err != nil {
			t.Fatalf("resolveExecutionWorkspaceRequest() error = %v", err)
		}
		if request == nil || request.TemplateName != "task-template" {
			t.Fatalf("request = %#v, want template task-template", request)
		}
	})

	t.Run("missing template fails before job creation", func(t *testing.T) {
		r := newUnitReconciler(scheme)
		r.AgentSandboxEnabled = true

		_, err := r.resolveExecutionWorkspaceRequest(context.Background(), task("task-missing", workspace("missing-template", "")))
		if err == nil {
			t.Fatal("resolveExecutionWorkspaceRequest() error = nil, want missing template error")
		}
		want := `execution workspace template "missing-template" not found in namespace "default"`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err.Error(), want)
		}
	})

	t.Run("explicit template namespace is accepted as claim namespace", func(t *testing.T) {
		template := &sandboxextv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-template", Namespace: "sandbox-templates"},
		}
		r := newUnitReconciler(scheme, template)
		r.AgentSandboxEnabled = true

		request, err := r.resolveExecutionWorkspaceRequest(context.Background(), task("task-cross-ns", workspace("shared-template", "sandbox-templates")))
		if err != nil {
			t.Fatalf("resolveExecutionWorkspaceRequest() error = %v", err)
		}
		if request.ClaimNamespace != "sandbox-templates" {
			t.Fatalf("ClaimNamespace = %q, want sandbox-templates", request.ClaimNamespace)
		}
	})
}

func TestValidateExecutionWorkspace(t *testing.T) {
	workspace := func(mutators ...func(*corev1alpha1.ExecutionWorkspaceSpec)) *corev1alpha1.ExecutionWorkspaceSpec {
		ws := &corev1alpha1.ExecutionWorkspaceSpec{
			Enabled:     true,
			TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "default"},
		}
		for _, mutate := range mutators {
			mutate(ws)
		}
		return ws
	}

	tests := []struct {
		name                string
		agentSandboxEnabled bool
		task                *corev1alpha1.Task
		agentSandboxConfig  AgentSandboxConfig
		wantErr             string
	}{
		{
			name: "nil execution",
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
			}},
		},
		{
			name: "workspace disabled",
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: &corev1alpha1.ExecutionWorkspaceSpec{Enabled: false},
				},
			}},
		},
		{
			name: "feature gate disabled",
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(),
				},
			}},
			wantErr: "requires agent sandbox",
		},
		{
			name:                "non-agent task",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAI,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(),
				},
			}},
			wantErr: "only supported for type: agent",
		},
		{
			name:                "missing templateRef",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.TemplateRef = nil }),
				},
			}},
			wantErr: "templateRef.name is required",
		},
		{
			name:                "missing templateRef name",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.TemplateRef.Name = "" }),
				},
			}},
			wantErr: "templateRef.name is required",
		},
		{
			name:                "default template satisfies missing templateRef",
			agentSandboxEnabled: true,
			agentSandboxConfig:  AgentSandboxConfig{DefaultTemplate: "controller-default"},
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) { ws.TemplateRef = nil }),
				},
			}},
		},
		{
			name:                "unsupported reusePolicy",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
						ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicy("forever")
					}),
				},
			}},
			wantErr: "unsupported execution workspace reusePolicy",
		},
		{
			name:                "unsupported cleanupPolicy",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
						ws.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicy("archive")
					}),
				},
			}},
			wantErr: "unsupported execution workspace cleanupPolicy",
		},
		{
			name:                "session reuse without sessionRef",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
						ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
					}),
				},
			}},
			wantErr: "requires spec.sessionRef.name",
		},
		{
			name:                "session reuse with empty sessionRef name",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type:       corev1alpha1.TaskTypeAgent,
				SessionRef: &corev1alpha1.SessionReference{Name: ""},
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
						ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
					}),
				},
			}},
			wantErr: "requires spec.sessionRef.name",
		},
		{
			name:                "valid defaults",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type: corev1alpha1.TaskTypeAgent,
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(),
				},
			}},
		},
		{
			name:                "valid session reuse",
			agentSandboxEnabled: true,
			task: &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type:       corev1alpha1.TaskTypeAgent,
				SessionRef: &corev1alpha1.SessionReference{Name: "session-1"},
				Execution: &corev1alpha1.ExecutionSpec{
					Workspace: workspace(func(ws *corev1alpha1.ExecutionWorkspaceSpec) {
						ws.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
						ws.CleanupPolicy = corev1alpha1.WorkspaceCleanupPolicyRetain
					}),
				},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &TaskReconciler{
				AgentSandboxEnabled: tt.agentSandboxEnabled,
				AgentSandboxConfig:  tt.agentSandboxConfig,
			}

			err := r.validateExecutionWorkspace(tt.task)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// shouldRetry / calculateRetryDelay
// ---------------------------------------------------------------------------

func TestShouldRetry(t *testing.T) {
	r := &TaskReconciler{}
	tests := []struct {
		name   string
		task   *corev1alpha1.Task
		expect bool
	}{
		{
			name:   "no retry policy",
			task:   &corev1alpha1.Task{},
			expect: false,
		},
		{
			name: "attempts < maxRetries",
			task: &corev1alpha1.Task{
				Spec:   corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3}},
				Status: corev1alpha1.TaskStatus{Attempts: 1},
			},
			expect: true,
		},
		{
			name: "attempts == maxRetries allows final retry",
			task: &corev1alpha1.Task{
				Spec:   corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3}},
				Status: corev1alpha1.TaskStatus{Attempts: 3},
			},
			expect: true,
		},
		{
			name: "attempts > maxRetries",
			task: &corev1alpha1.Task{
				Spec:   corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2}},
				Status: corev1alpha1.TaskStatus{Attempts: 5},
			},
			expect: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.shouldRetry(tc.task); got != tc.expect {
				t.Errorf("shouldRetry = %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestCalculateRetryDelay(t *testing.T) {
	r := &TaskReconciler{}

	t.Run("no retry policy returns default", func(t *testing.T) {
		task := &corev1alpha1.Task{}
		if d := r.calculateRetryDelay(task); d != 10*time.Second {
			t.Errorf("expected 10s, got %v", d)
		}
	})

	t.Run("nil initialDelay returns default", func(t *testing.T) {
		task := &corev1alpha1.Task{
			Spec: corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3}},
		}
		if d := r.calculateRetryDelay(task); d != 10*time.Second {
			t.Errorf("expected 10s, got %v", d)
		}
	})

	t.Run("first attempt uses initial delay", func(t *testing.T) {
		task := &corev1alpha1.Task{
			Spec: corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:   3,
				InitialDelay: &metav1.Duration{Duration: 5 * time.Second},
			}},
			Status: corev1alpha1.TaskStatus{Attempts: 1},
		}
		if d := r.calculateRetryDelay(task); d != 5*time.Second {
			t.Errorf("expected 5s, got %v", d)
		}
	})

	t.Run("exponential backoff", func(t *testing.T) {
		task := &corev1alpha1.Task{
			Spec: corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:        5,
				BackoffMultiplier: 2,
				InitialDelay:      &metav1.Duration{Duration: 1 * time.Second},
			}},
			Status: corev1alpha1.TaskStatus{Attempts: 3},
		}
		// 1s * 2 * 2 = 4s
		if d := r.calculateRetryDelay(task); d != 4*time.Second {
			t.Errorf("expected 4s, got %v", d)
		}
	})

	t.Run("capped at 5 minutes", func(t *testing.T) {
		task := &corev1alpha1.Task{
			Spec: corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:        20,
				BackoffMultiplier: 10,
				InitialDelay:      &metav1.Duration{Duration: 1 * time.Minute},
			}},
			Status: corev1alpha1.TaskStatus{Attempts: 10},
		}
		if d := r.calculateRetryDelay(task); d != 5*time.Minute {
			t.Errorf("expected 5m cap, got %v", d)
		}
	})

	t.Run("zero multiplier defaults to 2", func(t *testing.T) {
		task := &corev1alpha1.Task{
			Spec: corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{
				MaxRetries:        5,
				BackoffMultiplier: 0,
				InitialDelay:      &metav1.Duration{Duration: 2 * time.Second},
			}},
			Status: corev1alpha1.TaskStatus{Attempts: 2},
		}
		// 2s * 2 = 4s (multiplier 0 defaults to 2)
		if d := r.calculateRetryDelay(task); d != 4*time.Second {
			t.Errorf("expected 4s, got %v", d)
		}
	})
}

// ---------------------------------------------------------------------------
// enforceHistoryLimits
// ---------------------------------------------------------------------------

func TestEnforceHistoryLimits_DefaultLimits(t *testing.T) {
	scheme := newTestScheme()

	// Parent task
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}

	// Create 5 succeeded + 3 failed child tasks
	objs := make([]client.Object, 0, 10) //nolint:prealloc
	objs = append(objs, parent)
	for i := range 5 {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "child-s" + time.Now().Add(time.Duration(i)*time.Hour).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{labels.LabelParentTask: "parent"},
				CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Hour)),
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		})
	}
	for i := range 3 {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "child-f" + time.Now().Add(time.Duration(i)*time.Hour).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{labels.LabelParentTask: "parent"},
				CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Hour)),
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
		})
	}

	r := newUnitReconciler(scheme, objs...)
	err := r.enforceHistoryLimits(context.Background(), parent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Defaults: successLimit=3, failedLimit=1
	// Should have deleted 2 succeeded and 2 failed
	var remaining corev1alpha1.TaskList
	_ = r.List(context.Background(), &remaining, client.InNamespace("default"),
		client.MatchingLabels{labels.LabelParentTask: "parent"})

	succeeded, failed := 0, 0
	for _, task := range remaining.Items {
		switch task.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded:
			succeeded++
		case corev1alpha1.TaskPhaseFailed:
			failed++
		}
	}
	if succeeded != 3 {
		t.Errorf("expected 3 succeeded remaining, got %d", succeeded)
	}
	if failed != 1 {
		t.Errorf("expected 1 failed remaining, got %d", failed)
	}
}

func TestEnforceHistoryLimits_CustomLimits(t *testing.T) {
	scheme := newTestScheme()

	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:                       corev1alpha1.TaskTypeAI,
			SuccessfulRunsHistoryLimit: new(int32(1)),
			FailedRunsHistoryLimit:     new(int32(0)),
		},
	}

	objs := make([]client.Object, 0, 10) //nolint:prealloc
	objs = append(objs, parent)
	for i := range 4 {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cs-" + time.Now().Add(time.Duration(i)*time.Minute).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{labels.LabelParentTask: "parent"},
				CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Minute)),
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		})
	}
	for i := range 2 {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cf-" + time.Now().Add(time.Duration(i)*time.Minute).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{labels.LabelParentTask: "parent"},
				CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Minute)),
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
		})
	}

	r := newUnitReconciler(scheme, objs...)
	if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var remaining corev1alpha1.TaskList
	_ = r.List(context.Background(), &remaining, client.InNamespace("default"),
		client.MatchingLabels{labels.LabelParentTask: "parent"})

	succeeded, failed := 0, 0
	for _, task := range remaining.Items {
		switch task.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded:
			succeeded++
		case corev1alpha1.TaskPhaseFailed:
			failed++
		}
	}
	if succeeded != 1 {
		t.Errorf("expected 1 succeeded, got %d", succeeded)
	}
	if failed != 0 {
		t.Errorf("expected 0 failed, got %d", failed)
	}
}

func TestEnforceHistoryLimits_LongParentNameUsesSelectorValue(t *testing.T) {
	scheme := newTestScheme()

	parentName := "very-long-parent-task-name-that-exceeds-kubernetes-label-limits-1234567890"
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: parentName, Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-1",
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(parentName),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: parentName,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}

	r := newUnitReconciler(scheme, parent, child)
	if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var remaining corev1alpha1.TaskList
	if err := r.List(context.Background(), &remaining, client.InNamespace("default"),
		client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(parentName)}); err != nil {
		t.Fatalf("listing child tasks: %v", err)
	}
	if len(remaining.Items) != 1 {
		t.Fatalf("expected 1 child task, got %d", len(remaining.Items))
	}
	if labels.ParentTaskName(remaining.Items[0].Labels, remaining.Items[0].Annotations) != parentName {
		t.Fatalf("ParentTaskName() = %q, want %q", labels.ParentTaskName(remaining.Items[0].Labels, remaining.Items[0].Annotations), parentName)
	}
}

func TestEnforceHistoryLimits_NoChildTasks(t *testing.T) {
	scheme := newTestScheme()
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, parent)
	if err := r.enforceHistoryLimits(context.Background(), parent); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validateCoordinationConstraints
// ---------------------------------------------------------------------------

func TestValidateCoordinationConstraints_NoAnnotation(t *testing.T) {
	r := newUnitReconciler(newTestScheme())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
	}
	_, _, done := r.validateCoordinationConstraints(context.Background(), task)
	if done {
		t.Error("expected done=false when no coordination-depth annotation")
	}
}

func TestValidateCoordinationConstraints_CoordinationDisabled(t *testing.T) {
	scheme := newTestScheme()
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "parent-agent"},
		},
	}
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: false},
		},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "1",
			},
			Labels: map[string]string{
				labels.LabelParentTask: "parent",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}

	r := newUnitReconciler(scheme, parentTask, parentAgent, childTask)
	_, _, done := r.validateCoordinationConstraints(context.Background(), childTask)
	if !done {
		t.Error("expected done=true when coordination is disabled")
	}
}

func TestValidateCoordinationConstraints_MaxDepthExceeded(t *testing.T) {
	scheme := newTestScheme()
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "parent-agent"},
		},
	}
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:  true,
				MaxDepth: 2,
			},
		},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "3",
			},
			Labels: map[string]string{
				labels.LabelParentTask: "parent",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}

	r := newUnitReconciler(scheme, parentTask, parentAgent, childTask)
	_, _, done := r.validateCoordinationConstraints(context.Background(), childTask)
	if !done {
		t.Error("expected done=true when max depth exceeded")
	}
}

func TestValidateCoordinationConstraints_AllowedAgentPass(t *testing.T) {
	scheme := newTestScheme()
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "parent-agent"},
		},
	}
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:       true,
				AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "child-agent"}},
			},
		},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "1",
			},
			Labels: map[string]string{
				labels.LabelParentTask: "parent",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "child-agent"},
		},
	}

	r := newUnitReconciler(scheme, parentTask, parentAgent, childTask)
	_, _, done := r.validateCoordinationConstraints(context.Background(), childTask)
	if done {
		t.Error("expected done=false when agent is in allowed list")
	}
}

func TestValidateCoordinationConstraints_AgentNotAllowed(t *testing.T) {
	scheme := newTestScheme()
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "parent-agent"},
		},
	}
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:       true,
				AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "other-agent"}},
			},
		},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "1",
			},
			Labels: map[string]string{
				labels.LabelParentTask: "parent",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "unauthorized-agent"},
		},
	}

	r := newUnitReconciler(scheme, parentTask, parentAgent, childTask)
	_, _, done := r.validateCoordinationConstraints(context.Background(), childTask)
	if !done {
		t.Error("expected done=true when agent is not in allowed list")
	}
}

func TestValidateCoordinationConstraints_DynamicallyCreatedAgent(t *testing.T) {
	scheme := newTestScheme()
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "parent-agent"},
		},
	}
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:       true,
				AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "other-agent"}},
			},
		},
	}
	// Dynamically created agent by create_agent tool
	dynamicAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dynamic-agent",
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelCreatedBy:  "create_agent",
				labels.LabelParentTask: "parent",
			},
		},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "1",
			},
			Labels: map[string]string{
				labels.LabelParentTask: "parent",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "dynamic-agent"},
		},
	}

	r := newUnitReconciler(scheme, parentTask, parentAgent, dynamicAgent, childTask)
	_, _, done := r.validateCoordinationConstraints(context.Background(), childTask)
	if done {
		t.Error("expected done=false for dynamically created agent with matching parent")
	}
}

func TestValidateCoordinationConstraints_ConcurrencyLimit(t *testing.T) {
	scheme := newTestScheme()
	parentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "parent-agent"},
		},
	}
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:               true,
				MaxConcurrentChildren: 1,
			},
		},
	}
	// An active sibling
	sibling := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sibling",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: "1",
			},
			Labels: map[string]string{
				labels.LabelParentTask: "parent",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}

	r := newUnitReconciler(scheme, parentTask, parentAgent, sibling, childTask)
	result, _, done := r.validateCoordinationConstraints(context.Background(), childTask)
	if !done {
		t.Error("expected done=true when concurrency limit reached")
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected 10s requeue, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// ensureWorkerRBAC
// ---------------------------------------------------------------------------

func TestEnsureWorkerRBAC_CreatesResources(t *testing.T) {
	scheme := newTestScheme()
	r := newUnitReconciler(scheme)

	err := r.ensureWorkerRBAC(context.Background(), testNS)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []struct {
		serviceAccount     string
		clusterRoleBinding string
		clusterRole        string
	}{
		{AIWorkerServiceAccount, "orka-ai-worker-test-ns", DefaultAIWorkerClusterRoleName},
		{VendorWorkerServiceAccount, "orka-vendor-worker-test-ns", DefaultVendorWorkerClusterRoleName},
		{ContainerWorkerServiceAccount, "orka-container-worker-test-ns", DefaultContainerWorkerClusterRoleName},
	}

	for _, tt := range expected {
		t.Run(tt.serviceAccount, func(t *testing.T) {
			// Verify ServiceAccount was created.
			sa := &corev1.ServiceAccount{}
			if err := r.Get(context.Background(), types.NamespacedName{
				Name: tt.serviceAccount, Namespace: testNS,
			}, sa); err != nil {
				t.Fatalf("expected SA %s to exist: %v", tt.serviceAccount, err)
			}

			// Verify matching ClusterRoleBinding was created.
			crb := &rbacv1.ClusterRoleBinding{}
			if err := r.Get(context.Background(), types.NamespacedName{
				Name: tt.clusterRoleBinding,
			}, crb); err != nil {
				t.Fatalf("expected CRB %s to exist: %v", tt.clusterRoleBinding, err)
			}
			if crb.RoleRef.Name != tt.clusterRole {
				t.Errorf("expected roleRef %s, got %s", tt.clusterRole, crb.RoleRef.Name)
			}
			if len(crb.Subjects) != 1 {
				t.Fatalf("expected 1 subject, got %d", len(crb.Subjects))
			}
			subject := crb.Subjects[0]
			if subject.Kind != rbacv1.ServiceAccountKind || subject.Name != tt.serviceAccount || subject.Namespace != testNS {
				t.Errorf("unexpected subject: %#v", subject)
			}
		})
	}
}

func TestEnsureWorkerRBAC_UsesClusterRoleBindingPrefix(t *testing.T) {
	scheme := newTestScheme()
	r := newUnitReconciler(scheme)
	r.WorkerClusterRoleBindingNamePrefix = "orka-dev"
	ctx := context.Background()

	if err := r.ensureWorkerRBAC(ctx, testNS); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []struct {
		serviceAccount     string
		clusterRoleBinding string
		clusterRole        string
	}{
		{AIWorkerServiceAccount, "orka-dev-ai-worker-test-ns", DefaultAIWorkerClusterRoleName},
		{VendorWorkerServiceAccount, "orka-dev-vendor-worker-test-ns", DefaultVendorWorkerClusterRoleName},
		{ContainerWorkerServiceAccount, "orka-dev-container-worker-test-ns", DefaultContainerWorkerClusterRoleName},
	}

	for _, tt := range expected {
		t.Run(tt.clusterRoleBinding, func(t *testing.T) {
			crb := &rbacv1.ClusterRoleBinding{}
			if err := r.Get(ctx, types.NamespacedName{Name: tt.clusterRoleBinding}, crb); err != nil {
				t.Fatalf("expected prefixed CRB %s to exist: %v", tt.clusterRoleBinding, err)
			}
			if crb.RoleRef.Name != tt.clusterRole {
				t.Fatalf("expected roleRef %s, got %s", tt.clusterRole, crb.RoleRef.Name)
			}
			if len(crb.Subjects) != 1 {
				t.Fatalf("expected 1 subject, got %d", len(crb.Subjects))
			}
			subject := crb.Subjects[0]
			if subject.Kind != rbacv1.ServiceAccountKind || subject.Name != tt.serviceAccount || subject.Namespace != testNS {
				t.Fatalf("unexpected subject: %#v", subject)
			}
		})
	}
}

func TestWorkerClusterRoleBindingNameTruncatesLongNames(t *testing.T) {
	prefix := strings.Repeat("p", 230)
	namespace := strings.Repeat("n", 80)

	got := workerClusterRoleBindingName(prefix, "container", namespace)
	if len(got) != maxWorkerClusterRoleBindingNameLength {
		t.Fatalf("expected name length %d, got %d", maxWorkerClusterRoleBindingNameLength, len(got))
	}
	if got != workerClusterRoleBindingName(prefix, "container", namespace) {
		t.Fatal("expected truncated name to be stable")
	}
	if got == workerClusterRoleBindingName(prefix, "vendor", namespace) {
		t.Fatal("expected hash suffix to distinguish names that share a truncated prefix")
	}
}

func TestEnsureWorkerRBAC_Idempotent(t *testing.T) {
	scheme := newTestScheme()
	// Pre-create all SAs and CRBs.
	expected := []struct {
		serviceAccount     string
		clusterRoleBinding string
		clusterRole        string
	}{
		{AIWorkerServiceAccount, "orka-ai-worker-test-ns", DefaultAIWorkerClusterRoleName},
		{VendorWorkerServiceAccount, "orka-vendor-worker-test-ns", DefaultVendorWorkerClusterRoleName},
		{ContainerWorkerServiceAccount, "orka-container-worker-test-ns", DefaultContainerWorkerClusterRoleName},
	}

	objects := make([]client.Object, 0, len(expected)*2)
	for _, tt := range expected {
		objects = append(objects,
			&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: tt.serviceAccount, Namespace: testNS},
			},
			&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: tt.clusterRoleBinding},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     tt.clusterRole,
				},
				Subjects: []rbacv1.Subject{{
					Kind: rbacv1.ServiceAccountKind, Name: tt.serviceAccount, Namespace: testNS,
				}},
			},
		)
	}
	r := newUnitReconciler(scheme, objects...)

	// Should not fail when resources already exist.
	if err := r.ensureWorkerRBAC(context.Background(), testNS); err != nil {
		t.Fatalf("unexpected error on idempotent call: %v", err)
	}
}

func TestEnsureWorkerServiceAccountPreservesAppManagedByLabel(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()
	namespace := testNS
	appManagedBy := "Helm"
	existing := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AIWorkerServiceAccount,
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabelKey: appManagedBy,
				"custom":          "keep",
			},
		},
	}
	r := newUnitReconciler(scheme, existing)

	if err := r.ensureWorkerServiceAccount(ctx, namespace, AIWorkerServiceAccount); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: AIWorkerServiceAccount, Namespace: namespace}, got); err != nil {
		t.Fatalf("getting ServiceAccount: %v", err)
	}
	if got.Labels[managedByLabelKey] != appManagedBy {
		t.Fatalf("expected app managed-by label to be preserved, got labels %#v", got.Labels)
	}
	if got.Labels[orkaManagedByLabelKey] != managedByLabelValue {
		t.Fatalf("expected Orka managed-by label to be reconciled, got labels %#v", got.Labels)
	}
	if got.Labels["custom"] != "keep" {
		t.Fatalf("expected existing labels to be preserved, got labels %#v", got.Labels)
	}
}

func TestEnsureWorkerClusterRoleBindingAlreadyExistsRaceUpdatesExistingBinding(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()
	namespace := "race-ns"
	spec := workerRBACSpec{
		serviceAccountName:     AIWorkerServiceAccount,
		clusterRoleName:        DefaultAIWorkerClusterRoleName,
		clusterRoleBindingName: fmt.Sprintf("orka-ai-worker-%s", namespace),
	}

	interceptedCreate := false
	fc := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() != spec.clusterRoleBindingName {
				return c.Create(ctx, obj, opts...)
			}
			if _, ok := obj.(*rbacv1.ClusterRoleBinding); !ok {
				return c.Create(ctx, obj, opts...)
			}

			interceptedCreate = true
			existing := &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:   spec.clusterRoleBindingName,
					Labels: map[string]string{staleResourceLabelKey: staleResourceLabelValue},
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: rbacv1.GroupName,
					Kind:     "ClusterRole",
					Name:     spec.clusterRoleName,
				},
				Subjects: []rbacv1.Subject{{
					Kind:      rbacv1.ServiceAccountKind,
					Name:      "stale-worker",
					Namespace: namespace,
				}},
			}
			if err := c.Create(ctx, existing); err != nil {
				t.Fatalf("creating raced ClusterRoleBinding fixture: %v", err)
			}

			return apierrors.NewAlreadyExists(
				schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterrolebindings"},
				spec.clusterRoleBindingName,
			)
		},
	}).Build()

	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)

	if err := r.ensureWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !interceptedCreate {
		t.Fatal("expected create to be intercepted")
	}

	got := &rbacv1.ClusterRoleBinding{}
	if err := fc.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, got); err != nil {
		t.Fatalf("expected raced ClusterRoleBinding to exist: %v", err)
	}
	if got.Labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("expected managed-by label to be reconciled, got labels %#v", got.Labels)
	}
	if len(got.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(got.Subjects))
	}
	subject := got.Subjects[0]
	if subject.Kind != rbacv1.ServiceAccountKind || subject.Name != spec.serviceAccountName || subject.Namespace != namespace {
		t.Fatalf("expected desired subject to be reconciled, got %#v", subject)
	}
}

func TestEnsureWorkerClusterRoleBindingRecreatesStaleRoleRef(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()
	namespace := "stale-ns"
	spec := workerRBACSpec{
		serviceAccountName:     AIWorkerServiceAccount,
		clusterRoleName:        DefaultAIWorkerClusterRoleName,
		clusterRoleBindingName: fmt.Sprintf("orka-ai-worker-%s", namespace),
	}

	stale := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.clusterRoleBindingName,
			Labels: map[string]string{
				staleResourceLabelKey: staleResourceLabelValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "stale-worker-role",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      "stale-worker",
			Namespace: "old-ns",
		}},
	}
	r := newUnitReconciler(scheme, stale)

	if err := r.ensureWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, got); err != nil {
		t.Fatalf("expected ClusterRoleBinding to exist after remediation: %v", err)
	}
	wantRoleRef := rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "ClusterRole",
		Name:     spec.clusterRoleName,
	}
	if got.RoleRef != wantRoleRef {
		t.Fatalf("expected stale RoleRef to be remediated to %#v, got %#v", wantRoleRef, got.RoleRef)
	}
	if got.Labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("expected managed-by label on remediated binding, got labels %#v", got.Labels)
	}
	if got.Labels[staleResourceLabelKey] == staleResourceLabelValue {
		t.Fatalf("expected stale binding to be recreated, got stale labels %#v", got.Labels)
	}
	if len(got.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(got.Subjects))
	}
	subject := got.Subjects[0]
	if subject.Kind != rbacv1.ServiceAccountKind || subject.Name != spec.serviceAccountName || subject.Namespace != namespace {
		t.Fatalf("expected desired subject to be reconciled, got %#v", subject)
	}
}

// ---------------------------------------------------------------------------
// handleScheduledTask
// ---------------------------------------------------------------------------

func TestHandleScheduledTask_ValidCron(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sched1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "*/5 * * * *",
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduledTask(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter")
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseScheduled {
		t.Errorf("expected phase Scheduled, got %s", task.Status.Phase)
	}
	if task.Status.NextScheduleTime == nil {
		t.Error("expected NextScheduleTime to be set")
	}
}

func TestHandleScheduledTask_InvalidCron(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sched2", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "not-a-cron",
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.handleScheduledTask(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected phase Failed for invalid cron, got %s", task.Status.Phase)
	}
}

func TestHandleScheduledTask_WithTimeZone(t *testing.T) {
	scheme := newTestScheme()
	tz := "America/New_York"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sched3", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "0 12 * * *",
			TimeZone: &tz,
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduledTask(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter")
	}
}

// ---------------------------------------------------------------------------
// collectResult
// ---------------------------------------------------------------------------

func TestCollectResult_ResultAlreadyExists(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, task)

	// Pre-save a result
	_ = r.ResultStore.SaveResult(context.Background(), "default", "t1", []byte("result data"))

	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		t.Error("expected ResultRef.Available to be true")
	}
}

func TestCollectResult_NoResultNoKubeClient(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t2", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	r := newUnitReconciler(scheme, task)
	// KubeClient is nil by default in unit reconciler

	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No result and no kube client — should return nil
}

func TestCollectResult_ContainerWithoutJobDoesNotReadPodLogs(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "prejob-failure", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{JobName: ""},
	}
	r := newUnitReconciler(scheme, task)
	r.KubeClient = k8sfake.NewSimpleClientset()

	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.ResultRef != nil {
		t.Fatalf("expected ResultRef to remain nil without a job, got %#v", task.Status.ResultRef)
	}
}

func TestCollectResult_AITaskNoResult(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t3", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, task)

	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AI task without result in store, no kube client — should not fail
}

func TestCollectResult_NilResultStore_DoesNotPanic(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "nil-store-result", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, task)
	r.ResultStore = nil

	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.ResultRef != nil {
		t.Fatalf("expected ResultRef to remain nil when result store is nil, got %#v", task.Status.ResultRef)
	}
}

// ---------------------------------------------------------------------------
// readPodLogs — requires KubeClient; we test the error path (no pods found)
// ---------------------------------------------------------------------------

func TestReadPodLogs_NoPods(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{JobName: "j1"},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.readPodLogs(context.Background(), task)
	if err == nil {
		t.Error("expected error when no pods found")
	}
}

// ---------------------------------------------------------------------------
// Helpers used by handleScheduled — tested indirectly above but we also
// verify the suspend path.
// ---------------------------------------------------------------------------

func TestHandleScheduled_Suspended(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sched-susp", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "*/5 * * * *",
			Suspend:  new(true),
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseScheduled},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Minute {
		t.Errorf("expected 1m requeue for suspended task, got %v", result.RequeueAfter)
	}
}

func TestHandleScheduled_InvalidCron(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sched-bad", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "invalid",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseScheduled},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected Failed phase, got %s", task.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// resolveProvider (with fake client)
// ---------------------------------------------------------------------------

func TestResolveProvider_NilProviderRef(t *testing.T) {
	r := newUnitReconciler(newTestScheme())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	provider, err := r.resolveProvider(context.Background(), task, nil)
	if err != nil || provider != nil {
		t.Errorf("expected (nil, nil), got (%v, %v)", provider, err)
	}
}

func TestResolveProvider_NamespaceIsolation(t *testing.T) {
	scheme := newTestScheme()
	r := newUnitReconciler(scheme)
	r.EnforceNamespaceIsolation = true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "p1", Namespace: "other"},
			},
		},
	}
	_, err := r.resolveProvider(context.Background(), task, nil)
	if err == nil {
		t.Error("expected error for cross-namespace provider with isolation")
	}
}

// ---------------------------------------------------------------------------
// handleDeletion
// ---------------------------------------------------------------------------

func TestHandleDeletion_NoFinalizer(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "del-no-fin",
			Namespace: "default",
		},
	}
	r := newUnitReconciler(scheme, task)
	// Set deletion timestamp after creation (can't pass it to fake.NewClientBuilder)
	task.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	result, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestHandleDeletion_WithResultRef(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "del-result",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	r := newUnitReconciler(scheme, task)
	_ = r.ResultStore.SaveResult(context.Background(), "default", "del-result", []byte("data"))
	// handleDeletion calls r.Update to remove finalizer — this works when DeletionTimestamp
	// is not set on the local copy (the fake client rejects changes to DeletionTimestamp).
	_, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Result should have been cleaned up
	_, getErr := r.ResultStore.GetResult(context.Background(), "default", "del-result")
	if getErr == nil {
		t.Error("expected result to be deleted")
	}
}

func TestHandleDeletion_WithResultRefNilResultStore(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "del-result-no-store",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Status: corev1alpha1.TaskStatus{
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	r := newUnitReconciler(scheme, task)
	r.ResultStore = nil

	_, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleDeletion_WithSessionRef(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "del-sess",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: "sess1"},
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleDeletion_WithJobName(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job1", Namespace: "default"},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "del-job",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Status: corev1alpha1.TaskStatus{JobName: "job1"},
	}
	r := newUnitReconciler(scheme, task, job)
	_, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleDeletion_WithMessageStore(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "del-msg",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
	}
	r := newUnitReconciler(scheme, task)
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	r.MessageStore = ss
	_, err := r.handleDeletion(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleCompleted
// ---------------------------------------------------------------------------

func TestHandleCompleted_NoWebhook(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestHandleCompleted_WebhookAlreadyDelivered(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp2", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAI,
			WebhookURL: "http://example.com/hook",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseSucceeded,
			WebhookDelivered: true,
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestHandleCompleted_CancelledDeletesJob(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "cancel-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "cancel-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseCancelled,
			JobName: "cancel-job",
		},
	}
	r := newUnitReconciler(scheme, task, job)

	_, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: "cancel-job", Namespace: "default"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected cancelled task Job to be deleted, got %v", err)
	}
}

func TestHandleCompleted_FailedActiveJobDeletesJob(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-active-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-active-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseFailed,
			JobName: "failed-active-job",
		},
	}
	r := newUnitReconciler(scheme, task, job)

	_, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: "failed-active-job", Namespace: "default"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected active failed task Job to be deleted, got %v", err)
	}
}

func TestHandleCompleted_FailedInactiveJobRetainsJob(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-inactive-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-inactive-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseFailed,
			JobName: "failed-inactive-job",
		},
	}
	r := newUnitReconciler(scheme, task, job)

	_, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: "failed-inactive-job", Namespace: "default"}, &batchv1.Job{}); err != nil {
		t.Fatalf("expected inactive failed task Job to be retained, got %v", err)
	}
}

func TestHandleCompleted_EnforcesScheduledTaskHistoryLimit(t *testing.T) {
	scheme := newTestScheme()
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "sched-parent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:                       corev1alpha1.TaskTypeContainer,
			SuccessfulRunsHistoryLimit: new(int32(1)),
		},
	}
	oldChild := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-child-old",
			Namespace:         "default",
			Labels:            map[string]string{labels.LabelParentTask: "sched-parent", labels.LabelScheduledRun: scheduledRunLabelValue},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	currentChild := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-child-current",
			Namespace:         "default",
			Labels:            map[string]string{labels.LabelParentTask: "sched-parent", labels.LabelScheduledRun: scheduledRunLabelValue},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}

	r := newUnitReconciler(scheme, parent, oldChild, currentChild)
	result, err := r.handleCompleted(context.Background(), currentChild)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}

	remaining := &corev1alpha1.TaskList{}
	if err := r.List(context.Background(), remaining, client.InNamespace("default"),
		client.MatchingLabels{labels.LabelParentTask: "sched-parent"}); err != nil {
		t.Fatalf("listing child tasks: %v", err)
	}

	if len(remaining.Items) != 1 {
		t.Fatalf("expected 1 child task to remain, got %d", len(remaining.Items))
	}
	if remaining.Items[0].Name != "sched-child-current" {
		t.Fatalf("expected newest child to remain, got %s", remaining.Items[0].Name)
	}
}

// ---------------------------------------------------------------------------
// handleRunning
// ---------------------------------------------------------------------------

func TestHandleRunning_Timeout(t *testing.T) {
	scheme := newTestScheme()
	timeout := metav1.Duration{Duration: 1 * time.Second}
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Second))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-timeout", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeAI,
			Timeout: &timeout,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			StartTime: &startTime,
			JobName:   "job1",
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected phase Failed, got %s", task.Status.Phase)
	}
}

func TestHandleRunning_JobNotFound(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-nojob", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "missing-job",
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected phase Failed for missing job, got %s", task.Status.Phase)
	}
}

func TestHandleRunning_JobNotFoundWithRetryPolicy(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-nojob-retry", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  "missing-job",
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected positive requeue after scheduling retry, got %v", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected phase Pending after scheduling retry, got %s", task.Status.Phase)
	}
	if task.Status.JobName != "" {
		t.Errorf("expected JobName to be cleared for retry, got %q", task.Status.JobName)
	}
}

func TestHandleRunning_JobSucceeded(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-job-ok", Namespace: "default"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-ok", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "run-job-ok",
		},
	}
	r := newUnitReconciler(scheme, task, job)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Errorf("expected phase Succeeded, got %s", task.Status.Phase)
	}
}

func TestHandleRunning_JobFailed_NoRetry(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-job-fail", Namespace: "default"},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-fail", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "run-job-fail",
		},
	}
	r := newUnitReconciler(scheme, task, job)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected phase Failed, got %s", task.Status.Phase)
	}
}

func TestHandleRunning_JobFailed_WithRetry(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-job-retry", Namespace: "default"},
		Status:     batchv1.JobStatus{Failed: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-retry", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  "run-job-retry",
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task, job)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected phase Pending for retry, got %s", task.Status.Phase)
	}
}

func TestHandleRunning_PodFailedMountFailsTask(t *testing.T) {
	scheme := newTestScheme()
	startTime := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-failed-mount", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			StartTime: &startTime,
			JobName:   "run-failed-mount-job",
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-failed-mount-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-failed-mount-pod",
			Namespace: "default",
			UID:       "pod-uid",
			Labels: map[string]string{
				labels.LabelTask: labels.SelectorValue(task.Name),
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "prepare-workspace",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "PodInitializing",
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "worker",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "PodInitializing",
				}},
			}},
		},
	}
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-mount", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod",
			Name: pod.Name,
			UID:  pod.UID,
		},
		Reason:  "FailedMount",
		Message: `MountVolume.SetUp failed for volume "git-credentials": secret "missing" not found`,
	}
	r := newUnitReconciler(scheme, task, job, pod, event)

	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", task.Status.Phase)
	}
	if !strings.Contains(task.Status.Message, "secret") {
		t.Fatalf("message = %q, want failed mount detail", task.Status.Message)
	}
}

func TestHandleRunning_PodInitializingWithoutFailedMountRequeues(t *testing.T) {
	scheme := newTestScheme()
	startTime := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-pod-initializing", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			StartTime: &startTime,
			JobName:   "run-pod-initializing-job",
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-pod-initializing-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-pod-initializing-pod",
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelTask: labels.SelectorValue(task.Name),
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "prepare-workspace",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "PodInitializing",
				}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "worker",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "PodInitializing",
				}},
			}},
		},
	}
	r := newUnitReconciler(scheme, task, job, pod)

	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running", task.Status.Phase)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s, want positive duration", result.RequeueAfter)
	}
}

func TestHandleRunning_JobStillRunning(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-job-active", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "run-active", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "run-job-active",
		},
	}
	r := newUnitReconciler(scheme, task, job)
	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
}

func TestHandleRunning_ChildTaskStatuses(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-job-parent", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-run"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "child-agent"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-run", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "run-job-parent",
		},
	}
	r := newUnitReconciler(scheme, task, job, child)
	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
	if len(task.Status.ChildTasks) != 1 {
		t.Errorf("expected 1 child task status, got %d", len(task.Status.ChildTasks))
	}
}

// ---------------------------------------------------------------------------
// createTaskJob
// ---------------------------------------------------------------------------

func TestCreateTaskJob_ContainerTask(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-job",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"echo", "hello"},
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.createTaskJob(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Errorf("expected phase Running, got %s", task.Status.Phase)
	}
	if task.Status.JobName == "" {
		t.Error("expected JobName to be set")
	}
	if task.Status.Attempts != 1 {
		t.Errorf("expected Attempts=1, got %d", task.Status.Attempts)
	}
}

func TestCreateTaskJob_AITaskWithAgent(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-ai-job",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "ai-agent"},
			AI:       &corev1alpha1.AISpec{Prompt: "hello"},
		},
	}
	r := newUnitReconciler(scheme, task, agent)
	_, err := r.createTaskJob(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Errorf("expected phase Running, got %s", task.Status.Phase)
	}
}

func TestCreateTaskJob_RBACReconcileFailureEmitsWarningAndContinues(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-rbac-warn",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"echo", "hello"},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(task).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ServiceAccount); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Resource: "serviceaccounts"},
						obj.GetName(),
						errors.New("injected serviceaccount create failure"),
					)
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	recorder := record.NewFakeRecorder(100)
	r := newUnitReconciler(scheme)
	r.Client = fc
	r.JobBuilder = NewJobBuilder(fc)
	r.Recorder = recorder

	result, err := r.createTaskJob(ctx, task, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Errorf("expected phase Running, got %s", task.Status.Phase)
	}
	if task.Status.JobName == "" {
		t.Error("expected JobName to be set")
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: task.Namespace}, job); err != nil {
		t.Fatalf("expected Job to be created despite RBAC warning: %v", err)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, corev1.EventTypeWarning) || !strings.Contains(event, workerRBACReconcileFailedReason) {
			t.Fatalf("expected %s Warning event, got %q", workerRBACReconcileFailedReason, event)
		}
	default:
		t.Fatalf("expected %s Warning event", workerRBACReconcileFailedReason)
	}
}

func TestCreateTaskJob_WithProvider(t *testing.T) {
	scheme := newTestScheme()
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov1", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type: corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{
				Name: "prov-secret",
			},
		},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-prov-job",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Prompt:      "hello",
				ProviderRef: &corev1alpha1.ProviderReference{Name: "prov1"},
			},
		},
	}
	r := newUnitReconciler(scheme, task, provider)
	_, err := r.createTaskJob(context.Background(), task, nil, provider)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Errorf("expected phase Running, got %s", task.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// completeTask
// ---------------------------------------------------------------------------

func TestCompleteTask_Succeeded(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-succ", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Errorf("expected 1s requeue, got %v", result.RequeueAfter)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Errorf("expected Succeeded, got %s", task.Status.Phase)
	}
	if task.Status.CompletionTime == nil {
		t.Error("expected CompletionTime to be set")
	}
}

func TestCompleteTask_Failed(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-fail", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseFailed, "failed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected Failed, got %s", task.Status.Phase)
	}
}

func TestCompleteTask_Cancelled(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-cancel", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseCancelled, "cancelled")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		t.Errorf("expected Cancelled, got %s", task.Status.Phase)
	}
}

func TestCompleteTask_WithSessionRef(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-sess", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "sess1",
				Append: true,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// retryTask
// ---------------------------------------------------------------------------

func TestRetryTask(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-job", Namespace: "default"},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-t", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  "retry-job",
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task, job)
	result, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive requeue delay")
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected Pending, got %s", task.Status.Phase)
	}
	if task.Status.JobName != "" {
		t.Error("expected JobName to be cleared")
	}
}

func TestRetryTask_NoExistingJob(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-nojob", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "nonexistent",
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.retryTask(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected Pending, got %s", task.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// acquireSessionLock
// ---------------------------------------------------------------------------

func TestAcquireSessionLock_NoSessionRef(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "lock-none", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, task)
	result, err, locked := r.acquireSessionLock(context.Background(), task)
	if locked {
		t.Error("expected locked=false when no sessionRef")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestAcquireSessionLock_SessionNotExist_CreateTrue(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "lock-create", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "new-sess",
				Create: true,
			},
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err, locked := r.acquireSessionLock(context.Background(), task)
	if locked {
		t.Error("expected locked=false after acquiring lock on new session")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquireSessionLock_SessionNotExist_CreateFalse(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "lock-nocreat", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "nonexist-sess",
				Create: false,
			},
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err, locked := r.acquireSessionLock(context.Background(), task)
	if !locked {
		t.Error("expected locked=true after terminal failure")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "session nonexist-sess not found and create=false") {
		t.Fatalf("message = %q, want missing session failure", updated.Status.Message)
	}
}

// ---------------------------------------------------------------------------
// resolveProvider
// ---------------------------------------------------------------------------

func TestResolveProvider_Found(t *testing.T) {
	scheme := newTestScheme()
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{Name: "sec1"},
		},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-prov", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "p1"},
			},
		},
	}
	r := newUnitReconciler(scheme, provider, task)
	got, err := r.resolveProvider(context.Background(), task, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "p1" {
		t.Errorf("expected provider p1, got %v", got)
	}
}

func TestResolveProvider_NotReady(t *testing.T) {
	scheme := newTestScheme()
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "p-notready", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{Name: "sec1"},
		},
		Status: corev1alpha1.ProviderStatus{Ready: false, Message: "not configured"},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-prov-nr", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "p-notready"},
			},
		},
	}
	r := newUnitReconciler(scheme, provider, task)
	_, err := r.resolveProvider(context.Background(), task, nil)
	if err == nil {
		t.Error("expected error for not-ready provider")
	}
}

func TestResolveProvider_NotFound(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-prov-miss", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "nonexistent"},
			},
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.resolveProvider(context.Background(), task, nil)
	if err == nil {
		t.Error("expected error for missing provider")
	}
}

// ---------------------------------------------------------------------------
// handleScheduled — additional paths
// ---------------------------------------------------------------------------

func TestHandleScheduled_NotYetTime(t *testing.T) {
	scheme := newTestScheme()
	// Use a schedule far in the future
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-future",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "0 0 1 1 *", // Jan 1 only
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseScheduled},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter for future schedule")
	}
}

func TestHandleScheduled_WithTimeZone(t *testing.T) {
	scheme := newTestScheme()
	tz := "America/New_York"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-tz",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "0 0 1 1 *",
			TimeZone: &tz,
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseScheduled},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter")
	}
}

func TestHandleScheduled_MissedDeadline(t *testing.T) {
	scheme := newTestScheme()
	deadline := int64(1)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-missed",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:                    corev1alpha1.TaskTypeContainer,
			Schedule:                "* * * * *", // every minute
			StartingDeadlineSeconds: &deadline,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseScheduled,
			LastScheduleTime: new(metav1.NewTime(time.Now().Add(-24 * time.Hour))),
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter after missed deadline")
	}
}

func TestHandleScheduled_ConcurrencyForbid(t *testing.T) {
	scheme := newTestScheme()
	// Create a parent task that is due and has an active child
	activeChild := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "active-child",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "sched-concur"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-concur",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-1 * time.Hour)),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:              corev1alpha1.TaskTypeContainer,
			Schedule:          "* * * * *",
			ConcurrencyPolicy: corev1alpha1.ForbidConcurrent,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseScheduled,
			LastScheduleTime: new(metav1.NewTime(time.Now().Add(-2 * time.Minute))),
		},
	}
	r := newUnitReconciler(scheme, task, activeChild)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter when concurrency is forbidden")
	}
}

func TestHandleScheduled_CreateChildTask(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-create",
			Namespace:         "default",
			UID:               "12345678-abcd-efgh-ijkl-1234567890ab",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-1 * time.Hour)),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:                    corev1alpha1.TaskTypeContainer,
			Image:                   "busybox:latest",
			Schedule:                "* * * * *",
			StartingDeadlineSeconds: new(int64(300)),
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseScheduled,
			LastScheduleTime: new(metav1.NewTime(time.Now().Add(-2 * time.Minute))),
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter after creating child task")
	}
	if task.Status.LastScheduleTime == nil {
		t.Error("expected LastScheduleTime to be updated")
	}
}

func TestHandleScheduled_ExistingChildTaskStillUpdatesScheduleStatus(t *testing.T) {
	scheme := newTestScheme()
	lastSchedule := metav1.NewTime(time.Now().Add(-2 * time.Minute).UTC())
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sched-existing-child",
			Namespace:         "default",
			UID:               "12345678-abcd-efgh-ijkl-1234567890ac",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-1 * time.Hour).UTC()),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:                    corev1alpha1.TaskTypeContainer,
			Image:                   "busybox:latest",
			Schedule:                "* * * * *",
			StartingDeadlineSeconds: new(int64(300)),
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseScheduled,
			LastScheduleTime: &lastSchedule,
		},
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	schedule, err := parser.Parse(task.Spec.Schedule)
	if err != nil {
		t.Fatalf("parse cron: %v", err)
	}
	scheduledTime := schedule.Next(lastSchedule.Time)
	childName := fmt.Sprintf("%s-%d", task.Name, scheduledTime.Unix())
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      childName,
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelParentTask:   labels.SelectorValue(task.Name),
				labels.LabelScheduledRun: scheduledRunLabelValue,
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: task.Name,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}

	r := newUnitReconciler(scheme, task, child)
	result, err := r.handleScheduled(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive RequeueAfter after handling existing child")
	}
	if task.Status.LastScheduleTime == nil || !task.Status.LastScheduleTime.Time.Equal(scheduledTime) {
		t.Fatalf("expected LastScheduleTime %s, got %v", scheduledTime.Format(time.RFC3339), task.Status.LastScheduleTime)
	}
	if task.Status.NextScheduleTime == nil {
		t.Fatal("expected NextScheduleTime to be updated")
	}
}

// ---------------------------------------------------------------------------
// handleAutonomousIteration
// ---------------------------------------------------------------------------

func TestHandleAutonomousIteration_GoalComplete(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous:    true,
				MaxIterations: 10,
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "auto-agent"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			Iteration: 2,
		},
	}
	r := newUnitReconciler(scheme, task, agent)
	// Save plan state indicating goal is complete
	_ = r.PlanStore.SavePlan(context.Background(), "default", "auto-task", &store.PlanState{
		GoalComplete: true,
		Summary:      "All tasks done",
	})
	_, err := r.handleAutonomousIteration(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Errorf("expected Succeeded, got %s", task.Status.Phase)
	}
}

func TestHandleAutonomousIteration_MaxIterations(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-agent2", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous:    true,
				MaxIterations: 3,
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-max", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "auto-agent2"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			Iteration: 2,
		},
	}
	r := newUnitReconciler(scheme, task, agent)
	_, err := r.handleAutonomousIteration(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Errorf("expected Succeeded at max iterations, got %s", task.Status.Phase)
	}
}

func TestHandleAutonomousIteration_Continue(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-agent3", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous:    true,
				MaxIterations: 10,
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-job", Namespace: "default"},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-cont", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "auto-agent3"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			Iteration: 1,
			JobName:   "auto-job",
		},
	}
	r := newUnitReconciler(scheme, task, agent, job)
	result, err := r.handleAutonomousIteration(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected Pending for next iteration, got %s", task.Status.Phase)
	}
	if task.Status.Iteration != 2 {
		t.Errorf("expected iteration 2, got %d", task.Status.Iteration)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
}

func TestHandleAutonomousIteration_Suspended(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-agent4", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous:    true,
				MaxIterations: 10,
			},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-susp", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "auto-agent4"},
			Suspend:  new(true),
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			Iteration: 1,
		},
	}
	r := newUnitReconciler(scheme, task, agent)
	result, err := r.handleAutonomousIteration(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s requeue for suspended, got %v", result.RequeueAfter)
	}
}

func TestHandleAutonomousIteration_AgentNotFound(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-noagent", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "missing-agent"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			Iteration: 0,
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.handleAutonomousIteration(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Errorf("expected Failed, got %s", task.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

func TestReconcile_NotFound(t *testing.T) {
	scheme := newTestScheme()
	r := newUnitReconciler(scheme)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestReconcile_AddFinalizer(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "rec-fin", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-fin", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Errorf("expected 1s requeue after adding finalizer, got %v", result.RequeueAfter)
	}
}

func TestReconcile_InitializeStatus(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-init",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-init", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Errorf("expected 1s requeue after initializing status, got %v", result.RequeueAfter)
	}
}

func TestReconcile_CompletedPhase(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-comp",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-comp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ---------------------------------------------------------------------------
// handlePending — transaction token pending
// ---------------------------------------------------------------------------

func TestHandlePending_TransactionTokenPendingRequeuesWithoutJob(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-token",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationTransactionTokenPending: "true",
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("expected 1s requeue while transaction token is pending, got %v", result.RequeueAfter)
	}

	jobs := &batchv1.JobList{}
	if err := r.List(context.Background(), jobs, client.InNamespace(task.Namespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected no Job to be created while transaction token is pending, got %d", len(jobs.Items))
	}
}

// ---------------------------------------------------------------------------
// handlePending — namespace task limit
// ---------------------------------------------------------------------------

func TestHandlePending_NamespaceTaskLimit(t *testing.T) {
	scheme := newTestScheme()
	// Create active tasks to hit the limit
	active1 := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "active1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	active2 := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "active2", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pending-limit",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task, active1, active2)
	r.MaxTasksPerNamespace = 2
	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected 10s requeue at limit, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// readPodLogs — pods found but no KubeClient causes panic (guarded by caller)
// The readPodLogs method is called by collectResult which checks KubeClient != nil.
// We test this indirectly through collectResult to avoid the nil dereference.
// ---------------------------------------------------------------------------

func TestCollectResult_ContainerTask_NoKubeClient_NoPods(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-collect", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{JobName: "job-collect"},
	}
	r := newUnitReconciler(scheme, task)
	// KubeClient is nil — collectResult should return nil early
	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("expected nil error when KubeClient is nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleCompleted — webhook paths
// ---------------------------------------------------------------------------

func TestHandleCompleted_WebhookNotConfigured(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-nowh", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestHandleCompleted_WebhookFails(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-whfail", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAI,
			WebhookURL: "http://invalid.nonexistent.local:9999/hook",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseSucceeded,
			WebhookDelivered: false,
		},
	}
	r := newUnitReconciler(scheme, task)
	r.WebhookNotifier = NewWebhookNotifier()
	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should requeue for webhook retry
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s requeue for webhook retry, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// Reconcile — additional phase paths
// ---------------------------------------------------------------------------

func TestReconcile_ScheduledPhase(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "rec-sched",
			Namespace:         "default",
			Finalizers:        []string{labels.TaskFinalizer},
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "0 0 1 1 *",
			Suspend:  new(true),
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseScheduled},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-sched", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive requeue for scheduled task")
	}
}

func TestReconcile_RunningPhase_JobNotFound(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-run",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "nonexistent-job",
		},
	}
	r := newUnitReconciler(scheme, task)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-run", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReconcile_RunningPhase_JobNotFoundWithRetryPolicy(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-run-retry",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:        corev1alpha1.TaskTypeAI,
			RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 2},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhaseRunning,
			JobName:  "nonexistent-job",
			Attempts: 1,
		},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-run-retry", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected positive requeue after scheduling retry, got %v", result.RequeueAfter)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "rec-run-retry", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to fetch updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("expected phase Pending after retry scheduling, got %s", updated.Status.Phase)
	}
	if updated.Status.JobName != "" {
		t.Fatalf("expected JobName to be cleared after retry scheduling, got %q", updated.Status.JobName)
	}
}

func TestReconcile_FailedPhase(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-fail",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-fail", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestReconcile_CancelledPhase(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rec-cancel",
			Namespace:  "default",
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rec-cancel", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

// ---------------------------------------------------------------------------
// handlePending — scheduled task path
// ---------------------------------------------------------------------------

func TestHandlePending_ScheduledTask(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "pend-sched", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeContainer,
			Schedule: "*/5 * * * *",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Error("expected positive requeue for scheduled task")
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseScheduled {
		t.Errorf("expected Scheduled phase, got %s", task.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// handleRunning — autonomous task path
// ---------------------------------------------------------------------------

func TestHandleRunning_AutonomousTask(t *testing.T) {
	scheme := newTestScheme()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-run-agent", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4"},
			Coordination: &corev1alpha1.CoordinationConfig{
				Autonomous:    true,
				MaxIterations: 5,
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-run-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-run-task", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "auto-run-agent"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			JobName:   "auto-run-job",
			Iteration: 0,
		},
	}
	r := newUnitReconciler(scheme, task, agent, job)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should advance to next iteration (Pending)
	if task.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected Pending for next iteration, got %s", task.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// handleRunning — child task with result
// ---------------------------------------------------------------------------

func TestHandleRunning_ChildTaskWithResult(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job2", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-with-result",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-result"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-result", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "parent-job2",
		},
	}
	r := newUnitReconciler(scheme, task, job, child)
	// Save child result
	_ = r.ResultStore.SaveResult(context.Background(), "default", "child-with-result", []byte("child output"))
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(task.Status.ChildTasks) != 1 {
		t.Fatalf("expected 1 child status, got %d", len(task.Status.ChildTasks))
	}
	if task.Status.ChildTasks[0].Result == "" {
		t.Error("expected child task result to be populated")
	}
}

func TestHandleRunning_ChildTasksSortedByName(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job-sorted", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	childZ := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "z-child",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-sorted"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "z-agent"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	childA := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-child",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-sorted"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			AgentRef: &corev1alpha1.AgentReference{Name: "a-agent"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-sorted", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "parent-job-sorted",
		},
	}
	r := newUnitReconciler(scheme, task, job, childZ, childA)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(task.Status.ChildTasks) != 2 {
		t.Fatalf("expected 2 child statuses, got %d", len(task.Status.ChildTasks))
	}
	if task.Status.ChildTasks[0].Name != "a-child" || task.Status.ChildTasks[1].Name != "z-child" {
		t.Fatalf("expected child statuses to be sorted by name, got %#v", task.Status.ChildTasks)
	}
}

// ---------------------------------------------------------------------------
// completeTask — with plan store cleanup
// ---------------------------------------------------------------------------

func TestCompleteTask_WithPlanStore(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-plan", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task)
	// Save a plan
	_ = r.PlanStore.SavePlan(context.Background(), "default", "comp-plan", &store.PlanState{
		Summary: "test plan",
	})
	_, err := r.completeTask(context.Background(), task, corev1alpha1.TaskPhaseSucceeded, "done")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Plan should be cleaned up
	_, planErr := r.PlanStore.GetPlan(context.Background(), "default", "comp-plan")
	if planErr == nil {
		t.Error("expected plan to be deleted on completion")
	}
}

// ---------------------------------------------------------------------------
// handleCompleted — webhook success path
// ---------------------------------------------------------------------------

func TestHandleCompleted_WebhookSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "comp-whok", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAI,
			WebhookURL: srv.URL,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:            corev1alpha1.TaskPhaseSucceeded,
			WebhookDelivered: false,
		},
	}
	r := newUnitReconciler(scheme, task)
	notifier := NewWebhookNotifier()
	notifier.skipURLValidation = true
	r.WebhookNotifier = notifier
	result, err := r.handleCompleted(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after successful webhook, got %v", result.RequeueAfter)
	}
	if !task.Status.WebhookDelivered {
		t.Error("expected WebhookDelivered to be true")
	}
}

// ---------------------------------------------------------------------------
// collectResult — result already saved by worker (AI task)
// ---------------------------------------------------------------------------

func TestCollectResult_ResultSavedByWorker(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "collect-saved", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, task)
	_ = r.ResultStore.SaveResult(context.Background(), "default", "collect-saved", []byte("worker result"))
	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		t.Error("expected ResultRef.Available=true when result exists")
	}
}

func TestCollectResult_AITask_NoResult(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "collect-ai-none", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, task)
	err := r.collectResult(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AI task without result — should return nil (no attempt to read logs)
}

// ---------------------------------------------------------------------------
// createTaskJob — job already exists
// ---------------------------------------------------------------------------

func TestCreateTaskJob_JobAlreadyExists(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-exist",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"echo", "hello"},
		},
	}
	r := newUnitReconciler(scheme, task)
	// First call succeeds
	_, err := r.createTaskJob(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}
	jobName := task.Status.JobName

	// Reset task status to simulate second reconcile
	task.Status.Phase = corev1alpha1.TaskPhasePending
	task.Status.JobName = ""
	task.Status.Attempts = 0
	task.Status.StartTime = nil
	task.Status.Conditions = nil

	// Second call should handle AlreadyExists
	_, err = r.createTaskJob(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error on second call (AlreadyExists): %v", err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Errorf("expected phase Running, got %s", task.Status.Phase)
	}
	_ = jobName
}

func TestCreateTaskJob_DoesNotOverwriteCancelledStatus(t *testing.T) {
	scheme := newTestScheme()
	current := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "create-cancelled",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"sleep"},
			Args:    []string{"600"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseCancelled,
			Message: "cancelled by caller",
		},
	}
	stale := current.DeepCopy()
	stale.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}

	r := newUnitReconciler(scheme, current)
	result, err := r.createTaskJob(context.Background(), stale, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for cancelled task, got %v", result.RequeueAfter)
	}

	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: current.Name, Namespace: current.Namespace}, updated); err != nil {
		t.Fatalf("failed to get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		t.Fatalf("phase = %s, want Cancelled", updated.Status.Phase)
	}

	jobs := &batchv1.JobList{}
	if err := r.List(context.Background(), jobs, client.InNamespace(current.Namespace)); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected no jobs to be created, got %d", len(jobs.Items))
	}
}

// ---------------------------------------------------------------------------
// handlePending — with session ref
// ---------------------------------------------------------------------------

func TestHandlePending_WithSessionRef(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pend-sess",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ab",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"echo", "hi"},
			SessionRef: &corev1alpha1.SessionReference{
				Name:   "pend-session",
				Create: true,
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
}

func TestHandlePending_WithMissingSessionCreateFalseFailsTask(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pend-missing-session",
			Namespace: "default",
			UID:       "12345678-abcd-efgh-ijkl-1234567890ac",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"echo", "hi"},
			SessionRef: &corev1alpha1.SessionReference{
				Name: "missing-session",
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, time.Second)
	}

	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.JobName != "" {
		t.Fatalf("JobName = %q, want no job", updated.Status.JobName)
	}
	if !strings.Contains(updated.Status.Message, "session missing-session not found and create=false") {
		t.Fatalf("message = %q, want missing session failure", updated.Status.Message)
	}
}

// ---------------------------------------------------------------------------
// handleRunning — child task with empty phase
// ---------------------------------------------------------------------------

func TestHandleRunning_ChildWithEmptyPhase(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job3", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-empty-phase",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-empty"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{Phase: ""},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-empty", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "parent-job3",
		},
	}
	r := newUnitReconciler(scheme, task, job, child)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(task.Status.ChildTasks) != 1 {
		t.Fatalf("expected 1 child, got %d", len(task.Status.ChildTasks))
	}
	if task.Status.ChildTasks[0].Phase != corev1alpha1.TaskPhasePending {
		t.Errorf("expected empty phase to default to Pending, got %s", task.Status.ChildTasks[0].Phase)
	}
}

// ---------------------------------------------------------------------------
// handleRunning — child task result fetch error
// ---------------------------------------------------------------------------

func TestHandleRunning_ChildResultFetchError(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job4", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-err-result",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-err"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-err", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "parent-job4",
		},
	}
	r := newUnitReconciler(scheme, task, job, child)
	// Don't save the child's result — fetch will fail
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(task.Status.ChildTasks) != 1 {
		t.Fatalf("expected 1 child, got %d", len(task.Status.ChildTasks))
	}
	if task.Status.ChildTasks[0].Result != "(result fetch error)" {
		t.Errorf("expected error message in result, got %q", task.Status.ChildTasks[0].Result)
	}
}

func TestHandleRunning_ChildTaskNilResultStore(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job-no-store", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-result-no-store",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-no-store"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-no-store", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "parent-job-no-store",
		},
	}
	r := newUnitReconciler(scheme, task, job, child)
	r.ResultStore = nil

	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(task.Status.ChildTasks) != 1 {
		t.Fatalf("expected 1 child, got %d", len(task.Status.ChildTasks))
	}
	if task.Status.ChildTasks[0].Result != "" {
		t.Errorf("expected empty result when result store is nil, got %q", task.Status.ChildTasks[0].Result)
	}
}

// ---------------------------------------------------------------------------
// handleRunning — child task result truncation
// ---------------------------------------------------------------------------

func TestHandleRunning_ChildResultTruncated(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job5", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-trunc",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "parent-trunc"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-trunc", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "parent-job5",
		},
	}
	r := newUnitReconciler(scheme, task, job, child)
	// Save a large result > 4096 bytes
	largeResult := make([]byte, 5000)
	for i := range largeResult {
		largeResult[i] = 'x'
	}
	_ = r.ResultStore.SaveResult(context.Background(), "default", "child-trunc", largeResult)
	_, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(task.Status.ChildTasks) != 1 {
		t.Fatalf("expected 1 child, got %d", len(task.Status.ChildTasks))
	}
	if len(task.Status.ChildTasks[0].Result) > 4200 {
		t.Error("expected result to be truncated")
	}
}

// ---------------------------------------------------------------------------
// handleRunning — is child task (skip child status aggregation)
// ---------------------------------------------------------------------------

func TestHandleRunning_IsChildTask(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "child-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-self",
			Namespace: "default",
			Labels:    map[string]string{labels.LabelParentTask: "some-parent"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "child-job",
		},
	}
	r := newUnitReconciler(scheme, task, job)
	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
	// Child tasks should not aggregate child statuses
	if len(task.Status.ChildTasks) != 0 {
		t.Error("child task should not have child statuses")
	}
}

// ---------------------------------------------------------------------------
// handleRunning — no timeout (nil fields)
// ---------------------------------------------------------------------------

func TestHandleRunning_NoTimeoutFields(t *testing.T) {
	scheme := newTestScheme()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job-notimeout", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "no-timeout", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: "job-notimeout",
		},
	}
	r := newUnitReconciler(scheme, task, job)
	result, err := r.handleRunning(context.Background(), task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected 5s requeue, got %v", result.RequeueAfter)
	}
}

// ---------------------------------------------------------------------------
// resolveProvider — cross-namespace enforcement
// ---------------------------------------------------------------------------

func TestResolveProvider_CrossNamespaceEnforced(t *testing.T) {
	scheme := newTestScheme()
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-prov", Namespace: "other-ns"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{Name: "sec"},
		},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-cross", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "cross-prov", Namespace: "other-ns"},
			},
		},
	}
	r := newUnitReconciler(scheme, provider, task)
	r.EnforceNamespaceIsolation = true
	_, err := r.resolveProvider(context.Background(), task, nil)
	if err == nil {
		t.Error("expected error for cross-namespace provider with isolation")
	}
}

// ---------------------------------------------------------------------------
// resolveProvider — agent fallback
// ---------------------------------------------------------------------------

func TestResolveProvider_AgentFallback(t *testing.T) {
	scheme := newTestScheme()
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-prov", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: corev1alpha1.ProviderSecretRef{Name: "sec"},
		},
		Status: corev1alpha1.ProviderStatus{Ready: true},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-with-prov", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "agent-prov"},
			Model:       &corev1alpha1.ModelConfig{Provider: "openai"},
		},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t-agent-prov", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	r := newUnitReconciler(scheme, provider, task, agent)
	got, err := r.resolveProvider(context.Background(), task, agent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.Name != "agent-prov" {
		t.Errorf("expected agent-prov, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// ensureWorkerRBAC — error paths
// ---------------------------------------------------------------------------

func TestEnsureWorkerRBAC_SAExistsButCRBMissing(t *testing.T) {
	scheme := newTestScheme()
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: AIWorkerServiceAccount, Namespace: "test-ns2"},
	}
	r := newUnitReconciler(scheme, sa)
	err := r.ensureWorkerRBAC(context.Background(), "test-ns2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedBindings := []string{
		fmt.Sprintf("orka-ai-worker-%s", "test-ns2"),
		fmt.Sprintf("orka-vendor-worker-%s", "test-ns2"),
		fmt.Sprintf("orka-container-worker-%s", "test-ns2"),
	}
	for _, bindingName := range expectedBindings {
		// CRB should be created.
		crb := &rbacv1.ClusterRoleBinding{}
		if err := r.Get(context.Background(), types.NamespacedName{
			Name: bindingName,
		}, crb); err != nil {
			t.Errorf("expected CRB %s to be created: %v", bindingName, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Verify existing Ginkgo tests are unaffected (build check only)
// ---------------------------------------------------------------------------

// These standard Go tests live alongside the Ginkgo test file.
// They deliberately avoid TestMain or any Ginkgo bootstrap to stay independent.

// Ensure the store.ErrNotFound sentinel is used correctly in tests above.
var _ = store.ErrNotFound
