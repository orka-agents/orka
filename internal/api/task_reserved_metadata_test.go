/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"
	"testing"
)

// Client-submitted Task metadata must never claim controller-owned keys: the
// ACP workspace settlement marker and link label live under
// acp.workspace.orka.ai/ and forging them would skip controller-owned
// revocation and detach actions.
func TestRejectReservedTaskMetadataPrefixes(t *testing.T) {
	t.Parallel()
	const reservedValue = "reserved-value"
	for _, key := range []string{
		"orka.ai/requested-by",
		"acp.workspace.orka.ai/workspace-settled",
		"acp.workspace.orka.ai/execution-workspace",
	} {
		if err := rejectReservedTaskAnnotations(map[string]string{key: reservedValue}); err == nil ||
			!strings.Contains(err.Error(), "reserved") {
			t.Fatalf("annotation %q must be rejected as reserved, got %v", key, err)
		}
		if err := rejectReservedTaskLabels(map[string]string{key: reservedValue}); err == nil ||
			!strings.Contains(err.Error(), "reserved") {
			t.Fatalf("label %q must be rejected as reserved, got %v", key, err)
		}
	}
	if err := rejectReservedTaskAnnotations(map[string]string{"example.com/team": "core"}); err != nil {
		t.Fatalf("ordinary annotations must stay accepted: %v", err)
	}
}
