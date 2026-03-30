/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestParseRuntimeConfig_PreservesExplicitSecretRef(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "openai"},
		},
	}

	args := map[string]any{
		"runtime": map[string]any{
			"type":      "claude",
			"secretRef": "runtime-creds",
		},
	}

	parseRuntimeConfig(args, agent)

	if agent.Spec.Runtime == nil {
		t.Fatal("agent.Spec.Runtime is nil")
	}
	if agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeType("claude") {
		t.Errorf("runtime.type = %q, want %q", agent.Spec.Runtime.Type, "claude")
	}
	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != "runtime-creds" {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, "runtime-creds")
	}
	if agent.Spec.ProviderRef != nil {
		t.Errorf("providerRef = %v, want nil", agent.Spec.ProviderRef)
	}
}

func TestParseCoordinationConfig_EnabledClearsRuntimeAndSecretRef(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: "claude"},
			SecretRef: &corev1.LocalObjectReference{Name: "runtime-creds"},
		},
	}

	args := map[string]any{
		"coordination": map[string]any{
			"enabled": true,
		},
	}

	parseCoordinationConfig(args, agent)

	if agent.Spec.Coordination == nil {
		t.Fatal("agent.Spec.Coordination is nil")
	}
	if !agent.Spec.Coordination.Enabled {
		t.Fatal("coordination.enabled = false, want true")
	}
	if agent.Spec.Runtime != nil {
		t.Errorf("runtime = %v, want nil", agent.Spec.Runtime)
	}
	if agent.Spec.SecretRef != nil {
		t.Errorf("secretRef = %v, want nil", agent.Spec.SecretRef)
	}
}
