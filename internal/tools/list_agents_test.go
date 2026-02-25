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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestListAgentsTool_Name(t *testing.T) {
	tool := &ListAgentsTool{}
	if got := tool.Name(); got != "list_agents" {
		t.Errorf("Name() = %q, want %q", got, "list_agents")
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
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props["namespace"]; !ok {
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
						Namespace: "default",
						Annotations: map[string]string{
							"description": "A coding agent",
						},
					},
					Spec: corev1alpha1.AgentSpec{
						Model: &corev1alpha1.ModelConfig{
							Provider: "anthropic",
							Name:     "claude-sonnet-4-20250514",
						},
						Tools: []corev1alpha1.ToolReference{
							{Name: "web_search"},
							{Name: "code_exec"},
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
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "agent-2",
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{},
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
						Namespace: "default",
					},
					Spec: corev1alpha1.AgentSpec{
						Runtime: &corev1alpha1.AgentCLIRuntime{
							Type: corev1alpha1.AgentRuntimeClaude,
						},
					},
				},
			},
			wantCount: 1,
		},
		{
			name: "custom namespace",
			args: `{"namespace": "prod"}`,
			agents: []*corev1alpha1.Agent{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "prod-agent",
						Namespace: "prod",
					},
					Spec: corev1alpha1.AgentSpec{},
				},
			},
			wantCount: 1,
		},
		{
			name:       "invalid JSON args",
			args:       `{bad}`,
			wantErrStr: "failed to parse arguments",
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
				Namespace: "default",
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
				if agent["name"] != "coder-agent" {
					t.Errorf("agent name = %q, want %q", agent["name"], "coder-agent")
				}
				if agent["model"] != "anthropic/claude-sonnet-4-20250514" {
					t.Errorf("agent model = %q, want %q", agent["model"], "anthropic/claude-sonnet-4-20250514")
				}
				if agent["description"] != "A coding agent" {
					t.Errorf("agent description = %q, want %q", agent["description"], "A coding agent")
				}
				tools, ok := agent["tools"].([]any)
				if !ok || len(tools) != 2 {
					t.Errorf("expected 2 tools, got %v", agent["tools"])
				}
			}

			// Verify runtime field
			if tt.name == "agent with runtime" && len(data) > 0 {
				agent := data[0].(map[string]any)
				if agent["runtime"] != "claude" {
					t.Errorf("agent runtime = %q, want %q", agent["runtime"], "claude")
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
