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

// runCommand kills only the command itself when ctx ends. Without a signal
// status, a command still running when ctx ended counts as stopped by it.
func runCommand(ctx context.Context, cmd *exec.Cmd) (stopped bool, err error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	err = cmd.Wait()
	return !stop() && err != nil, err
}
