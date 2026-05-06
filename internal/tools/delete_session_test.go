/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// mockSessionDeleter implements the SessionDeleter interface for testing.
type mockSessionDeleter struct {
	deletedNamespace string
	deletedSessionID string
	err              error
}

func (m *mockSessionDeleter) DeleteSession(_ context.Context, namespace, sessionID string) error {
	m.deletedNamespace = namespace
	m.deletedSessionID = sessionID
	return m.err
}

func TestDeleteSessionTool_Name(t *testing.T) {
	tool := &DeleteSessionTool{}
	if got := tool.Name(); got != deleteSessionToolName {
		t.Errorf("Name() = %v, want %v", got, deleteSessionToolName)
	}
}

func TestDeleteSessionTool_Description(t *testing.T) {
	tool := &DeleteSessionTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestDeleteSessionTool_Parameters(t *testing.T) {
	tool := &DeleteSessionTool{}
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
	for _, key := range []string{sessionIDField, namespaceField} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestDeleteSessionTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		deleter    *mockSessionDeleter
		nilDeleter bool
		wantErr    bool
		errType    string
	}{
		{
			name:    "success - session deleted",
			args:    map[string]any{sessionIDField: testSessionID},
			deleter: &mockSessionDeleter{},
		},
		{
			name:    "success - explicit namespace",
			args:    map[string]any{sessionIDField: "sess-456", namespaceField: testProdNamespace},
			deleter: &mockSessionDeleter{},
		},
		{
			name:    "missing sessionId",
			args:    map[string]any{},
			deleter: &mockSessionDeleter{},
			wantErr: true,
			errType: errTypeInvalidArgs,
		},
		{
			name:       "session manager not configured",
			args:       map[string]any{sessionIDField: testSessionID},
			nilDeleter: true,
			wantErr:    true,
			errType:    internalErrorType,
		},
		{
			name:    "delete error",
			args:    map[string]any{sessionIDField: testSessionID},
			deleter: &mockSessionDeleter{err: fmt.Errorf("delete failed")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient()
			tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
			if !tt.nilDeleter {
				tc.SessionDeleter = tt.deleter
			}
			ctx := WithToolContext(context.Background(), tc)

			argsJSON, _ := json.Marshal(tt.args)
			tool := &DeleteSessionTool{}
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
			if data[messageField] != "Session deleted" {
				t.Errorf("expected message 'Session deleted', got %v", data[messageField])
			}

			// Verify the deleter was called with correct args
			if tt.deleter != nil {
				expectedNS := defaultNamespace
				if ns, ok := tt.args[namespaceField].(string); ok && ns != "" {
					expectedNS = ns
				}
				if tt.deleter.deletedNamespace != expectedNS {
					t.Errorf("deleter namespace = %q, want %q", tt.deleter.deletedNamespace, expectedNS)
				}
				if tt.deleter.deletedSessionID != tt.args[sessionIDField] {
					t.Errorf("deleter sessionID = %q, want %q", tt.deleter.deletedSessionID, tt.args[sessionIDField])
				}
			}
		})
	}
}

func TestDeleteSessionTool_Execute_MissingToolContext(t *testing.T) {
	tool := &DeleteSessionTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"sessionId":"x"}`))
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

func TestDeleteSessionTool_Execute_InvalidJSON(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	tool := &DeleteSessionTool{}
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
