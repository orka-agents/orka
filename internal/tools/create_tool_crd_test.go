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

func TestCreateToolCRDTool_Name(t *testing.T) {
	tool := &CreateToolCRDTool{}
	if got := tool.Name(); got != "create_tool" {
		t.Errorf("Name() = %v, want %v", got, "create_tool")
	}
}

func TestCreateToolCRDTool_Description(t *testing.T) {
	tool := &CreateToolCRDTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCreateToolCRDTool_Parameters(t *testing.T) {
	tool := &CreateToolCRDTool{}
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
	for _, key := range []string{"name", "namespace", "description", "url", "method"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestCreateToolCRDTool_Execute(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		wantErr bool
		errType string
	}{
		{
			name: "success - create tool",
			args: map[string]any{
				"name":        "my-tool",
				"description": "A test tool",
				"url":         "http://example.com/api",
			},
		},
		{
			name: "success - custom method",
			args: map[string]any{
				"name":        "get-tool",
				"description": "A GET tool",
				"url":         "http://example.com/get",
				"method":      "GET",
			},
		},
		{
			name: "missing name",
			args: map[string]any{
				"description": "A test tool",
				"url":         "http://example.com",
			},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
		{
			name: "missing description",
			args: map[string]any{
				"name": "my-tool",
				"url":  "http://example.com",
			},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
		{
			name: "missing url",
			args: map[string]any{
				"name":        "my-tool",
				"description": "A test tool",
			},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient()
			tc := &ToolContext{Client: fc, Namespace: "default"}
			ctx := WithToolContext(context.Background(), tc)

			argsJSON, _ := json.Marshal(tt.args)
			tool := &CreateToolCRDTool{}
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
			if data["message"] != "Tool created" {
				t.Errorf("expected message 'Tool created', got %v", data["message"])
			}
		})
	}
}

func TestCreateToolCRDTool_Execute_VerifyCreated(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{
		"name":        "my-tool",
		"description": "A test tool",
		"url":         "http://example.com/api",
		"method":      "GET",
	}
	argsJSON, _ := json.Marshal(args)

	tool := &CreateToolCRDTool{}
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

	// Verify the tool CRD was actually created
	created := &corev1alpha1.Tool{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: "my-tool", Namespace: "default"}, created); err != nil {
		t.Fatalf("failed to get created tool: %v", err)
	}
	if created.Spec.Description != "A test tool" {
		t.Errorf("description = %q, want %q", created.Spec.Description, "A test tool")
	}
	if created.Spec.HTTP.URL != "http://example.com/api" {
		t.Errorf("url = %q, want %q", created.Spec.HTTP.URL, "http://example.com/api")
	}
	if created.Spec.HTTP.Method != "GET" {
		t.Errorf("method = %q, want %q", created.Spec.HTTP.Method, "GET")
	}
}

func TestCreateToolCRDTool_Execute_DefaultMethod(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{
		"name":        "my-tool",
		"description": "A test tool",
		"url":         "http://example.com/api",
	}
	argsJSON, _ := json.Marshal(args)

	tool := &CreateToolCRDTool{}
	if _, err := tool.Execute(ctx, argsJSON); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	created := &corev1alpha1.Tool{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: "my-tool", Namespace: "default"}, created); err != nil {
		t.Fatalf("failed to get created tool: %v", err)
	}
	if created.Spec.HTTP.Method != "POST" {
		t.Errorf("method = %q, want %q (default)", created.Spec.HTTP.Method, "POST")
	}
}

func TestCreateToolCRDTool_Execute_AlreadyExists(t *testing.T) {
	existing := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-tool",
			Namespace: "default",
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "existing",
			HTTP:        corev1alpha1.HTTPExecution{URL: "http://old.com", Method: "POST"},
		},
	}
	fc := newFakeClient(existing)
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{
		"name":        "existing-tool",
		"description": "new tool",
		"url":         "http://new.com",
	}
	argsJSON, _ := json.Marshal(args)

	tool := &CreateToolCRDTool{}
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Success {
		t.Error("expected failure for already existing tool")
	}
	if res.ErrorType != "already_exists" {
		t.Errorf("expected errorType 'already_exists', got %q", res.ErrorType)
	}
}

func TestCreateToolCRDTool_Execute_NamespaceIsolation(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{
		Client:                    fc,
		Namespace:                 "allowed-ns",
		EnforceNamespaceIsolation: true,
	}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{
		"name":        "my-tool",
		"description": "A tool",
		"url":         "http://example.com",
		"namespace":   "other-ns",
	}
	argsJSON, _ := json.Marshal(args)

	tool := &CreateToolCRDTool{}
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Success {
		t.Error("expected failure for namespace isolation violation")
	}
	if res.ErrorType != "permission_denied" {
		t.Errorf("expected errorType 'permission_denied', got %q", res.ErrorType)
	}
}

func TestCreateToolCRDTool_Execute_MissingToolContext(t *testing.T) {
	tool := &CreateToolCRDTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"x","description":"d","url":"u"}`))
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

func TestCreateToolCRDTool_Execute_InvalidJSON(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: "default"}
	ctx := WithToolContext(context.Background(), tc)

	tool := &CreateToolCRDTool{}
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
