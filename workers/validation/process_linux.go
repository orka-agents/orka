//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

const guardReady = "orka-validation-guard-v1\n"

type childProcess struct {
	pid        int
	pidfd      int
	uid        int
	fixture    bool
	stopping   bool
	executed   bool
	waited     bool
	status     unix.WaitStatus
	startedAt  time.Time
	finishedAt time.Time
	stdout     *outputCapture
	stderr     *outputCapture
	handshake  <-chan bool
	drains     []<-chan error
	pipeEnds   []*os.File
}

type processSpec struct {
	command     []string
	environment []string
	directory   string
	stdin       string
	uid         int
	fixture     bool
	readyOutput string
	listener    *os.File
}

type processManager struct {
	processes  []*childProcess
	overflow   chan struct{}
	overflowed sync.Once
	noChildren bool
}

func newProcessManager() *processManager {
	return &processManager{overflow: make(chan struct{})}
}

func (manager *processManager) start(spec processSpec) (*childProcess, error) {
	if len(spec.command) == 0 || (spec.listener != nil) != spec.fixture {
		return nil, errors.New("invalid supervised process specification")
	}
	ends := make([]*os.File, 0, 8)
	success := false
	defer func() {
		if !success {
			for _, file := range ends {
				_ = file.Close()
			}
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		read, write, err := os.Pipe()
		if err == nil {
			ends = append(ends, read, write)
		}
		return read, write, err
	}
	input, inputWrite, err := pipe()
	if err != nil {
		return nil, errors.New("supervised input pipe is unavailable")
	}
	output, outputWrite, err := pipe()
	if err != nil {
		return nil, errors.New("supervised output pipe is unavailable")
	}
	diagnostics, diagnosticsWrite, err := pipe()
	if err != nil {
		return nil, errors.New("supervised diagnostic pipe is unavailable")
	}
	control, controlWrite, err := pipe()
	if err != nil {
		return nil, errors.New("sandbox control pipe is unavailable")
	}
	files := []*os.File{input, outputWrite, diagnosticsWrite}
	if spec.fixture {
		files = append(files, spec.listener)
	}
	files = append(files, controlWrite)
	process := &childProcess{
		uid: spec.uid, fixture: spec.fixture, pidfd: -1,
		startedAt: time.Now().UTC(),
		pipeEnds:  []*os.File{inputWrite, output, diagnostics, control},
	}
	child, err := os.StartProcess(spec.command[0], spec.command, &os.ProcAttr{
		Dir: spec.directory, Env: spec.environment, Files: files,
		Sys: &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL},
	})
	if err != nil {
		return nil, errors.New("trusted sandbox guard could not be started")
	}
	process.pid = child.Pid
	process.pidfd, err = unix.PidfdOpen(child.Pid, 0)
	if err != nil {
		_ = child.Kill()
		_, _ = child.Wait()
		return nil, errors.New("exact process signaling is unavailable")
	}
	_ = child.Release()
	for _, file := range []*os.File{input, outputWrite, diagnosticsWrite, controlWrite} {
		_ = file.Close()
	}
	manager.processes = append(manager.processes, process)
	manager.noChildren = false
	onOverflow := func() { manager.overflowed.Do(func() { close(manager.overflow) }) }
	process.stdout, process.stderr = newCapture(spec.readyOutput, onOverflow), newCapture("", onOverflow)
	for _, stream := range []struct {
		reader  *os.File
		capture *outputCapture
	}{{output, process.stdout}, {diagnostics, process.stderr}} {
		done := make(chan error, 1)
		process.drains = append(process.drains, done)
		go func() {
			_, err := io.Copy(stream.capture, stream.reader)
			_ = stream.reader.Close()
			done <- err
		}()
	}
	handshake := make(chan bool, 1)
	process.handshake = handshake
	go func() {
		content, err := io.ReadAll(io.LimitReader(control, int64(len(guardReady)+1)))
		_ = control.Close()
		handshake <- err == nil && string(content) == guardReady
	}()
	go func() {
		_, _ = io.WriteString(inputWrite, spec.stdin)
		_ = inputWrite.Close()
	}()
	success = true
	return process, nil
}

func (manager *processManager) poll() error {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.ECHILD) {
			manager.noChildren = true
			return nil
		}
		if err != nil {
			return errors.New("owned process exit could not be observed")
		}
		if pid == 0 {
			manager.noChildren = false
			return nil
		}
		for _, process := range manager.processes {
			if process.pid == pid && !process.waited {
				process.status, process.waited, process.finishedAt = status, true, time.Now().UTC()
				break
			}
		}
	}
}

func (manager *processManager) fixturesAlive() bool {
	for _, process := range manager.processes {
		if process.fixture && !process.stopping && process.waited {
			return false
		}
	}
	return true
}

func (manager *processManager) waitGuard(ctx context.Context, process *childProcess) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := manager.poll(); err != nil {
			return err
		}
		if !manager.fixturesAlive() {
			return errors.New("local fixture exited before execution was complete")
		}
		select {
		case accepted := <-process.handshake:
			if !accepted {
				return errors.New("sandbox guard did not attest successful setup and exec")
			}
			process.executed = true
			return nil
		case <-manager.overflow:
			return errors.New("execution output exceeded the capture limit")
		case <-ctx.Done():
			return errors.New("sandbox startup timed out or was canceled")
		case <-ticker.C:
		}
	}
}

func (manager *processManager) waitReady(ctx context.Context, process *childProcess) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := manager.poll(); err != nil {
			return err
		}
		if !manager.fixturesAlive() {
			return errors.New("local fixture exited before declared readiness")
		}
		select {
		case <-process.stdout.ready:
			// Readiness is necessary, not a substitute for an observed live fixture.
			if manager.poll() != nil || !manager.fixturesAlive() {
				return errors.New("local fixture was not alive at declared readiness")
			}
			return nil
		case <-manager.overflow:
			return errors.New("local fixture output exceeded the capture limit")
		case <-ctx.Done():
			return errors.New("local fixture readiness timed out or was canceled")
		case <-ticker.C:
		}
	}
}

func (manager *processManager) waitSubject(ctx context.Context, process *childProcess) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := manager.poll(); err != nil {
			return err
		}
		if !manager.fixturesAlive() {
			return errors.New("local fixture exited while the check was running")
		}
		if ctx.Err() != nil {
			return errors.New("check timed out or was canceled")
		}
		select {
		case <-manager.overflow:
			return errors.New("execution output exceeded the capture limit")
		default:
		}
		if process.waited {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-manager.overflow:
		case <-ticker.C:
		}
	}
}

func (manager *processManager) observeHTTP(
	ctx context.Context, server *childProcess, check pv.Check, port int,
) ([]byte, error) {
	if server == nil || !server.fixture || !server.executed ||
		len(manager.processes) != 1 || manager.processes[0] != server {
		return nil, errors.New("HTTP observation requires its exact supervised server role")
	}
	request, cancel := context.WithCancel(ctx)
	type response struct {
		content []byte
		err     error
	}
	responses := make(chan response, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		content, err := pv.ObserveHTTP(request, check, port)
		responses <- response{content: content, err: err}
	}()
	defer func() {
		cancel()
		<-done
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var received *response
	for {
		if err := manager.poll(); err != nil {
			return nil, err
		}
		if !manager.fixturesAlive() {
			return nil, errors.New("HTTP subject exited before observation completed")
		}
		if ctx.Err() != nil {
			return nil, errors.New("HTTP observation timed out or was canceled")
		}
		select {
		case <-manager.overflow:
			return nil, errors.New("HTTP subject output exceeded the capture limit")
		default:
		}
		if received != nil {
			return received.content, received.err
		}
		select {
		case result := <-responses:
			received = &result
		case <-ctx.Done():
		case <-manager.overflow:
		case <-ticker.C:
		}
	}
}

func (manager *processManager) runHTTPObservation(
	ctx context.Context, server *childProcess, check pv.Check, port int, evidence *pv.ExecutionEvidence,
) error {
	observation := &evidence.Observation
	observation.HTTPCompleted = false
	observation.StartedAt, observation.Executed = time.Now().UTC(), true
	defer func() {
		observation.FinishedAt = time.Now().UTC()
		observation.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}()
	content, err := manager.observeHTTP(ctx, server, check, port)
	if err != nil {
		return err
	}
	if err := manager.stopFixtures(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("HTTP observation was canceled before orderly completion")
	}
	capture := newCapture("", nil)
	_, _ = capture.Write(content)
	output := captureEvidence(evidence, capture)
	observation.StdoutDigest, observation.StdoutBytes = output.Digest, output.Bytes
	observation.ExitCode, observation.HTTPCompleted = new(0), true
	return nil
}

type processIdentity struct {
	pid     int
	parent  int
	uids    [4]int
	started string
}

func readProcessIdentity(pid int) (processIdentity, error) {
	identity := processIdentity{pid: pid}
	read := func(name string) ([]byte, error) {
		file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), name))
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		content, err := io.ReadAll(io.LimitReader(file, 32<<10+1))
		if err != nil || len(content) > 32<<10 {
			return nil, errors.New("process identity exceeds its observation limit")
		}
		return content, nil
	}
	status, err := read("status")
	if err != nil {
		return identity, err
	}
	found := false
	for line := range strings.SplitSeq(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, "Uid:"); ok {
			fields := strings.Fields(value)
			if found || len(fields) != 4 {
				return identity, errors.New("process identity is malformed")
			}
			for index, field := range fields {
				number, err := strconv.Atoi(field)
				if err != nil || number < 0 {
					return identity, errors.New("process identity is malformed")
				}
				identity.uids[index] = number
			}
			found = true
		}
	}
	stat, err := read("stat")
	if err != nil {
		return identity, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if !found || end < 0 {
		return identity, errors.New("process identity is unavailable")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return identity, errors.New("process identity is malformed")
	}
	identity.parent, err = strconv.Atoi(fields[1])
	if err != nil {
		return identity, errors.New("process parent identity is malformed")
	}
	identity.started = fields[19]
	return identity, nil
}

func processInventory() (map[int]processIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil || len(entries) > 65536 {
		return nil, errors.New("owned process inventory is unavailable")
	}
	result := make(map[int]processIdentity)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		identity, err := readProcessIdentity(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, errors.New("owned process inventory could not be verified")
		}
		result[pid] = identity
	}
	return result, nil
}

func (manager *processManager) ownedProcesses() ([]processIdentity, error) {
	inventory, err := processInventory()
	if err != nil {
		return nil, err
	}
	return manager.ownedProcessInventory(inventory, os.Getpid())
}

func (manager *processManager) ownedProcessInventory(
	inventory map[int]processIdentity, supervisor int,
) ([]processIdentity, error) {
	uids := make(map[int]bool)
	for _, process := range manager.processes {
		uids[process.uid] = true
	}
	var result []processIdentity
	for _, identity := range inventory {
		if identity.pid == supervisor || !uids[identity.uids[0]] {
			continue
		}
		parent := identity.parent
		for depth := 0; depth < len(inventory); depth++ {
			if parent == supervisor {
				if identity.uids != [4]int{identity.uids[0], identity.uids[0], identity.uids[0], identity.uids[0]} {
					return nil, errors.New("owned process changed its sandbox identity")
				}
				result = append(result, identity)
				break
			}
			ancestor, exists := inventory[parent]
			if !exists || ancestor.parent == parent {
				break
			}
			parent = ancestor.parent
		}
	}
	return result, nil
}

func signalExact(pidfd int, signal unix.Signal) error {
	if err := unix.PidfdSendSignal(pidfd, signal, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return errors.New("exact owned process could not be signaled")
	}
	return nil
}

func signalIdentity(identity processIdentity, signal unix.Signal) error {
	fd, err := unix.PidfdOpen(identity.pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return errors.New("owned descendant signaling is unavailable")
	}
	defer func() { _ = unix.Close(fd) }()
	current, err := readProcessIdentity(identity.pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil || current.started != identity.started || current.uids != identity.uids {
		return errors.New("owned descendant identity changed before signaling")
	}
	return signalExact(fd, signal)
}

func (manager *processManager) hasDescendants(uid int) (bool, error) {
	processes, err := manager.ownedProcesses()
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if process.uids[0] == uid {
			return true, nil
		}
	}
	return false, nil
}

func (manager *processManager) stopFixture(ctx context.Context, process *childProcess) error {
	if manager.poll() != nil || process.waited {
		return errors.New("local fixture exited before orderly shutdown")
	}
	process.stopping = true
	stoppingAt := time.Now().UTC()
	if unix.PidfdSendSignal(process.pidfd, unix.SIGTERM, nil, 0) != nil {
		return errors.New("local fixture could not receive orderly shutdown")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if manager.poll() != nil || !manager.fixturesAlive() {
			return errors.New("local fixture lifecycle observation failed")
		}
		if process.waited {
			if !process.status.Exited() || process.status.ExitStatus() != 0 || process.finishedAt.Before(stoppingAt) {
				return errors.New("local fixture did not exit cleanly on shutdown")
			}
			remaining, err := manager.hasDescendants(process.uid)
			if err != nil {
				return err
			}
			if !remaining {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("local fixture did not finish an orderly shutdown")
		case <-manager.overflow:
			return errors.New("local fixture output exceeded the capture limit")
		case <-ticker.C:
		}
	}
}

func (manager *processManager) stopFixtures(ctx context.Context) error {
	for _, process := range manager.processes {
		if !process.fixture {
			continue
		}
		if ctx.Err() != nil {
			return errors.New("validation was canceled during fixture shutdown")
		}
		stopping, cancel := context.WithTimeout(ctx, shutdownTimeout)
		err := manager.stopFixture(stopping, process)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func (manager *processManager) cleanup(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var result error
	for {
		if err := manager.poll(); err != nil {
			result = err
		}
		owned, err := manager.ownedProcesses()
		if err != nil {
			result = err
		}
		liveLeaders := false
		for _, process := range manager.processes {
			if !process.waited {
				liveLeaders = true
				if err := signalExact(process.pidfd, unix.SIGKILL); err != nil {
					result = err
				}
			}
		}
		for _, process := range owned {
			if err := signalIdentity(process, unix.SIGKILL); err != nil {
				result = err
			}
		}
		if err == nil && !liveLeaders && len(owned) == 0 && manager.noChildren {
			return result
		}
		select {
		case <-ctx.Done():
			return errors.New("owned process cleanup could not be confirmed")
		case <-ticker.C:
		}
	}
}

func (manager *processManager) drain(ctx context.Context) error {
	var result error
	for _, process := range manager.processes {
		for _, done := range process.drains {
			select {
			case err := <-done:
				if err != nil {
					result = errors.New("execution output could not be completely observed")
				}
			case <-ctx.Done():
				result = errors.New("execution output did not close after process cleanup")
			}
		}
		for _, file := range process.pipeEnds {
			_ = file.Close()
		}
		if process.pidfd >= 0 {
			_ = unix.Close(process.pidfd)
			process.pidfd = -1
		}
	}
	return result
}
