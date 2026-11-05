/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

func callbackTokenVolume(spec corev1.PodSpec) *corev1.Volume {
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == taskCallbackTokenVolume {
			return &spec.Volumes[i]
		}
	}
	return nil
}

func TestBuildGivesCustomImageContainerPodsOnlyAnOrkaAudienceToken(t *testing.T) {
	builder := setupJobBuilder()
	task := envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage,
		corev1.EnvVar{Name: workerenv.ServiceAccountTokenPath, Value: "/tmp/elsewhere"})

	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	spec := job.Spec.Template.Spec
	assertAutomountServiceAccountToken(t, spec.AutomountServiceAccountToken, false)

	volume := callbackTokenVolume(spec)
	if volume == nil || volume.Projected == nil || len(volume.Projected.Sources) != 1 {
		t.Fatalf("callback token volume = %#v, want one projected source", volume)
	}
	projection := volume.Projected.Sources[0].ServiceAccountToken
	if projection == nil || projection.Audience != workerenv.TaskCallbackTokenAudience ||
		projection.ExpirationSeconds == nil || *projection.ExpirationSeconds != 3600 || projection.Path != "token" {
		t.Fatalf("callback token projection = %#v", projection)
	}

	container := spec.Containers[0]
	var mounted bool
	for _, mount := range container.VolumeMounts {
		if mount.Name == taskCallbackTokenVolume {
			mounted = mount.MountPath == "/var/run/secrets/orka.ai/task-callback" && mount.ReadOnly
		}
	}
	if !mounted {
		t.Fatalf("worker mounts = %#v, want the callback token read-only at its directory", container.VolumeMounts)
	}
	var paths []string
	for _, envVar := range container.Env {
		if envVar.Name == workerenv.ServiceAccountTokenPath {
			paths = append(paths, envVar.Value)
		}
	}
	if len(paths) != 1 || paths[0] != workerenv.TaskCallbackTokenFile {
		t.Fatalf("%s = %v, want only the controller-set token path", workerenv.ServiceAccountTokenPath, paths)
	}
}

func TestBuildKeepsCallbackTokenOffTrustedAndValidationPods(t *testing.T) {
	for _, tt := range []struct {
		name       string
		task       *corev1alpha1.Task
		validation bool
	}{
		{name: "managed general worker", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, "")},
		{name: "ai worker", task: envSourceTestTask(corev1alpha1.TaskTypeAI, "")},
		{name: "repository validation", task: envSourceTestTask(corev1alpha1.TaskTypeContainer, testBusyboxImage), validation: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if taskUsesCallbackToken(tt.task, tt.validation) {
				t.Fatal("taskUsesCallbackToken() = true")
			}
			if tt.validation {
				return
			}
			job, err := setupJobBuilder().Build(context.Background(), tt.task, nil, nil)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if volume := callbackTokenVolume(job.Spec.Template.Spec); volume != nil {
				t.Fatalf("callback token volume = %#v, want none", volume)
			}
			if got, ok := findEnvVar(job.Spec.Template.Spec.Containers[0].Env, workerenv.ServiceAccountTokenPath); ok {
				t.Fatalf("%s = %q, want unset", workerenv.ServiceAccountTokenPath, got.Value)
			}
		})
	}
}
