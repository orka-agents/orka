/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

// newTestScheme creates a scheme with all types needed for unit tests.
func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
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
		SessionStore:   ss,
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
			name: "attempts == maxRetries",
			task: &corev1alpha1.Task{
				Spec:   corev1alpha1.TaskSpec{RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 3}},
				Status: corev1alpha1.TaskStatus{Attempts: 3},
			},
			expect: false,
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
				MaxRetries:  3,
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
	var objs []client.Object
	objs = append(objs, parent)
	for i := 0; i < 5; i++ {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "child-s" + time.Now().Add(time.Duration(i)*time.Hour).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{"orka.ai/parent-task": "parent"},
				CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Hour)),
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		})
	}
	for i := 0; i < 3; i++ {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "child-f" + time.Now().Add(time.Duration(i)*time.Hour).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{"orka.ai/parent-task": "parent"},
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
		client.MatchingLabels{"orka.ai/parent-task": "parent"})

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
			SuccessfulRunsHistoryLimit: ptr.To(int32(1)),
			FailedRunsHistoryLimit:     ptr.To(int32(0)),
		},
	}

	var objs []client.Object
	objs = append(objs, parent)
	for i := 0; i < 4; i++ {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cs-" + time.Now().Add(time.Duration(i)*time.Minute).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{"orka.ai/parent-task": "parent"},
				CreationTimestamp: metav1.NewTime(time.Now().Add(time.Duration(i) * time.Minute)),
			},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		})
	}
	for i := 0; i < 2; i++ {
		objs = append(objs, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "cf-" + time.Now().Add(time.Duration(i)*time.Minute).Format("150405"),
				Namespace:         "default",
				Labels:            map[string]string{"orka.ai/parent-task": "parent"},
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
		client.MatchingLabels{"orka.ai/parent-task": "parent"})

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
				"orka.ai/coordination-depth": "1",
			},
			Labels: map[string]string{
				"orka.ai/parent-task": "parent",
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
				"orka.ai/coordination-depth": "3",
			},
			Labels: map[string]string{
				"orka.ai/parent-task": "parent",
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
				"orka.ai/coordination-depth": "1",
			},
			Labels: map[string]string{
				"orka.ai/parent-task": "parent",
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
				"orka.ai/coordination-depth": "1",
			},
			Labels: map[string]string{
				"orka.ai/parent-task": "parent",
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
				"orka.ai/created-by":  "create_agent",
				"orka.ai/parent-task": "parent",
			},
		},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				"orka.ai/coordination-depth": "1",
			},
			Labels: map[string]string{
				"orka.ai/parent-task": "parent",
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
			Labels:    map[string]string{"orka.ai/parent-task": "parent"},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child",
			Namespace: "default",
			Annotations: map[string]string{
				"orka.ai/coordination-depth": "1",
			},
			Labels: map[string]string{
				"orka.ai/parent-task": "parent",
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

	err := r.ensureWorkerRBAC(context.Background(), "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify ServiceAccount was created
	sa := &corev1.ServiceAccount{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: workerServiceAccountName, Namespace: "test-ns",
	}, sa); err != nil {
		t.Errorf("expected SA to exist: %v", err)
	}

	// Verify ClusterRoleBinding was created
	crb := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "orka-worker-test-ns",
	}, crb); err != nil {
		t.Errorf("expected CRB to exist: %v", err)
	}
	if crb.RoleRef.Name != workerClusterRoleName {
		t.Errorf("expected roleRef %s, got %s", workerClusterRoleName, crb.RoleRef.Name)
	}
}

func TestEnsureWorkerRBAC_Idempotent(t *testing.T) {
	scheme := newTestScheme()
	// Pre-create the SA and CRB
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: workerServiceAccountName, Namespace: "test-ns"},
	}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "orka-worker-test-ns"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     workerClusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: workerServiceAccountName, Namespace: "test-ns",
		}},
	}
	r := newUnitReconciler(scheme, sa, crb)

	// Should not fail when resources already exist
	if err := r.ensureWorkerRBAC(context.Background(), "test-ns"); err != nil {
		t.Fatalf("unexpected error on idempotent call: %v", err)
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
			Suspend:  ptr.To(true),
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
// Verify existing Ginkgo tests are unaffected (build check only)
// ---------------------------------------------------------------------------

// These standard Go tests live alongside the Ginkgo test file.
// They deliberately avoid TestMain or any Ginkgo bootstrap to stay independent.

// Ensure the store.ErrNotFound sentinel is used correctly in tests above.
var _ = store.ErrNotFound
