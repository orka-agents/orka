package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	OperatorTagFirstGID = 61000
	OperatorTagLastGID  = 64999
)

type OperatorCommandConfig struct {
	ExecHelperCommand string
	UID               int
	GID               int
	TagGID            int
	Argv              []string
	Environment       []string
	Dir               string
	TTY               *os.File
}

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
	if cfg.TagGID < OperatorTagFirstGID || cfg.TagGID > OperatorTagLastGID {
		return nil, fmt.Errorf("operator command requires a tag group in %d-%d", OperatorTagFirstGID, OperatorTagLastGID)
	}
	cmd, err := newOperatorChildCommand(cfg)
	if err != nil {
		return nil, err
	}
	cmd.Dir = cfg.Dir
	cmd.Env = append([]string(nil), cfg.Environment...)
	if cfg.TTY != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = cfg.TTY, cfg.TTY, cfg.TTY
	}
	return cmd, nil
}

var operatorLeaders = struct {
	mu   sync.Mutex
	pids map[int]*operatorLeaderRegistration
}{pids: map[int]*operatorLeaderRegistration{}}

type operatorLeaderRegistration struct{ pid int }

func StartOperatorCommand(cmd *exec.Cmd) (release func(), err error) {
	operatorLeaders.mu.Lock()
	defer operatorLeaders.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	registration := &operatorLeaderRegistration{pid: cmd.Process.Pid}
	operatorLeaders.pids[registration.pid] = registration
	return func() { releaseOperatorLeader(registration) }, nil
}

func releaseOperatorLeader(registration *operatorLeaderRegistration) {
	operatorLeaders.mu.Lock()
	defer operatorLeaders.mu.Unlock()
	if operatorLeaders.pids[registration.pid] == registration {
		delete(operatorLeaders.pids, registration.pid)
	}
}

func SignalOperatorCommand(cmd *exec.Cmd, signal os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return signalProcessGroup(cmd.Process.Pid, signal)
}
