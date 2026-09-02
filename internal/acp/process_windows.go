//go:build windows

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

func childSysProcAttr(_, _ int) *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
func signalProcessGroup(_ int, _ os.Signal) error    { return syscall.EWINDOWS }
func terminateSignal() os.Signal                     { return os.Interrupt }
func killSignal() os.Signal                          { return os.Kill }
func stopSignal() os.Signal                          { return os.Interrupt }
func continueSignal() os.Signal                      { return os.Interrupt }
func processesForUID(_ int) ([]int, bool)            { return nil, false }
func reapExitedProcessesForUID(_ int)                {}
func reapExitedProcessesForUIDExcept(_, _ int)       {}

func processesStoppedForUID(_, _ int) (bool, bool, error) { return false, false, nil }
func processesResumedForUID(_, _ int) (bool, bool, error) { return false, false, nil }

func signalProcessesForUID(_ int, _ os.Signal) (bool, error) { return false, nil }
