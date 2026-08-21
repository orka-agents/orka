package security

import (
	"strings"
	"testing"
)

func TestNormalizeTaskInputSnapshot(t *testing.T) {
	got := NormalizeTaskInputSnapshot("  line one\r\napi_key=" + strings.Repeat("x", 24) + "  ")
	if got != "line one\napi_key=[REDACTED]" {
		t.Fatalf("NormalizeTaskInputSnapshot() = %q", got)
	}
}
