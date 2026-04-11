/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestParseRuntimeConfig_ResolvesExplicitSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-credentials",
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "openai"},
		},
	}

	args := map[string]any{
		"runtime": map[string]any{
			"type":      "claude",
			"secretRef": "claude-credentials",
		},
	}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}

	if agent.Spec.Runtime == nil {
		t.Fatal("agent.Spec.Runtime is nil")
	}
	if agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeType("claude") {
		t.Errorf("runtime.type = %q, want %q", agent.Spec.Runtime.Type, "claude")
	}
	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != "claude-credentials" {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, "claude-credentials")
	}
	if agent.Spec.ProviderRef != nil {
		t.Errorf("providerRef = %v, want nil", agent.Spec.ProviderRef)
	}
}

func TestParseRuntimeConfig_AutoDiscoversSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-api-key",
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "openai"},
		},
	}

	args := map[string]any{
		"runtime": map[string]any{
			"type": "claude",
		},
	}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}

	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != "claude-api-key" {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, "claude-api-key")
	}
}

func TestParseRuntimeConfig_RejectsUnsupportedSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-creds",
			Namespace: defaultNamespace,
		},
	})
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

	errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent)
	if ok {
		t.Fatal("expected parseRuntimeConfig to fail")
	}
	if !strings.Contains(errResult, "not allowed") {
		t.Fatalf("error = %q, want it to mention not allowed", errResult)
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
