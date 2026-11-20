package main

import (
	"io"
	"os"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

func clientExecStreams(stdin io.Reader, controls <-chan operatoraccess.Control) client.OperatorExecStreams {
	return client.OperatorExecStreams{Stdin: stdin, Stdout: os.Stdout, Stderr: os.Stderr, Controls: controls}
}
