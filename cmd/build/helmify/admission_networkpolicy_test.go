package main

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

func TestSharedAdmissionNetworkPolicyAllowsKubernetesAPIServiceAndBackendPorts(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "..", "config", "orka-admission", "networkpolicy.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read admission NetworkPolicy: %v", err)
	}
	assertKubernetesAPIEgressPorts(t, manifest)
}

func assertKubernetesAPIEgressPorts(t *testing.T, manifest []byte) {
	t.Helper()

	var policy networkingv1.NetworkPolicy
	if err := yaml.Unmarshal(manifest, &policy); err != nil {
		t.Fatalf("decode admission NetworkPolicy: %v", err)
	}
	if policy.Kind != "NetworkPolicy" {
		t.Fatalf("kind = %q, want NetworkPolicy", policy.Kind)
	}

	ports := make(map[int32]bool)
	for _, rule := range policy.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port == nil || port.Port.Type != intstr.Int {
				continue
			}
			if port.Protocol != nil && *port.Protocol != corev1.ProtocolTCP {
				continue
			}
			ports[port.Port.IntVal] = true
		}
	}
	for _, required := range []int32{443, 6443} {
		if !ports[required] {
			t.Errorf("admission NetworkPolicy TCP egress ports = %v, missing %d", ports, required)
		}
	}
}
