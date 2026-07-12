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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testProviderOpenAI = "openai"

func TestChatCreateAgentTool_ParametersRequireRuntimeSecretRef(t *testing.T) {
	tool := &ChatCreateAgentTool{}
	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("Parameters returned invalid JSON: %v", err)
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatalf("properties = %T, want map[string]any", schema[jsonSchemaPropertiesField])
	}
	runtimeSchema, ok := props[runtimeField].(map[string]any)
	if !ok {
		t.Fatalf("runtime schema = %T, want map[string]any", props[runtimeField])
	}
	required, ok := runtimeSchema[jsonSchemaRequiredField].([]any)
	if !ok {
		t.Fatalf("runtime.required = %T, want []any", runtimeSchema[jsonSchemaRequiredField])
	}
	if !containsAnyString(required, jsonSchemaTypeField) || !containsAnyString(required, secretRefField) {
		t.Fatalf("runtime.required = %#v, want type and secretRef", required)
	}
}

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

func TestChatCreateAgentTool_Execute_RollsBackAgentWhenInitialTaskAuthorizationFails(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:     fc,
		Namespace:  defaultNamespace,
		TaskLabels: func() map[string]string { return map[string]string{} },
		CheckTaskLimit: func() *ChatToolError {
			return nil
		},
		GenerateTaskName: func() string { return "blocked-task" },
		AuthorizeTaskCreate: func(context.Context, *corev1alpha1.Task) *ChatToolError {
			return &ChatToolError{Type: "permission_denied", Message: "task blocked by context token"}
		},
	})

	tool := &ChatCreateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"name":"agent-rollback","initialPrompt":"run this"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Fatalf("expected authorization failure, got success: %#v", r)
	}
	if r.ErrorType != "permission_denied" {
		t.Fatalf("errorType = %q, want permission_denied", r.ErrorType)
	}

	var created corev1alpha1.Agent
	err = fc.Get(context.Background(), client.ObjectKey{Name: "agent-rollback", Namespace: defaultNamespace}, &created)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("agent should have been rolled back, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_AuthorizesAgentBeforeCreate(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		AuthorizeAgentCreate: func(context.Context, *corev1alpha1.Agent) *ChatToolError {
			return &ChatToolError{Type: "authorization_failed", Message: "agent blocked by context token"}
		},
	})

	tool := &ChatCreateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"name":"agent-blocked"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Fatalf("expected authorization failure, got success: %#v", r)
	}

	var created corev1alpha1.Agent
	err = fc.Get(context.Background(), client.ObjectKey{Name: "agent-blocked", Namespace: defaultNamespace}, &created)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("agent should not have been created, get err=%v", err)
	}
}

func TestParseRuntimeConfig_ResolvesExplicitSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claudeCredentialsSecretName,
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, secretRefField: claudeCredentialsSecretName}}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}

	if agent.Spec.Runtime == nil {
		t.Fatal("agent.Spec.Runtime is nil")
	}
	if agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeType(runtimeTypeClaude) {
		t.Errorf("runtime.type = %q, want %q", agent.Spec.Runtime.Type, runtimeTypeClaude)
	}
	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != claudeCredentialsSecretName {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, claudeCredentialsSecretName)
	}
	if agent.Spec.ProviderRef != nil {
		t.Errorf("providerRef = %v, want nil", agent.Spec.ProviderRef)
	}
}

func TestParseRuntimeConfig_RejectsOmittedSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claudeAPIKeySecretName,
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude}}

	errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent)
	if ok {
		t.Fatal("expected parseRuntimeConfig to fail")
	}
	if !strings.Contains(errResult, "runtime secretRef is required") {
		t.Fatalf("error = %q, want it to mention required secretRef", errResult)
	}
	if strings.Contains(errResult, claudeCredentialsSecretName) || strings.Contains(errResult, claudeAPIKeySecretName) {
		t.Fatalf("error = %q, must not disclose runtime secret candidates", errResult)
	}
	if agent.Spec.SecretRef != nil {
		t.Fatalf("agent.Spec.SecretRef = %#v, want nil", agent.Spec.SecretRef)
	}
}

func TestParseRuntimeConfig_RejectsOmittedCodexSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      codexRuntimeCopilotSecretName,
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "codex"}}

	errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent)
	if ok {
		t.Fatal("expected parseRuntimeConfig to fail")
	}
	if !strings.Contains(errResult, "runtime secretRef is required") {
		t.Fatalf("error = %q, want it to mention required secretRef", errResult)
	}
	if strings.Contains(errResult, codexRuntimeCopilotSecretName) || strings.Contains(errResult, codexProxyTokenSecretName) {
		t.Fatalf("error = %q, must not disclose runtime secret candidates", errResult)
	}
	if agent.Spec.SecretRef != nil {
		t.Fatalf("agent.Spec.SecretRef = %#v, want nil", agent.Spec.SecretRef)
	}
}

func TestParseRuntimeConfig_AppliesRuntimeDefaults(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claudeAPIKeySecretName,
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, secretRefField: claudeAPIKeySecretName, "defaultMaxTurns": float64(15),
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

func TestParseRuntimeConfig_RejectsUnauthorizedSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRuntimeCredsSecretName,
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{}
	ctx := WithToolContext(context.Background(), &ToolContext{
		AuthorizeSecretRead: func(context.Context, string, string) *ChatToolError {
			return &ChatToolError{Type: "authorization_failed", Message: "secret blocked by context token"}
		},
	})
	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, secretRefField: testRuntimeCredsSecretName}}

	errResult, ok := parseRuntimeConfig(ctx, fc, defaultNamespace, args, agent)
	if ok {
		t.Fatal("expected parseRuntimeConfig to fail")
	}
	if !strings.Contains(errResult, "not authorized") || !strings.Contains(errResult, "secret blocked by context token") {
		t.Fatalf("error = %q, want authorization failure", errResult)
	}
	if agent.Spec.SecretRef != nil {
		t.Fatalf("agent.Spec.SecretRef = %#v, want nil", agent.Spec.SecretRef)
	}
}

func TestParseRuntimeConfig_AcceptsCustomSecretRef(t *testing.T) {
	fc := newFakeClient(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRuntimeCredsSecretName,
			Namespace: defaultNamespace,
		},
	})
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, secretRefField: testRuntimeCredsSecretName}}

	if errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}
	if agent.Spec.SecretRef == nil {
		t.Fatal("agent.Spec.SecretRef is nil")
	}
	if agent.Spec.SecretRef.Name != testRuntimeCredsSecretName {
		t.Errorf("secretRef.name = %q, want %q", agent.Spec.SecretRef.Name, testRuntimeCredsSecretName)
	}
}

func TestParseRuntimeConfig_RejectsMissingSecretRef(t *testing.T) {
	fc := newFakeClient()
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, secretRefField: testRuntimeCredsSecretName}}

	errResult, ok := parseRuntimeConfig(context.Background(), fc, defaultNamespace, args, agent)
	if ok {
		t.Fatal("expected parseRuntimeConfig to fail")
	}
	if !strings.Contains(errResult, notFoundMessage) {
		t.Fatalf("error = %q, want it to mention not found", errResult)
	}
}

func TestParseCoordinationConfig_EnabledClearsRuntimeAndSecretRef(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: runtimeTypeClaude},
			SecretRef: &corev1.LocalObjectReference{Name: testRuntimeCredsSecretName},
		},
	}

	args := map[string]any{
		"coordination": map[string]any{
			enabledString: true,
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
