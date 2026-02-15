/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// mockTool is a simple mock tool for testing
type mockTool struct {
	name        string
	description string
	parameters  json.RawMessage
	executeFunc func(ctx context.Context, args json.RawMessage) (string, error)
}

func (m *mockTool) Name() string {
	return m.name
}

func (m *mockTool) Description() string {
	return m.description
}

func (m *mockTool) Parameters() json.RawMessage {
	return m.parameters
}

func (m *mockTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, args)
	}
	return "executed", nil
}

func TestNewRegistry(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("NewRegistry returned nil")
	}
	if r.tools == nil {
		t.Fatal("tools map is nil")
	}
	if len(r.tools) != 0 {
		t.Errorf("expected empty registry, got %d tools", len(r.tools))
	}
}

func TestRegistry_Register(t *testing.T) {
	r := NewRegistry()
	tool := &mockTool{name: "test_tool", description: "A test tool"}

	r.Register(tool)

	if len(r.tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(r.tools))
	}
	if _, ok := r.tools["test_tool"]; !ok {
		t.Error("tool not registered with correct name")
	}
}

func TestRegistry_Register_Overwrite(t *testing.T) {
	r := NewRegistry()
	tool1 := &mockTool{name: "test_tool", description: "First tool"}
	tool2 := &mockTool{name: "test_tool", description: "Second tool"}

	r.Register(tool1)
	r.Register(tool2)

	if len(r.tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(r.tools))
	}
	if r.tools["test_tool"].Description() != "Second tool" {
		t.Error("tool was not overwritten")
	}
}

func TestRegistry_Get(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		wantFound bool
	}{
		{
			name:      "found",
			toolName:  "test_tool",
			wantFound: true,
		},
		{
			name:      "not found",
			toolName:  "nonexistent",
			wantFound: false,
		},
	}

	r := NewRegistry()
	r.Register(&mockTool{name: "test_tool", description: "A test tool"})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, ok := r.Get(tt.toolName)
			if ok != tt.wantFound {
				t.Errorf("Get() found = %v, want %v", ok, tt.wantFound)
			}
			if tt.wantFound && tool == nil {
				t.Error("Get() returned nil tool when found")
			}
			if !tt.wantFound && tool != nil {
				t.Error("Get() returned non-nil tool when not found")
			}
		})
	}
}

func TestRegistry_List(t *testing.T) {
	tests := []struct {
		name    string
		tools   []Tool
		wantLen int
	}{
		{
			name:    "empty registry",
			tools:   nil,
			wantLen: 0,
		},
		{
			name: "one tool",
			tools: []Tool{
				&mockTool{name: "tool1"},
			},
			wantLen: 1,
		},
		{
			name: "multiple tools",
			tools: []Tool{
				&mockTool{name: "tool1"},
				&mockTool{name: "tool2"},
				&mockTool{name: "tool3"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			for _, tool := range tt.tools {
				r.Register(tool)
			}

			list := r.List()
			if len(list) != tt.wantLen {
				t.Errorf("List() len = %d, want %d", len(list), tt.wantLen)
			}
		})
	}
}

func TestRegistry_Execute(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		args     json.RawMessage
		wantErr  bool
		wantResp string
	}{
		{
			name:     "tool found",
			toolName: "test_tool",
			args:     json.RawMessage(`{"key": "value"}`),
			wantErr:  false,
			wantResp: "executed",
		},
		{
			name:     "tool not found",
			toolName: "nonexistent",
			args:     json.RawMessage(`{}`),
			wantErr:  true,
		},
	}

	r := NewRegistry()
	r.Register(&mockTool{name: "test_tool"})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := r.Execute(context.Background(), tt.toolName, tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && result != tt.wantResp {
				t.Errorf("Execute() = %v, want %v", result, tt.wantResp)
			}
		})
	}
}

func TestRegistry_ToLLMTools(t *testing.T) {
	tests := []struct {
		name    string
		tools   []Tool
		names   []string
		wantLen int
	}{
		{
			name: "all tools exist",
			tools: []Tool{
				&mockTool{name: "tool1", description: "Tool 1", parameters: json.RawMessage(`{"type": "object"}`)},
				&mockTool{name: "tool2", description: "Tool 2", parameters: json.RawMessage(`{"type": "object"}`)},
			},
			names:   []string{"tool1", "tool2"},
			wantLen: 2,
		},
		{
			name: "some tools don't exist",
			tools: []Tool{
				&mockTool{name: "tool1", description: "Tool 1", parameters: json.RawMessage(`{"type": "object"}`)},
			},
			names:   []string{"tool1", "tool2", "tool3"},
			wantLen: 1,
		},
		{
			name: "no tools exist",
			tools: []Tool{
				&mockTool{name: "tool1", description: "Tool 1", parameters: json.RawMessage(`{"type": "object"}`)},
			},
			names:   []string{"nonexistent1", "nonexistent2"},
			wantLen: 0,
		},
		{
			name:    "empty names",
			tools:   []Tool{&mockTool{name: "tool1"}},
			names:   []string{},
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			for _, tool := range tt.tools {
				r.Register(tool)
			}

			llmTools := r.ToLLMTools(tt.names)
			if len(llmTools) != tt.wantLen {
				t.Errorf("ToLLMTools() len = %d, want %d", len(llmTools), tt.wantLen)
			}
		})
	}
}

func TestDefaultRegistry(t *testing.T) {
	// DefaultRegistry should have built-in tools registered via init()
	if DefaultRegistry == nil {
		t.Fatal("DefaultRegistry is nil")
	}

	// Check that built-in tools are registered
	expectedTools := []string{"web_search", "code_exec", "file_read", "web_fetch", "file_write"}
	for _, name := range expectedTools {
		if _, ok := DefaultRegistry.Get(name); !ok {
			t.Errorf("expected built-in tool %q to be registered", name)
		}
	}
}
