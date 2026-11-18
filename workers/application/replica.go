//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func runReplicaProcess(ctx context.Context, argv, environment []string) error {
	deadline, bounded := ctx.Deadline()
	if !bounded || !time.Now().Before(deadline) || ctx.Err() != nil || len(argv) == 0 {
		return errors.New("replica fixture requires the original absolute execution deadline")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = environment
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		return errors.New("fixed replica executable could not start")
	}
	waitErr := command.Wait()
	killErr := unix.Kill(-command.Process.Pid, unix.SIGKILL)
	if killErr != nil && !errors.Is(killErr, unix.ESRCH) {
		return pv.ErrEvidence
	}
	// This trusted PID 1 reaps orphaned fixed-fixture descendants before exit.
	// The Pod deadline and exact controller/Pod cleanup remain independent bounds.
	until := time.Now().Add(5 * time.Second)
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) {
			break
		}
		if err != nil || !time.Now().Before(until) {
			return errors.New("replica descendants did not finish bounded cleanup")
		}
		if pid == 0 {
			timer := time.NewTimer(10 * time.Millisecond)
			<-timer.C
		}
	}
	if ctx.Err() != nil || waitErr != nil {
		return errors.New("replica fixture terminated or reached its frozen deadline")
	}
	return nil
}
