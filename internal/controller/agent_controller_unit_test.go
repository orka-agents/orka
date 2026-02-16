/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const testNS = "test-ns"

func setupAgentReconciler(objs ...runtime.Object) *AgentReconciler {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	clientObjs := make([]runtime.Object, len(objs))
	copy(clientObjs, objs)

	builder := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(clientObjs...)
	return &AgentReconciler{
		Client: builder.Build(),
		Scheme: scheme,
	}
}

func boolPtr(b bool) *bool { return &b }

func baseAgent(name string) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
		},
	}
}

// ---------- validateAgent ----------

func TestValidateAgent_ValidModelProvider(t *testing.T) {
	r := setupAgentReconciler()
	agent := baseAgent("valid")

	if err := r.validateAgent(context.Background(), agent); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestValidateAgent_RuntimeAndProviderRefMutuallyExclusive(t *testing.T) {
	r := setupAgentReconciler()
	agent := baseAgent("exclusive")
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot}
	agent.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "my-provider"}

	err := r.validateAgent(context.Background(), agent)
	if err == nil {
		t.Fatal("expected error for mutually exclusive runtime+providerRef")
	}
	if got := err.Error(); got != "runtime and providerRef are mutually exclusive" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestValidateAgent_NoProviderRefNoModelProvider(t *testing.T) {
	r := setupAgentReconciler()
	agent := baseAgent("missing")
	agent.Spec.Model = nil
	agent.Spec.ProviderRef = nil
	agent.Spec.Runtime = nil

	err := r.validateAgent(context.Background(), agent)
	if err == nil {
		t.Fatal("expected error when neither providerRef nor model.provider is set")
	}
}

func TestValidateAgent_RuntimeOnly(t *testing.T) {
	r := setupAgentReconciler()
	agent := baseAgent("runtime-only")
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude}
	agent.Spec.ProviderRef = nil
	agent.Spec.Model = nil

	if err := r.validateAgent(context.Background(), agent); err != nil {
		t.Errorf("runtime-only agent should be valid, got %v", err)
	}
}

// ---------- validateProviderRef ----------

func TestValidateProviderRef(t *testing.T) {
	readyProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-prov", Namespace: testNS},
		Status:     corev1alpha1.ProviderStatus{Ready: true},
	}
	notReadyProvider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ready-prov", Namespace: testNS},
		Status:     corev1alpha1.ProviderStatus{Ready: false},
	}

	tests := []struct {
		name      string
		agent     *corev1alpha1.Agent
		objs      []runtime.Object
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "nil providerRef",
			agent: baseAgent("no-ref"),
		},
		{
			name: "provider exists and ready",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("with-ref")
				a.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "ready-prov"}
				return a
			}(),
			objs: []runtime.Object{readyProvider},
		},
		{
			name: "provider not found",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("missing-ref")
				a.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "nonexistent"}
				return a
			}(),
			wantErr:   true,
			errSubstr: "not found",
		},
		{
			name: "provider not ready",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("not-ready-ref")
				a.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "not-ready-prov"}
				return a
			}(),
			objs:      []runtime.Object{notReadyProvider},
			wantErr:   true,
			errSubstr: "not ready",
		},
		{
			name: "provider in custom namespace",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("cross-ns")
				a.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "ready-prov", Namespace: "other-ns"}
				return a
			}(),
			objs: []runtime.Object{
				&corev1alpha1.Provider{
					ObjectMeta: metav1.ObjectMeta{Name: "ready-prov", Namespace: "other-ns"},
					Status:     corev1alpha1.ProviderStatus{Ready: true},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			err := r.validateProviderRef(context.Background(), tt.agent)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.errSubstr != "" && err != nil {
				if got := err.Error(); !strContains(got, tt.errSubstr) {
					t.Errorf("error %q should contain %q", got, tt.errSubstr)
				}
			}
		})
	}
}

// ---------- validateSecretRef ----------

func TestValidateSecretRef(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: testNS},
		Data:       map[string][]byte{"api-key": []byte("secret-val")},
	}

	tests := []struct {
		name      string
		agent     *corev1alpha1.Agent
		objs      []runtime.Object
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "nil secretRef",
			agent: baseAgent("no-secret"),
		},
		{
			name: "secret exists",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("with-secret")
				a.Spec.SecretRef = &corev1.LocalObjectReference{Name: "my-secret"}
				return a
			}(),
			objs: []runtime.Object{secret},
		},
		{
			name: "secret not found",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("missing-secret")
				a.Spec.SecretRef = &corev1.LocalObjectReference{Name: "nonexistent"}
				return a
			}(),
			wantErr:   true,
			errSubstr: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			err := r.validateSecretRef(context.Background(), tt.agent)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.errSubstr != "" && err != nil {
				if got := err.Error(); !strContains(got, tt.errSubstr) {
					t.Errorf("error %q should contain %q", got, tt.errSubstr)
				}
			}
		})
	}
}

// ---------- validateTools ----------

func TestValidateTools(t *testing.T) {
	existingTool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-tool", Namespace: testNS},
	}

	tests := []struct {
		name    string
		agent   *corev1alpha1.Agent
		objs    []runtime.Object
		wantErr bool
	}{
		{
			name:  "no tools",
			agent: baseAgent("no-tools"),
		},
		{
			name: "existing tool",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("with-tool")
				a.Spec.Tools = []corev1alpha1.ToolReference{{Name: "existing-tool"}}
				return a
			}(),
			objs: []runtime.Object{existingTool},
		},
		{
			name: "missing tool treated as built-in (no error)",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("builtin-tool")
				a.Spec.Tools = []corev1alpha1.ToolReference{{Name: "code_exec"}}
				return a
			}(),
		},
		{
			name: "disabled tool skipped",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("disabled-tool")
				a.Spec.Tools = []corev1alpha1.ToolReference{{Name: "nonexistent", Enabled: boolPtr(false)}}
				return a
			}(),
		},
		{
			name: "enabled tool that exists",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("enabled-tool")
				a.Spec.Tools = []corev1alpha1.ToolReference{{Name: "existing-tool", Enabled: boolPtr(true)}}
				return a
			}(),
			objs: []runtime.Object{existingTool},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			err := r.validateTools(context.Background(), tt.agent)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ---------- validateSkills ----------

func TestValidateSkills(t *testing.T) {
	skillCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "skill-cm", Namespace: testNS},
		Data:       map[string]string{"skill.yaml": "content"},
	}

	tests := []struct {
		name      string
		agent     *corev1alpha1.Agent
		objs      []runtime.Object
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "no skills",
			agent: baseAgent("no-skills"),
		},
		{
			name: "skill configmap exists",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("with-skill")
				a.Spec.Skills = []corev1alpha1.SkillReference{
					{ConfigMapRef: corev1alpha1.ConfigMapReference{Name: "skill-cm"}},
				}
				return a
			}(),
			objs: []runtime.Object{skillCM},
		},
		{
			name: "skill configmap not found",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("missing-skill")
				a.Spec.Skills = []corev1alpha1.SkillReference{
					{ConfigMapRef: corev1alpha1.ConfigMapReference{Name: "nonexistent"}},
				}
				return a
			}(),
			wantErr:   true,
			errSubstr: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			err := r.validateSkills(context.Background(), tt.agent)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.errSubstr != "" && err != nil {
				if got := err.Error(); !strContains(got, tt.errSubstr) {
					t.Errorf("error %q should contain %q", got, tt.errSubstr)
				}
			}
		})
	}
}

// ---------- validateSystemPromptConfigMap ----------

func TestValidateSystemPromptConfigMap(t *testing.T) {
	promptCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "prompt-cm", Namespace: testNS},
		Data:       map[string]string{"prompt": "You are a helpful assistant."},
	}

	tests := []struct {
		name      string
		agent     *corev1alpha1.Agent
		objs      []runtime.Object
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "no system prompt",
			agent: baseAgent("no-prompt"),
		},
		{
			name: "inline prompt only",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("inline-prompt")
				a.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "hello"}
				return a
			}(),
		},
		{
			name: "configmap exists with correct key",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("valid-cm")
				a.Spec.SystemPrompt = &corev1alpha1.PromptSource{
					ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "prompt-cm", Key: "prompt"},
				}
				return a
			}(),
			objs: []runtime.Object{promptCM},
		},
		{
			name: "configmap not found",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("missing-cm")
				a.Spec.SystemPrompt = &corev1alpha1.PromptSource{
					ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "nonexistent", Key: "prompt"},
				}
				return a
			}(),
			wantErr:   true,
			errSubstr: "not found",
		},
		{
			name: "configmap exists but key missing",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("wrong-key")
				a.Spec.SystemPrompt = &corev1alpha1.PromptSource{
					ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "prompt-cm", Key: "missing-key"},
				}
				return a
			}(),
			objs:      []runtime.Object{promptCM},
			wantErr:   true,
			errSubstr: "key \"missing-key\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			err := r.validateSystemPromptConfigMap(context.Background(), tt.agent)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.errSubstr != "" && err != nil {
				if got := err.Error(); !strContains(got, tt.errSubstr) {
					t.Errorf("error %q should contain %q", got, tt.errSubstr)
				}
			}
		})
	}
}

// ---------- validateCoordination ----------

func TestValidateCoordination(t *testing.T) {
	delegateAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "delegate", Namespace: testNS},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Provider: "openai"},
		},
	}

	tests := []struct {
		name      string
		agent     *corev1alpha1.Agent
		objs      []runtime.Object
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "nil coordination",
			agent: baseAgent("no-coord"),
		},
		{
			name: "coordination disabled",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("disabled-coord")
				a.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: false}
				return a
			}(),
		},
		{
			name: "coordination enabled, no allowed agents",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("coord-empty")
				a.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true}
				return a
			}(),
		},
		{
			name: "coordination enabled, delegate exists",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("coord-ok")
				a.Spec.Coordination = &corev1alpha1.CoordinationConfig{
					Enabled:       true,
					AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "delegate"}},
				}
				return a
			}(),
			objs: []runtime.Object{delegateAgent},
		},
		{
			name: "coordination enabled, delegate not found",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("coord-missing")
				a.Spec.Coordination = &corev1alpha1.CoordinationConfig{
					Enabled:       true,
					AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "nonexistent"}},
				}
				return a
			}(),
			wantErr:   true,
			errSubstr: "not found",
		},
		{
			name: "coordination with cross-namespace delegate",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("coord-cross-ns")
				a.Spec.Coordination = &corev1alpha1.CoordinationConfig{
					Enabled:       true,
					AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "other-agent", Namespace: "other-ns"}},
				}
				return a
			}(),
			objs: []runtime.Object{
				&corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: "other-agent", Namespace: "other-ns"},
					Spec:       corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Provider: "openai"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			err := r.validateCoordination(context.Background(), tt.agent)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.errSubstr != "" && err != nil {
				if got := err.Error(); !strContains(got, tt.errSubstr) {
					t.Errorf("error %q should contain %q", got, tt.errSubstr)
				}
			}
		})
	}
}

// ---------- countActiveTasks ----------

func TestCountActiveTasks(t *testing.T) {
	agent := baseAgent("count-agent")

	pendingTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-pending", Namespace: testNS},
		Spec: corev1alpha1.TaskSpec{
			AgentRef: &corev1alpha1.AgentReference{Name: "count-agent"},
			Prompt:   "do something",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	runningTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-running", Namespace: testNS},
		Spec: corev1alpha1.TaskSpec{
			AgentRef: &corev1alpha1.AgentReference{Name: "count-agent"},
			Prompt:   "do something",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	succeededTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-succeeded", Namespace: testNS},
		Spec: corev1alpha1.TaskSpec{
			AgentRef: &corev1alpha1.AgentReference{Name: "count-agent"},
			Prompt:   "do something",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	failedTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-failed", Namespace: testNS},
		Spec: corev1alpha1.TaskSpec{
			AgentRef: &corev1alpha1.AgentReference{Name: "count-agent"},
			Prompt:   "do something",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed},
	}
	otherAgentTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-other", Namespace: testNS},
		Spec: corev1alpha1.TaskSpec{
			AgentRef: &corev1alpha1.AgentReference{Name: "other-agent"},
			Prompt:   "do something",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	noAgentRefTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-no-ref", Namespace: testNS},
		Spec: corev1alpha1.TaskSpec{
			Prompt: "do something",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}

	tests := []struct {
		name string
		objs []runtime.Object
		want int32
	}{
		{
			name: "no tasks",
			want: 0,
		},
		{
			name: "only pending and running count",
			objs: []runtime.Object{pendingTask, runningTask, succeededTask, failedTask},
			want: 2,
		},
		{
			name: "other agent tasks not counted",
			objs: []runtime.Object{runningTask, otherAgentTask},
			want: 1,
		},
		{
			name: "tasks without agentRef not counted",
			objs: []runtime.Object{noAgentRefTask},
			want: 0,
		},
		{
			name: "all terminal - zero active",
			objs: []runtime.Object{succeededTask, failedTask},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.objs...)
			got, err := r.countActiveTasks(context.Background(), agent)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("countActiveTasks = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---------- updateStatus ----------

func TestUpdateStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	t.Run("validation success sets Ready condition true", func(t *testing.T) {
		agent := baseAgent("status-ok")
		fc := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).
			WithStatusSubresource(agent).Build()
		r := &AgentReconciler{Client: fc, Scheme: scheme}

		result, err := r.updateStatus(context.Background(), agent, 0, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.RequeueAfter != 0 {
			t.Errorf("expected no requeue, got %v", result.RequeueAfter)
		}
		if agent.Status.ActiveTasks != 0 {
			t.Errorf("ActiveTasks = %d, want 0", agent.Status.ActiveTasks)
		}
		cond := findCondition(agent.Status.Conditions, "Ready")
		if cond == nil {
			t.Fatal("Ready condition not found")
		}
		if cond.Status != metav1.ConditionTrue {
			t.Errorf("Ready status = %s, want True", cond.Status)
		}
		if cond.Reason != "ValidationSucceeded" {
			t.Errorf("Reason = %s, want ValidationSucceeded", cond.Reason)
		}
	})

	t.Run("validation failure sets Ready condition false", func(t *testing.T) {
		agent := baseAgent("status-fail")
		fc := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).
			WithStatusSubresource(agent).Build()
		r := &AgentReconciler{Client: fc, Scheme: scheme}

		_, err := r.updateStatus(context.Background(), agent, 0, errTest("bad config"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cond := findCondition(agent.Status.Conditions, "Ready")
		if cond == nil {
			t.Fatal("Ready condition not found")
		}
		if cond.Status != metav1.ConditionFalse {
			t.Errorf("Ready status = %s, want False", cond.Status)
		}
		if cond.Reason != "ValidationFailed" {
			t.Errorf("Reason = %s, want ValidationFailed", cond.Reason)
		}
		if cond.Message != "bad config" {
			t.Errorf("Message = %q, want %q", cond.Message, "bad config")
		}
	})

	t.Run("active tasks sets LastUsed", func(t *testing.T) {
		agent := baseAgent("status-active")
		fc := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).
			WithStatusSubresource(agent).Build()
		r := &AgentReconciler{Client: fc, Scheme: scheme}

		_, err := r.updateStatus(context.Background(), agent, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if agent.Status.ActiveTasks != 3 {
			t.Errorf("ActiveTasks = %d, want 3", agent.Status.ActiveTasks)
		}
		if agent.Status.LastUsed == nil {
			t.Fatal("LastUsed should be set when activeTasks > 0")
		}
	})

	t.Run("TTL requeue when idle", func(t *testing.T) {
		agent := baseAgent("status-ttl")
		ttl := metav1.Duration{Duration: 10 * time.Minute}
		agent.Spec.TTLAfterLastTask = &ttl
		lastUsed := metav1.NewTime(time.Now().Add(-5 * time.Minute))
		agent.Status.LastUsed = &lastUsed

		fc := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).
			WithStatusSubresource(agent).Build()
		r := &AgentReconciler{Client: fc, Scheme: scheme}

		result, err := r.updateStatus(context.Background(), agent, 0, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.RequeueAfter <= 0 {
			t.Error("expected positive RequeueAfter for non-expired TTL")
		}
		if result.RequeueAfter > 6*time.Minute {
			t.Errorf("RequeueAfter too large: %v", result.RequeueAfter)
		}
	})
}

// ---------- checkTTLExpiry ----------

func TestCheckTTLExpiry(t *testing.T) {
	tests := []struct {
		name        string
		agent       *corev1alpha1.Agent
		activeTasks int32
		wantDeleted bool
	}{
		{
			name:  "no TTL set",
			agent: baseAgent("no-ttl"),
		},
		{
			name: "zero duration TTL",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("zero-ttl")
				a.Spec.TTLAfterLastTask = &metav1.Duration{Duration: 0}
				return a
			}(),
		},
		{
			name: "active tasks prevent deletion",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("active-ttl")
				a.Spec.TTLAfterLastTask = &metav1.Duration{Duration: time.Second}
				lastUsed := metav1.NewTime(time.Now().Add(-time.Hour))
				a.Status.LastUsed = &lastUsed
				return a
			}(),
			activeTasks: 1,
		},
		{
			name: "TTL not expired yet",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("not-expired")
				a.Spec.TTLAfterLastTask = &metav1.Duration{Duration: time.Hour}
				lastUsed := metav1.NewTime(time.Now())
				a.Status.LastUsed = &lastUsed
				return a
			}(),
		},
		{
			name: "TTL expired, no LastUsed (uses creation time)",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("expired-no-last")
				a.Spec.TTLAfterLastTask = &metav1.Duration{Duration: time.Second}
				a.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
				return a
			}(),
			wantDeleted: true,
		},
		{
			name: "TTL expired with LastUsed",
			agent: func() *corev1alpha1.Agent {
				a := baseAgent("expired")
				a.Spec.TTLAfterLastTask = &metav1.Duration{Duration: time.Second}
				lastUsed := metav1.NewTime(time.Now().Add(-time.Hour))
				a.Status.LastUsed = &lastUsed
				return a
			}(),
			wantDeleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := setupAgentReconciler(tt.agent)
			_, deleted := r.checkTTLExpiry(context.Background(), tt.agent, tt.activeTasks)
			if deleted != tt.wantDeleted {
				t.Errorf("deleted = %v, want %v", deleted, tt.wantDeleted)
			}
		})
	}
}

// ---------- helpers ----------

type errTest string

func (e errTest) Error() string { return string(e) }

func strContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
