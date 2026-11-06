//go:build !linux

package acp

import (
	"fmt"
	"os"
	"os/exec"
)

func newOperatorChildCommand(cfg OperatorCommandConfig) (*exec.Cmd, error) {
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.SysProcAttr = childSysProcAttr(cfg.UID, cfg.GID)
	return cmd, nil
}

// OpenOperatorPTY is only supported on Linux.
func OpenOperatorPTY(_, _ int, _, _ uint16) (*os.File, *os.File, error) {
	return nil, nil, fmt.Errorf("operator terminals are only supported on Linux")
}

// ResizeOperatorPTY is only supported on Linux.
func ResizeOperatorPTY(_ *os.File, _, _ uint16) error {
	return fmt.Errorf("operator terminals are only supported on Linux")
}

// OperatorSignal maps an allowed operator signal name.
func OperatorSignal(name string) (os.Signal, bool) {
	switch name {
	case "INT":
		return os.Interrupt, true
	case "KILL":
		return os.Kill, true
	default:
		return nil, false
	}
}
