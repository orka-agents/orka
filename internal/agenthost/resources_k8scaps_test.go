/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/k8scaps"
)

func testAgentHostFixtures() (*corev1alpha1.AgentHost, *corev1alpha1.AgentHostTemplate) {
	host := &corev1alpha1.AgentHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-host",
			Namespace: "default",
			UID:       "11111111-2222-3333-4444-555555555555",
		},
	}
	template := &corev1alpha1.AgentHostTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dev-template",
			UID:  "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		},
		Spec: corev1alpha1.AgentHostTemplateSpec{
			ListenPort: 8080,
			SecurityContext: corev1alpha1.AgentHostSecurityContext{
				RunAsUser:  1000,
				RunAsGroup: 1000,
			},
			Containers: []corev1alpha1.AgentHostContainer{
				{
					Name:  corev1alpha1.AgentHostContainerName,
					Image: "example.com/agent-host:v1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
	}
	return host, template
}

func TestHostPodClusterCapabilitiesBackwardsCompat(t *testing.T) {
	host, template := testAgentHostFixtures()
	names := ObjectNames(host, "orka-agent-hosts")

	pod := HostPod(host, names, "nonce-1", template, PodOptions{
		AgentImage: "example.com/host-agent:v1",
	})
	if pod.Spec.SecurityContext.SupplementalGroupsPolicy != nil {
		t.Fatalf("SupplementalGroupsPolicy = %v, want nil on 1.33-compat", pod.Spec.SecurityContext.SupplementalGroupsPolicy)
	}
	if pod.Spec.HostUsers != nil {
		t.Fatalf("HostUsers = %v, want nil when disabled", pod.Spec.HostUsers)
	}
	if pod.Spec.Resources != nil {
		t.Fatalf("Pod Resources = %v, want nil on 1.33-compat", pod.Spec.Resources)
	}
	if len(pod.Spec.Containers[0].ResizePolicy) != 0 {
		t.Fatalf("ResizePolicy = %v, want empty when pods/resize unavailable", pod.Spec.Containers[0].ResizePolicy)
	}
}

func TestHostPodClusterCapabilities135Enabled(t *testing.T) {
	host, template := testAgentHostFixtures()
	names := ObjectNames(host, "orka-agent-hosts")

	caps := k8scaps.Capabilities{
		ServerVersion:            "v1.35.0",
		SupplementalGroupsStrict: true,
		PodLevelResources:        true,
		InPlacePodResize:         true,
		PodUserNamespaces:        true,
	}
	pod := HostPod(host, names, "nonce-2", template, PodOptions{
		AgentImage:          "example.com/host-agent:v1",
		ClusterCapabilities: caps,
	})
	if pod.Spec.SecurityContext.SupplementalGroupsPolicy == nil ||
		*pod.Spec.SecurityContext.SupplementalGroupsPolicy != corev1.SupplementalGroupsPolicyStrict {
		t.Fatalf("SupplementalGroupsPolicy = %v, want Strict", pod.Spec.SecurityContext.SupplementalGroupsPolicy)
	}
	if pod.Spec.HostUsers == nil || *pod.Spec.HostUsers {
		t.Fatalf("HostUsers = %v, want false", pod.Spec.HostUsers)
	}
	if len(pod.Spec.Containers[0].ResizePolicy) != 2 {
		t.Fatalf("ResizePolicy len = %d, want 2", len(pod.Spec.Containers[0].ResizePolicy))
	}
	if pod.Spec.Resources == nil {
		t.Fatal("expected Pod-level Resources to be set on 1.35+")
	}
	wantCPUReq := resource.MustParse("510m")
	if got := pod.Spec.Resources.Requests[corev1.ResourceCPU]; got.Cmp(wantCPUReq) != 0 {
		t.Fatalf("Pod CPU request = %s, want %s", got.String(), wantCPUReq.String())
	}
	wantMemReq := resource.MustParse("528Mi")
	if got := pod.Spec.Resources.Requests[corev1.ResourceMemory]; got.Cmp(wantMemReq) != 0 {
		t.Fatalf("Pod memory request = %s, want %s", got.String(), wantMemReq.String())
	}
	wantCPULimit := resource.MustParse("2200m")
	if got := pod.Spec.Resources.Limits[corev1.ResourceCPU]; got.Cmp(wantCPULimit) != 0 {
		t.Fatalf("Pod CPU limit = %s, want %s", got.String(), wantCPULimit.String())
	}
	wantMemLimit := resource.MustParse("2112Mi")
	if got := pod.Spec.Resources.Limits[corev1.ResourceMemory]; got.Cmp(wantMemLimit) != 0 {
		t.Fatalf("Pod memory limit = %s, want %s", got.String(), wantMemLimit.String())
	}
}

func TestHostPodLevelResourcesOmitsUnboundedLimits(t *testing.T) {
	host, template := testAgentHostFixtures()
	template.Spec.Containers[0].Resources.Limits = nil
	names := ObjectNames(host, "orka-agent-hosts")

	pod := HostPod(host, names, "nonce-3", template, PodOptions{
		AgentImage: "example.com/host-agent:v1",
		ClusterCapabilities: k8scaps.Capabilities{
			ServerVersion:     "v1.35.0",
			PodLevelResources: true,
		},
	})
	if pod.Spec.Resources == nil {
		t.Fatal("expected Pod-level Resources requests to be set")
	}
	if len(pod.Spec.Resources.Limits) != 0 {
		t.Fatalf("Pod-level Limits = %v, want empty when app container has no limits", pod.Spec.Resources.Limits)
	}
	wantCPUReq := resource.MustParse("510m")
	if got := pod.Spec.Resources.Requests[corev1.ResourceCPU]; got.Cmp(wantCPUReq) != 0 {
		t.Fatalf("Pod CPU request = %s, want %s", got.String(), wantCPUReq.String())
	}
}
