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

func TestListToolsTool_Name(t *testing.T) {
	tool := &ListToolsTool{}
	if got := tool.Name(); got != listToolsToolName {
		t.Errorf("Name() = %q, want %q", got, listToolsToolName)
	}
}

func TestListToolsTool_Description(t *testing.T) {
	tool := &ListToolsTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestListToolsTool_Parameters(t *testing.T) {
	tool := &ListToolsTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props[namespaceField]; !ok {
		t.Error("missing namespace property")
	}
}

func TestListToolsTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		tools      []*corev1alpha1.Tool
		wantCount  int
		wantErrStr string
	}{
		{
			name:      "empty list",
			args:      `{}`,
			tools:     nil,
			wantCount: 0,
		},
		{
			name: "single tool",
			args: `{}`,
			tools: []*corev1alpha1.Tool{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testMyToolName,
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "A custom tool for testing",
					},
				},
			},
			wantCount: 1,
		},
		{
			name: "multiple tools",
			args: `{}`,
			tools: []*corev1alpha1.Tool{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tool-1",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "First tool",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "tool-2",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.ToolSpec{
						Description: testSecondToolDescription,
					},
				},
			},
			wantCount: 2,
		},
		{
			name: testCustomNamespaceCaseName,
			args: `{"namespace": "staging"}`,
			tools: []*corev1alpha1.Tool{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "staging-tool",
						Namespace: "staging",
					},
					Spec: corev1alpha1.ToolSpec{
						Description: "Staging tool",
					},
				},
			},
			wantCount: 1,
		},
		{
			name:       invalidJSONArgsCaseName,
			args:       invalidJSONText,
			wantErrStr: failedToParseArgumentsMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolUnderTest := &ListToolsTool{}

			fc := newFakeClient()
			if len(tt.tools) > 0 {
				fc = newFakeClientWithTools(tt.tools)
			}

			tc := &ToolContext{
				Client:    fc,
				Namespace: defaultNamespace,
			}
			ctx := WithToolContext(context.Background(), tc)

			result, err := toolUnderTest.Execute(ctx, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantErrStr != "" {
				if !strings.Contains(result, tt.wantErrStr) {
					t.Errorf("result = %q, want to contain %q", result, tt.wantErrStr)
				}
				return
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Success {
				t.Errorf("expected success=true, got false; result: %s", result)
			}

			data, ok := res.Data.([]any)
			if !ok {
				t.Fatalf("expected data array, got %T", res.Data)
			}
			if len(data) != tt.wantCount {
				t.Errorf("got %d tools, want %d", len(data), tt.wantCount)
			}

			// Verify specific fields for single tool test
			if tt.name == "single tool" && len(data) > 0 {
				toolData := data[0].(map[string]any)
				if toolData[nameField] != testMyToolName {
					t.Errorf("tool name = %q, want %q", toolData[nameField], testMyToolName)
				}
				if toolData[jsonSchemaDescriptionField] != "A custom tool for testing" {
					t.Errorf("tool description = %q, want %q", toolData[jsonSchemaDescriptionField], "A custom tool for testing")
				}
				if toolData["builtin"] != false {
					t.Errorf("tool builtin = %v, want false", toolData["builtin"])
				}
			}
		})
	}
}

func TestListToolsTool_Execute_MissingToolContext(t *testing.T) {
	tool := &ListToolsTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "missing tool context") {
		t.Errorf("expected missing tool context error, got %q", result)
	}
}
