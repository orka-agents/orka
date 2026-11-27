//go:build !unix

package main

import (
	"context"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

func watchTerminalResize(_ context.Context, _ chan<- operatoraccess.Control) func() { return func() {} }
