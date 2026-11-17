//go:build unix

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

// watchTerminalResize forwards local window size changes to the remote
// terminal until stopped.
func watchTerminalResize(ctx context.Context, controls chan<- operatoraccess.Control) func() {
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-changes:
				cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					continue
				}
				select {
				case controls <- operatoraccess.Control{Type: operatoraccess.ControlResize, Rows: clampDimension(rows), Cols: clampDimension(cols)}:
				default:
				}
			}
		}
	}()
	return func() {
		signal.Stop(changes)
		close(done)
	}
}
