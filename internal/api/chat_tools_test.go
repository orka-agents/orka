/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"testing"

	"github.com/sozercan/orka/internal/tools"
)

const schemaTypeObject = "object"

func TestChatRegistry_AllToolsRegistered(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)

	expectedNames := tools.ChatToolNames()
	for _, name := range expectedNames {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("chat registry missing expected tool %q", name)
		}
	}
}

func TestChatRegistry_CoreToolNames(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)

	coreNames := []string{
		"create_ai_task",
		"create_pr_monitor",
		"create_container_task",
		"create_agent_task",
		"check_task_progress",
		"fetch_task_output",
		"wait_for_task",
		"cancel_task",
		"list_agents",
		"list_tools",
		"list_tasks",
	}

	for _, name := range coreNames {
		tool, ok := reg.Get(name)
		if !ok {
			t.Errorf("missing core tool %q", name)
			continue
		}
		if tool.Description() == "" {
			t.Errorf("core tool %q has empty description", name)
		}
		if tool.Parameters() == nil {
			t.Errorf("core tool %q has nil parameters", name)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.Parameters(), &params); err != nil {
			t.Errorf("core tool %q has invalid JSON parameters: %v", name, err)
			continue
		}
		if params["type"] != schemaTypeObject {
			t.Errorf("core tool %q parameters type=%v, want object", name, params["type"])
		}
	}
}

func TestChatRegistry_ManagementToolNames(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)

	managementNames := []string{
		"create_agent",
		"update_agent",
		"delete_agent",
		"create_tool",
		"delete_tool",
		"delete_session",
	}

	for _, name := range managementNames {
		tool, ok := reg.Get(name)
		if !ok {
			t.Errorf("missing management tool %q", name)
			continue
		}
		if tool.Description() == "" {
			t.Errorf("management tool %q has empty description", name)
		}
		if tool.Parameters() == nil {
			t.Errorf("management tool %q has nil parameters", name)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.Parameters(), &params); err != nil {
			t.Errorf("management tool %q has invalid JSON parameters: %v", name, err)
			continue
		}
		if params["type"] != schemaTypeObject {
			t.Errorf("management tool %q parameters type=%v, want object", name, params["type"])
		}
	}
}

func TestChatRegistry_RequiredFields(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)

	tests := []struct {
		toolName     string
		wantRequired []string
	}{
		{"create_agent", []string{"name"}},
		{"update_agent", []string{"name"}},
		{"delete_agent", []string{"name"}},
		{"create_tool", []string{"name", "description", "url"}},
		{"delete_tool", []string{"name"}},
		{"delete_session", []string{"sessionId"}},
	}

	for _, tt := range tests {
		t.Run(tt.toolName, func(t *testing.T) {
			tool, ok := reg.Get(tt.toolName)
			if !ok {
				t.Fatalf("tool %q not found", tt.toolName)
			}

			var schema map[string]any
			if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
				t.Fatalf("invalid JSON schema: %v", err)
			}

			required, _ := schema["required"].([]any)
			reqSet := make(map[string]bool, len(required))
			for _, r := range required {
				if s, ok := r.(string); ok {
					reqSet[s] = true
				}
			}

			for _, want := range tt.wantRequired {
				if !reqSet[want] {
					t.Errorf("tool %q missing required field %q", tt.toolName, want)
				}
			}
		})
	}
}

func TestChatRegistry_NoOverlapBetweenCoreAndManagement(t *testing.T) {
	coreNames := map[string]bool{
		"create_ai_task":        true,
		"create_pr_monitor":     true,
		"create_container_task": true,
		"create_agent_task":     true,
		"check_task_progress":   true,
		"fetch_task_output":     true,
		"wait_for_task":         true,
		"cancel_task":           true,
		"list_agents":           true,
		"list_tools":            true,
		"list_tasks":            true,
	}

	managementNames := []string{
		"create_agent",
		"update_agent",
		"delete_agent",
		"create_tool",
		"delete_tool",
		"delete_session",
	}

	for _, name := range managementNames {
		if coreNames[name] {
			t.Errorf("tool %q appears in both core and management tool sets", name)
		}
	}
}

func TestChatToolNames_Idempotent(t *testing.T) {
	names1 := tools.ChatToolNames()
	names2 := tools.ChatToolNames()

	if len(names1) != len(names2) {
		t.Fatalf("ChatToolNames() not idempotent: %d vs %d", len(names1), len(names2))
	}
	for i := range names1 {
		if names1[i] != names2[i] {
			t.Errorf("ChatToolNames()[%d] = %q, want %q", i, names2[i], names1[i])
		}
	}
}

func TestChatToolNames_Count(t *testing.T) {
	names := tools.ChatToolNames()
	if len(names) != 17 {
		t.Errorf("ChatToolNames() returned %d tools, want 17", len(names))
	}
}

func TestChatRegistry_ToLLMTools(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterChatTools(reg)

	llmTools := reg.ToLLMTools(tools.ChatToolNames())
	if len(llmTools) != 17 {
		t.Errorf("ToLLMTools() returned %d tools, want 17", len(llmTools))
	}

	nameSet := make(map[string]bool, len(llmTools))
	for _, tool := range llmTools {
		nameSet[tool.Name] = true
	}

	for _, name := range tools.ChatToolNames() {
		if !nameSet[name] {
			t.Errorf("ToLLMTools() missing tool %q", name)
		}
	}
}
