/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
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
	systemPrompt, ok := props[systemPromptField].(map[string]any)
	description, _ := systemPrompt[jsonSchemaDescriptionField].(string)
	if !ok || !strings.Contains(description, "OpenCode runtime Agents do not support") {
		t.Fatalf("systemPrompt schema = %#v, want OpenCode restriction guidance", systemPrompt)
	}
	model, ok := props[modelField].(map[string]any)
	if !ok {
		t.Fatalf("model schema = %#v, want object", props[modelField])
	}
	modelProps, ok := model[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatalf("model properties = %#v, want object", model[jsonSchemaPropertiesField])
	}
	for _, key := range []string{"provider", nameField, "temperature", "contextWindow", "maxTokens"} {
		if _, ok := modelProps[key]; !ok {
			t.Errorf("model schema missing %s property", key)
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

func TestUpdateAgentTool_Execute_UpdatesNestedModelFields(t *testing.T) {
	temperature := 0.2
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{
			Provider:      providerOpenAI,
			Name:          testGPT4OModel,
			Temperature:   &temperature,
			ContextWindow: &contextWindow,
			MaxTokens:     &maxTokens,
		}},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":{"provider":"anthropic","name":"claude-sonnet-4-20250514","temperature":0.4,"contextWindow":200000,"maxTokens":8192}
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

	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Model == nil || updated.Spec.Model.Provider != "anthropic" || updated.Spec.Model.Name != "claude-sonnet-4-20250514" {
		t.Fatalf("model identity = %#v, want nested provider/name update", updated.Spec.Model)
	}
	if updated.Spec.Model.Temperature == nil || *updated.Spec.Model.Temperature != 0.4 {
		t.Fatalf("model.temperature = %#v, want 0.4", updated.Spec.Model.Temperature)
	}
	if updated.Spec.Model.ContextWindow == nil || *updated.Spec.Model.ContextWindow != 200000 {
		t.Fatalf("model.contextWindow = %#v, want 200000", updated.Spec.Model.ContextWindow)
	}
	if updated.Spec.Model.MaxTokens == nil || *updated.Spec.Model.MaxTokens != 8192 {
		t.Fatalf("model.maxTokens = %#v, want 8192", updated.Spec.Model.MaxTokens)
	}
}

func TestUpdateAgentTool_Execute_LegacyModelStringKeepsProviderBackedSplitting(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{
			Provider: providerOpenAI,
			Name:     testGPT4OModel,
		}},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":"anthropic/claude-haiku-4-5"
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

	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Model == nil || updated.Spec.Model.Provider != "anthropic" || updated.Spec.Model.Name != "claude-haiku-4-5" {
		t.Fatalf("model identity = %#v, want legacy provider/name split", updated.Spec.Model)
	}
}

func TestUpdateAgentTool_Execute_RejectsModelLimitsForNonOpenCodeBuiltIns(t *testing.T) {
	for _, tt := range []struct {
		name      string
		runtime   corev1alpha1.AgentRuntimeType
		modelJSON string
		wantError string
	}{
		{name: "codex context window", runtime: corev1alpha1.AgentRuntimeCodex, modelJSON: `{"contextWindow":32768}`, wantError: "contextWindow"},
		{name: "claude max tokens", runtime: corev1alpha1.AgentRuntimeClaude, modelJSON: `{"maxTokens":4096}`, wantError: "maxTokens"},
		{name: "copilot context window", runtime: corev1alpha1.AgentRuntimeCopilot, modelJSON: `{"contextWindow":32768}`, wantError: "contextWindow"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
				Spec: corev1alpha1.AgentSpec{
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: tt.runtime},
					Model:   &corev1alpha1.ModelConfig{Name: "model"},
				},
			}
			fc := newFakeClient(original.DeepCopy())
			ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
			result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"my-agent","model":`+tt.modelJSON+`}`))
			if err != nil {
				t.Fatal(err)
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				t.Fatal(err)
			}
			if response.Success || !strings.Contains(response.Error, tt.wantError) {
				t.Fatalf("response = %#v, want %q rejection", response, tt.wantError)
			}
			var persisted corev1alpha1.Agent
			if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted.Spec, original.Spec) {
				t.Fatalf("persisted spec changed after rejection: got %#v want %#v", persisted.Spec, original.Spec)
			}
		})
	}
}

func TestUpdateAgentTool_Execute_NormalizesOpenCodeModelAndPreservesOmittedFields(t *testing.T) {
	temperature := 0.7
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
			Model: &corev1alpha1.ModelConfig{
				Name:          "openai/gpt-5.4",
				Temperature:   &temperature,
				ContextWindow: &contextWindow,
				MaxTokens:     &maxTokens,
			},
		},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":{"name":"gpt-5.5"}
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

	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Model == nil || updated.Spec.Model.Name != "openai/gpt-5.5" || updated.Spec.Model.Provider != "" {
		t.Fatalf("model identity = %#v, want normalized OpenCode model", updated.Spec.Model)
	}
	if updated.Spec.Model.Temperature == nil || *updated.Spec.Model.Temperature != temperature ||
		updated.Spec.Model.ContextWindow == nil || *updated.Spec.Model.ContextWindow != contextWindow ||
		updated.Spec.Model.MaxTokens == nil || *updated.Spec.Model.MaxTokens != maxTokens {
		t.Fatalf("model = %#v, want omitted controls preserved", updated.Spec.Model)
	}
}

func TestUpdateAgentTool_Execute_PreservesNestedOpenCodeLegacyModelString(t *testing.T) {
	agent := testOpenCodeAgent()
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":"openrouter/anthropic/claude-sonnet-4"
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

	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Model == nil || updated.Spec.Model.Name != "openrouter/anthropic/claude-sonnet-4" || updated.Spec.Model.Provider != "" {
		t.Fatalf("model identity = %#v, want full nested OpenCode model ID", updated.Spec.Model)
	}
}

func TestUpdateAgentTool_Execute_PreservesNestedOpenCodeObjectModelName(t *testing.T) {
	agent := testOpenCodeAgent()
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":{"name":"openrouter/google/gemini-3.1-pro"}
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

	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Model == nil || updated.Spec.Model.Name != "openrouter/google/gemini-3.1-pro" || updated.Spec.Model.Provider != "" {
		t.Fatalf("model identity = %#v, want full nested OpenCode object model ID", updated.Spec.Model)
	}
}

func TestUpdateAgentTool_Execute_UpdatesAllOpenCodeModelFields(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
			Model: &corev1alpha1.ModelConfig{
				Name:          "openai/gpt-5.4",
				ContextWindow: &contextWindow,
				MaxTokens:     &maxTokens,
			},
		},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{
		"name":"my-agent",
		"model":{"provider":"anthropic","name":"claude-sonnet-4-20250514","temperature":0.7,"contextWindow":200000,"maxTokens":8192}
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

	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.Model == nil || updated.Spec.Model.Name != "anthropic/claude-sonnet-4-20250514" || updated.Spec.Model.Provider != "" {
		t.Fatalf("model identity = %#v, want normalized OpenCode provider/model", updated.Spec.Model)
	}
	if updated.Spec.Model.Temperature == nil || *updated.Spec.Model.Temperature != 0.7 ||
		updated.Spec.Model.ContextWindow == nil || *updated.Spec.Model.ContextWindow != 200000 ||
		updated.Spec.Model.MaxTokens == nil || *updated.Spec.Model.MaxTokens != 8192 {
		t.Fatalf("model controls = %#v, want all nested fields persisted", updated.Spec.Model)
	}
}

func TestUpdateAgentTool_Execute_RejectsInvalidOpenCodeModelUpdates(t *testing.T) {
	tests := []struct {
		name      string
		modelJSON string
		wantError string
	}{
		{name: "unsupported temperature", modelJSON: `{"temperature":1}`, wantError: "temperature"},
		{name: "invalid token limits", modelJSON: `{"contextWindow":4096,"maxTokens":4096}`, wantError: "must exceed"},
		{name: "substitution model", modelJSON: `{"name":"{env:PROVIDER}/gpt"}`, wantError: "substitution braces"},
		{name: "conflicting provider", modelJSON: `{"provider":"anthropic","name":"openai/gpt-5.4"}`, wantError: "provider"},
		{name: "nested model with conflicting provider", modelJSON: `{"provider":"openrouter","name":"anthropic/claude-sonnet-4"}`, wantError: "provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := testOpenCodeAgent()
			fc := newFakeClient(original.DeepCopy())
			ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
			result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"my-agent","model":`+tt.modelJSON+`}`))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var response ChatToolResult
			if err := json.Unmarshal([]byte(result), &response); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, tt.wantError) {
				t.Fatalf("response = %#v, want rejection containing %q", response, tt.wantError)
			}

			var persisted corev1alpha1.Agent
			if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted.Spec, original.Spec) {
				t.Fatalf("persisted spec changed after rejected update:\n got: %#v\nwant: %#v", persisted.Spec, original.Spec)
			}
		})
	}
}

func TestUpdateAgentTool_Execute_RejectsUnusableResultingOpenCodeAgent(t *testing.T) {
	agent := testOpenCodeAgent()
	unsupportedTemperature := 1.0
	agent.Spec.Model.Temperature = &unsupportedTemperature
	fc := newFakeClient(agent.DeepCopy())
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"my-agent","tools":["web_search"]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var response ChatToolResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if response.Success || response.ErrorType != errTypeInvalidArgs || !strings.Contains(response.Error, "temperature") {
		t.Fatalf("response = %#v, want unusable resulting Agent rejection", response)
	}
	var persisted corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.Spec, agent.Spec) {
		t.Fatalf("persisted spec changed after rejected update:\n got: %#v\nwant: %#v", persisted.Spec, agent.Spec)
	}
}

func TestUpdateAgentTool_Execute_RejectsOpenCodeSystemPrompt(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
		},
	}
	fc := newFakeClient(agent)
	ctx := WithToolContext(context.Background(), &ToolContext{Client: fc, Namespace: defaultNamespace})
	result, err := (&UpdateAgentTool{}).Execute(ctx, json.RawMessage(`{"name":"my-agent","systemPrompt":"You write code"}`))
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
	var updated corev1alpha1.Agent
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyAgentName, Namespace: defaultNamespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.SystemPrompt != nil {
		t.Fatalf("systemPrompt = %#v, want unchanged nil value", updated.Spec.SystemPrompt)
	}
}

func testOpenCodeAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: testMyAgentName, Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode},
			Model:   testOpenCodeModelConfig("openai/gpt-5.4"),
		},
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
