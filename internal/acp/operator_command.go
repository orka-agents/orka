package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Operator tag groups. Each operator operation runs with exactly one
// supplementary group from this range, unique among live processes. No file
// in the runtime is owned by or grants access to these groups.
const (
	OperatorTagFirstGID = 61000
	OperatorTagLastGID  = 64999
)

// OperatorCommandConfig describes one operator debugging command. It runs
// through the same exec helper, UID/GID, empty supplementary groups, resource
// limits, and parent-death fence as the RuntimeSession's ACP adapter, so an
// operator never gains the supervisor's root identity or capabilities.
type OperatorCommandConfig struct {
	ExecHelperCommand string
	UID               int
	GID               int
	// TagGID is the unique supplementary group that marks every process of
	// this operation (see OperatorTagFirstGID).
	TagGID int
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

// operatorLeaders are the PIDs of started operator commands whose owner has
// not reaped them yet. An operator leader runs under the RuntimeSession's UID
// and is a direct child of the supervisor, so the session-wide reaper that
// proves adapter cleanup would otherwise reap it. The owner would then lose
// the exit status, and the leader's PID, which is also its process group ID,
// could be given to an unrelated process while the owner still signals that
// group.
//
// Each entry carries the registration that made it. A leader's PID can be
// given to a later operator command between its owner's reap and its release,
// and that release must not withdraw the later command's entry.
var operatorLeaders = struct {
	mu   sync.Mutex
	pids map[int]*operatorLeaderRegistration
}{pids: map[int]*operatorLeaderRegistration{}}

type operatorLeaderRegistration struct{ pid int }

// StartOperatorCommand starts cmd and withholds its leader from the
// session-wide reaper until release is called. The owner must reap the leader
// itself and call release only afterwards: until then the PID stays allocated,
// alive or as a zombie, and names nothing but this command. A zombie leader
// counts against the session's cleanup proof until the owner reaps it.
func StartOperatorCommand(cmd *exec.Cmd) (release func(), err error) {
	// Held across the start so the reaper never sees the leader before it
	// is registered.
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

// SignalOperatorCommand signals the command's process group. It never kills
// the operation; use KillOperatorTag for containment.
func SignalOperatorCommand(cmd *exec.Cmd, signal os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return signalProcessGroup(cmd.Process.Pid, signal)
}
