package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultStderrLimit       = 256 << 10
	DefaultStopGrace         = 10 * time.Second
	DefaultExecHelperCommand = "/usr/local/bin/orka-acp-exec-helper"

	cleanupProofObservationInterval = 25 * time.Millisecond
	cleanupPollInterval             = 10 * time.Millisecond
	cleanupKillSettleTimeout        = time.Second
	sessionProcessPollInterval      = 10 * time.Millisecond
	sessionThawVerificationTimeout  = time.Second
)

type ProcessConfig struct {
	Command           string
	Args              []string
	Environment       []string
	Paths             SessionPaths
	UID               int
	GID               int
	StderrLimit       int
	ClientOptions     Options
	ExecHelperCommand string
}

type Process struct {
	cmd    *exec.Cmd
	client *Client
	stderr *boundedBuffer
	uid    int

	done     chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	stopOnce sync.Once
}

type CleanupStatus struct {
	Proven        bool
	RemainingPIDs []int
}

func StartProcess(cfg ProcessConfig) (*Process, error) {
	cfg.Command = strings.TrimSpace(cfg.Command)
	if cfg.Command == "" || !filepath.IsAbs(cfg.Command) {
		return nil, fmt.Errorf("ACP adapter command must be an absolute path")
	}
	if cfg.Paths.Workspace == "" || !filepath.IsAbs(cfg.Paths.Workspace) {
		return nil, fmt.Errorf("ACP adapter workspace must be absolute")
	}
	if cfg.UID <= 0 || cfg.GID <= 0 {
		return nil, fmt.Errorf("ACP adapter requires a non-root UID and GID")
	}
	if os.Geteuid() != 0 && (cfg.UID != os.Getuid() || cfg.GID != os.Getgid()) {
		return nil, fmt.Errorf("non-root supervisor can only launch the adapter as its own identity")
	}
	if cfg.StderrLimit <= 0 {
		cfg.StderrLimit = DefaultStderrLimit
	}
	cmd, err := newChildCommand(cfg)
	if err != nil {
		return nil, err
	}
	cmd.Dir = cfg.Paths.Workspace
	cmd.Env = append([]string(nil), cfg.Environment...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create ACP adapter stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create ACP adapter stdout: %w", err)
	}
	stderr := newBoundedBuffer(cfg.StderrLimit)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start ACP adapter: %w", err)
	}
	process := &Process{
		cmd:    cmd,
		client: NewClient(stdout, stdin, cfg.ClientOptions),
		stderr: stderr,
		uid:    cfg.UID,
		done:   make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		_ = stdin.Close()
		close(process.done)
	}()
	return process, nil
}

func (p *Process) Client() *Client {
	if p == nil {
		return nil
	}
	return p.client
}

func (p *Process) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *Process) Done() <-chan struct{} {
	if p == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.done
}

func (p *Process) Wait(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("ACP adapter process is required")
	}
	select {
	case <-p.done:
		p.waitMu.Lock()
		defer p.waitMu.Unlock()
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Process) Stop(ctx context.Context, grace time.Duration) (CleanupStatus, error) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return CleanupStatus{Proven: true}, nil
	}
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	p.stopOnce.Do(func() { p.signalForStop(terminateSignal()) })

	status, complete, err := p.waitForCleanup(ctx, grace)
	if err != nil {
		p.signalForStop(killSignal())
		return status, err
	}
	if complete {
		return status, nil
	}

	p.signalForStop(killSignal())
	status, complete, err = p.waitForCleanup(ctx, cleanupKillSettleTimeout)
	if err != nil {
		return status, err
	}
	if !complete {
		return status, fmt.Errorf("ACP adapter descendant cleanup could not be proven")
	}
	return status, nil
}

func (p *Process) waitForCleanup(ctx context.Context, timeout time.Duration) (CleanupStatus, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(cleanupPollInterval)
	defer ticker.Stop()

	done := p.done
	leaderExited := false
	select {
	case <-done:
		leaderExited = true
		done = nil
	default:
	}
	status := CleanupStatus{Proven: false}
	for {
		if leaderExited {
			status = p.cleanupStatus()
			if status.Proven {
				return status, true, nil
			}
		}
		select {
		case <-ctx.Done():
			return status, false, ctx.Err()
		case <-timer.C:
			return status, false, nil
		case <-done:
			leaderExited = true
			done = nil
		case <-ticker.C:
		}
	}
}

func (p *Process) signalForStop(signal os.Signal) {
	// Production supervisors run as root and assign a non-reused UID to each
	// RuntimeSession. UID-scoped signaling cannot hit a recycled process-group
	// ID and reaches descendants that detached from the adapter's group.
	if p.usesUIDProcessScope() {
		_, _ = signalProcessesForUID(p.uid, signal)
		return
	}
	select {
	case <-p.done:
		return
	default:
		_ = signalProcessGroup(p.cmd.Process.Pid, signal)
	}
}

func (p *Process) cleanupStatus() CleanupStatus {
	return proveProcessCleanup(
		p.uid,
		reapExitedProcessesForUID,
		processesForUID,
		func() { time.Sleep(cleanupProofObservationInterval) },
	)
}

func proveProcessCleanup(
	uid int,
	reap func(int),
	inventory func(int) ([]int, bool),
	betweenObservations func(),
) CleanupStatus {
	reap(uid)
	remaining, supported := inventory(uid)
	if !supported {
		return CleanupStatus{Proven: false}
	}
	if len(remaining) != 0 {
		return CleanupStatus{Proven: false, RemainingPIDs: append([]int(nil), remaining...)}
	}

	betweenObservations()
	reap(uid)
	remaining, supported = inventory(uid)
	if !supported {
		return CleanupStatus{Proven: false}
	}
	return CleanupStatus{
		Proven:        len(remaining) == 0,
		RemainingPIDs: append([]int(nil), remaining...),
	}
}

func (p *Process) Diagnostics() string {
	if p == nil || p.stderr == nil {
		return ""
	}
	return p.stderr.String()
}

type boundedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(data)
	if b.limit <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(data) >= b.limit {
		b.data = append(b.data[:0], data[len(data)-b.limit:]...)
		b.truncated = true
		return original, nil
	}
	if overflow := len(b.data) + len(data) - b.limit; overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
		b.truncated = true
	}
	b.data = append(b.data, data...)
	return original, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncated {
		return string(b.data)
	}
	return "[truncated]\n" + string(b.data)
}

func (b *boundedBuffer) ReadFrom(reader io.Reader) (int64, error) {
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			_, _ = b.Write(buffer[:n])
			total += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

func (p *Process) Freeze(ctx context.Context) error {
	if p == nil || p.PID() <= 0 {
		return fmt.Errorf("ACP adapter process is not running")
	}
	select {
	case <-p.done:
		return fmt.Errorf("ACP adapter exited while freezing")
	default:
	}
	ticker := time.NewTicker(sessionProcessPollInterval)
	defer ticker.Stop()
	for {
		if err := p.signalSessionProcesses(stopSignal()); err != nil {
			return fmt.Errorf("stop ACP session processes: %w", err)
		}
		reapExitedProcessesForUIDExcept(p.uid, p.PID())
		stopped, supported, err := processesStoppedForUID(p.uid, p.PID())
		if err != nil {
			return err
		}
		if !supported {
			return fmt.Errorf("process-freeze verification is unsupported on this platform")
		}
		if stopped {
			select {
			case <-p.done:
				return fmt.Errorf("ACP adapter exited while freezing")
			default:
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return fmt.Errorf("ACP adapter exited while freezing")
		case <-ticker.C:
		}
	}
}

func (p *Process) Thaw() error {
	if p == nil || p.PID() <= 0 {
		return fmt.Errorf("ACP adapter process is not running")
	}
	select {
	case <-p.done:
		return fmt.Errorf("ACP adapter exited while thawing")
	default:
	}
	if err := p.signalSessionProcesses(continueSignal()); err != nil {
		return fmt.Errorf("continue ACP session processes: %w", err)
	}
	uidScoped := p.usesUIDProcessScope()
	ticker := time.NewTicker(sessionProcessPollInterval)
	defer ticker.Stop()
	timer := time.NewTimer(sessionThawVerificationTimeout)
	defer timer.Stop()
	for {
		resumed, supported, err := processesResumedForUID(p.uid, p.PID())
		if err != nil {
			return err
		}
		if !supported {
			if uidScoped {
				return fmt.Errorf("process-thaw verification is unsupported on this platform")
			}
			return nil
		}
		if resumed {
			select {
			case <-p.done:
				return fmt.Errorf("ACP adapter exited while thawing")
			default:
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("ACP adapter exited while thawing")
		case <-timer.C:
			return fmt.Errorf("ACP session process thaw could not be proven")
		case <-ticker.C:
			if err := p.signalSessionProcesses(continueSignal()); err != nil {
				return fmt.Errorf("continue ACP session processes: %w", err)
			}
		}
	}
}

func (p *Process) usesUIDProcessScope() bool {
	return os.Geteuid() == 0 && p.uid != os.Getuid()
}

func (p *Process) signalSessionProcesses(signal os.Signal) error {
	if p.usesUIDProcessScope() {
		supported, err := signalProcessesForUID(p.uid, signal)
		if err != nil {
			return err
		}
		if !supported {
			return fmt.Errorf("UID-scoped process signaling is unsupported on this platform")
		}
		return nil
	}
	return signalProcessGroup(p.PID(), signal)
}
