//go:build !linux

package acp

import (
	"fmt"
	"os"
)

// OperatorShimSubcommand is the hidden orka-acp-runtime operator subcommand.
const OperatorShimSubcommand = "__orka_operator_exec"

// RunOperatorShim is only supported on Linux.
func RunOperatorShim(_ []string) int {
	_, _ = fmt.Fprintln(os.Stderr, "operator shim is only supported on Linux")
	return 126
}

// KillOperatorTree falls back to the process group outside Linux.
func KillOperatorTree(leader, _ int) {
	if leader > 0 {
		_ = signalProcessGroup(leader, killSignal())
	}
}
