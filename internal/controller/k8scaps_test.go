/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/k8scaps"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestJobBuilderK8sCapabilities(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-caps", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   "busybox:1.36",
			Command: []string{"echo", "ok"},
		},
	}

	builder := NewJobBuilder(fake.NewClientBuilder().WithScheme(scheme).Build())
	job, err := builder.Build(t.Context(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if job.Spec.PodFailurePolicy == nil || len(job.Spec.PodFailurePolicy.Rules) != 2 {
		t.Fatalf("PodFailurePolicy = %#v, want 2 rules", job.Spec.PodFailurePolicy)
	}
	if job.Spec.PodFailurePolicy.Rules[0].Action != batchv1.PodFailurePolicyActionIgnore {
		t.Fatalf("rule 0 action = %q, want Ignore", job.Spec.PodFailurePolicy.Rules[0].Action)
	}
	if job.Spec.PodFailurePolicy.Rules[1].Action != batchv1.PodFailurePolicyActionFailJob {
		t.Fatalf("rule 1 action = %q, want FailJob", job.Spec.PodFailurePolicy.Rules[1].Action)
	}
	if job.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy != nil {
		t.Fatalf("SupplementalGroupsPolicy = %v, want nil on 1.33-compat", job.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy)
	}
	if job.Spec.Template.Spec.HostUsers != nil {
		t.Fatalf("HostUsers = %v, want nil when disabled", job.Spec.Template.Spec.HostUsers)
	}

	builder.ClusterCapabilities = k8scaps.Capabilities{
		ServerVersion:            "v1.35.0",
		SupplementalGroupsStrict: true,
		PodUserNamespaces:        true,
	}
	job135, err := builder.Build(t.Context(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build(1.35) error = %v", err)
	}
	if job135.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy == nil ||
		*job135.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy != corev1.SupplementalGroupsPolicyStrict {
		t.Fatalf("SupplementalGroupsPolicy = %v, want Strict", job135.Spec.Template.Spec.SecurityContext.SupplementalGroupsPolicy)
	}
	if job135.Spec.Template.Spec.HostUsers == nil || *job135.Spec.Template.Spec.HostUsers {
		t.Fatalf("HostUsers = %v, want false", job135.Spec.Template.Spec.HostUsers)
	}
	if policy := taskPodFailurePolicy(true); policy != nil {
		t.Fatalf("taskPodFailurePolicy(validationTask=true) = %#v, want nil", policy)
	}
}

func TestJobBuilderForwardsCodeExecK8sCapabilitiesToAIWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ai-task-caps", Namespace: "default"},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: "hello",
		},
	}
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: corev1alpha1.ProviderSpec{
			Type:         corev1alpha1.ProviderTypeOpenAI,
			DefaultModel: "gpt-4o",
		},
	}
	builder := NewJobBuilder(fake.NewClientBuilder().WithScheme(scheme).Build())
	builder.ClusterCapabilities = k8scaps.Capabilities{
		ServerVersion:            "v1.35.0",
		SupplementalGroupsStrict: true,
		PodUserNamespaces:        true,
	}
	job, err := builder.Build(t.Context(), task, nil, provider)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	envs := map[string]string{}
	for _, ev := range job.Spec.Template.Spec.Containers[0].Env {
		envs[ev.Name] = ev.Value
	}
	if envs[workerenv.CodeExecKubernetesSuppStrict] != "true" {
		t.Fatalf("%s = %q, want true", workerenv.CodeExecKubernetesSuppStrict, envs[workerenv.CodeExecKubernetesSuppStrict])
	}
	if envs[workerenv.CodeExecKubernetesHostUsers] != "false" {
		t.Fatalf("%s = %q, want false", workerenv.CodeExecKubernetesHostUsers, envs[workerenv.CodeExecKubernetesHostUsers])
	}
}

func TestObservedImageVolumeDigests(t *testing.T) {
	status := corev1.ContainerStatus{
		VolumeMounts: []corev1.VolumeMountStatus{
			{
				Name: "toolbox-0",
				VolumeStatus: &corev1.VolumeStatus{
					Image: &corev1.ImageVolumeStatus{ImageRef: "ghcr.io/orka/tools@sha256:aaa"},
				},
			},
			{
				Name: "skill-0",
				VolumeStatus: &corev1.VolumeStatus{
					Image: &corev1.ImageVolumeStatus{ImageRef: "ghcr.io/orka/skill@sha256:bbb"},
				},
			},
			{
				Name: "tmp",
			},
		},
	}
	got := observedImageVolumeDigests(status)
	if len(got) != 2 || got["toolbox-0"] != "ghcr.io/orka/tools@sha256:aaa" || got["skill-0"] != "ghcr.io/orka/skill@sha256:bbb" {
		t.Fatalf("observedImageVolumeDigests() = %#v", got)
	}
	if empty := observedImageVolumeDigests(corev1.ContainerStatus{}); empty != nil {
		t.Fatalf("observedImageVolumeDigests(empty) = %#v, want nil", empty)
	}
	prior := runtimePoolBootWitness{SchemaVersion: 1, PodName: "p1", ContainerID: "c1", ImageID: "img1"}
	upgraded := prior
	upgraded.ImageVolumes = got
	if !runtimePoolBootWitnessEqual(prior, upgraded) {
		t.Fatal("expected pre-upgrade witness without ImageVolumes to match observed witness with ImageVolumes")
	}
	changed := upgraded
	changed.ImageVolumes = map[string]string{"toolbox-0": "ghcr.io/orka/tools@sha256:ccc"}
	if runtimePoolBootWitnessEqual(upgraded, changed) {
		t.Fatal("expected witness with different ImageVolumes to differ")
	}
}

func TestRuntimePoolResizePolicy(t *testing.T) {
	if got := runtimePoolResizePolicy(k8scaps.Capabilities{}); got != nil {
		t.Fatalf("runtimePoolResizePolicy(disabled) = %#v, want nil", got)
	}
	got := runtimePoolResizePolicy(k8scaps.Capabilities{InPlacePodResize: true})
	if len(got) != 2 || got[0].ResourceName != corev1.ResourceCPU || got[0].RestartPolicy != corev1.NotRequired ||
		got[1].ResourceName != corev1.ResourceMemory || got[1].RestartPolicy != corev1.NotRequired {
		t.Fatalf("runtimePoolResizePolicy(enabled) = %#v", got)
	}
}
