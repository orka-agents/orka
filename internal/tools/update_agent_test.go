/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestUpdateAgentTool_Name(t *testing.T) {
	tool := &UpdateAgentTool{}
	if got := tool.Name(); got != "update_agent" {
		t.Errorf("Name() = %v, want %v", got, "update_agent")
	}
}

func TestUpdateAgentTool_Description(t *testing.T) {
	tool := &UpdateAgentTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestUpdateAgentTool_Parameters(t *testing.T) {
	tool := &UpdateAgentTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{"name", "namespace", "systemPrompt", "tools", "model"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestUpdateAgentTool_Execute(t *testing.T) {
	existingAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4o",
			},
		},
	}

	tests := []struct {
		name     string
		args     map[string]any
		setup    func() *corev1alpha1.Agent
		wantErr  bool
		wantData map[string]any
	}{
		{
			name:  "update system prompt",
			args:  map[string]any{"name": "my-agent", "systemPrompt": "You are helpful"},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:  "update model with provider/name format",
			args:  map[string]any{"name": "my-agent", "model": "anthropic/claude-sonnet-4-20250514"},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:  "update model name only",
			args:  map[string]any{"name": "my-agent", "model": "gpt-4o-mini"},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:  "update tools list",
			args:  map[string]any{"name": "my-agent", "tools": []any{"web_search", "code_exec"}},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:    "missing name",
			args:    map[string]any{},
			setup:   func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
			wantErr: true,
		},
		{
			name:    "agent not found",
			args:    map[string]any{"name": "nonexistent"},
			setup:   func() *corev1alpha1.Agent { return nil },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := tt.setup()
			var fc = newFakeClient()
			if agent != nil {
				fc = newFakeClient(agent)
			}
			tc := &ToolContext{Client: fc, Namespace: "default"}
			ctx := WithToolContext(context.Background(), tc)

			argsJSON, _ := json.Marshal(tt.args)
			tool := &UpdateAgentTool{}
			result, err := tool.Execute(ctx, argsJSON)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}

			if tt.wantErr {
				if res.Success {
					t.Error("expected failure")
				}
				return
			}

			if !res.Success {
				t.Fatalf("expected success, got error: %s", res.Error)
			}

			data, ok := res.Data.(map[string]any)
			if !ok {
				t.Fatalf("expected data to be map, got %T", res.Data)
			}
			if data["name"] != "my-agent" {
				t.Errorf("expected name 'my-agent', got %v", data["name"])
			}
			if data["message"] != "Agent updated" {
				t.Errorf("expected message 'Agent updated', got %v", data["message"])
			}
		})
	}
}

func TestUpdateAgentTool_Execute_VerifyUpdatedFields(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}

	fc := newFakeClient(agent)
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{
		"name":         "my-agent",
		"systemPrompt": "Updated prompt",
		"tools":        []any{"web_search"},
	}
	argsJSON, _ := json.Marshal(args)

	tool := &UpdateAgentTool{}
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}

	// Verify persisted changes
	updated := &corev1alpha1.Agent{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: "my-agent", Namespace: "default"}, updated); err != nil {
		t.Fatalf("failed to get updated agent: %v", err)
	}
	if updated.Spec.SystemPrompt == nil || updated.Spec.SystemPrompt.Inline != "Updated prompt" {
		t.Errorf("systemPrompt not updated, got %v", updated.Spec.SystemPrompt)
	}
	if len(updated.Spec.Tools) != 1 || updated.Spec.Tools[0].Name != "web_search" {
		t.Errorf("tools not updated, got %v", updated.Spec.Tools)
	}
}

func TestUpdateAgentTool_Execute_MissingToolContext(t *testing.T) {
	tool := &UpdateAgentTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"x"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Success {
		t.Error("expected failure for missing tool context")
	}
}

func TestUpdateAgentTool_Execute_InvalidJSON(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	tool := &UpdateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{invalid}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Success {
		t.Error("expected failure for invalid JSON")
	}
	if res.ErrorType != "invalid_arguments" {
		t.Errorf("expected errorType 'invalid_arguments', got %q", res.ErrorType)
	}
}
