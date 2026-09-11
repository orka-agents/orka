package main

import (
	"fmt"
	"os"

	"github.com/orka-agents/orka/internal/acp"
)

func main() {
	if err := acp.RunExecHelper(os.Args[1:], os.Environ()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ACP exec helper failed: %v\n", err)
		os.Exit(126)
	}
}
