//go:build !unix

package main

import (
	"context"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

// watchTerminalResize is unavailable without SIGWINCH.
func watchTerminalResize(_ context.Context, _ chan<- operatoraccess.Control) func() { return func() {} }
