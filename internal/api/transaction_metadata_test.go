/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"
	"testing"
)

func TestSafeTransactionContextOmitsLongAllowlistedValues(t *testing.T) {
	ctx := map[string]any{
		"purpose":  strings.Repeat("x", maxSafeTransactionContextValueLength+1),
		"trace_id": "trace-123",
	}

	safe := safeTransactionContext(ctx)
	if _, ok := safe["purpose"]; ok {
		t.Fatalf("safeTransactionContext included overlong purpose value")
	}
	if got := safe["trace_id"]; got != "trace-123" {
		t.Fatalf("safeTransactionContext[trace_id] = %q, want trace-123", got)
	}
}

func TestSafeTransactionContextOmitsLongRenderedLists(t *testing.T) {
	ctx := map[string]any{
		"allowedTools": []string{strings.Repeat("x", maxSafeTransactionContextValueLength)},
	}

	if safe := safeTransactionContext(ctx); safe != nil {
		t.Fatalf("safeTransactionContext() = %#v, want nil for overlong rendered list", safe)
	}
}
