/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tracing

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestTaskAttributesOmitEmptyFieldsAndUseTenantFallback(t *testing.T) {
	attrs := TaskAttributes("task-a", "team-a", "", "agent-a", "")
	got := attrMap(attrs)

	if got[AttrTaskID].AsString() != "task-a" {
		t.Fatalf("%s = %q", AttrTaskID, got[AttrTaskID].AsString())
	}
	if got[AttrTaskNamespace].AsString() != "team-a" {
		t.Fatalf("%s = %q", AttrTaskNamespace, got[AttrTaskNamespace].AsString())
	}
	if got[AttrTenant].AsString() != "team-a" {
		t.Fatalf("%s = %q, want namespace fallback", AttrTenant, got[AttrTenant].AsString())
	}
	if got[AttrAgentName].AsString() != "agent-a" {
		t.Fatalf("%s = %q", AttrAgentName, got[AttrAgentName].AsString())
	}
	if _, ok := got[AttrUserSub]; ok {
		t.Fatalf("%s was emitted for empty user subject", AttrUserSub)
	}
}

func TestToolAttributesIncludeExactSafeKeys(t *testing.T) {
	attrs := ToolAttributes("delegate_task", ToolKindDelegate, 123, "invalid_arguments")
	got := attrMap(attrs)

	if got[AttrToolName].AsString() != "delegate_task" {
		t.Fatalf("%s = %q", AttrToolName, got[AttrToolName].AsString())
	}
	if got[AttrToolKind].AsString() != ToolKindDelegate {
		t.Fatalf("%s = %q", AttrToolKind, got[AttrToolKind].AsString())
	}
	if got[AttrToolResultSizeBytes].AsInt64() != 123 {
		t.Fatalf("%s = %d", AttrToolResultSizeBytes, got[AttrToolResultSizeBytes].AsInt64())
	}
	if got["error.type"].AsString() != "invalid_arguments" {
		t.Fatalf("error.type = %q", got["error.type"].AsString())
	}
}

func TestToolAttributesOmitUnknownResultSizeAndEmptyError(t *testing.T) {
	attrs := ToolAttributes("web_search", ToolKindBuiltin, -1, "")
	got := attrMap(attrs)
	if _, ok := got[AttrToolResultSizeBytes]; ok {
		t.Fatalf("%s was emitted for unknown result size", AttrToolResultSizeBytes)
	}
	if _, ok := got["error.type"]; ok {
		t.Fatal("error.type was emitted for empty error type")
	}
}

func TestDelegateAttributesOmitEmptyValues(t *testing.T) {
	attrs := DelegateAttributes("parent-a", "")
	got := attrMap(attrs)
	if got[AttrParentTaskID].AsString() != "parent-a" {
		t.Fatalf("%s = %q", AttrParentTaskID, got[AttrParentTaskID].AsString())
	}
	if _, ok := got[AttrChildTaskID]; ok {
		t.Fatalf("%s was emitted for empty child id", AttrChildTaskID)
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value
	}
	return out
}
