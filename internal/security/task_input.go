package security

import (
	"strings"

	"github.com/orka-agents/orka/internal/redact"
)

// NormalizeTaskInputSnapshot produces the exact bounded text used by both a
// persisted immutable Task-input record and the Task spec built from it.
func NormalizeTaskInputSnapshot(content string) string {
	return redact.SensitiveText(strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n")))
}
