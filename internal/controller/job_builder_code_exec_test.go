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
	"github.com/orka-agents/orka/internal/hyperlight"
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
	builder.Hyperlight = HyperlightPodConfig{DeviceGID: 65534, BundleImage: "example.com/hyperlight-bundle@sha256:0000000000000000000000000000000000000000000000000000000000000000"}
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
	if env[hyperlightDeviceGIDEnv] != "65534" {
		t.Fatalf("device group env = %q", env[hyperlightDeviceGIDEnv])
	}

	// The bundle reaches the worker through an init container and an emptyDir.
	initContainers := job.Spec.Template.Spec.InitContainers
	bundle := initContainers[len(initContainers)-1]
	if bundle.Name != hyperlightBundleInit || bundle.Image != builder.Hyperlight.BundleImage {
		t.Fatalf("last init container = %s (%s), want the Hyperlight bundle", bundle.Name, bundle.Image)
	}
	if !slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == hyperlightVolume && m.MountPath == hyperlightDir && m.ReadOnly
	}) {
		t.Fatalf("worker mounts = %v, want the Hyperlight bundle read-only at %s", container.VolumeMounts, hyperlightDir)
	}
	if !slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == hyperlightCacheVolume && m.MountPath == hyperlightCacheDir && !m.ReadOnly
	}) {
		t.Fatalf("worker mounts = %v, want a writable snapshot cache at %s", container.VolumeMounts, hyperlightCacheDir)
	}
	if env["ORKA_HYPERLIGHT_BINARY"] != hyperlightDir+"/bin/hluk" || env["ORKA_HYPERLIGHT_CACHE_DIR"] != hyperlightCacheDir+"/c" {
		t.Fatalf("Hyperlight env = %v", env)
	}
}

func TestJobBuilderDropsTaskHyperlightSettingsWhenPinned(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodeExecBackend = "hyperlight"
	task := codeExecTestTask(corev1alpha1.TaskTypeAI)
	task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: "ORKA_HYPERLIGHT_BINARY", Value: "/tmp/not-hluk"})
	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := codeExecEnv(job.Spec.Template.Spec.Containers[0])[hyperlight.EnvBinary]; got != hyperlightDir+"/bin/hluk" {
		t.Fatalf("pinned hluk binary = %q, want the standard image path", got)
	}
}

func TestJobBuilderCodeExecBackendLeavesOtherWorkersAlone(t *testing.T) {
	builder := setupJobBuilder()
	builder.CodeExecBackend = "kubernetes"
	builder.Hyperlight = HyperlightPodConfig{DeviceResource: "example.com/kvm"}
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

func TestJobBuilderPinsHyperlightSettingsOverAgentSecretEnvFrom(t *testing.T) {
	for _, bundle := range []string{"", "example.com/hyperlight-bundle:test"} {
		t.Run(bundle, func(t *testing.T) {
			builder := setupJobBuilder()
			builder.CodeExecBackend = "hyperlight"
			builder.Hyperlight = HyperlightPodConfig{BundleImage: bundle}
			task := codeExecTestTask(corev1alpha1.TaskTypeAI)
			task.Spec.Env = append(task.Spec.Env,
				corev1.EnvVar{Name: hyperlight.EnvBinary, Value: "/tmp/untrusted"},
				corev1.EnvVar{Name: hyperlight.EnvRootfsDir, Value: "/tmp/untrusted-images"},
				corev1.EnvVar{Name: hyperlight.EnvScratchMB, Value: "99999"},
				corev1.EnvVar{Name: hyperlight.EnvBinary, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "test-secret"}, Key: "binary",
				}}},
				corev1.EnvVar{Name: hyperlight.EnvRootfsDir, ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "test-config"}, Key: "rootfs",
				}}},
			)
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{SecretRef: &corev1.LocalObjectReference{Name: "test-agent-secret"}}}
			job, err := builder.Build(context.Background(), task, agent, nil)
			if err != nil {
				t.Fatal(err)
			}
			container := job.Spec.Template.Spec.Containers[0]
			if len(container.EnvFrom) != 1 || container.EnvFrom[0].SecretRef.Name != agent.Spec.SecretRef.Name {
				t.Fatalf("legitimate Agent Secret envFrom was lost: %+v", container.EnvFrom)
			}
			// Kubernetes explicit Env overrides envFrom, including empty values.
			env := codeExecEnv(container)
			for name, want := range map[string]string{
				hyperlight.EnvBinary:    hyperlightDir + "/bin/hluk",
				hyperlight.EnvRootfsDir: hyperlightDir + "/rootfs",
				hyperlight.EnvCacheDir:  hyperlightCacheDir + "/c",
				hyperlight.EnvScratchMB: "",
				hyperlightDeviceGIDEnv:  "0",
			} {
				got, reserved := env[name]
				if !reserved || got != want {
					t.Errorf("%s = %q, present=%t, want explicit %q", name, got, reserved, want)
				}
				count := 0
				for _, value := range container.Env {
					if value.Name == name {
						count++
						if value.ValueFrom != nil {
							t.Errorf("%s retained workload ValueFrom", name)
						}
					}
				}
				if count != 1 {
					t.Errorf("%s has %d explicit values, want one", name, count)
				}
			}
		})
	}
}
