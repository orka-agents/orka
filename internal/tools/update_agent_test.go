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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestUpdateAgentTool_Name(t *testing.T) {
	tool := &UpdateAgentTool{}
	if got := tool.Name(); got != updateAgentToolName {
		t.Errorf("Name() = %v, want %v", got, updateAgentToolName)
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
	if schema[jsonSchemaTypeField] != jsonSchemaTypeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{nameField, namespaceField, systemPromptField, toolsField, modelField} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestUpdateAgentTool_Execute(t *testing.T) {
	existingAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMyAgentName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: providerOpenAI,
				Name:     testGPT4OModel,
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
			args:  map[string]any{nameField: testMyAgentName, systemPromptField: "You are helpful"},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:  "update model with provider/name format",
			args:  map[string]any{nameField: testMyAgentName, modelField: "anthropic/claude-sonnet-4-20250514"},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:  "update model name only",
			args:  map[string]any{nameField: testMyAgentName, modelField: "gpt-4o-mini"},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:  "update tools list",
			args:  map[string]any{nameField: testMyAgentName, toolsField: []any{webSearchToolName, codeExecToolName}},
			setup: func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
		},
		{
			name:    missingNameCaseName,
			args:    map[string]any{},
			setup:   func() *corev1alpha1.Agent { return existingAgent.DeepCopy() },
			wantErr: true,
		},
		{
			name:    "agent not found",
			args:    map[string]any{nameField: testNonexistentName},
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
			tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
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
			if data[nameField] != testMyAgentName {
				t.Errorf("expected name 'my-agent', got %v", data[nameField])
			}
			if data[messageField] != "Agent updated" {
				t.Errorf("expected message 'Agent updated', got %v", data[messageField])
			}
		})
	}
}

func TestUpdateAgentTool_Execute_VerifyUpdatedFields(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMyAgentName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{},
	}

	fc := newFakeClient(agent)
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{nameField: testMyAgentName, systemPromptField: "Updated prompt", toolsField: []any{webSearchToolName}}
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
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, updated); err != nil {
		t.Fatalf("failed to get updated agent: %v", err)
	}
	if updated.Spec.SystemPrompt == nil || updated.Spec.SystemPrompt.Inline != "Updated prompt" {
		t.Errorf("systemPrompt not updated, got %v", updated.Spec.SystemPrompt)
	}
	if len(updated.Spec.Tools) != 1 || updated.Spec.Tools[0].Name != webSearchToolName {
		t.Errorf("tools not updated, got %v", updated.Spec.Tools)
	}
}

func TestUpdateAgentTool_Execute_ModelObjectUpdatesAndPreservesUnspecifiedFields(t *testing.T) {
	originalTemperature := 0.2
	originalMaxTokens := int32(4096)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMyAgentName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider:    providerOpenAI,
				Name:        testGPT4OModel,
				Temperature: &originalTemperature,
				MaxTokens:   &originalMaxTokens,
			},
		},
	}

	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":{"provider":"anthropic","name":"claude-sonnet-4-20250514","temperature":0.4}
	}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Success {
		t.Fatalf("Execute() result = %#v, want success", res)
	}

	updated := &corev1alpha1.Agent{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, updated); err != nil {
		t.Fatalf("failed to get updated agent: %v", err)
	}
	if updated.Spec.Model == nil {
		t.Fatal("model is nil")
	}
	if updated.Spec.Model.Provider != "anthropic" {
		t.Errorf("model.provider = %q, want anthropic", updated.Spec.Model.Provider)
	}
	if updated.Spec.Model.Name != "claude-sonnet-4-20250514" {
		t.Errorf("model.name = %q, want claude-sonnet-4-20250514", updated.Spec.Model.Name)
	}
	if updated.Spec.Model.Temperature == nil || *updated.Spec.Model.Temperature != 0.4 {
		t.Errorf("model.temperature = %v, want 0.4", updated.Spec.Model.Temperature)
	}
	if updated.Spec.Model.MaxTokens == nil || *updated.Spec.Model.MaxTokens != originalMaxTokens {
		t.Errorf("model.maxTokens = %v, want preserved value %d", updated.Spec.Model.MaxTokens, originalMaxTokens)
	}
}

func TestUpdateAgentTool_Execute_ModelObjectPreservesOmittedModelFields(t *testing.T) {
	originalTemperature := 0.2
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{
			Provider:    providerOpenAI,
			Name:        testGPT4OModel,
			Temperature: &originalTemperature,
		}},
	}

	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"my-agent","model":{"temperature":0}}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Success {
		t.Fatalf("Execute() result = %#v, want success", res)
	}

	updated := &corev1alpha1.Agent{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, updated); err != nil {
		t.Fatalf("failed to get updated agent: %v", err)
	}
	if updated.Spec.Model == nil {
		t.Fatal("model is nil")
	}
	if updated.Spec.Model.Provider != providerOpenAI {
		t.Errorf("model.provider = %q, want preserved value %q", updated.Spec.Model.Provider, providerOpenAI)
	}
	if updated.Spec.Model.Name != testGPT4OModel {
		t.Errorf("model.name = %q, want preserved value %q", updated.Spec.Model.Name, testGPT4OModel)
	}
	if updated.Spec.Model.Temperature == nil || *updated.Spec.Model.Temperature != 0 {
		t.Errorf("model.temperature = %v, want 0", updated.Spec.Model.Temperature)
	}
}

func TestUpdateAgentTool_Execute_RejectsUnsupportedModelTypes(t *testing.T) {
	tests := []struct {
		name  string
		model any
	}{
		{name: "model must be object or legacy string", model: []any{"openai", testGPT4OModel}},
		{name: "provider must be string", model: map[string]any{"provider": true}},
		{name: "name must be string", model: map[string]any{nameField: 42}},
		{name: "temperature must be number", model: map[string]any{"temperature": "warm"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
				Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{
					Provider: providerOpenAI,
					Name:     testGPT4OModel,
				}},
			}
			fc := newFakeClient(agent)
			ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
			args, err := json.Marshal(map[string]any{nameField: testMyAgentName, modelField: tt.model})
			if err != nil {
				t.Fatalf("failed to marshal args: %v", err)
			}

			result, err := (&UpdateAgentTool{}).Execute(ctx, args)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if res.Success {
				t.Fatalf("Execute() result = %#v, want invalid_arguments", res)
			}
			if res.ErrorType != "invalid_arguments" {
				t.Fatalf("errorType = %q, want invalid_arguments", res.ErrorType)
			}

			unchanged := &corev1alpha1.Agent{}
			if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, unchanged); err != nil {
				t.Fatalf("failed to get agent: %v", err)
			}
			if unchanged.Spec.Model == nil || unchanged.Spec.Model.Provider != providerOpenAI || unchanged.Spec.Model.Name != testGPT4OModel {
				t.Fatalf("model changed after rejected input: %#v", unchanged.Spec.Model)
			}
		})
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
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	tool := &UpdateAgentTool{}
	result, err := tool.Execute(ctx, json.RawMessage(invalidJSONText))
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
