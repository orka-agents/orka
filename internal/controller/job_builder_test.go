/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
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
	return NewJobBuilder(fakeClient)
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

	// Verify session volume
	hasSessionVolume := false
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "session" {
			hasSessionVolume = true
			break
		}
	}
	if !hasSessionVolume {
		t.Error("Job should have session volume")
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
	hasResultConfigMap := false
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
		case ResultConfigMapEnvVar:
			hasResultConfigMap = true
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
	if !hasResultConfigMap {
		t.Error("Missing MERCAN_RESULT_CONFIGMAP")
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
	if ResultConfigMapEnvVar != "MERCAN_RESULT_CONFIGMAP" {
		t.Errorf("ResultConfigMapEnvVar = %s", ResultConfigMapEnvVar)
	}
	if TaskNameEnvVar != "MERCAN_TASK_NAME" {
		t.Errorf("TaskNameEnvVar = %s", TaskNameEnvVar)
	}
	if TaskNamespaceEnvVar != "MERCAN_TASK_NAMESPACE" {
		t.Errorf("TaskNamespaceEnvVar = %s", TaskNamespaceEnvVar)
	}
}
