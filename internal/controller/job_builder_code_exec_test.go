/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

func codeExecTestTask(taskType corev1alpha1.TaskType) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:   taskType,
			Prompt: "p",
			Env: []corev1.EnvVar{
				{Name: workerenv.CodeExecBackend, Value: "in-process"},
				{Name: workerenv.CodeExecBackend + "_TENANT_DEFAULT", Value: "in-process"},
				{Name: workerenv.CodeExecBackendEnforced, Value: "false"},
				{Name: "KEEP_ME", Value: "1"},
			},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
		},
	}
}

func codeExecEnv(container corev1.Container) map[string]string {
	env := map[string]string{}
	for _, envVar := range container.Env {
		if _, seen := env[envVar.Name]; seen {
			env[envVar.Name+"#dup"] = envVar.Value
		}
		env[envVar.Name] = envVar.Value
	}
	return env
}

func TestJobBuilderPinsTheHyperlightCodeExecBackend(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodeExecBackend = "Hyperlight"
	builder.HyperlightDeviceGID = 65534
	task := codeExecTestTask(corev1alpha1.TaskTypeAI)

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	env := codeExecEnv(container)
	if env[workerenv.CodeExecBackend] != "hyperlight" || env[workerenv.CodeExecBackendEnforced] != "true" {
		t.Fatalf("backend env = %v, want hyperlight pinned", env)
	}
	if _, ok := env[workerenv.CodeExecBackend+"_TENANT_DEFAULT"]; ok {
		t.Fatalf("a Task-supplied scoped backend survived: %v", env)
	}
	if _, dup := env[workerenv.CodeExecBackend+"#dup"]; dup {
		t.Fatalf("the backend is set twice: %v", container.Env)
	}
	if env["KEEP_ME"] != "1" {
		t.Fatalf("an unrelated Task variable was dropped: %v", env)
	}

	device := corev1.ResourceName(DefaultHyperlightDeviceResource)
	if got := container.Resources.Limits[device]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("device limit = %v, want 1", got)
	}
	if got := container.Resources.Requests[device]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("device request = %v, want 1", got)
	}
	if got := container.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("memory limit = %v, want the Task's 1Gi", got)
	}
	if _, mutated := task.Spec.Resources.Limits[device]; mutated {
		t.Fatal("the Task's own resource map was changed")
	}
	if !slices.Contains(job.Spec.Template.Spec.SecurityContext.SupplementalGroups, int64(65534)) {
		t.Fatalf("supplementalGroups = %v, want the device group", job.Spec.Template.Spec.SecurityContext.SupplementalGroups)
	}
}

func TestJobBuilderCodeExecBackendLeavesOtherWorkersAlone(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodeExecBackend = "kubernetes"
	builder.HyperlightDeviceResource = "example.com/kvm"
	job, err := builder.Build(context.Background(), codeExecTestTask(corev1alpha1.TaskTypeAI), nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if env := codeExecEnv(container); env[workerenv.CodeExecBackend] != "kubernetes" || env[workerenv.CodeExecBackendEnforced] != "true" {
		t.Fatalf("backend env = %v, want kubernetes pinned", env)
	}
	if _, ok := container.Resources.Limits["example.com/kvm"]; ok {
		t.Fatal("a non-hyperlight backend requested the hypervisor device")
	}

	job, err = builder.Build(context.Background(), codeExecTestTask(corev1alpha1.TaskTypeContainer), nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if env := codeExecEnv(job.Spec.Template.Spec.Containers[0]); env[workerenv.CodeExecBackendEnforced] == "true" {
		t.Fatalf("a container Task got the AI worker's backend: %v", env)
	}

	builder.CodeExecBackend = ""
	job, err = builder.Build(context.Background(), codeExecTestTask(corev1alpha1.TaskTypeAI), nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if env := codeExecEnv(job.Spec.Template.Spec.Containers[0]); env[workerenv.CodeExecBackendEnforced] == "true" {
		t.Fatalf("no pinned backend, yet the worker is enforced: %v", env)
	}
}
