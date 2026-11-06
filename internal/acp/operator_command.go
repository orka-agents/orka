package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OperatorCommandConfig describes one operator debugging command. It runs
// through the same exec helper, UID/GID, empty supplementary groups, resource
// limits, and parent-death fence as the RuntimeSession's ACP adapter, so an
// operator never gains the supervisor's root identity or capabilities.
type OperatorCommandConfig struct {
	ExecHelperCommand string
	UID               int
	GID               int
	// Argv[0] must already be an absolute executable path.
	Argv        []string
	Environment []string
	Dir         string
	// TTY is the terminal slave. When set the command becomes a session
	// leader with TTY as its controlling terminal and stdio.
	TTY *os.File
}

// NewOperatorCommand builds, but does not start, an operator command.
func NewOperatorCommand(cfg OperatorCommandConfig) (*exec.Cmd, error) {
	if len(cfg.Argv) == 0 || !filepath.IsAbs(cfg.Argv[0]) {
		return nil, fmt.Errorf("operator command must be an absolute path")
	}
	if cfg.UID <= 0 || cfg.GID <= 0 {
		return nil, fmt.Errorf("operator command requires a non-root UID and GID")
	}
	if os.Geteuid() != 0 && (cfg.UID != os.Getuid() || cfg.GID != os.Getgid()) {
		return nil, fmt.Errorf("non-root supervisor can only launch operator commands as its own identity")
	}
	if strings.TrimSpace(cfg.Dir) == "" || !filepath.IsAbs(cfg.Dir) {
		return nil, fmt.Errorf("operator command directory must be absolute")
	}
	cmd, err := newChildCommand(ProcessConfig{
		Command:           cfg.Argv[0],
		Args:              cfg.Argv[1:],
		UID:               cfg.UID,
		GID:               cfg.GID,
		ExecHelperCommand: cfg.ExecHelperCommand,
	})
	if err != nil {
		return nil, err
	}
	cmd.Dir = cfg.Dir
	cmd.Env = append([]string(nil), cfg.Environment...)
	if cfg.TTY != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.TTY, cfg.TTY, cfg.TTY
		cmd.SysProcAttr = operatorTTYSysProcAttr(cfg.UID, cfg.GID)
	}
	return cmd, nil
}

// SignalOperatorCommand signals the command's process group.
func SignalOperatorCommand(cmd *exec.Cmd, signal os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return signalProcessGroup(cmd.Process.Pid, signal)
}
