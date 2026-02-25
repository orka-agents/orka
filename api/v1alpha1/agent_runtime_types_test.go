/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestTaskTypeAgentConstant(t *testing.T) {
	if TaskTypeAgent != "agent" {
		t.Errorf("TaskTypeAgent = %q, want %q", TaskTypeAgent, "agent")
	}
}

func TestTaskTypeConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant TaskType
		want     string
	}{
		{"container", TaskTypeContainer, "container"},
		{"ai", TaskTypeAI, "ai"},
		{"agent", TaskTypeAgent, "agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.constant) != tt.want {
				t.Errorf("TaskType constant %s = %q, want %q", tt.name, tt.constant, tt.want)
			}
		})
	}
}

func TestAgentRuntimeTypeConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant AgentRuntimeType
		want     string
	}{
		{"copilot", AgentRuntimeCopilot, "copilot"},
		{"claude", AgentRuntimeClaude, "claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.constant) != tt.want {
				t.Errorf("AgentRuntimeType %s = %q, want %q", tt.name, tt.constant, tt.want)
			}
		})
	}
}

func TestAgentRuntimeSpecFields(t *testing.T) {
	maxTurns := int32(10)
	allowBash := true
	spec := AgentRuntimeSpec{
		MaxTurns:        &maxTurns,
		AllowedTools:    []string{"read", "write"},
		DisallowedTools: []string{"delete"},
		AllowBash:       &allowBash,
		Workspace: &WorkspaceConfig{
			GitRepo: "https://github.com/example/repo",
			Branch:  "main",
		},
	}

	if *spec.MaxTurns != 10 {
		t.Errorf("MaxTurns = %d, want 10", *spec.MaxTurns)
	}
	if len(spec.AllowedTools) != 2 {
		t.Errorf("AllowedTools len = %d, want 2", len(spec.AllowedTools))
	}
	if spec.AllowedTools[0] != "read" || spec.AllowedTools[1] != "write" {
		t.Errorf("AllowedTools = %v, want [read write]", spec.AllowedTools)
	}
	if len(spec.DisallowedTools) != 1 || spec.DisallowedTools[0] != "delete" {
		t.Errorf("DisallowedTools = %v, want [delete]", spec.DisallowedTools)
	}
	if *spec.AllowBash != true {
		t.Errorf("AllowBash = %v, want true", *spec.AllowBash)
	}
	if spec.Workspace == nil {
		t.Fatal("Workspace should not be nil")
	}
	if spec.Workspace.GitRepo != "https://github.com/example/repo" {
		t.Errorf("Workspace.GitRepo = %q, want %q", spec.Workspace.GitRepo, "https://github.com/example/repo")
	}
}

func TestAgentRuntimeSpecDefaults(t *testing.T) {
	spec := AgentRuntimeSpec{}

	if spec.MaxTurns != nil {
		t.Errorf("MaxTurns should be nil by default, got %v", spec.MaxTurns)
	}
	if spec.AllowedTools != nil {
		t.Errorf("AllowedTools should be nil by default, got %v", spec.AllowedTools)
	}
	if spec.DisallowedTools != nil {
		t.Errorf("DisallowedTools should be nil by default, got %v", spec.DisallowedTools)
	}
	if spec.AllowBash != nil {
		t.Errorf("AllowBash should be nil by default, got %v", spec.AllowBash)
	}
	if spec.Workspace != nil {
		t.Errorf("Workspace should be nil by default, got %v", spec.Workspace)
	}
}

func TestWorkspaceConfigFields(t *testing.T) {
	secretRef := &corev1.LocalObjectReference{Name: "git-secret"}
	ws := WorkspaceConfig{
		GitRepo:      "https://github.com/example/repo",
		Branch:       "develop",
		Ref:          "abc123",
		GitSecretRef: secretRef,
		SubPath:      "src/app",
	}

	if ws.GitRepo != "https://github.com/example/repo" {
		t.Errorf("GitRepo = %q, want %q", ws.GitRepo, "https://github.com/example/repo")
	}
	if ws.Branch != "develop" {
		t.Errorf("Branch = %q, want %q", ws.Branch, "develop")
	}
	if ws.Ref != "abc123" {
		t.Errorf("Ref = %q, want %q", ws.Ref, "abc123")
	}
	if ws.GitSecretRef == nil || ws.GitSecretRef.Name != "git-secret" {
		t.Errorf("GitSecretRef.Name = %v, want %q", ws.GitSecretRef, "git-secret")
	}
	if ws.SubPath != "src/app" {
		t.Errorf("SubPath = %q, want %q", ws.SubPath, "src/app")
	}
}

func TestWorkspaceConfigDefaults(t *testing.T) {
	ws := WorkspaceConfig{}

	if ws.GitRepo != "" {
		t.Errorf("GitRepo should be empty by default, got %q", ws.GitRepo)
	}
	if ws.Branch != "" {
		t.Errorf("Branch should be empty by default, got %q", ws.Branch)
	}
	if ws.Ref != "" {
		t.Errorf("Ref should be empty by default, got %q", ws.Ref)
	}
	if ws.GitSecretRef != nil {
		t.Errorf("GitSecretRef should be nil by default, got %v", ws.GitSecretRef)
	}
	if ws.SubPath != "" {
		t.Errorf("SubPath should be empty by default, got %q", ws.SubPath)
	}
}

func TestAgentCLIRuntimeFields(t *testing.T) {
	maxTurns := int32(50)
	allowBash := true
	runtime := AgentCLIRuntime{
		Type:                AgentRuntimeCopilot,
		DefaultMaxTurns:     &maxTurns,
		DefaultAllowedTools: []string{"bash", "edit"},
		DefaultAllowBash:    &allowBash,
	}

	if runtime.Type != AgentRuntimeCopilot {
		t.Errorf("Type = %q, want %q", runtime.Type, AgentRuntimeCopilot)
	}
	if *runtime.DefaultMaxTurns != 50 {
		t.Errorf("DefaultMaxTurns = %d, want 50", *runtime.DefaultMaxTurns)
	}
	if len(runtime.DefaultAllowedTools) != 2 {
		t.Errorf("DefaultAllowedTools len = %d, want 2", len(runtime.DefaultAllowedTools))
	}
	if runtime.DefaultAllowBash == nil || !*runtime.DefaultAllowBash {
		t.Error("DefaultAllowBash should be true")
	}
}

func TestAgentCLIRuntimeOnAgentSpec(t *testing.T) {
	maxTurns := int32(25)
	allowBash := false
	agent := AgentSpec{
		Runtime: &AgentCLIRuntime{
			Type:             AgentRuntimeClaude,
			DefaultMaxTurns:  &maxTurns,
			DefaultAllowBash: &allowBash,
		},
	}

	if agent.Runtime == nil {
		t.Fatal("Runtime should not be nil")
	}
	if agent.Runtime.Type != AgentRuntimeClaude {
		t.Errorf("Runtime.Type = %q, want %q", agent.Runtime.Type, AgentRuntimeClaude)
	}
	if *agent.Runtime.DefaultMaxTurns != 25 {
		t.Errorf("Runtime.DefaultMaxTurns = %d, want 25", *agent.Runtime.DefaultMaxTurns)
	}
	if agent.Runtime.DefaultAllowBash == nil || *agent.Runtime.DefaultAllowBash {
		t.Error("Runtime.DefaultAllowBash should be false")
	}
}

func TestTaskSpecAgentRuntimeField(t *testing.T) {
	maxTurns := int32(15)
	task := TaskSpec{
		Type:   TaskTypeAgent,
		Prompt: "Fix the bug",
		AgentRef: &AgentReference{
			Name: "my-agent",
		},
		AgentRuntime: &AgentRuntimeSpec{
			MaxTurns:     &maxTurns,
			AllowedTools: []string{"bash", "read"},
			Workspace: &WorkspaceConfig{
				GitRepo: "https://github.com/example/repo",
				Branch:  "main",
			},
		},
	}

	if task.Type != TaskTypeAgent {
		t.Errorf("Type = %q, want %q", task.Type, TaskTypeAgent)
	}
	if task.AgentRuntime == nil {
		t.Fatal("AgentRuntime should not be nil")
	}
	if *task.AgentRuntime.MaxTurns != 15 {
		t.Errorf("AgentRuntime.MaxTurns = %d, want 15", *task.AgentRuntime.MaxTurns)
	}
	if task.AgentRuntime.Workspace == nil {
		t.Fatal("AgentRuntime.Workspace should not be nil")
	}
	if task.AgentRuntime.Workspace.Branch != "main" {
		t.Errorf("Workspace.Branch = %q, want %q", task.AgentRuntime.Workspace.Branch, "main")
	}
}

func TestAgentRuntimeRequiredForAgentType(t *testing.T) {
	// When type is "agent", AgentRuntime and AgentRef should be set for proper configuration.
	// This tests the structural expectation (not webhook validation).
	task := TaskSpec{
		Type: TaskTypeAgent,
	}

	if task.AgentRuntime != nil {
		t.Error("AgentRuntime should be nil when not explicitly set")
	}
	if task.AgentRef != nil {
		t.Error("AgentRef should be nil when not explicitly set")
	}

	// A well-formed agent task should have AgentRef
	task.AgentRef = &AgentReference{Name: "my-agent"}
	if task.AgentRef == nil {
		t.Error("AgentRef should not be nil after being set")
	}

	// And optionally AgentRuntime for overrides
	maxTurns := int32(20)
	task.AgentRuntime = &AgentRuntimeSpec{MaxTurns: &maxTurns}
	if task.AgentRuntime == nil {
		t.Error("AgentRuntime should not be nil after being set")
	}
}

func TestAgentRuntimeTypeAssignment(t *testing.T) {
	// Verify AgentRuntimeType can be used in AgentCLIRuntime
	tests := []struct {
		name        string
		runtimeType AgentRuntimeType
	}{
		{"copilot runtime", AgentRuntimeCopilot},
		{"claude runtime", AgentRuntimeClaude},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := AgentCLIRuntime{Type: tt.runtimeType}
			if runtime.Type != tt.runtimeType {
				t.Errorf("Type = %q, want %q", runtime.Type, tt.runtimeType)
			}
		})
	}
}
