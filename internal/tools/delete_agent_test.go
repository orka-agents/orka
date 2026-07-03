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

func testAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testAgentName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{},
	}
}

func TestDeleteAgentTool_Name(t *testing.T) {
	tool := NewDeleteAgentTool(newFakeClient())
	if tool.Name() != deleteAgentToolName {
		t.Errorf("expected name 'delete_agent', got '%s'", tool.Name())
	}
}

func TestDeleteAgentTool_Description(t *testing.T) {
	tool := NewDeleteAgentTool(newFakeClient())
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
	if !strings.Contains(desc, "Delete") {
		t.Errorf("unexpected description: %s", desc)
	}
}

func TestDeleteAgentTool_Parameters(t *testing.T) {
	tool := NewDeleteAgentTool(newFakeClient())
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props[nameField]; !ok {
		t.Error("missing name property")
	}
	if _, ok := props[namespaceField]; !ok {
		t.Error("missing namespace property")
	}
}

func TestDeleteAgentTool_Execute_DefaultNamespace(t *testing.T) {
	// Test that without env var and without explicit namespace, default namespace is used
	t.Setenv(envOrkaTaskNamespace, "")
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testAgentName,
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.AgentSpec{},
	}
	tool := NewDeleteAgentTool(newFakeClient(agent))
	argsJSON, _ := json.Marshal(DeleteAgentArgs{Name: testAgentName})
	result, err := tool.Execute(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res DeleteAgentResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if res.Status != deletedStatusString {
		t.Errorf("expected status 'deleted', got %q", res.Status)
	}
}

func TestDeleteAgentTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewDeleteAgentTool(newFakeClient())
	_, err := tool.Execute(context.Background(), json.RawMessage(invalidJSONText))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDeleteAgentTool_Execute(t *testing.T) {
	tests := []struct {
		name      string
		args      DeleteAgentArgs
		setup     func() *DeleteAgentTool
		envNS     string
		wantErr   bool
		errSubstr string
	}{
		{
			name: "success - agent exists and is deleted",
			args: DeleteAgentArgs{Name: testAgentName, Namespace: defaultNamespace},
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient(testAgent()))
			},
		},
		{
			name: "error - name is empty",
			args: DeleteAgentArgs{Name: ""},
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient())
			},
			wantErr:   true,
			errSubstr: "agent name is required",
		},
		{
			name: "error - agent not found",
			args: DeleteAgentArgs{Name: testNonexistentName, Namespace: defaultNamespace},
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient())
			},
			wantErr:   true,
			errSubstr: "failed to get agent",
		},
		{
			name:  "success - namespace from env var",
			args:  DeleteAgentArgs{Name: testAgentName},
			envNS: defaultNamespace,
			setup: func() *DeleteAgentTool {
				return NewDeleteAgentTool(newFakeClient(testAgent()))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envNS != "" {
				t.Setenv(envOrkaTaskNamespace, tt.envNS)
			}

			tool := tt.setup()
			argsJSON, _ := json.Marshal(tt.args)
			result, err := tool.Execute(context.Background(), argsJSON)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("expected error containing %q, got %q", tt.errSubstr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res DeleteAgentResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}
			if res.Name != tt.args.Name {
				t.Errorf("expected name %q, got %q", tt.args.Name, res.Name)
			}
			if res.Status != deletedStatusString {
				t.Errorf("expected status 'deleted', got %q", res.Status)
			}
		})
	}
}
