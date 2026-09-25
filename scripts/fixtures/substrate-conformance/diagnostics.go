package main

import (
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/workspace"
)

const maxCommandFailureBytes = 2048

func verifyCommandTimeout(cfg workspace.SubstrateConfig, result *workspace.ExecResult, err error) error {
	if result != nil && result.ExitCode == 124 && workspace.IsKind(err, workspace.ErrorKindCommandFailed) {
		return nil
	}
	return commandFailure("native command timeout did not return the daemon timeout result", cfg, result, err)
}

func commandFailure(stage string, cfg workspace.SubstrateConfig, result *workspace.ExecResult, err error) error {
	detail := fmt.Sprintf("kind=%s error=%v", workspace.KindOf(err), err)
	if result == nil {
		detail += " result=nil"
	} else {
		detail += fmt.Sprintf(" exit=%d stdoutBytes=%d stderr=%s", result.ExitCode, len(result.Stdout), result.Stderr)
	}
	for _, secret := range []string{cfg.HandoffToken, cfg.BootstrapToken} {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
	}
	detail = redact.SensitiveText(detail)
	detail = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(detail)
	// Redact before truncating so a boundary cannot expose a credential prefix.
	if len(detail) > maxCommandFailureBytes {
		detail = detail[:maxCommandFailureBytes] + " (truncated)"
	}
	return fmt.Errorf("%s: %s", stage, detail)
}
