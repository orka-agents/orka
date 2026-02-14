/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"encoding/json"
	"testing"
)

func TestUpdatePlanTool_Name(t *testing.T) {
	tool := NewUpdatePlanTool()
	if got := tool.Name(); got != "update_plan" {
		t.Errorf("Name() = %q, want %q", got, "update_plan")
	}
}

func TestUpdatePlanTool_Description(t *testing.T) {
	tool := NewUpdatePlanTool()
	if got := tool.Description(); got == "" {
		t.Error("Description() should not be empty")
	}
}

func TestUpdatePlanTool_Parameters(t *testing.T) {
	tool := NewUpdatePlanTool()
	params := tool.Parameters()
	if len(params) == 0 {
		t.Fatal("Parameters() should not be empty")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() should be valid JSON: %v", err)
	}

	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema should have properties")
	}

	for _, required := range []string{"summary", "progress_pct", "goal_complete", "plan_document"} {
		if _, ok := props[required]; !ok {
			t.Errorf("schema missing property %q", required)
		}
	}
}

func TestUpdatePlanTool_Execute_MissingEnvVars(t *testing.T) {
	tool := NewUpdatePlanTool()
	args := json.RawMessage(`{
		"summary": "test",
		"progress_pct": 50,
		"goal_complete": false,
		"plan_document": "# Test Plan"
	}`)

	// Without ORKA_CONTROLLER_URL set, should fail
	t.Setenv("ORKA_CONTROLLER_URL", "")
	t.Setenv("ORKA_TASK_NAME", "")
	t.Setenv("ORKA_TASK_NAMESPACE", "")

	_, err := tool.Execute(t.Context(), args)
	if err == nil {
		t.Error("Execute should fail without env vars")
	}
}

func TestUpdatePlanTool_Execute_InvalidArgs(t *testing.T) {
	tool := NewUpdatePlanTool()

	t.Run("empty summary", func(t *testing.T) {
		args := json.RawMessage(`{"summary": "", "plan_document": "# Plan"}`)
		_, err := tool.Execute(t.Context(), args)
		if err == nil {
			t.Error("should fail with empty summary")
		}
	})

	t.Run("empty plan_document", func(t *testing.T) {
		args := json.RawMessage(`{"summary": "test", "plan_document": ""}`)
		_, err := tool.Execute(t.Context(), args)
		if err == nil {
			t.Error("should fail with empty plan_document")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		args := json.RawMessage(`{invalid}`)
		_, err := tool.Execute(t.Context(), args)
		if err == nil {
			t.Error("should fail with invalid JSON")
		}
	})
}
