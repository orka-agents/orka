/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

var forwardedSignals = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2,
}

func runInit(args []string) (int, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return 0, errors.New("usage: orka-host-agent init -- command [argument...]")
	}
	if os.Getpid() != 1 {
		if err := becomeSubreaper(); err != nil {
			return 0, fmt.Errorf("become a subreaper: %w", err)
		}
	}
	signals := make(chan os.Signal, 32)
	signal.Notify(signals, append([]os.Signal{syscall.SIGCHLD}, forwardedSignals...)...)
	defer signal.Stop(signals)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", args[0], err)
	}
	for {
		if code, exited := reap(cmd.Process.Pid); exited {
			return code, nil
		}
		if sig := <-signals; sig != syscall.SIGCHLD {
			_ = cmd.Process.Signal(sig)
		}
	}
}

func reap(command int) (int, bool) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case err != nil || pid <= 0:
			return 0, false
		case pid != command:
			continue
		case status.Signaled():
			return 128 + int(status.Signal()), true
		default:
			return status.ExitStatus(), true
		}
	}
}
