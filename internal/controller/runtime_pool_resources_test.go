/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestNewRuntimePoolPodResourcesValidates(t *testing.T) {
	resources, err := NewRuntimePoolPodResources("1", "2Gi", "4", "8Gi")
	if err != nil {
		t.Fatal(err)
	}
	if resources.CPURequest.Cmp(resource.MustParse("1")) != 0 || resources.MemoryLimit.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Fatalf("resources = %+v", resources)
	}
	for _, invalid := range []struct {
		name                                             string
		cpuRequest, memoryRequest, cpuLimit, memoryLimit string
	}{
		{"unparsable", "lots", "512Mi", "2", "4Gi"},
		{"empty", "250m", "", "2", "4Gi"},
		{"zero", "0", "512Mi", "2", "4Gi"},
		{"negative", "250m", "512Mi", "-1", "4Gi"},
		{"CPU request above limit", "3", "512Mi", "2", "4Gi"},
		{"memory request above limit", "250m", "5Gi", "2", "4Gi"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			if _, err := NewRuntimePoolPodResources(invalid.cpuRequest, invalid.memoryRequest, invalid.cpuLimit, invalid.memoryLimit); err == nil {
				t.Fatal("invalid runtime Pod resources were accepted")
			}
		})
	}
}

// Unconfigured, the standard class keeps the original sizes; configured, it
// uses the configured CPU and memory and keeps the fixed ephemeral storage.
// Other resource classes stay unsized.
func TestRuntimePoolResourceRequirementsUseConfiguredCPUAndMemory(t *testing.T) {
	defaults := (&RuntimePoolReconciler{}).runtimePoolResourceRequirements(runtimePoolResourceClassStandard)
	for name, want := range map[corev1.ResourceName]string{
		corev1.ResourceCPU: "250m", corev1.ResourceMemory: "512Mi", corev1.ResourceEphemeralStorage: "1Gi",
	} {
		if got := defaults.Requests[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("default request %s = %s, want %s", name, got.String(), want)
		}
	}
	for name, want := range map[corev1.ResourceName]string{
		corev1.ResourceCPU: "2", corev1.ResourceMemory: "4Gi", corev1.ResourceEphemeralStorage: "5Gi",
	} {
		if got := defaults.Limits[name]; got.Cmp(resource.MustParse(want)) != 0 {
			t.Fatalf("default limit %s = %s, want %s", name, got.String(), want)
		}
	}

	configured, err := NewRuntimePoolPodResources("1", "2Gi", "4", "8Gi")
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &RuntimePoolReconciler{PodResources: &configured}
	got := reconciler.runtimePoolResourceRequirements(runtimePoolResourceClassStandard)
	if got.Requests.Cpu().Cmp(resource.MustParse("1")) != 0 || got.Requests.Memory().Cmp(resource.MustParse("2Gi")) != 0 ||
		got.Limits.Cpu().Cmp(resource.MustParse("4")) != 0 || got.Limits.Memory().Cmp(resource.MustParse("8Gi")) != 0 {
		t.Fatalf("configured resources = %#v", got)
	}
	if got.Requests.StorageEphemeral().Cmp(resource.MustParse("1Gi")) != 0 || got.Limits.StorageEphemeral().Cmp(resource.MustParse("5Gi")) != 0 {
		t.Fatalf("ephemeral storage changed: %#v", got)
	}
	// The rendered requirements are copies: mutating them leaves the
	// configuration untouched for the next Pod.
	got.Requests[corev1.ResourceCPU] = resource.MustParse("9")
	if again := reconciler.runtimePoolResourceRequirements(runtimePoolResourceClassStandard); again.Requests.Cpu().Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("configuration was mutated through a rendered template: %s", again.Requests.Cpu().String())
	}
	if other := reconciler.runtimePoolResourceRequirements("large"); len(other.Requests) != 0 || len(other.Limits) != 0 {
		t.Fatalf("unknown resource class was sized: %#v", other)
	}
}
