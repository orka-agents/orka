package main

import (
	"fmt"
	"os"

	"github.com/orka-agents/orka/internal/acp"
)

func main() {
	// A successful exec replaces this process; any return is a launch failure.
	err := acp.RunExecHelper(os.Args[1:], os.Environ())
	_, _ = fmt.Fprintf(os.Stderr, "ACP exec helper failed: %v\n", err)
	os.Exit(126)
}
