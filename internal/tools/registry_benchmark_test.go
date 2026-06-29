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

func BenchmarkRegistryExecuteTelemetryDisabled(b *testing.B) {
	registry := NewRegistry()
	registry.Register(tracingTestTool{})
	ctx := WithToolContext(context.Background(), &ToolContext{TaskID: "task-a", Namespace: "default", Tenant: "default", ToolCallID: "call-a"})
	args := json.RawMessage(`{}`)
	b.ReportAllocs()

	for b.Loop() {
		if _, err := registry.Execute(ctx, tracingToolName, args); err != nil {
			b.Fatalf("Execute() error = %v", err)
		}
	}
}
