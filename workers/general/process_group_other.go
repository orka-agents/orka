//go:build !unix

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"os/exec"
)

// runCommand kills only the command itself when ctx ends.
func runCommand(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	stop()
	return err
}
