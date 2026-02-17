/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"testing"
)

func TestMustMarshal(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string
	}{
		{
			name:  "simple string",
			input: "hello",
			want:  `"hello"`,
		},
		{
			name:  "integer",
			input: 42,
			want:  `42`,
		},
		{
			name:  "map",
			input: map[string]string{"key": "value"},
			want:  `{"key":"value"}`,
		},
		{
			name:  "nil",
			input: nil,
			want:  `null`,
		},
		{
			name:  "slice",
			input: []string{"a", "b"},
			want:  `["a","b"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustMarshal(tt.input)
			if string(got) != tt.want {
				t.Errorf("mustMarshal() = %s, want %s", string(got), tt.want)
			}
		})
	}
}

func TestMustMarshal_ReturnsValidJSON(t *testing.T) {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
		},
		"required": []string{"name"},
	}
	got := mustMarshal(nested)

	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("mustMarshal() produced invalid JSON: %v", err)
	}
	if parsed["type"] != "object" {
		t.Errorf("expected type=object, got %v", parsed["type"])
	}
}

func TestCoreTools(t *testing.T) {
	tools := CoreTools()

	expectedNames := []string{
		"create_ai_task",
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

	if len(tools) != len(expectedNames) {
		t.Fatalf("CoreTools() returned %d tools, want %d", len(tools), len(expectedNames))
	}

	nameSet := make(map[string]bool, len(tools))
	for _, tool := range tools {
		nameSet[tool.Name] = true
	}

	for _, name := range expectedNames {
		if !nameSet[name] {
			t.Errorf("CoreTools() missing expected tool %q", name)
		}
	}
}

func TestCoreTools_HaveDescriptions(t *testing.T) {
	for _, tool := range CoreTools() {
		if tool.Description == "" {
			t.Errorf("CoreTools tool %q has empty description", tool.Name)
		}
	}
}

func TestCoreTools_HaveValidParameters(t *testing.T) {
	for _, tool := range CoreTools() {
		if tool.Parameters == nil {
			t.Errorf("CoreTools tool %q has nil parameters", tool.Name)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.Parameters, &params); err != nil {
			t.Errorf("CoreTools tool %q has invalid JSON parameters: %v", tool.Name, err)
			continue
		}
		if params["type"] != "object" {
			t.Errorf("CoreTools tool %q parameters type=%v, want object", tool.Name, params["type"])
		}
	}
}

func TestCoreTools_Idempotent(t *testing.T) {
	tools1 := CoreTools()
	tools2 := CoreTools()

	if len(tools1) != len(tools2) {
		t.Fatalf("CoreTools() not idempotent: %d vs %d", len(tools1), len(tools2))
	}
	for i := range tools1 {
		if tools1[i].Name != tools2[i].Name {
			t.Errorf("CoreTools()[%d].Name = %q, want %q", i, tools2[i].Name, tools1[i].Name)
		}
	}
}

func TestManagementTools(t *testing.T) {
	tools := ManagementTools()

	expectedNames := []string{
		"create_agent",
		"update_agent",
		"delete_agent",
		"create_tool",
		"delete_tool",
		"delete_session",
	}

	if len(tools) != len(expectedNames) {
		t.Fatalf("ManagementTools() returned %d tools, want %d", len(tools), len(expectedNames))
	}

	nameSet := make(map[string]bool, len(tools))
	for _, tool := range tools {
		nameSet[tool.Name] = true
	}

	for _, name := range expectedNames {
		if !nameSet[name] {
			t.Errorf("ManagementTools() missing expected tool %q", name)
		}
	}
}

func TestManagementTools_HaveDescriptions(t *testing.T) {
	for _, tool := range ManagementTools() {
		if tool.Description == "" {
			t.Errorf("ManagementTools tool %q has empty description", tool.Name)
		}
	}
}

func TestManagementTools_HaveValidParameters(t *testing.T) {
	for _, tool := range ManagementTools() {
		if tool.Parameters == nil {
			t.Errorf("ManagementTools tool %q has nil parameters", tool.Name)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.Parameters, &params); err != nil {
			t.Errorf("ManagementTools tool %q has invalid JSON parameters: %v", tool.Name, err)
			continue
		}
		if params["type"] != "object" {
			t.Errorf("ManagementTools tool %q parameters type=%v, want object", tool.Name, params["type"])
		}
	}
}

func TestManagementTools_Idempotent(t *testing.T) {
	tools1 := ManagementTools()
	tools2 := ManagementTools()

	if len(tools1) != len(tools2) {
		t.Fatalf("ManagementTools() not idempotent: %d vs %d", len(tools1), len(tools2))
	}
	for i := range tools1 {
		if tools1[i].Name != tools2[i].Name {
			t.Errorf("ManagementTools()[%d].Name = %q, want %q", i, tools2[i].Name, tools1[i].Name)
		}
	}
}

// TestManagementTools_CoversAllSignals verifies that ManagementTools includes all
// CRUD trigger signals for agents, tools, and sessions.
func TestManagementTools_CoversAllSignals(t *testing.T) {
	tools := ManagementTools()
	nameSet := make(map[string]bool, len(tools))
	for _, tool := range tools {
		nameSet[tool.Name] = true
	}

	// All required management signals
	signals := []struct {
		name   string
		action string
		target string
	}{
		{"create_agent", "create", "agent"},
		{"update_agent", "update", "agent"},
		{"delete_agent", "delete", "agent"},
		{"create_tool", "create", "tool"},
		{"delete_tool", "delete", "tool"},
		{"delete_session", "delete", "session"},
	}

	for _, sig := range signals {
		if !nameSet[sig.name] {
			t.Errorf("ManagementTools() missing %s signal for %s: %q", sig.action, sig.target, sig.name)
		}
	}
}

// TestManagementTools_RequiredFields checks that tools with required parameters
// actually declare them in the schema.
func TestManagementTools_RequiredFields(t *testing.T) {
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

	tools := ManagementTools()
	toolMap := make(map[string]json.RawMessage, len(tools))
	for _, tool := range tools {
		toolMap[tool.Name] = tool.Parameters
	}

	for _, tt := range tests {
		t.Run(tt.toolName, func(t *testing.T) {
			params, ok := toolMap[tt.toolName]
			if !ok {
				t.Fatalf("tool %q not found", tt.toolName)
			}

			var schema map[string]any
			if err := json.Unmarshal(params, &schema); err != nil {
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

func TestCoreTools_NoOverlapWithManagementTools(t *testing.T) {
	coreNames := make(map[string]bool)
	for _, tool := range CoreTools() {
		coreNames[tool.Name] = true
	}

	for _, tool := range ManagementTools() {
		if coreNames[tool.Name] {
			t.Errorf("tool %q appears in both CoreTools and ManagementTools", tool.Name)
		}
	}
}
