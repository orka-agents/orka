/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"math"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/orka-agents/orka/internal/hyperlight"
)

func TestHyperlightPodConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config HyperlightPodConfig
		want   string
	}{
		{"default", HyperlightPodConfig{}, ""},
		{"custom device", HyperlightPodConfig{DeviceResource: "example.com/kvm", DeviceGID: 990}, ""},
		{"maximum group", HyperlightPodConfig{DeviceGID: math.MaxInt32}, ""},
		{"negative group", HyperlightPodConfig{DeviceGID: -1}, "GID"},
		{"oversized group", HyperlightPodConfig{DeviceGID: int64(math.MaxInt32) + 1}, "GID"},
		{"uint32 group", HyperlightPodConfig{DeviceGID: math.MaxUint32}, "GID"},
		{"CPU", HyperlightPodConfig{DeviceResource: "cpu"}, "extended-resource"},
		{"memory", HyperlightPodConfig{DeviceResource: "memory"}, "extended-resource"},
		{"reserved domain", HyperlightPodConfig{DeviceResource: "kubernetes.io/kvm"}, "extended-resource"},
		{"reserved subdomain", HyperlightPodConfig{DeviceResource: "node.kubernetes.io/kvm"}, "extended-resource"},
		{"quota prefix", HyperlightPodConfig{DeviceResource: "requests.example.com/kvm"}, "extended-resource"},
		{"malformed name", HyperlightPodConfig{DeviceResource: "example.com/kvm/extra"}, "extended-resource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid config rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestHyperlightPodReservesGuestMemory(t *testing.T) {
	// Keep the reservation in sync with every language exposed by the tools.
	runner := hyperlight.NewRunner(hyperlight.Config{})
	for _, language := range []string{"bash", "python", "node"} {
		if mb := runner.ScratchMB(language); mb > 512 {
			t.Fatalf("%s guest needs %d MiB, exceeding the Pod's 512 MiB reservation", language, mb)
		}
	}
	for _, tc := range []struct {
		name, request, limit, wantRequest, wantLimit string
	}{
		{"default worker", "512Mi", "2Gi", "1Gi", "2560Mi"},
		{"limit only", "", "1Gi", "1536Mi", "1536Mi"},
		{"request only", "256Mi", "", "768Mi", ""},
		{"small worker", "64Mi", "128Mi", "576Mi", "640Mi"},
		{"unbounded worker", "", "", "512Mi", ""},
		{"explicit zero limit", "", "0", "512Mi", ""},
		{"explicit zero request", "0", "1Gi", "512Mi", "1536Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}
			if tc.request != "" {
				resources.Requests[corev1.ResourceMemory] = resource.MustParse(tc.request)
			}
			if tc.limit != "" {
				resources.Limits[corev1.ResourceMemory] = resource.MustParse(tc.limit)
			}
			original := resources.DeepCopy()
			spec := corev1.PodSpec{Containers: []corev1.Container{{Resources: resources}}}
			HyperlightPodConfig{}.apply(&spec, &spec.Containers[0])
			got := spec.Containers[0].Resources
			if request := got.Requests[corev1.ResourceMemory]; request.Cmp(resource.MustParse(tc.wantRequest)) != 0 {
				t.Fatalf("memory request = %s, want %s", request.String(), tc.wantRequest)
			}
			limit, limited := got.Limits[corev1.ResourceMemory]
			if limited != (tc.wantLimit != "") || (limited && limit.Cmp(resource.MustParse(tc.wantLimit)) != 0) {
				t.Fatalf("memory limit = %s, present=%t, want %q", limit.String(), limited, tc.wantLimit)
			}
			if request := got.Requests[corev1.ResourceMemory]; limited && request.Cmp(limit) > 0 {
				t.Fatalf("memory request %s exceeds limit %s", request.String(), limit.String())
			}
			if !apiequality.Semantic.DeepEqual(resources, *original) {
				t.Fatal("shared worker resource requirements were mutated")
			}
			for _, values := range []corev1.ResourceList{got.Requests, got.Limits} {
				delete(values, corev1.ResourceMemory)
				delete(values, DefaultHyperlightDeviceResource)
			}
			for _, values := range []corev1.ResourceList{original.Requests, original.Limits} {
				delete(values, corev1.ResourceMemory)
			}
			if !apiequality.Semantic.DeepEqual(got, *original) {
				t.Fatal("non-memory worker resources changed")
			}
		})
	}
}
