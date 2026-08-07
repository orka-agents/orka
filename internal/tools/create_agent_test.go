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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
)

func TestCreateAgentTool_Name(t *testing.T) {
	tool := NewCreateAgentTool(newFakeClient(), executionmode.HarnessV2)
	if got := tool.Name(); got != createAgentToolName {
		t.Errorf("Name() = %v, want %v", got, createAgentToolName)
	}
}

func TestCreateAgentTool_Description(t *testing.T) {
	tool := NewCreateAgentTool(newFakeClient(), executionmode.HarnessV2)
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCreateAgentTool_Parameters(t *testing.T) {
	tool := NewCreateAgentTool(newFakeClient(), executionmode.HarnessV2)
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("Parameters schema required field is not an array")
	}
	if slices.Contains(required, any("systemPrompt")) {
		t.Error("Parameters schema must not require systemPrompt for OpenCode")
	}
	properties, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("Parameters schema properties field is not an object")
	}
	systemPrompt, ok := properties["systemPrompt"].(map[string]any)
	description, _ := systemPrompt[jsonSchemaDescriptionField].(string)
	if !ok || !strings.Contains(description, "Required unless runtime.type is opencode") {
		t.Fatalf("systemPrompt schema = %#v, want OpenCode omission guidance", systemPrompt)
	}
}

func TestCreateAgentTool_Execute(t *testing.T) {
	tests := []struct {
		name        string
		envVars     map[string]string
		args        json.RawMessage
		wantErr     bool
		wantErrMsg  string
		checkResult bool
		wantStatus  string
	}{
		{
			name: "success with all args",
			envVars: map[string]string{
				envOrkaTaskName:      parentTaskName,
				envOrkaTaskNamespace: defaultNamespace,
			},
			args: json.RawMessage(`{
				"role": "coder",
				"systemPrompt": "You are a coder agent",
				"model": {"provider": "anthropic", "name": "claude-sonnet-4-20250514"},
				"providerRef": "my-provider",
				"tools": ["web_search", "code_exec"],
				"skills": ["go-coding"],
				"coordination": {
					"enabled": true,
					"maxDepth": 2,
					"maxConcurrentChildren": 3,
					"allowedAgents": [{"name": "reviewer", "namespace": "` + defaultNamespace + `"}]
				}
			}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  GitHubPullRequestStatusCreated,
		},
		{
			name: "success with minimal args inherited model/provider",
			envVars: map[string]string{
				envOrkaTaskName:      parentTaskName,
				envOrkaTaskNamespace: defaultNamespace,
				"ORKA_AI_PROVIDER":   providerOpenAI,
				"ORKA_AI_MODEL":      testGPT4OModel,
			},
			args:        json.RawMessage(`{"role": "reviewer", "systemPrompt": "You review code"}`),
			wantErr:     false,
			checkResult: true,
			wantStatus:  GitHubPullRequestStatusCreated,
		},
		{
			name: "error when role is empty",
			envVars: map[string]string{
				envOrkaTaskName:      parentTaskName,
				envOrkaTaskNamespace: defaultNamespace,
			},
			args:       json.RawMessage(`{"role": "", "systemPrompt": "prompt"}`),
			wantErr:    true,
			wantErrMsg: "role is required",
		},
		{
			name: "error when systemPrompt is empty",
			envVars: map[string]string{
				envOrkaTaskName:      parentTaskName,
				envOrkaTaskNamespace: defaultNamespace,
			},
			args:       json.RawMessage(`{"role": "coder", "systemPrompt": ""}`),
			wantErr:    true,
			wantErrMsg: "systemPrompt is required",
		},
		{
			name: invalidJSONArgsCaseName,
			envVars: map[string]string{
				envOrkaTaskName:      parentTaskName,
				envOrkaTaskNamespace: defaultNamespace,
			},
			args:       json.RawMessage(invalidJSONText),
			wantErr:    true,
			wantErrMsg: invalidArgumentsMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			k8sClient := newFakeClient(parentTask())
			tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

			result, err := tool.Execute(context.Background(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.wantErrMsg != "" {
				if err == nil || !contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("Execute() error = %v, want error containing %q", err, tt.wantErrMsg)
				}
				return
			}

			if tt.checkResult {
				var agentResult CreateAgentResult
				if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if agentResult.Status != tt.wantStatus {
					t.Errorf("Execute() status = %q, want %q", agentResult.Status, tt.wantStatus)
				}
				if agentResult.AgentName == "" {
					t.Error("Execute() returned empty agent name")
				}
				if agentResult.Namespace == "" {
					t.Error("Execute() returned empty namespace")
				}
			}
		})
	}
}

func TestCreateAgentTool_Execute_OwnerReference(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	k8sClient := newFakeClient(parentTask())
	tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

	args := json.RawMessage(`{"role": "coder", "systemPrompt": "You code things"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Fetch the created agent
	agent := &corev1alpha1.Agent{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: agentResult.AgentName, Namespace: agentResult.Namespace,
	}, agent); err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}

	// Verify owner reference
	if len(agent.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(agent.OwnerReferences))
	}
	ownerRef := agent.OwnerReferences[0]
	if ownerRef.Name != parentTaskName {
		t.Errorf("ownerRef.Name = %q, want %q", ownerRef.Name, parentTaskName)
	}
	if ownerRef.UID != apitypes.UID("parent-uid-1234") {
		t.Errorf("ownerRef.UID = %q, want %q", ownerRef.UID, "parent-uid-1234")
	}
	if ownerRef.Kind != taskKindString {
		t.Errorf("ownerRef.Kind = %q, want %q", ownerRef.Kind, "Task")
	}
	if ownerRef.Controller == nil || !*ownerRef.Controller {
		t.Error("ownerRef.Controller should be true")
	}
	if ownerRef.BlockOwnerDeletion == nil || !*ownerRef.BlockOwnerDeletion {
		t.Error("ownerRef.BlockOwnerDeletion should be true")
	}
}

func TestCreateAgentTool_Execute_AutoNaming(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	k8sClient := newFakeClient(parentTask())
	tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

	args := json.RawMessage(`{"role": "reviewer", "systemPrompt": "You review code"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Verify naming format: {parentTaskName}-{role}-{shortHash}
	if !strings.HasPrefix(agentResult.AgentName, parentTaskName+"-reviewer-") {
		t.Errorf("agent name %q should have prefix %q", agentResult.AgentName, parentTaskName+"-reviewer-")
	}

	// Verify the short hash is 6 hex chars
	parts := strings.SplitN(agentResult.AgentName, "-", 4)
	if len(parts) < 4 {
		t.Fatalf("expected at least 4 parts in name, got %d: %q", len(parts), agentResult.AgentName)
	}
	shortHash := parts[len(parts)-1]
	if len(shortHash) != 6 {
		t.Errorf("short hash %q should be 6 chars", shortHash)
	}
}

func TestCreateAgentTool_Execute_AllFields(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	k8sClient := newFakeClient(parentTask())
	tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

	args := json.RawMessage(`{
		"role": "coder",
		"systemPrompt": "You are a Go coder",
		"model": {"provider": "anthropic", "name": "claude-sonnet-4-20250514"},
		"providerRef": "my-provider",
		"tools": ["web_search", "code_exec"],
		"skills": ["go-coding"],
		"coordination": {
			"enabled": true,
			"maxDepth": 2,
			"maxConcurrentChildren": 3,
			"allowedAgents": [{"name": "reviewer", "namespace": "test-ns"}]
		}
	}`)

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Fetch the created agent
	agent := &corev1alpha1.Agent{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: agentResult.AgentName, Namespace: agentResult.Namespace,
	}, agent); err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}

	// Verify model
	if agent.Spec.Model == nil {
		t.Fatal("agent.Spec.Model is nil")
	}
	if agent.Spec.Model.Provider != "" {
		t.Errorf("model.provider = %q, want empty (cleared to avoid mismatch with providerRef)", agent.Spec.Model.Provider)
	}
	if agent.Spec.Model.Name != "claude-sonnet-4-20250514" {
		t.Errorf("model.name = %q, want %q", agent.Spec.Model.Name, "claude-sonnet-4-20250514")
	}

	// Verify provider ref
	if agent.Spec.ProviderRef == nil || agent.Spec.ProviderRef.Name != "my-provider" {
		t.Errorf("providerRef = %v, want my-provider", agent.Spec.ProviderRef)
	}

	// Verify system prompt
	if agent.Spec.SystemPrompt == nil || agent.Spec.SystemPrompt.Inline != "You are a Go coder" {
		t.Errorf("systemPrompt.inline = %v, want %q", agent.Spec.SystemPrompt, "You are a Go coder")
	}

	// Verify tools
	if len(agent.Spec.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(agent.Spec.Tools))
	}
	if agent.Spec.Tools[0].Name != webSearchToolName {
		t.Errorf("tools[0].name = %q, want %q", agent.Spec.Tools[0].Name, webSearchToolName)
	}
	if agent.Spec.Tools[1].Name != codeExecToolName {
		t.Errorf("tools[1].name = %q, want %q", agent.Spec.Tools[1].Name, codeExecToolName)
	}

	// Verify skills
	if len(agent.Spec.Skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(agent.Spec.Skills))
	}
	if agent.Spec.Skills[0].Name != "go-coding" {
		t.Errorf("skills[0].name = %q, want %q", agent.Spec.Skills[0].Name, "go-coding")
	}

	// Verify coordination
	if agent.Spec.Coordination == nil {
		t.Fatal("agent.Spec.Coordination is nil")
	}
	if !agent.Spec.Coordination.Enabled {
		t.Error("coordination.enabled should be true")
	}
	if agent.Spec.Coordination.MaxDepth != 2 {
		t.Errorf("coordination.maxDepth = %d, want 2", agent.Spec.Coordination.MaxDepth)
	}
	if agent.Spec.Coordination.MaxConcurrentChildren != 3 {
		t.Errorf("coordination.maxConcurrentChildren = %d, want 3", agent.Spec.Coordination.MaxConcurrentChildren)
	}
	if len(agent.Spec.Coordination.AllowedAgents) != 1 {
		t.Fatalf("expected 1 allowed agent, got %d", len(agent.Spec.Coordination.AllowedAgents))
	}
	if agent.Spec.Coordination.AllowedAgents[0].Name != "reviewer" {
		t.Errorf("allowedAgents[0].name = %q, want %q", agent.Spec.Coordination.AllowedAgents[0].Name, "reviewer")
	}
	if agent.Spec.Coordination.AllowedAgents[0].Namespace != testNamespace {
		t.Errorf("allowedAgents[0].namespace = %q, want %q", agent.Spec.Coordination.AllowedAgents[0].Namespace, testNamespace)
	}

	// Verify labels
	if agent.Labels[labels.LabelParentTask] != labels.SelectorValue(parentTaskName) {
		t.Errorf("label orka.ai/parent-task = %q, want %q", agent.Labels[labels.LabelParentTask], labels.SelectorValue(parentTaskName))
	}
	if agent.Labels[labels.LabelCreatedBy] != createAgentToolName {
		t.Errorf("label orka.ai/created-by = %q, want %q", agent.Labels[labels.LabelCreatedBy], createAgentToolName)
	}
	if agent.Labels[labels.LabelAgentRole] != testCoderAgentName {
		t.Errorf("label orka.ai/agent-role = %q, want %q", agent.Labels[labels.LabelAgentRole], testCoderAgentName)
	}
	if agent.Annotations[labels.AnnotationParentTaskName] != parentTaskName {
		t.Errorf("annotation orka.ai/parent-task-name = %q, want %q", agent.Annotations[labels.AnnotationParentTaskName], parentTaskName)
	}
}

func TestCreateAgentTool_Execute_InheritedModelProvider(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	t.Setenv("ORKA_AI_PROVIDER", providerOpenAI)
	t.Setenv("ORKA_AI_MODEL", testGPT4OModel)

	k8sClient := newFakeClient(parentTask())
	tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

	args := json.RawMessage(`{"role": "analyst", "systemPrompt": "You analyze data"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	agent := &corev1alpha1.Agent{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: agentResult.AgentName, Namespace: agentResult.Namespace,
	}, agent); err != nil {
		t.Fatalf("failed to get agent: %v", err)
	}

	// Verify model was inherited from env vars
	if agent.Spec.Model == nil {
		t.Fatal("agent.Spec.Model is nil")
	}
	if agent.Spec.Model.Provider != "" {
		t.Errorf("model.provider = %q, want empty (cleared to avoid mismatch with providerRef)", agent.Spec.Model.Provider)
	}
	if agent.Spec.Model.Name != testGPT4OModel {
		t.Errorf("model.name = %q, want %q (inherited)", agent.Spec.Model.Name, testGPT4OModel)
	}

	// Verify provider ref was inherited
	if agent.Spec.ProviderRef == nil || agent.Spec.ProviderRef.Name != providerOpenAI {
		t.Errorf("providerRef = %v, want openai (inherited)", agent.Spec.ProviderRef)
	}
}

func TestCreateAgentTool_Execute_BuiltInRuntimesAreCredentialFree(t *testing.T) {
	tests := []struct {
		name        string
		runtimeArgs string
		runtimeType corev1alpha1.AgentRuntimeType
	}{
		{
			name:        "claude without legacy secret",
			runtimeArgs: `{"type":"claude"}`,
			runtimeType: corev1alpha1.AgentRuntimeClaude,
		},
		{
			name:        "codex without legacy secret",
			runtimeArgs: `{"type":"codex"}`,
			runtimeType: corev1alpha1.AgentRuntimeCodex,
		},
		{
			name:        "copilot without legacy secret",
			runtimeArgs: `{"type":"copilot"}`,
			runtimeType: corev1alpha1.AgentRuntimeCopilot,
		},
		{
			name:        "legacy secretRef is not retained",
			runtimeArgs: `{"type":"claude","secretRef":"missing-legacy-secret"}`,
			runtimeType: corev1alpha1.AgentRuntimeClaude,
		},
		{
			name:        "runtime type is normalized",
			runtimeArgs: `{"type":"  claude  "}`,
			runtimeType: corev1alpha1.AgentRuntimeClaude,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envOrkaTaskName, parentTaskName)
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)

			k8sClient := newFakeClient(parentTask())
			tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)
			args := json.RawMessage(`{"role":"coder","systemPrompt":"You write code","runtime":` + tt.runtimeArgs + `}`)

			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var agentResult CreateAgentResult
			if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}

			agent := &corev1alpha1.Agent{}
			if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
				Name:      agentResult.AgentName,
				Namespace: agentResult.Namespace,
			}, agent); err != nil {
				t.Fatalf("failed to get agent: %v", err)
			}

			if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != tt.runtimeType {
				t.Fatalf("runtime = %#v, want type %q", agent.Spec.Runtime, tt.runtimeType)
			}
			if got := agent.BuiltInContractVersion(); got != corev1alpha1.AgentRuntimeContractHarnessV2 {
				t.Fatalf("contractVersion = %q, want %q", got, corev1alpha1.AgentRuntimeContractHarnessV2)
			}
			if agent.Spec.ProviderRef != nil {
				t.Fatalf("providerRef = %#v, want nil for runtime agent", agent.Spec.ProviderRef)
			}
			if agent.Spec.SecretRef != nil {
				t.Fatalf("secretRef = %#v, want nil for built-in ACP runtime", agent.Spec.SecretRef)
			}
		})
	}
}

func TestCreateAgentTool_Execute_DefaultsHarnessV1Contract(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)

	k8sClient := newFakeClient(parentTask())
	result, err := NewCreateAgentTool(k8sClient, executionmode.HarnessV1).Execute(
		context.Background(),
		json.RawMessage(`{"role":"coder","systemPrompt":"You write code","runtime":{"type":"codex"}}`),
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var createdResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &createdResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	created := &corev1alpha1.Agent{}
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{
		Name: createdResult.AgentName, Namespace: createdResult.Namespace,
	}, created); err != nil {
		t.Fatalf("failed to get Agent: %v", err)
	}
	if got := created.BuiltInContractVersion(); got != corev1alpha1.AgentRuntimeContractHarnessV1 {
		t.Fatalf("contractVersion = %q, want %q", got, corev1alpha1.AgentRuntimeContractHarnessV1)
	}
}

func TestCreateAgentTool_Execute_RejectsModelLimitsForNonOpenCodeBuiltIns(t *testing.T) {
	for _, tt := range []struct {
		name      string
		runtime   string
		modelJSON string
		wantError string
	}{
		{name: "codex context window", runtime: "codex", modelJSON: `{"name":"gpt-5.4","contextWindow":32768}`, wantError: "contextWindow"},
		{name: "claude max tokens", runtime: "claude", modelJSON: `{"name":"claude-sonnet","maxTokens":4096}`, wantError: "maxTokens"},
		{name: "copilot context window", runtime: "copilot", modelJSON: `{"name":"gpt-5.3-codex","contextWindow":32768}`, wantError: "contextWindow"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envOrkaTaskName, parentTaskName)
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)
			_, err := NewCreateAgentTool(newFakeClient(parentTask()), executionmode.HarnessV2).Execute(context.Background(), json.RawMessage(
				`{"role":"coder","systemPrompt":"You write code","model":`+tt.modelJSON+`,"runtime":{"type":"`+tt.runtime+`"}}`,
			))
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Execute() error = %v, want %q rejection", err, tt.wantError)
			}
		})
	}
}

func TestNormalizedCreateAgentModelAcceptsQualifiedIDWithNestedSlashes(t *testing.T) {
	contextWindow := int32(32768)
	maxTokens := int32(4096)
	got, err := normalizedCreateAgentModel(
		&RuntimeArgs{Type: string(corev1alpha1.AgentRuntimeOpencode)},
		&ModelArgs{Name: "openrouter/anthropic/claude-sonnet-4", ContextWindow: &contextWindow, MaxTokens: &maxTokens},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "openrouter/anthropic/claude-sonnet-4" {
		t.Fatalf("normalized model = %q, want full provider-scoped ID", got)
	}
}

func TestCreateAgentTool_Execute_UsesBuiltInOpenCode(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	k8sClient := newFakeClient(parentTask())
	tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"role":"coder",
		"model":{"provider":"moonshotai","name":"Kimi-K2-Instruct-0905","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatal(err)
	}
	var agent corev1alpha1.Agent
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{Name: agentResult.AgentName, Namespace: agentResult.Namespace}, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeOpencode {
		t.Fatalf("runtime = %#v, want opencode", agent.Spec.Runtime)
	}
	if agent.Spec.Model == nil || agent.Spec.Model.Name != "moonshotai/Kimi-K2-Instruct-0905" || agent.Spec.Model.Provider != "" {
		t.Fatalf("model = %#v, want provider-qualified OpenCode model", agent.Spec.Model)
	}
	wantTools := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
	if !slices.Equal(agent.Spec.Runtime.DefaultAllowedTools, wantTools) {
		t.Fatalf("defaultAllowedTools = %#v, want %#v", agent.Spec.Runtime.DefaultAllowedTools, wantTools)
	}
	if agent.Spec.Runtime.DefaultAllowBash == nil || !*agent.Spec.Runtime.DefaultAllowBash {
		t.Fatalf("defaultAllowBash = %v, want true", agent.Spec.Runtime.DefaultAllowBash)
	}
	if agent.Spec.ProviderRef != nil || agent.Spec.SecretRef != nil {
		t.Fatalf("providerRef=%#v secretRef=%#v, want credential-free runtime", agent.Spec.ProviderRef, agent.Spec.SecretRef)
	}
	if agent.Spec.SystemPrompt != nil {
		t.Fatalf("systemPrompt = %#v, want nil for OpenCode runtime", agent.Spec.SystemPrompt)
	}
}

func TestCreateAgentTool_Execute_PreservesExplicitEmptyOpenCodeTools(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	k8sClient := newFakeClient(parentTask())
	result, err := NewCreateAgentTool(k8sClient, executionmode.HarnessV2).Execute(context.Background(), json.RawMessage(`{
		"role":"coder","model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode","defaultAllowedTools":[],"defaultAllowBash":false}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatal(err)
	}
	var agent corev1alpha1.Agent
	if err := k8sClient.Get(context.Background(), apitypes.NamespacedName{Name: agentResult.AgentName, Namespace: agentResult.Namespace}, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.DefaultAllowedTools == nil || len(agent.Spec.Runtime.DefaultAllowedTools) != 0 {
		t.Fatalf("defaultAllowedTools = %#v, want explicit empty", agent.Spec.Runtime)
	}
}

func TestCreateAgentTool_Execute_RejectsOpenCodeSystemPrompt(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	t.Setenv(envOrkaTaskNamespace, defaultNamespace)
	_, err := NewCreateAgentTool(newFakeClient(parentTask()), executionmode.HarnessV2).Execute(context.Background(), json.RawMessage(`{
		"role":"coder",
		"systemPrompt":"You write code",
		"model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
		"runtime":{"type":"opencode"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "does not support systemPrompt") {
		t.Fatalf("Execute() error = %v, want OpenCode systemPrompt rejection", err)
	}
}

func TestCreateAgentTool_Execute_RejectsInvalidOpenCodeConfiguration(t *testing.T) {
	for name, args := range map[string]string{
		"missing model":     `{"role":"coder","runtime":{"type":"opencode"}}`,
		"bare model":        `{"role":"coder","model":{"name":"gpt-5.4","contextWindow":32768,"maxTokens":4096},"runtime":{"type":"opencode"}}`,
		"substitution":      `{"role":"coder","model":{"name":"{env:PROVIDER}/gpt","contextWindow":32768,"maxTokens":4096},"runtime":{"type":"opencode"}}`,
		"provider conflict": `{"role":"coder","model":{"provider":"anthropic","name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},"runtime":{"type":"opencode"}}`,
		"missing context":   `{"role":"coder","model":{"name":"openai/gpt-5.4","maxTokens":4096},"runtime":{"type":"opencode"}}`,
		"missing output":    `{"role":"coder","model":{"name":"openai/gpt-5.4","contextWindow":32768},"runtime":{"type":"opencode"}}`,
		"inverted limits":   `{"role":"coder","model":{"name":"openai/gpt-5.4","contextWindow":4096,"maxTokens":4096},"runtime":{"type":"opencode"}}`,
		"legacy secret":     `{"role":"coder","model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},"runtime":{"type":"opencode","secretRef":"legacy"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envOrkaTaskName, parentTaskName)
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)
			_, err := NewCreateAgentTool(newFakeClient(parentTask()), executionmode.HarnessV2).Execute(context.Background(), json.RawMessage(args))
			if err == nil {
				t.Fatal("Execute() accepted invalid OpenCode configuration")
			}
		})
	}
}

func TestCreateAgentTool_Execute_RejectsUnsupportedRuntime(t *testing.T) {
	for _, runtimeArgs := range []string{
		`{"type":"unknown"}`,
		`{}`,
		`{"type":"   "}`,
	} {
		t.Run(runtimeArgs, func(t *testing.T) {
			t.Setenv(envOrkaTaskName, parentTaskName)
			t.Setenv(envOrkaTaskNamespace, defaultNamespace)
			k8sClient := newFakeClient(parentTask())
			tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)
			args := json.RawMessage(`{"role":"coder","systemPrompt":"You write code","runtime":` + runtimeArgs + `}`)
			_, err := tool.Execute(context.Background(), args)
			if err == nil || (!strings.Contains(err.Error(), "unsupported runtime type") && !strings.Contains(err.Error(), "runtime.type is required")) {
				t.Fatalf("Execute() error = %v, want invalid runtime rejection", err)
			}
		})
	}
}

func TestCreateAgentTool_Execute_DefaultNamespace(t *testing.T) {
	t.Setenv(envOrkaTaskName, parentTaskName)
	// No ORKA_TASK_NAMESPACE set — should default to defaultNamespace

	// Create parent task in defaultNamespace namespace
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      parentTaskName,
			Namespace: defaultNamespace,
			UID:       apitypes.UID("parent-uid-1234"),
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}

	k8sClient := newFakeClient(parent)
	tool := NewCreateAgentTool(k8sClient, executionmode.HarnessV2)

	args := json.RawMessage(`{"role": "coder", "systemPrompt": "Code stuff"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var agentResult CreateAgentResult
	if err := json.Unmarshal([]byte(result), &agentResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if agentResult.Namespace != defaultNamespace {
		t.Errorf("namespace = %q, want %q", agentResult.Namespace, defaultNamespace)
	}
}
