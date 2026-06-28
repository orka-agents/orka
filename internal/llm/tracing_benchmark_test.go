/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package llm

import (
	"context"
	"testing"
)

func BenchmarkTracingProviderCompleteTelemetryDisabled(b *testing.B) {
	provider := NewTracingProvider(&mockProvider{name: "bench"})
	req := &CompletionRequest{Model: "bench-model", Messages: []Message{{Role: "user", Content: "hello"}}}
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		if _, err := provider.Complete(ctx, req); err != nil {
			b.Fatalf("Complete() error = %v", err)
		}
	}
}
