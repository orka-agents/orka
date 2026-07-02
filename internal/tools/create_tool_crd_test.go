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

func TestCreateToolCRDTool_Name(t *testing.T) {
	tool := &CreateToolCRDTool{}
	if got := tool.Name(); got != createToolCRDToolName {
		t.Errorf("Name() = %v, want %v", got, createToolCRDToolName)
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
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{nameField, namespaceField, jsonSchemaDescriptionField, urlField, methodField} {
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
			args: map[string]any{nameField: testMyToolName, jsonSchemaDescriptionField: testToolDescription, urlField: exampleAPIURL},
		},
		{
			name: "success - custom method",
			args: map[string]any{nameField: "get-tool", jsonSchemaDescriptionField: "A GET tool", urlField: "http://example.com/get", methodField: httpMethodGetString},
		},
		{
			name: missingNameCaseName,
			args: map[string]any{
				jsonSchemaDescriptionField: testToolDescription, urlField: exampleDotComURL,
			},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
		{
			name:    "missing description",
			args:    map[string]any{nameField: testMyToolName, urlField: exampleDotComURL},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
		{
			name:    "missing url",
			args:    map[string]any{nameField: testMyToolName, jsonSchemaDescriptionField: testToolDescription},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient()
			tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
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
			if data[messageField] != "Tool created" {
				t.Errorf("expected message 'Tool created', got %v", data[messageField])
			}
		})
	}
}

func TestCreateToolCRDTool_Execute_VerifyCreated(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{nameField: testMyToolName, jsonSchemaDescriptionField: testToolDescription, urlField: exampleAPIURL, methodField: httpMethodGetString}
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
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyToolName, Namespace: defaultNamespace}, created); err != nil {
		t.Fatalf("failed to get created tool: %v", err)
	}
	if created.Spec.Description != testToolDescription {
		t.Errorf("description = %q, want %q", created.Spec.Description, testToolDescription)
	}
	if created.Spec.HTTP.URL != exampleAPIURL {
		t.Errorf("url = %q, want %q", created.Spec.HTTP.URL, exampleAPIURL)
	}
	if created.Spec.HTTP.Method != httpMethodGetString {
		t.Errorf("method = %q, want %q", created.Spec.HTTP.Method, httpMethodGetString)
	}
}

func TestCreateToolCRDTool_Execute_DefaultMethod(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{nameField: testMyToolName, jsonSchemaDescriptionField: testToolDescription, urlField: exampleAPIURL}
	argsJSON, _ := json.Marshal(args)

	tool := &CreateToolCRDTool{}
	if _, err := tool.Execute(ctx, argsJSON); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	created := &corev1alpha1.Tool{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testMyToolName, Namespace: defaultNamespace}, created); err != nil {
		t.Fatalf("failed to get created tool: %v", err)
	}
	if created.Spec.HTTP.Method != httpMethodPostString {
		t.Errorf("method = %q, want %q (default)", created.Spec.HTTP.Method, httpMethodPostString)
	}
}

func TestCreateToolCRDTool_Execute_AlreadyExists(t *testing.T) {
	existing := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-tool",
			Namespace: defaultNamespace,
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "existing",
			HTTP:        &corev1alpha1.HTTPExecution{URL: "http://old.com", Method: httpMethodPostString},
		},
	}
	fc := newFakeClient(existing)
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	args := map[string]any{nameField: "existing-tool", jsonSchemaDescriptionField: "new tool", urlField: "http://new.com"}
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

	args := map[string]any{nameField: testMyToolName, jsonSchemaDescriptionField: "A tool", urlField: exampleDotComURL, namespaceField: "other-ns"}
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
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	tool := &CreateToolCRDTool{}
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
	if res.ErrorType != errTypeInvalidArgs {
		t.Errorf("expected errorType 'invalid_arguments', got %q", res.ErrorType)
	}
}
