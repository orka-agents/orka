//go:build unix && !linux

package acp

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func newChildCommand(cfg ProcessConfig) (*exec.Cmd, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.SysProcAttr = childSysProcAttr(cfg.UID, cfg.GID)
	return cmd, nil
}

func RunExecHelper(_, _ []string) error {
	return fmt.Errorf("ACP exec helper is only supported on Linux")
}

func childSysProcAttr(uid, gid int) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	return attr
}

func signalProcessGroup(pid int, signal os.Signal) error {
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}

func terminateSignal() os.Signal { return syscall.SIGTERM }
func killSignal() os.Signal      { return syscall.SIGKILL }
func stopSignal() os.Signal      { return syscall.SIGSTOP }
func continueSignal() os.Signal  { return syscall.SIGCONT }

func processesForUID(_ int) ([]int, bool) { return nil, false }

func reapExitedProcessesForUID(_ int)          {}
func reapExitedProcessesForUIDExcept(_, _ int) {}

func processesStoppedForUID(_, _ int) (bool, bool, error) { return false, false, nil }
func processesResumedForUID(_, _ int) (bool, bool, error) { return false, false, nil }

func signalProcessesForUID(_ int, _ os.Signal) (bool, error) { return false, nil }
