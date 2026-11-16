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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestListAgentsTool_Name(t *testing.T) {
	tool := &ListAgentsTool{}
	if got := tool.Name(); got != listAgentsToolName {
		t.Errorf("Name() = %q, want %q", got, listAgentsToolName)
	}
}

func TestListAgentsTool_Description(t *testing.T) {
	tool := &ListAgentsTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestListAgentsTool_Parameters(t *testing.T) {
	tool := &ListAgentsTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props[namespaceField]; !ok {
		t.Error("missing namespace property")
	}
}

func TestListAgentsTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		agents     []*corev1alpha1.Agent
		wantCount  int
		wantErrStr string
	}{
		{
			name:      "empty list",
			args:      `{}`,
			agents:    nil,
			wantCount: 0,
		},
		{
			name: "single agent with model and tools",
			args: `{}`,
			agents: []*corev1alpha1.Agent{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "coder-agent",
						Namespace: defaultNamespace,
						Annotations: map[string]string{
							jsonSchemaDescriptionField: "A coding agent",
						},
					},
					Spec: corev1alpha1.AgentSpec{
						Model: &corev1alpha1.ModelConfig{
							Provider: "anthropic",
							Name:     "claude-sonnet-4-20250514",
						},
						Tools: []corev1alpha1.ToolReference{
							{Name: webSearchToolName},
							{Name: codeExecToolName},
						},
					},
				},
			},
			wantCount: 1,
		},
		{
			name: "multiple agents",
			args: `{}`,
			agents: []*corev1alpha1.Agent{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "agent-1",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.AgentSpec{},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "agent-2",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.AgentSpec{},
				},
			},
			wantCount: 2,
		},
		{
			name: "agent with providerRef",
			args: `{}`,
			agents: []*corev1alpha1.Agent{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "provider-agent", Namespace: defaultNamespace},
					Spec: corev1alpha1.AgentSpec{
						ProviderRef: &corev1alpha1.ProviderReference{Name: "copilot-vekil"},
						Model:       &corev1alpha1.ModelConfig{Name: "gpt-4.1"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "provider-default-model", Namespace: defaultNamespace},
					Spec: corev1alpha1.AgentSpec{
						ProviderRef: &corev1alpha1.ProviderReference{Name: "copilot-vekil"},
					},
				},
			},
			wantCount: 2,
		},
		{
			name: "agent with runtime",
			args: `{}`,
			agents: []*corev1alpha1.Agent{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cli-agent",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.AgentSpec{
						Runtime: &corev1alpha1.AgentCLIRuntime{
							Type: corev1alpha1.AgentRuntimeClaude,
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "external-agent",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.AgentSpec{
						Runtime: &corev1alpha1.AgentCLIRuntime{
							RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "byo-harness"},
						},
					},
				},
			},
			wantCount: 2,
		},
		{
			name: testCustomNamespaceCaseName,
			args: `{"namespace": "prod"}`,
			agents: []*corev1alpha1.Agent{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-agent",
						Namespace: testProdNamespace,
					},
					Spec: corev1alpha1.AgentSpec{},
				},
			},
			wantCount: 1,
		},
		{
			name:       invalidJSONArgsCaseName,
			args:       `{bad}`,
			wantErrStr: failedToParseArgumentsMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &ListAgentsTool{}

			fc := newFakeClient()
			if len(tt.agents) > 0 {
				fc = newFakeClientWithAgents(tt.agents)
			}

			tc := &ToolContext{
				Client:    fc,
				Namespace: defaultNamespace,
			}
			ctx := WithToolContext(context.Background(), tc)

			result, err := tool.Execute(ctx, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantErrStr != "" {
				if !strings.Contains(result, tt.wantErrStr) {
					t.Errorf("result = %q, want to contain %q", result, tt.wantErrStr)
				}
				return
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Success {
				t.Errorf("expected success=true, got false; result: %s", result)
			}

			data, ok := res.Data.([]any)
			if !ok {
				t.Fatalf("expected data array, got %T", res.Data)
			}
			if len(data) != tt.wantCount {
				t.Errorf("got %d agents, want %d", len(data), tt.wantCount)
			}

			// Verify specific fields for single agent test
			if tt.name == "single agent with model and tools" && len(data) > 0 {
				agent := data[0].(map[string]any)
				if agent[nameField] != "coder-agent" {
					t.Errorf("agent name = %q, want %q", agent[nameField], "coder-agent")
				}
				if agent[modelField] != "anthropic/claude-sonnet-4-20250514" {
					t.Errorf("agent model = %q, want %q", agent[modelField], "anthropic/claude-sonnet-4-20250514")
				}
				if agent[jsonSchemaDescriptionField] != "A coding agent" {
					t.Errorf("agent description = %q, want %q", agent[jsonSchemaDescriptionField], "A coding agent")
				}
				tools, ok := agent[toolsField].([]any)
				if !ok || len(tools) != 2 {
					t.Errorf("expected 2 tools, got %v", agent[toolsField])
				}
			}

			if tt.name == "agent with providerRef" {
				byName := map[string]map[string]any{}
				for _, item := range data {
					agent := item.(map[string]any)
					byName[agent[nameField].(string)] = agent
				}
				if got := byName["provider-agent"]; got[modelField] != "copilot-vekil/gpt-4.1" || got[providerRefField] != "copilot-vekil" {
					t.Errorf("provider-agent = %v, want model copilot-vekil/gpt-4.1 and providerRef copilot-vekil", got)
				}
				if got := byName["provider-default-model"]; got[providerRefField] != "copilot-vekil" {
					t.Errorf("provider-default-model providerRef = %v, want copilot-vekil", got[providerRefField])
				} else if model, ok := got[modelField]; ok {
					t.Errorf("provider-default-model model = %v, want it omitted for the Provider default", model)
				}
			}

			// Verify runtime field
			if tt.name == "agent with runtime" {
				byName := map[string]map[string]any{}
				for _, item := range data {
					agent := item.(map[string]any)
					byName[agent[nameField].(string)] = agent
				}
				if got := byName["cli-agent"]; got[runtimeField] != runtimeTypeClaude {
					t.Errorf("cli-agent = %v, want runtime %q", got, runtimeTypeClaude)
				} else if ref, ok := got[runtimeRefField]; ok {
					t.Errorf("cli-agent runtimeRef = %v, want it omitted", ref)
				}
				if got := byName["external-agent"]; got[runtimeField] != "external" || got[runtimeRefField] != "byo-harness" {
					t.Errorf("external-agent = %v, want runtime external and runtimeRef byo-harness", got)
				}
			}
		})
	}
}

func TestListAgentsTool_Execute_MissingToolContext(t *testing.T) {
	tool := &ListAgentsTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "missing tool context") {
		t.Errorf("expected missing tool context error, got %q", result)
	}
}
