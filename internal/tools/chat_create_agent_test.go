/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const testProviderOpenAI = "openai"

func TestChatCreateAgentTool_Execute_OmittedProviderRefLeavesNil(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
	})

	tool := &ChatCreateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"name":"agent-no-provider","model":{"provider":"openai","name":"gpt-4.1-mini"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}

	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      "agent-no-provider",
		Namespace: defaultNamespace,
	}, &created); err != nil {
		t.Fatalf("failed to get created agent: %v", err)
	}

	if created.Spec.ProviderRef != nil {
		t.Fatalf("providerRef = %#v, want nil when providerRef argument is omitted", created.Spec.ProviderRef)
	}
	if created.Spec.Model == nil {
		t.Fatal("model is nil")
	}
	if created.Spec.Model.Provider != testProviderOpenAI {
		t.Fatalf("model.provider = %q, want openai when no providerRef is set", created.Spec.Model.Provider)
	}
}

func TestParseRuntimeConfig_ResolvesExplicitSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-credentials",
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
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
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
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

func TestParseRuntimeConfig_AutoDiscoversCodexSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "codex-runtime-copilot",
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{
		"runtime": map[string]any{
			"type": "codex",
		},
	}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}

	if agent.Spec.Runtime == nil {
		t.Fatal("agent.Spec.Runtime is nil")
	}
	if agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeType("codex") {
		t.Errorf("runtime.type = %q, want %q", agent.Spec.Runtime.Type, "codex")
	}
	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != "codex-runtime-copilot" {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, "codex-runtime-copilot")
	}
}

func TestParseRuntimeConfig_AppliesRuntimeDefaults(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-api-key",
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{
		"runtime": map[string]any{
			"type":                "claude",
			"defaultMaxTurns":     float64(15),
			"defaultAllowedTools": []any{"Read", "Write", "Bash"},
			"defaultAllowBash":    false,
		},
	}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}

	if agent.Spec.Runtime == nil {
		t.Fatal("agent.Spec.Runtime is nil")
	}
	if agent.Spec.Runtime.DefaultMaxTurns == nil || *agent.Spec.Runtime.DefaultMaxTurns != 15 {
		t.Fatalf("defaultMaxTurns = %v, want 15", agent.Spec.Runtime.DefaultMaxTurns)
	}
	if got := agent.Spec.Runtime.DefaultAllowedTools; len(got) != 3 || got[0] != "Read" || got[1] != "Write" || got[2] != "Bash" {
		t.Fatalf("defaultAllowedTools = %#v, want Read/Write/Bash", got)
	}
	if agent.Spec.Runtime.DefaultAllowBash == nil || *agent.Spec.Runtime.DefaultAllowBash {
		t.Fatalf("defaultAllowBash = %v, want false", agent.Spec.Runtime.DefaultAllowBash)
	}
}

func TestParseRuntimeConfig_AcceptsCustomSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-creds",
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{
		"runtime": map[string]any{
			"type":      "claude",
			"secretRef": "runtime-creds",
		},
	}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}
	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != "runtime-creds" {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, "runtime-creds")
	}
}

func TestParseRuntimeConfig_RejectsMissingSecretRef(t *testing.T) {
	fc := newFakeClient()
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
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
	if !strings.Contains(errResult, "not found") {
		t.Fatalf("error = %q, want it to mention not found", errResult)
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
