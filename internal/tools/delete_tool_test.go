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

func TestDeleteToolTool_Name(t *testing.T) {
	tool := &DeleteToolTool{}
	if got := tool.Name(); got != "delete_tool" {
		t.Errorf("Name() = %v, want %v", got, "delete_tool")
	}
}

func TestDeleteToolTool_Description(t *testing.T) {
	tool := &DeleteToolTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestDeleteToolTool_Parameters(t *testing.T) {
	tool := &DeleteToolTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{"name", "namespace"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestDeleteToolTool_Execute(t *testing.T) {
	existingTool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tool",
			Namespace: "default",
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "test tool",
			HTTP: corev1alpha1.HTTPExecution{
				URL:    "http://example.com",
				Method: "POST",
			},
		},
	}

	tests := []struct {
		name    string
		args    map[string]any
		objs    []metav1.Object
		wantErr bool
		errType string
	}{
		{
			name: "success - tool deleted",
			args: map[string]any{"name": "my-tool"},
		},
		{
			name:    "missing name",
			args:    map[string]any{},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
		{
			name:    "tool not found",
			args:    map[string]any{"name": "nonexistent"},
			wantErr: true,
			errType: "not_found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient()
			if tt.name == "success - tool deleted" {
				fc = newFakeClient(existingTool.DeepCopy())
			}
			tc := &ToolContext{Client: fc, Namespace: "default"}
			ctx := WithToolContext(context.Background(), tc)

			argsJSON, _ := json.Marshal(tt.args)
			tool := &DeleteToolTool{}
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
				if tt.errType != "" && res.ErrorType != tt.errType {
					t.Errorf("expected errorType %q, got %q", tt.errType, res.ErrorType)
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
			if data["name"] != "my-tool" {
				t.Errorf("expected name 'my-tool', got %v", data["name"])
			}
			if data["message"] != "Tool deleted" {
				t.Errorf("expected message 'Tool deleted', got %v", data["message"])
			}

			// Verify the tool is actually deleted
			deleted := &corev1alpha1.Tool{}
			err = fc.Get(context.Background(), apitypes.NamespacedName{Name: "my-tool", Namespace: "default"}, deleted)
			if err == nil {
				t.Error("expected tool to be deleted, but it still exists")
			}
		})
	}
}

func TestDeleteToolTool_Execute_MissingToolContext(t *testing.T) {
	tool := &DeleteToolTool{}
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

func TestDeleteToolTool_Execute_InvalidJSON(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	tool := &DeleteToolTool{}
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
	if res.ErrorType != errTypeInvalidArgs {
		t.Errorf("expected errorType 'invalid_arguments', got %q", res.ErrorType)
	}
}
