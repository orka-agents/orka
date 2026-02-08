/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

func setupJobBuilder() *JobBuilder {
	scheme := runtime.NewScheme()
	corev1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	b := NewJobBuilder(fakeClient)
	b.ControllerURL = "http://mercan-controller.mercan-system.svc:8080"
	return b
}

func TestNewJobBuilder(t *testing.T) {
	builder := setupJobBuilder()
	if builder == nil {
		t.Fatal("NewJobBuilder returned nil")
	}
	if builder.AIWorkerImage != DefaultAIWorkerImage {
		t.Errorf("AIWorkerImage = %s, want %s", builder.AIWorkerImage, DefaultAIWorkerImage)
	}
	if builder.GeneralWorkerImage != DefaultGeneralWorkerImage {
		t.Errorf("GeneralWorkerImage = %s, want %s", builder.GeneralWorkerImage, DefaultGeneralWorkerImage)
	}
}

func TestJobBuilder_Build_ContainerTask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:latest",
			Command: []string{"echo"},
			Args:    []string{"hello"},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if job == nil {
		t.Fatal("Build() returned nil job")
	}

	// Verify container settings
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "busybox:latest" {
		t.Errorf("Image = %s, want busybox:latest", container.Image)
	}
	if len(container.Command) != 1 || container.Command[0] != "echo" {
		t.Errorf("Command = %v, want [echo]", container.Command)
	}
	if len(container.Args) != 1 || container.Args[0] != "hello" {
		t.Errorf("Args = %v, want [hello]", container.Args)
	}
}

func TestJobBuilder_Build_AITask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider: "anthropic",
				Model:    "claude-3-5-sonnet",
				Prompt:   "Hello",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultAIWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultAIWorkerImage)
	}
}

func TestJobBuilder_Build_WithTimeout(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Timeout: &metav1.Duration{Duration: 5 * time.Minute},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Error("ActiveDeadlineSeconds should be set")
	}
	if *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 300", *job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobBuilder_Build_WithSession(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			SessionRef: &corev1alpha1.SessionReference{
				Name: "test-session",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Verify session-data emptyDir volume
	hasSessionVolume := false
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "session-data" {
			hasSessionVolume = true
			if vol.EmptyDir == nil {
				t.Error("session-data volume should be emptyDir")
			}
			break
		}
	}
	if !hasSessionVolume {
		t.Error("Job should have session-data volume")
	}

	// Verify init container exists
	if len(job.Spec.Template.Spec.InitContainers) == 0 {
		t.Fatal("Job should have init container for session fetch")
	}
	initContainer := job.Spec.Template.Spec.InitContainers[0]
	if initContainer.Name != "fetch-session" {
		t.Errorf("Init container name = %s, want fetch-session", initContainer.Name)
	}
}

func TestJobBuilder_buildPodSecurityContext(t *testing.T) {
	builder := setupJobBuilder()
	psc := builder.buildPodSecurityContext()

	if psc == nil {
		t.Fatal("buildPodSecurityContext returned nil")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("RunAsNonRoot should be true")
	}
	if psc.RunAsUser == nil || *psc.RunAsUser != 1000 {
		t.Errorf("RunAsUser = %v, want 1000", psc.RunAsUser)
	}
}

func TestJobBuilder_buildContainerSecurityContext(t *testing.T) {
	builder := setupJobBuilder()
	csc := builder.buildContainerSecurityContext()

	if csc == nil {
		t.Fatal("buildContainerSecurityContext returned nil")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem should be true")
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) == 0 {
		t.Error("Should drop all capabilities")
	}
}

func TestJobBuilder_buildResources_TaskResources(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
	}

	resources := builder.buildResources(task, nil)
	cpuReq := resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "200m" {
		t.Errorf("CPU request = %s, want 200m", cpuReq.String())
	}
}

func TestJobBuilder_buildResources_AgentResources(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{}, // No resources
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("300m"),
				},
			},
		},
	}

	resources := builder.buildResources(task, agent)
	cpuReq := resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "300m" {
		t.Errorf("CPU request = %s, want 300m", cpuReq.String())
	}
}

func TestJobBuilder_buildResources_Defaults(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{}, // No resources
	}

	resources := builder.buildResources(task, nil)
	cpuReq := resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "100m" {
		t.Errorf("CPU request = %s, want 100m (default)", cpuReq.String())
	}
}

func TestJobBuilder_buildEnvVars(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeContainer,
			Env: []corev1.EnvVar{
				{Name: "CUSTOM_VAR", Value: "custom-value"},
			},
		},
	}

	envVars := builder.buildEnvVars(task, nil, nil)

	// Check required env vars
	hasTaskName := false
	hasTaskNamespace := false
	hasResultEndpoint := false
	hasControllerURL := false
	hasCustomVar := false

	for _, env := range envVars {
		switch env.Name {
		case TaskNameEnvVar:
			hasTaskName = true
			if env.Value != "test-task" {
				t.Errorf("MERCAN_TASK_NAME = %s, want test-task", env.Value)
			}
		case TaskNamespaceEnvVar:
			hasTaskNamespace = true
			if env.Value != "default" {
				t.Errorf("MERCAN_TASK_NAMESPACE = %s, want default", env.Value)
			}
		case ResultEndpointEnvVar:
			hasResultEndpoint = true
		case ControllerURLEnvVar:
			hasControllerURL = true
		case "CUSTOM_VAR":
			hasCustomVar = true
			if env.Value != "custom-value" {
				t.Errorf("CUSTOM_VAR = %s, want custom-value", env.Value)
			}
		}
	}

	if !hasTaskName {
		t.Error("Missing MERCAN_TASK_NAME")
	}
	if !hasTaskNamespace {
		t.Error("Missing MERCAN_TASK_NAMESPACE")
	}
	if !hasResultEndpoint {
		t.Error("Missing MERCAN_RESULT_ENDPOINT")
	}
	if !hasControllerURL {
		t.Error("Missing MERCAN_CONTROLLER_URL")
	}
	if !hasCustomVar {
		t.Error("Missing CUSTOM_VAR")
	}
}

func TestJobBuilder_buildEnvVars_AITask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI: &corev1alpha1.AISpec{
				Provider:     "anthropic",
				Model:        "claude-3-5-sonnet",
				Prompt:       "Hello",
				SystemPrompt: "You are helpful",
				Tools:        []string{"web_search"},
			},
		},
	}

	envVars := builder.buildEnvVars(task, nil, nil)

	// Check AI-specific env vars
	hasProvider := false
	hasModel := false
	hasPrompt := false

	for _, env := range envVars {
		switch env.Name {
		case "MERCAN_AI_PROVIDER":
			hasProvider = true
			if env.Value != "anthropic" {
				t.Errorf("MERCAN_AI_PROVIDER = %s, want anthropic", env.Value)
			}
		case "MERCAN_AI_MODEL":
			hasModel = true
			if env.Value != "claude-3-5-sonnet" {
				t.Errorf("MERCAN_AI_MODEL = %s, want claude-3-5-sonnet", env.Value)
			}
		case "MERCAN_AI_PROMPT":
			hasPrompt = true
			if env.Value != "Hello" {
				t.Errorf("MERCAN_AI_PROMPT = %s, want Hello", env.Value)
			}
		}
	}

	if !hasProvider {
		t.Error("Missing MERCAN_AI_PROVIDER")
	}
	if !hasModel {
		t.Error("Missing MERCAN_AI_MODEL")
	}
	if !hasPrompt {
		t.Error("Missing MERCAN_AI_PROMPT")
	}
}

func TestJobBuilder_buildEnvVars_WithAgent(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Hello from task",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
			SystemPrompt: &corev1alpha1.PromptSource{
				Inline: "You are an assistant",
			},
			Tools: []corev1alpha1.ToolReference{
				{Name: "code_exec"},
			},
		},
	}

	envVars := builder.buildEnvVars(task, agent, nil)

	// Agent values should be used when task doesn't specify them
	hasProvider := false
	hasModel := false

	for _, env := range envVars {
		switch env.Name {
		case "MERCAN_AI_PROVIDER":
			hasProvider = true
			if env.Value != "openai" {
				t.Errorf("MERCAN_AI_PROVIDER = %s, want openai", env.Value)
			}
		case "MERCAN_AI_MODEL":
			hasModel = true
			if env.Value != "gpt-4" {
				t.Errorf("MERCAN_AI_MODEL = %s, want gpt-4", env.Value)
			}
		}
	}

	if !hasProvider {
		t.Error("Missing MERCAN_AI_PROVIDER")
	}
	if !hasModel {
		t.Error("Missing MERCAN_AI_MODEL")
	}
}

func TestJobBuilder_buildEnvVars_WithProvider(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Hello",
		},
	}
	provider := &corev1alpha1.Provider{
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeAnthropic,
			DefaultModel: "claude-3-opus",
			BaseURL:      "https://custom.api.com",
		},
	}

	envVars := builder.buildEnvVars(task, nil, provider)

	hasBaseURL := false
	for _, env := range envVars {
		if env.Name == "MERCAN_AI_BASE_URL" {
			hasBaseURL = true
			if env.Value != "https://custom.api.com" {
				t.Errorf("MERCAN_AI_BASE_URL = %s, want https://custom.api.com", env.Value)
			}
		}
	}

	if !hasBaseURL {
		t.Error("Missing MERCAN_AI_BASE_URL")
	}
}

func TestJobBuilder_buildContainer_ContainerWithoutImage(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{
			Type:  corev1alpha1.TaskTypeContainer,
			Image: "", // No image specified
		},
	}

	container := builder.buildContainer(task, nil, nil)
	if container.Image != DefaultGeneralWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultGeneralWorkerImage)
	}
}

func TestConstants(t *testing.T) {
	if DefaultAIWorkerImage != "mercan-ai-worker:latest" {
		t.Errorf("DefaultAIWorkerImage = %s", DefaultAIWorkerImage)
	}
	if DefaultGeneralWorkerImage != "mercan-general-worker:latest" {
		t.Errorf("DefaultGeneralWorkerImage = %s", DefaultGeneralWorkerImage)
	}
	if ResultEndpointEnvVar != "MERCAN_RESULT_ENDPOINT" {
		t.Errorf("ResultEndpointEnvVar = %s", ResultEndpointEnvVar)
	}
	if ControllerURLEnvVar != "MERCAN_CONTROLLER_URL" {
		t.Errorf("ControllerURLEnvVar = %s", ControllerURLEnvVar)
	}
	if TaskNameEnvVar != "MERCAN_TASK_NAME" {
		t.Errorf("TaskNameEnvVar = %s", TaskNameEnvVar)
	}
	if TaskNamespaceEnvVar != "MERCAN_TASK_NAMESPACE" {
		t.Errorf("TaskNamespaceEnvVar = %s", TaskNamespaceEnvVar)
	}
}

func TestJobBuilder_buildEnvVars_WithCoordination(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Coordinate work",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:               true,
				MaxDepth:              3,
				MaxConcurrentChildren: 5,
				AllowedAgents: []corev1alpha1.AllowedAgent{
					{Name: "backend-dev"},
					{Name: "frontend-dev"},
				},
			},
		},
	}

	envVars := builder.buildEnvVars(task, agent, nil)

	tests := []struct {
		name  string
		value string
	}{
		{"MERCAN_COORDINATION_ENABLED", "true"},
		{"MERCAN_COORDINATION_MAX_DEPTH", "3"},
		{"MERCAN_COORDINATION_MAX_CHILDREN", "5"},
		{"MERCAN_COORDINATION_ALLOWED_AGENTS", "backend-dev,frontend-dev"},
		{"MERCAN_COORDINATION_DEPTH", "0"},
	}
	for _, tt := range tests {
		env, found := findEnvVar(envVars, tt.name)
		if !found {
			t.Errorf("Missing %s", tt.name)
		} else if env.Value != tt.value {
			t.Errorf("%s = %s, want %s", tt.name, env.Value, tt.value)
		}
	}

	toolsEnv, found := findEnvVar(envVars, "MERCAN_AI_TOOLS")
	if !found {
		t.Fatal("Missing MERCAN_AI_TOOLS")
	}
	for _, tool := range []string{"delegate_task", "wait_for_tasks"} {
		if !strings.Contains(toolsEnv.Value, tool) {
			t.Errorf("MERCAN_AI_TOOLS = %s, want to contain %s", toolsEnv.Value, tool)
		}
	}
}

func TestJobBuilder_buildEnvVars_WithCoordination_ChildTask(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-task",
			Namespace: "default",
			Annotations: map[string]string{
				"mercan.ai/coordination-depth": "2",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Sub-task work",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
			Coordination: &corev1alpha1.CoordinationConfig{
				Enabled:               true,
				MaxDepth:              3,
				MaxConcurrentChildren: 5,
				AllowedAgents: []corev1alpha1.AllowedAgent{
					{Name: "backend-dev"},
				},
			},
		},
	}

	envVars := builder.buildEnvVars(task, agent, nil)

	env, found := findEnvVar(envVars, "MERCAN_COORDINATION_DEPTH")
	if !found {
		t.Fatal("Missing MERCAN_COORDINATION_DEPTH")
	}
	if env.Value != "2" {
		t.Errorf("MERCAN_COORDINATION_DEPTH = %s, want 2", env.Value)
	}
}

func TestJobBuilder_buildEnvVars_NoCoordination(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "Simple task",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "anthropic",
				Name:     "claude-3-5-sonnet",
			},
		},
	}

	envVars := builder.buildEnvVars(task, agent, nil)

	coordinationVars := []string{
		"MERCAN_COORDINATION_ENABLED",
		"MERCAN_COORDINATION_MAX_DEPTH",
		"MERCAN_COORDINATION_MAX_CHILDREN",
		"MERCAN_COORDINATION_ALLOWED_AGENTS",
		"MERCAN_COORDINATION_DEPTH",
	}
	for _, name := range coordinationVars {
		if _, found := findEnvVar(envVars, name); found {
			t.Errorf("Unexpected env var %s present without coordination", name)
		}
	}
}

// --- Agent task path tests ---

// helper to find env var by name
func findEnvVar(envVars []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envVars {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// helper to check volume exists by name
func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// helper to check volume mount exists by name
func hasVolumeMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

func TestJobBuilder_Build_AgentTask_CopilotRuntime(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCopilot,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultCopilotWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultCopilotWorkerImage)
	}
	if len(container.Command) != 1 || container.Command[0] != "/worker" {
		t.Errorf("Command = %v, want [/worker]", container.Command)
	}
	if len(container.Args) != 1 || container.Args[0] != "--mode=agent" {
		t.Errorf("Args = %v, want [--mode=agent]", container.Args)
	}
}

func TestJobBuilder_Build_AgentTask_ClaudeRuntime(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeClaude,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Errorf("Image = %s, want %s", container.Image, DefaultClaudeWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_NilAgent_FallbackToClaude(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Errorf("Image = %s, want %s (fallback)", container.Image, DefaultClaudeWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_NilRuntime_FallbackToClaude(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Fix the bug",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			// Runtime is nil
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != DefaultClaudeWorkerImage {
		t.Errorf("Image = %s, want %s (fallback)", container.Image, DefaultClaudeWorkerImage)
	}
}

func TestJobBuilder_Build_AgentTask_EnvVars(t *testing.T) {
	builder := setupJobBuilder()
	maxTurns := int32(20)
	allowBash := true
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "test-ns",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Refactor the code",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				MaxTurns:        &maxTurns,
				AllowedTools:    []string{"Read", "Write", "Bash"},
				DisallowedTools: []string{"WebFetch"},
				AllowBash:       &allowBash,
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo:    "https://github.com/example/repo",
					Branch:     "main",
					Ref:        "abc123",
					SubPath:    "src",
					PushBranch: "feature/my-change",
				},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Name: "claude-sonnet-4-20250514",
			},
			SystemPrompt: &corev1alpha1.PromptSource{
				Inline: "You are a coding assistant",
			},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeClaude,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env

	tests := []struct {
		name  string
		value string
	}{
		{TaskNameEnvVar, "agent-task"},
		{TaskNamespaceEnvVar, "test-ns"},
		{ResultEndpointEnvVar, "http://mercan-controller.mercan-system.svc:8080/internal/v1/results/test-ns/agent-task"},
		{ControllerURLEnvVar, "http://mercan-controller.mercan-system.svc:8080"},
		{"MERCAN_PROMPT", "Refactor the code"},
		{"MERCAN_MODEL", "claude-sonnet-4-20250514"},
		{"MERCAN_SYSTEM_PROMPT", "You are a coding assistant"},
		{"MERCAN_MAX_TURNS", "20"},
		{"MERCAN_ALLOWED_TOOLS", "Read,Write,Bash"},
		{"MERCAN_DISALLOWED_TOOLS", "WebFetch"},
		{"MERCAN_ALLOW_BASH", "true"},
		{"MERCAN_GIT_REPO", "https://github.com/example/repo"},
		{"MERCAN_GIT_BRANCH", "main"},
		{"MERCAN_GIT_REF", "abc123"},
		{"MERCAN_WORKSPACE_SUBPATH", "src"},
		{"MERCAN_PUSH_BRANCH", "feature/my-change"},
	}

	for _, tt := range tests {
		ev, ok := findEnvVar(envVars, tt.name)
		if !ok {
			t.Errorf("Missing env var %s", tt.name)
			continue
		}
		if ev.Value != tt.value {
			t.Errorf("%s = %q, want %q", tt.name, ev.Value, tt.value)
		}
	}
}

func TestJobBuilder_Build_AgentTask_MaxTurns_Default(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_MAX_TURNS")
	if !ok {
		t.Fatal("Missing MERCAN_MAX_TURNS")
	}
	if ev.Value != "50" {
		t.Errorf("MERCAN_MAX_TURNS = %s, want 50 (default)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_MaxTurns_AgentDefault(t *testing.T) {
	builder := setupJobBuilder()
	agentMaxTurns := int32(100)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeClaude,
				DefaultMaxTurns: &agentMaxTurns,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_MAX_TURNS")
	if !ok {
		t.Fatal("Missing MERCAN_MAX_TURNS")
	}
	if ev.Value != "100" {
		t.Errorf("MERCAN_MAX_TURNS = %s, want 100 (from agent)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_MaxTurns_TaskOverridesAgent(t *testing.T) {
	builder := setupJobBuilder()
	agentMaxTurns := int32(100)
	taskMaxTurns := int32(30)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				MaxTurns: &taskMaxTurns,
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeClaude,
				DefaultMaxTurns: &agentMaxTurns,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_MAX_TURNS")
	if !ok {
		t.Fatal("Missing MERCAN_MAX_TURNS")
	}
	if ev.Value != "30" {
		t.Errorf("MERCAN_MAX_TURNS = %s, want 30 (task override)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowedTools_AgentDefault(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeClaude,
				DefaultAllowedTools: []string{"Read", "Write"},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_ALLOWED_TOOLS")
	if !ok {
		t.Fatal("Missing MERCAN_ALLOWED_TOOLS")
	}
	if ev.Value != "Read,Write" {
		t.Errorf("MERCAN_ALLOWED_TOOLS = %s, want Read,Write", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowedTools_TaskOverridesAgent(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: []string{"Bash"},
			},
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:                corev1alpha1.AgentRuntimeClaude,
				DefaultAllowedTools: []string{"Read", "Write"},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_ALLOWED_TOOLS")
	if !ok {
		t.Fatal("Missing MERCAN_ALLOWED_TOOLS")
	}
	if ev.Value != "Bash" {
		t.Errorf("MERCAN_ALLOWED_TOOLS = %s, want Bash (task override)", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowBash_AgentDefault(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:             corev1alpha1.AgentRuntimeClaude,
				DefaultAllowBash: true,
			},
		},
	}

	job, err := builder.Build(context.Background(), task, agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_ALLOW_BASH")
	if !ok {
		t.Fatal("Missing MERCAN_ALLOW_BASH")
	}
	if ev.Value != "true" {
		t.Errorf("MERCAN_ALLOW_BASH = %s, want true", ev.Value)
	}
}

func TestJobBuilder_Build_AgentTask_AllowBash_NotSetWhenFalse(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	_, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_ALLOW_BASH")
	if ok {
		t.Error("MERCAN_ALLOW_BASH should not be set when allowBash is false")
	}
}

func TestJobBuilder_Build_AgentTask_PromptFromAISpec(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AI: &corev1alpha1.AISpec{
				Prompt: "Prompt from AI spec",
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_PROMPT")
	if !ok {
		t.Fatal("Missing MERCAN_PROMPT")
	}
	if ev.Value != "Prompt from AI spec" {
		t.Errorf("MERCAN_PROMPT = %q, want %q", ev.Value, "Prompt from AI spec")
	}
}

func TestJobBuilder_Build_AgentTask_Volumes(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	volumes := job.Spec.Template.Spec.Volumes
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts

	// tmp volume (always present)
	if !hasVolume(volumes, "tmp") {
		t.Error("Missing tmp volume")
	}
	if !hasVolumeMount(mounts, "tmp") {
		t.Error("Missing tmp volume mount")
	}

	// workspace emptyDir
	if !hasVolume(volumes, "workspace") {
		t.Error("Missing workspace volume")
	}
	if !hasVolumeMount(mounts, "workspace") {
		t.Error("Missing workspace volume mount")
	}
	// Verify workspace is emptyDir
	for _, v := range volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Error("workspace volume should be emptyDir")
			}
		}
	}

	// home emptyDir
	if !hasVolume(volumes, "home") {
		t.Error("Missing home volume")
	}
	if !hasVolumeMount(mounts, "home") {
		t.Error("Missing home volume mount")
	}
	// Verify home is emptyDir
	for _, v := range volumes {
		if v.Name == "home" {
			if v.EmptyDir == nil {
				t.Error("home volume should be emptyDir")
			}
		}
	}

	// Verify mount paths
	for _, m := range mounts {
		switch m.Name {
		case "workspace":
			if m.MountPath != "/workspace" {
				t.Errorf("workspace mountPath = %s, want /workspace", m.MountPath)
			}
		case "home":
			if m.MountPath != "/home/worker" {
				t.Errorf("home mountPath = %s, want /home/worker", m.MountPath)
			}
		case "tmp":
			if m.MountPath != "/tmp" {
				t.Errorf("tmp mountPath = %s, want /tmp", m.MountPath)
			}
		}
	}
}

func TestJobBuilder_Build_AgentTask_GitSecretVolume(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Clone and fix",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					GitSecretRef: &corev1.LocalObjectReference{
						Name: "my-git-creds",
					},
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	volumes := job.Spec.Template.Spec.Volumes
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts

	// git-credentials volume
	if !hasVolume(volumes, "git-credentials") {
		t.Fatal("Missing git-credentials volume")
	}
	for _, v := range volumes {
		if v.Name == "git-credentials" {
			if v.Secret == nil {
				t.Error("git-credentials should be a Secret volume")
			} else if v.Secret.SecretName != "my-git-creds" {
				t.Errorf("git-credentials secretName = %s, want my-git-creds", v.Secret.SecretName)
			}
		}
	}

	// git-credentials mount
	if !hasVolumeMount(mounts, "git-credentials") {
		t.Fatal("Missing git-credentials volume mount")
	}
	for _, m := range mounts {
		if m.Name == "git-credentials" {
			if m.MountPath != "/secrets/git" {
				t.Errorf("git-credentials mountPath = %s, want /secrets/git", m.MountPath)
			}
			if !m.ReadOnly {
				t.Error("git-credentials mount should be read-only")
			}
		}
	}
}

func TestJobBuilder_Build_AgentTask_NoGitSecretVolume_WhenNotSpecified(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: "https://github.com/example/repo",
					// No GitSecretRef
				},
			},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if hasVolume(job.Spec.Template.Spec.Volumes, "git-credentials") {
		t.Error("git-credentials volume should not exist when GitSecretRef is not specified")
	}
}

func TestJobBuilder_Build_AgentTask_SecurityContext(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Pod security context
	psc := job.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("Pod security context is nil")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("RunAsNonRoot should be true")
	}
	if psc.RunAsUser == nil || *psc.RunAsUser != 1000 {
		t.Errorf("RunAsUser = %v, want 1000", psc.RunAsUser)
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("SeccompProfile should be RuntimeDefault")
	}

	// Container security context
	csc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if csc == nil {
		t.Fatal("Container security context is nil")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem should be true")
	}
	if csc.RunAsNonRoot == nil || !*csc.RunAsNonRoot {
		t.Error("Container RunAsNonRoot should be true")
	}
	if csc.RunAsUser == nil || *csc.RunAsUser != 1000 {
		t.Errorf("Container RunAsUser = %v, want 1000", csc.RunAsUser)
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) == 0 {
		t.Error("Should drop capabilities")
	}
	if csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("Capabilities.Drop = %v, want [ALL]", csc.Capabilities.Drop)
	}
}

func TestJobBuilder_Build_AgentTask_NoAgentEnvVars_WhenNoWorkspace(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Simple task",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env

	// Workspace env vars should not be present
	for _, name := range []string{"MERCAN_GIT_REPO", "MERCAN_GIT_BRANCH", "MERCAN_GIT_REF", "MERCAN_WORKSPACE_SUBPATH"} {
		if _, ok := findEnvVar(envVars, name); ok {
			t.Errorf("%s should not be set when no workspace is configured", name)
		}
	}

	// But core env vars and agent env vars should still exist
	if _, ok := findEnvVar(envVars, "MERCAN_PROMPT"); !ok {
		t.Error("Missing MERCAN_PROMPT")
	}
	if _, ok := findEnvVar(envVars, "MERCAN_MAX_TURNS"); !ok {
		t.Error("Missing MERCAN_MAX_TURNS")
	}
}

func TestJobBuilder_Build_AgentTask_ContainerTaskNoAgentVolumes(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "container-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox",
			Command: []string{"echo", "hi"},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Container tasks should NOT have workspace or home volumes
	if hasVolume(job.Spec.Template.Spec.Volumes, "workspace") {
		t.Error("Container task should not have workspace volume")
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "home") {
		t.Error("Container task should not have home volume")
	}
}

func TestJobBuilder_Build_AgentTask_Labels(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "Do something",
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if job.Labels["mercan.ai/task-type"] != "agent" {
		t.Errorf("Job label mercan.ai/task-type = %s, want agent", job.Labels["mercan.ai/task-type"])
	}
	if job.Spec.Template.Labels["mercan.ai/task-type"] != "agent" {
		t.Errorf("Pod label mercan.ai/task-type = %s, want agent", job.Spec.Template.Labels["mercan.ai/task-type"])
	}
}

func TestJobBuilder_Build_AgentTask_WithTimeout(t *testing.T) {
	builder := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-task",
			Namespace: "default",
			UID:       types.UID("12345678-1234-1234-1234-123456789012"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeAgent,
			Prompt:  "Do something",
			Timeout: &metav1.Duration{Duration: 10 * time.Minute},
		},
	}

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// Verify timeout env var
	ev, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "MERCAN_TIMEOUT_SECONDS")
	if !ok {
		t.Fatal("Missing MERCAN_TIMEOUT_SECONDS")
	}
	if ev.Value != "600" {
		t.Errorf("MERCAN_TIMEOUT_SECONDS = %s, want 600", ev.Value)
	}

	// Verify job active deadline
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("ActiveDeadlineSeconds should be set")
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 600", *job.Spec.ActiveDeadlineSeconds)
	}
}

func TestJobBuilder_Build_PriorTaskRef(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
			UID:       "uid-1234-5678",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "fix the issue",
			PriorTaskRef: &corev1alpha1.PriorTaskReference{
				Name:      "prior-task-abc",
				Namespace: "staging",
			},
		},
	}

	job, err := jb.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	var foundPriorTask, foundPriorNS bool
	for _, env := range envVars {
		if env.Name == "MERCAN_PRIOR_TASK" && env.Value == "prior-task-abc" {
			foundPriorTask = true
		}
		if env.Name == "MERCAN_PRIOR_TASK_NAMESPACE" && env.Value == "staging" {
			foundPriorNS = true
		}
	}
	if !foundPriorTask {
		t.Error("expected MERCAN_PRIOR_TASK env var")
	}
	if !foundPriorNS {
		t.Error("expected MERCAN_PRIOR_TASK_NAMESPACE env var")
	}
}

func TestJobBuilder_Build_PriorTaskRef_DefaultNamespace(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "my-ns",
			UID:       "uid-4567-8901",
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "fix it",
			PriorTaskRef: &corev1alpha1.PriorTaskReference{
				Name: "prior-task-def",
				// No namespace — should default to task namespace
			},
		},
	}

	job, err := jb.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	envVars := job.Spec.Template.Spec.Containers[0].Env
	for _, env := range envVars {
		if env.Name == "MERCAN_PRIOR_TASK_NAMESPACE" && env.Value == "my-ns" {
			return // success
		}
	}
	t.Error("expected MERCAN_PRIOR_TASK_NAMESPACE to default to task namespace 'my-ns'")
}
