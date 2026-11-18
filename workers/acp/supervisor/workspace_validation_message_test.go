/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

// The validation message is forwarded into controller structured logs, so an
// agent-controlled path - which can be named after a credential - must never
// appear in it; only the operation and safety category may.
func TestBoundedWorkspaceValidationMessageRedactsPaths(t *testing.T) {
	t.Parallel()
	secretPath := "config/ghp_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX.token"
	buildErr := &workspacedelta.PathError{Op: "scan workspace entry", Path: secretPath, Err: workspacedelta.ErrUnsafeFileType}
	message := boundedWorkspaceValidationMessage(buildErr)
	if strings.Contains(message, secretPath) || strings.Contains(message, "ghp_") {
		t.Fatalf("message leaks the workspace path: %q", message)
	}
	if !strings.Contains(message, "scan workspace entry") || !strings.Contains(message, workspacedelta.ErrUnsafeFileType.Error()) {
		t.Fatalf("message lost its diagnosable category: %q", message)
	}

	wrapped := fmt.Errorf("delta build: %w", buildErr)
	if got := boundedWorkspaceValidationMessage(wrapped); strings.Contains(got, secretPath) {
		t.Fatalf("wrapped message leaks the workspace path: %q", got)
	}

	if got := boundedWorkspaceValidationMessage(errors.New("open /workspace/" + secretPath + ": permission denied")); strings.Contains(got, secretPath) {
		t.Fatalf("uncategorized message must stay generic, got %q", got)
	}
	if got := boundedWorkspaceValidationMessage(nil); got != "workspace validation failed" {
		t.Fatalf("nil error message = %q", got)
	}
}
