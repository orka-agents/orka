package cliwrapper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type CommandRunner struct {
	StdoutLimitBytes int64
	StderrLimitBytes int64
	CancelGrace      time.Duration
}

var errChildCredentialProcessCleanupUnproven = errors.New("child credential process cleanup was not proven")

func NewCommandRunner(cfg Config) CommandRunner {
	stdoutLimit := cfg.StdoutLimitBytes
	if stdoutLimit == 0 {
		stdoutLimit = DefaultOutputLimitBytes
	}
	stderrLimit := cfg.StderrLimitBytes
	if stderrLimit == 0 {
		stderrLimit = DefaultOutputLimitBytes
	}
	grace := cfg.CancelGrace
	if grace == 0 {
		grace = DefaultCancelGrace
	}
	return CommandRunner{StdoutLimitBytes: stdoutLimit, StderrLimitBytes: stderrLimit, CancelGrace: grace}
}

func (r CommandRunner) Run(ctx context.Context, spec *CommandSpec) (CommandResult, error) { //nolint:gocyclo
	if spec == nil {
		return CommandResult{}, fmt.Errorf("command spec is required")
	}
	if strings.TrimSpace(spec.Path) == "" {
		return CommandResult{}, fmt.Errorf("command path is required")
	}

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	// The wrapper Pod can carry controller-only credentials through envFrom or
	// secretKeyRef. Agent-controlled children receive only a fixed process
	// baseline plus the environment explicitly frozen for this turn.
	baseEnv := baselineChildProcessEnv()
	if spec.ClearEnv {
		baseEnv = nil
	}
	cmd.Env = mergeCommandEnv(baseEnv, spec.Env)
	cmd.Env = unsetCommandEnv(cmd.Env, spec.UnsetEnv)
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	cmd.SysProcAttr = commandSysProcAttr()

	stdout := newLimitedBuffer(r.StdoutLimitBytes)
	stdoutFull := newLimitedBuffer(maxStoredResultBytes)
	stderr := newLimitedBuffer(r.StderrLimitBytes)
	// Parent-owned pipes instead of cmd.StdoutPipe/StderrPipe: Wait closes the
	// pipes it created as soon as the process exits, racing the copy goroutines
	// and silently truncating child output. With os.Pipe the read ends stay
	// open until the copies drain to EOF or waitForPipeCopies times out.
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return CommandResult{}, fmt.Errorf("open stdout pipe: %w", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return CommandResult{}, fmt.Errorf("open stderr pipe: %w", err)
	}
	closePipes := func(pipes ...*os.File) {
		for _, pipe := range pipes {
			_ = pipe.Close()
		}
	}
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite

	started := time.Now().UTC()
	if err := ctx.Err(); err != nil {
		closePipes(stdoutRead, stdoutWrite, stderrRead, stderrWrite)
		return CommandResult{
			StartedAt:  started,
			FinishedAt: time.Now().UTC(),
			ExitCode:   -1,
			TimedOut:   errors.Is(err, context.DeadlineExceeded),
			Cancelled:  errors.Is(err, context.Canceled),
			ResultFile: spec.ResultFile,
		}, err
	}
	if err := cmd.Start(); err != nil {
		closePipes(stdoutRead, stdoutWrite, stderrRead, stderrWrite)
		return CommandResult{StartedAt: started, FinishedAt: time.Now().UTC(), ExitCode: -1, ResultFile: spec.ResultFile}, err
	}
	// The child holds duplicated write ends; the parent's copies must close so
	// the readers reach EOF once the process (group) exits.
	closePipes(stdoutWrite, stderrWrite)

	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(io.MultiWriter(stdout, stdoutFull), stdoutRead)
	}()
	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(stderr, stderrRead)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	cancelled := false
	timedOut := false
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		cancelled = true
		timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		terminateProcessGroup(cmd.Process, r.CancelGrace)
		waitErr = <-waitCh
	}
	if !cancelled {
		terminateProcessGroup(cmd.Process, 0)
	}
	cleanupErr := terminateChildCredentialProcesses(r.CancelGrace)
	if waitErr == nil {
		if err := ctx.Err(); err != nil {
			cancelled = true
			timedOut = errors.Is(err, context.DeadlineExceeded)
			waitErr = err
		}
	}
	if cleanupErr != nil {
		waitErr = errors.Join(
			waitErr,
			fmt.Errorf("%w: %w", errChildCredentialProcessCleanupUnproven, cleanupErr),
		)
	}
	waitForPipeCopies(&copyWG, stdoutRead, stderrRead, 5*time.Second)
	// waitForPipeCopies closes the readers only on its timeout path; close
	// them on every path so successful commands do not leak two descriptors
	// per turn (Close after close is a harmless ErrClosed).
	closePipes(stdoutRead, stderrRead)

	finished := time.Now().UTC()
	exitCode := exitCodeFromError(waitErr)
	result := CommandResult{
		Stdout:              stdout.String(),
		FullStdout:          stdoutFull.String(),
		FullStdoutTruncated: stdoutFull.Truncated(),
		Stderr:              stderr.String(),
		ExitCode:            exitCode,
		StartedAt:           started,
		FinishedAt:          finished,
		TimedOut:            timedOut,
		Cancelled:           cancelled && !timedOut,
		ResultFile:          spec.ResultFile,
	}
	if stdout.Truncated() {
		result.Stdout += fmt.Sprintf("\n[stdout truncated at %d bytes]", r.StdoutLimitBytes)
	}
	if stderr.Truncated() {
		result.Stderr += fmt.Sprintf("\n[stderr truncated at %d bytes]", r.StderrLimitBytes)
	}
	return result, waitErr
}

func waitForPipeCopies(
	copyWG *sync.WaitGroup,
	stdoutPipe io.Closer,
	stderrPipe io.Closer,
	timeout time.Duration,
) {
	done := make(chan struct{})
	go func() {
		copyWG.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
		<-done
	}
}

func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func sanitizedProcessEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "ORKA_HARNESS_WRAPPER_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func baselineChildProcessEnv() []string {
	return []string{
		"PATH=" + wrapperSafeCommandPath,
		"TMPDIR=/tmp",
	}
}

func mergeCommandEnv(base, overrides []string) []string {
	out := append([]string(nil), base...)
	for _, entry := range overrides {
		if strings.TrimSpace(entry) == "" || !strings.Contains(entry, "=") {
			continue
		}
		key, _, _ := strings.Cut(entry, "=")
		out = setEnv(out, key, strings.TrimPrefix(entry, key+"="))
	}
	return out
}

func unsetCommandEnv(env, names []string) []string {
	if len(env) == 0 || len(names) == 0 {
		return env
	}
	unset := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			unset[name] = struct{}{}
		}
	}
	if len(unset) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, remove := unset[key]; remove {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}

func removeTempFiles(paths []string) {
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			_ = os.RemoveAll(path)
		}
	}
}

type limitedBuffer struct {
	limit     int64
	buf       bytes.Buffer
	written   int64
	truncated bool
}

func newLimitedBuffer(limit int64) *limitedBuffer {
	if limit <= 0 {
		limit = DefaultOutputLimitBytes
	}
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.written += int64(len(p))
	remaining := b.limit - int64(b.buf.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = b.buf.Write(p[:int(remaining)])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func (b *limitedBuffer) Truncated() bool { return b.truncated || b.written > b.limit }
