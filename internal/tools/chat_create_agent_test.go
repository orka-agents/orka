/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testProviderOpenAI = "openai"

func TestChatCreateAgentTool_ParametersDescribeOpenCodeACP(t *testing.T) {
	params := string((&ChatCreateAgentTool{}).Parameters())
	for _, want := range []string{"copilot, claude, codex, or opencode", "OpenCode defaults to Read, Write, Edit, Bash, Glob, and Grep", "OpenCode defaults to true", "Omit for OpenCode runtime Agents"} {
		if !strings.Contains(params, want) {
			t.Errorf("Parameters() missing %q", want)
		}
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

func TestChatCreateAgentTool_Execute_RejectsOpenCodeSystemPrompt(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-prompt-agent",
		"systemPrompt":"You write code",
		"model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "does not support systemPrompt") {
		t.Fatalf("response = %#v, want OpenCode systemPrompt rejection", response)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-prompt-agent", Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_RejectsUnsupportedOpenCodeTemperature(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-temperature-agent",
		"model":{"name":"openai/gpt-5.4","temperature":1,"contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "temperature") {
		t.Fatalf("response = %#v, want unsupported OpenCode temperature rejection", response)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-temperature-agent", Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_AcceptsLegacyOpenCodeTemperature(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-legacy-temperature-agent",
		"model":{"name":"openai/gpt-5.4","temperature":0.7,"contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !response.Success {
		t.Fatalf("expected success, got error: %s", response.Error)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-legacy-temperature-agent", Namespace: defaultNamespace}, &created); err != nil {
		t.Fatalf("failed to get created Agent: %v", err)
	}
	if created.Spec.Model == nil || created.Spec.Model.Temperature == nil || *created.Spec.Model.Temperature != 0.7 {
		t.Fatalf("model.temperature = %#v, want legacy value 0.7", created.Spec.Model)
	}
}

func TestChatCreateAgentTool_Execute_RejectsFractionalOpenCodeModelLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentName string
		args      json.RawMessage
		wantField string
	}{
		{
			name:      "context window",
			agentName: "fractional-context-window",
			args: json.RawMessage(`{
				"name":"fractional-context-window",
				"model":{"name":"openai/gpt-5.4","contextWindow":32768.5,"maxTokens":4096},
				"runtime":{"type":"opencode"}
			}`),
			wantField: "model.contextWindow",
		},
		{
			name:      "max tokens",
			agentName: "fractional-max-tokens",
			args: json.RawMessage(`{
				"name":"fractional-max-tokens",
				"model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096.5},
				"runtime":{"type":"opencode"}
			}`),
			wantField: "model.maxTokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeClient()
			ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
			result, err := (&ChatCreateAgentTool{}).Execute(ctx, tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, tc.wantField) {
				t.Fatalf("response = %#v, want %s integer rejection", response, tc.wantField)
			}
			var created corev1alpha1.Agent
			if err := fc.Get(context.Background(), client.ObjectKey{Name: tc.agentName, Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
				t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
			}
		})
	}
}

func TestParseChatPositiveAgentModelInt32(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
		want int32
	}{
		{name: "decoded JSON integer", raw: float64(4096), want: 4096},
		{name: "int", raw: int(4096), want: 4096},
		{name: "int8", raw: int8(127), want: 127},
		{name: "int16", raw: int16(4096), want: 4096},
		{name: "int32", raw: int32(4096), want: 4096},
		{name: "int64", raw: int64(4096), want: 4096},
		{name: "uint", raw: uint(4096), want: 4096},
		{name: "uint8", raw: uint8(127), want: 127},
		{name: "uint16", raw: uint16(4096), want: 4096},
		{name: "uint32", raw: uint32(4096), want: 4096},
		{name: "uint64", raw: uint64(4096), want: 4096},
		{name: "max int32", raw: int64(1<<31 - 1), want: 1<<31 - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChatPositiveAgentModelInt32("model.contextWindow", tc.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name      string
		raw       any
		wantError string
	}{
		{name: "fractional decoded JSON number", raw: float64(4096.5), wantError: "must be an integer"},
		{name: "zero", raw: int(0), wantError: "positive 32-bit integer"},
		{name: "negative", raw: int64(-1), wantError: "positive 32-bit integer"},
		{name: "signed overflow", raw: int64(1 << 31), wantError: "positive 32-bit integer"},
		{name: "unsigned overflow", raw: uint64(1 << 31), wantError: "positive 32-bit integer"},
		{name: "wrong type", raw: "4096", wantError: "must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseChatPositiveAgentModelInt32("model.contextWindow", tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestChatCreateAgentTool_Execute_RejectsOpenCodeLegacySecret(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name": "legacy-runtime-agent",
		"model": "openai/gpt-5.4",
		"runtime": {"type": "opencode", "secretRef": "legacy-runtime"}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "does not accept runtime.secretRef") {
		t.Fatalf("response = %#v, want legacy secret rejection", response)
	}
	var created corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "legacy-runtime-agent", Namespace: defaultNamespace}, &created); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid OpenCode Agent should not be created, get err=%v", err)
	}
}

func TestChatCreateAgentTool_Execute_UsesBuiltInOpenCode(t *testing.T) {
	fc := newFakeClient()
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&ChatCreateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"opencode-agent",
		"model":{"name":"moonshotai/Kimi-K2-Instruct-0905","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success {
		t.Fatalf("response = %#v, want success", response)
	}
	var agent corev1alpha1.Agent
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "opencode-agent", Namespace: defaultNamespace}, &agent); err != nil {
		t.Fatal(err)
	}
	wantTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeOpencode || !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, wantTools) {
		t.Fatalf("runtime = %#v, want OpenCode defaults %#v", agent.Spec.Runtime, wantTools)
	}
	if agent.Spec.Model == nil || agent.Spec.Model.Name != "moonshotai/Kimi-K2-Instruct-0905" || agent.Spec.Model.Provider != "" {
		t.Fatalf("model = %#v, want provider-qualified OpenCode model", agent.Spec.Model)
	}
	if agent.Spec.SystemPrompt != nil {
		t.Fatalf("systemPrompt = %#v, want nil for OpenCode runtime", agent.Spec.SystemPrompt)
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
func TestParseRuntimeConfig_BuiltInRuntimesAreCredentialFree(t *testing.T) {
	for _, runtimeType := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCopilot,
		corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCodex,
	} {
		t.Run(string(runtimeType), func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
				SecretRef:   &corev1.LocalObjectReference{Name: "legacy-runtime"},
			}}
			args := map[string]any{runtimeField: map[string]any{
				jsonSchemaTypeField: string(runtimeType),
				secretRefField:      "missing-legacy-runtime",
			}}
			if errResult, ok := parseRuntimeConfig(args, agent); !ok {
				t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
			}
			if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != runtimeType {
				t.Fatalf("runtime = %#v, want %q", agent.Spec.Runtime, runtimeType)
			}
			if agent.Spec.ProviderRef != nil || agent.Spec.SecretRef != nil {
				t.Fatalf("providerRef=%#v secretRef=%#v, want credential-free runtime", agent.Spec.ProviderRef, agent.Spec.SecretRef)
			}
		})
	}
	t.Run("opencode", func(t *testing.T) {
		agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
			Model:       testOpenCodeModelConfig("openai/gpt-5.4"),
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
			SecretRef:   &corev1.LocalObjectReference{Name: "legacy-runtime"},
		}}
		args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "opencode"}}
		if errResult, ok := parseRuntimeConfig(args, agent); !ok {
			t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
		}
		wantTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
		if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeOpencode || !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, wantTools) {
			t.Fatalf("runtime = %#v, want OpenCode defaults %#v", agent.Spec.Runtime, wantTools)
		}
		if agent.Spec.ProviderRef != nil || agent.Spec.SecretRef != nil || agent.Spec.Model.Provider != "" {
			t.Fatalf("providerRef=%#v secretRef=%#v model=%#v, want credential-free OpenCode runtime", agent.Spec.ProviderRef, agent.Spec.SecretRef, agent.Spec.Model)
		}
	})
	t.Run("normalizes runtime type", func(t *testing.T) {
		agent := &corev1alpha1.Agent{}
		args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "  claude  "}}
		if errResult, ok := parseRuntimeConfig(args, agent); !ok {
			t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
		}
		if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeClaude {
			t.Fatalf("runtime = %#v, want normalized claude", agent.Spec.Runtime)
		}
	})
}

func TestParseRuntimeConfig_RejectsModelLimitsForNonOpenCodeBuiltIns(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	for _, tt := range []struct {
		name      string
		runtime   corev1alpha1.AgentRuntimeType
		model     *corev1alpha1.ModelConfig
		wantError string
	}{
		{name: "codex context window", runtime: corev1alpha1.AgentRuntimeCodex, model: &corev1alpha1.ModelConfig{ContextWindow: &contextWindow}, wantError: "contextWindow"},
		{name: "claude max tokens", runtime: corev1alpha1.AgentRuntimeClaude, model: &corev1alpha1.ModelConfig{MaxTokens: &maxTokens}, wantError: "maxTokens"},
		{name: "copilot context window", runtime: corev1alpha1.AgentRuntimeCopilot, model: &corev1alpha1.ModelConfig{ContextWindow: &contextWindow}, wantError: "contextWindow"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: tt.model}}
			result, ok := parseRuntimeConfig(map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: string(tt.runtime)}}, agent)
			if ok {
				t.Fatal("parseRuntimeConfig accepted ignored built-in model limits")
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(response.Error, tt.wantError) {
				t.Fatalf("response = %#v, want %q rejection", response, tt.wantError)
			}
		})
	}
}

func TestParseRuntimeConfig_RejectsUnsupportedRuntime(t *testing.T) {
	for _, runtimeType := range []string{"unknown", "", "   "} {
		t.Run(runtimeType, func(t *testing.T) {
			agent := &corev1alpha1.Agent{}
			args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeType}}
			errResult, ok := parseRuntimeConfig(args, agent)
			if ok {
				t.Fatal("parseRuntimeConfig accepted invalid runtime")
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(errResult), &response); err != nil {
				t.Fatalf("unmarshal error result: %v", err)
			}
			if response.ErrorType != errTypeInvalidArgs || (!strings.Contains(response.Error, "unsupported runtime type") && !strings.Contains(response.Error, "runtime.type is required")) {
				t.Fatalf("response = %#v, want invalid runtime rejection", response)
			}
		})
	}
}

func TestParseRuntimeConfig_AppliesRuntimeDefaults(t *testing.T) {
	agent := &corev1alpha1.Agent{
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: testProviderOpenAI},
		},
	}

	args := map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: runtimeTypeClaude, "defaultMaxTurns": float64(15),
		"defaultAllowedTools": []any{"Read", "Write", "Bash"},
		"defaultAllowBash":    false,
	},
	}

	if errResult, ok := parseRuntimeConfig(args, agent); !ok {
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

func TestParseRuntimeConfig_PreservesExplicitEmptyOpenCodeTools(t *testing.T) {
	allowBash := false
	agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: testOpenCodeModelConfig("openai/gpt-5.4")}}
	args := map[string]any{runtimeField: map[string]any{
		jsonSchemaTypeField:   "opencode",
		"defaultAllowedTools": []any{},
		"defaultAllowBash":    allowBash,
	}}
	if errResult, ok := parseRuntimeConfig(args, agent); !ok {
		t.Fatalf("parseRuntimeConfig returned error: %s", errResult)
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.DefaultAllowedTools == nil || len(agent.Spec.Runtime.DefaultAllowedTools) != 0 {
		t.Fatalf("defaultAllowedTools = %#v, want explicit empty", agent.Spec.Runtime)
	}
	if agent.Spec.Runtime.DefaultAllowBash == nil || *agent.Spec.Runtime.DefaultAllowBash {
		t.Fatalf("defaultAllowBash = %v, want false", agent.Spec.Runtime.DefaultAllowBash)
	}
}

func TestParseRuntimeConfig_RejectsInvalidOpenCodeModel(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	inverted := int32(4096)
	for name, model := range map[string]*corev1alpha1.ModelConfig{
		"missing":         nil,
		"bare":            {Name: "gpt-5.4", ContextWindow: &contextWindow, MaxTokens: &maxTokens},
		"substitution":    {Name: "{env:PROVIDER}/gpt", ContextWindow: &contextWindow, MaxTokens: &maxTokens},
		"missing context": {Name: "openai/gpt-5.4", MaxTokens: &maxTokens},
		"missing output":  {Name: "openai/gpt-5.4", ContextWindow: &contextWindow},
		"inverted":        {Name: "openai/gpt-5.4", ContextWindow: &inverted, MaxTokens: &maxTokens},
	} {
		t.Run(name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Model: model}}
			errResult, ok := parseRuntimeConfig(map[string]any{runtimeField: map[string]any{jsonSchemaTypeField: "opencode"}}, agent)
			if ok {
				t.Fatal("parseRuntimeConfig accepted invalid OpenCode model")
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(errResult), &response); err != nil {
				t.Fatal(err)
			}
			if response.ErrorType != errTypeInvalidArgs {
				t.Fatalf("response = %#v, want invalid arguments", response)
			}
		})
	}
}
