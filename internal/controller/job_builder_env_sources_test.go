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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

func envSourceTestTask(taskType corev1alpha1.TaskType, image string, env ...corev1.EnvVar) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "env-source-task", Namespace: defaultNS, UID: types.UID("env-source-uid")},
		Spec: corev1alpha1.TaskSpec{
			Type:    taskType,
			Image:   image,
			Command: []string{"env"},
			Prompt:  "inspect env",
			Env:     env,
		},
	}
}

func secretKeyEnv(name string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "other-team-secret"},
		Key:                  "token",
	}}}
}

func TestBuildRejectsContainerTaskEnvObjectSourcesWithoutDirectSecretMounts(t *testing.T) {
	tests := []struct {
		name   string
		env    corev1.EnvVar
		source string
	}{
		{name: "secretKeyRef", env: secretKeyEnv("API_TOKEN"), source: "valueFrom.secretKeyRef"},
		{
			name: "configMapKeyRef",
			env: corev1.EnvVar{Name: "SETTINGS", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
				Key:                  "value",
			}}},
			source: "valueFrom.configMapKeyRef",
		},
		{
			name: "fileKeyRef",
			env: corev1.EnvVar{Name: "FROM_FILE", ValueFrom: &corev1.EnvVarSource{FileKeyRef: &corev1.FileKeySelector{
				VolumeName: "workspace", Path: "env", Key: "TOKEN",
			}}},
			source: "valueFrom.fileKeyRef",
		},
	}
	for _, tt := range tests {
		for worker, image := range map[string]string{"managed": "", "custom": testBusyboxImage} {
			t.Run(tt.name+"/"+worker, func(t *testing.T) {
				builder := setupJobBuilder()
				literal := corev1.EnvVar{Name: "MODE", Value: "smoke"}
				task := envSourceTestTask(corev1alpha1.TaskTypeContainer, image, literal, tt.env)

				job, err := builder.Build(context.Background(), task, nil, nil)
				if err == nil {
					t.Fatalf("Build() = %v, want an error for %s", job, tt.source)
				}
				for _, want := range []string{
					"spec.env[1] (" + tt.env.Name + ") uses " + tt.source,
					directSecretMountsEnvVar + "=true",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("Build() error = %q, want it to contain %q", err, want)
					}
				}
			})
		}
	}
}

func TestBuildAllowsContainerTaskPodMetadataEnvSources(t *testing.T) {
	builder := setupJobBuilder()
	podName := corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	cpuLimit := corev1.EnvVar{Name: "CPU_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, podName, cpuLimit)

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	for _, want := range []corev1.EnvVar{podName, cpuLimit} {
		got, ok := findEnvVar(env, want.Name)
		if !ok || got.ValueFrom == nil {
			t.Fatalf("env %s = %#v, found=%v; want the pod-metadata source passed through", want.Name, got, ok)
		}
	}
}

func TestBuildPassesContainerTaskEnvSecretSourceWithDirectSecretMounts(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "true")
	builder := setupJobBuilder()
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, secretKeyEnv("API_TOKEN"))

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	got, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "API_TOKEN")
	if !ok || got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil || got.ValueFrom.SecretKeyRef.Name != "other-team-secret" {
		t.Fatalf("API_TOKEN = %#v, found=%v; want the opted-in secretKeyRef passed through", got, ok)
	}
}

func TestBuildLeavesAITaskEnvSourcesToTheTrustedWorkerPolicy(t *testing.T) {
	builder := setupJobBuilder()
	task := envSourceTestTask(corev1alpha1.TaskTypeAI, "", secretKeyEnv("API_TOKEN"))

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if _, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, "API_TOKEN"); !ok {
		t.Fatal("AI Task env secretKeyRef should keep its existing behavior")
	}
}

func TestBuildPassesContainerTaskRuntimeClassAllowlistOnlyToAIWorkers(t *testing.T) {
	builder := setupJobBuilder()
	builder.ContainerTaskRuntimeClasses = []string{"gvisor", "kata-qemu"}
	spoofed := corev1.EnvVar{Name: workerenv.ContainerTaskRuntimeClasses, Value: "runc-host"}

	aiTask := envSourceTestTask(corev1alpha1.TaskTypeAI, "", spoofed)
	job, err := builder.Build(context.Background(), aiTask, nil, nil)
	if err != nil {
		t.Fatalf("Build(ai) error = %v", err)
	}
	var values []string
	for _, envVar := range job.Spec.Template.Spec.Containers[0].Env {
		if envVar.Name == workerenv.ContainerTaskRuntimeClasses {
			values = append(values, envVar.Value)
		}
	}
	if len(values) != 1 || values[0] != "gvisor,kata-qemu" {
		t.Fatalf("AI worker %s = %v, want only the operator allowlist", workerenv.ContainerTaskRuntimeClasses, values)
	}

	containerTask := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, spoofed)
	job, err = builder.Build(context.Background(), containerTask, nil, nil)
	if err != nil {
		t.Fatalf("Build(container) error = %v", err)
	}
	if got, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, workerenv.ContainerTaskRuntimeClasses); ok {
		t.Fatalf("container Task env %s = %q, want it removed", workerenv.ContainerTaskRuntimeClasses, got.Value)
	}
}

func TestBuildTellsContainerWorkersTheirTimeout(t *testing.T) {
	spoofed := corev1.EnvVar{Name: workerenv.TimeoutSeconds, Value: "99999"}
	for _, tt := range []struct {
		name    string
		task    *corev1alpha1.Task
		timeout *metav1.Duration
		want    string
	}{
		{name: "managed worker", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, "", spoofed), timeout: &metav1.Duration{Duration: 90 * time.Second}, want: "90"},
		{name: "custom image", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, spoofed), timeout: &metav1.Duration{Duration: 30 * time.Minute}, want: "1800"},
		{name: "no timeout", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage, spoofed)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.task.Spec.Timeout = tt.timeout
			job, err := setupJobBuilder().Build(context.Background(), tt.task, nil, nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			var values []string
			for _, envVar := range job.Spec.Template.Spec.Containers[0].Env {
				if envVar.Name == workerenv.TimeoutSeconds {
					values = append(values, envVar.Value)
				}
			}
			if tt.want == "" && len(values) != 0 || tt.want != "" && (len(values) != 1 || values[0] != tt.want) {
				t.Fatalf("%s = %v, want %q set only by the controller", workerenv.TimeoutSeconds, values, tt.want)
			}
		})
	}
}
