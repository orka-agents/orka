/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"slices"
	"testing"
)

func TestRegisterBrokeredWebToolsIsIdempotentAndBounded(t *testing.T) {
	registry := NewRegistry()

	if err := RegisterBrokeredWebTools(registry); err != nil {
		t.Fatalf("RegisterBrokeredWebTools() error = %v", err)
	}
	first := registry.Names()
	if err := RegisterBrokeredWebTools(registry); err != nil {
		t.Fatalf("second RegisterBrokeredWebTools() error = %v", err)
	}
	if got := registry.Names(); !slices.Equal(got, first) {
		t.Fatalf("second registration changed names: first=%v second=%v", first, got)
	}

	want := []string{webFetchToolName, webSearchToolName}
	slices.Sort(want)
	if !slices.Equal(first, want) {
		t.Fatalf("registered broker web tools = %v, want %v", first, want)
	}
	if tool, ok := registry.Get(webSearchToolName); !ok {
		t.Fatalf("broker web registry is missing %q", webSearchToolName)
	} else if _, ok := tool.(*WebSearchTool); !ok {
		t.Fatalf("registered %q implementation = %T, want *WebSearchTool", webSearchToolName, tool)
	}
	if tool, ok := registry.Get(webFetchToolName); !ok {
		t.Fatalf("broker web registry is missing %q", webFetchToolName)
	} else if _, ok := tool.(*WebFetchTool); !ok {
		t.Fatalf("registered %q implementation = %T, want *WebFetchTool", webFetchToolName, tool)
	}
}

func TestRegisterBrokeredWebToolsRequiresRegistry(t *testing.T) {
	if err := RegisterBrokeredWebTools(nil); err == nil {
		t.Fatal("RegisterBrokeredWebTools(nil) expected error")
	}
}
