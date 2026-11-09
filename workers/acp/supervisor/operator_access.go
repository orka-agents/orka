package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

const (
	// OperatorAccessCapabilityHeader opts a capabilities request into the
	// operator access field. Older controllers decode capabilities strictly.
	OperatorAccessCapabilityHeader = "X-Orka-Operator-Access-Capability"

	maxConcurrentOperatorOperations = 4
	operatorFileTimeout             = 5 * time.Minute
	operatorOutputDrainGrace        = 2 * time.Second
	operatorFileStatusLineBytes     = 1024
)

// operatorAccessStates are the RuntimeSession states that admit operator
// access. Every validation, publication, finalization, cancellation, and
// deletion state is refused: the workspace is frozen or its delta is being
// published from a captured snapshot, and creation has not finished. A
// poisoned session is refused too: poisoning is also how unproven operator
// cleanup fails closed.
var operatorAccessStates = map[harnessv2.RuntimeSessionState]struct{}{
	harnessv2.RuntimeSessionStateIdle:          {},
	harnessv2.RuntimeSessionStatePromptRunning: {},
}

// operatorChildIdentity is the RuntimeSession's immutable child UID/GID.
var operatorChildIdentity = func(state *sessionState) (int, int) { return state.runtime.ChildIdentity() }

type operatorTarget struct {
	uid       int
	gid       int
	paths     acp.SessionPaths
	childPath string
	sessionID harnessv2.RuntimeSessionID
	state     *sessionState
	token     uint64
}

// operatorRegistration tracks one admitted operator operation on its
// session from admission until its owning handler has killed and reaped every
// process of the operation. Only that handler removes it, so the quiesce
// barrier can never observe completion early.
type operatorRegistration struct {
	process operatorProcess // nil until started and bound
	revoked bool
}

// revokeSessionOperatorsLocked revokes every operator registration and starts
// killing bound operations. Registrations stay until their handlers finish
// cleanup. The caller holds s.mu.
func revokeSessionOperatorsLocked(state *sessionState) {
	for _, registration := range state.operators {
		if registration.revoked {
			continue
		}
		registration.revoked = true
		if registration.process != nil {
			go registration.process.Kill()
		}
	}
}

// bindOperatorProcess atomically re-checks the session and records a
// started process. A false result means the session left an admitting state,
// closed admission, or was replaced while the process started; the caller
// must kill it, and the registration remains until the caller finishes.
func (s *Server) bindOperatorProcess(target operatorTarget, process operatorProcess) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.sessions[target.sessionID]
	registration := target.state.operators[target.token]
	if registration == nil {
		return false
	}
	_, admitted := operatorAccessStates[target.state.descriptor.State]
	if current != target.state || registration.revoked || !admitted || target.state.creating || target.state.operatorsClosed {
		registration.revoked = true
		return false
	}
	registration.process = process
	return true
}

// quiesceOperators is the barrier before a workspace freeze or session
// deletion: it closes operator admission, revokes every operation, kills bound
// ones, and returns only when every handler has finished its proven cleanup.
// An unproven kill or a timeout returns an error; the caller fails closed.
func (s *Server) quiesceOperators(state *sessionState, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		state.operatorsClosed = true
		var bound []operatorProcess
		for _, registration := range state.operators {
			registration.revoked = true
			if registration.process != nil {
				bound = append(bound, registration.process)
			}
		}
		remaining := len(state.operators)
		s.mu.Unlock()
		for _, process := range bound {
			if !process.Kill() {
				return fmt.Errorf("operator process termination could not be proven")
			}
		}
		if remaining == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("operator operations could not be quiesced")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// quiesceOperatorsBeforeTeardown is the barrier every runtime teardown path
// applies before it accepts termination proofs: drain cleanup, supervisor
// shutdown, native capture, and explicit deletion. An admitted launch could
// otherwise start after descendant cleanup was proven, while session files
// are removed or captured.
func (s *Server) quiesceOperatorsBeforeTeardown(state *sessionState) error {
	return s.quiesceOperators(state, defaultDuration(s.cfg.CancelGrace, acp.DefaultStopGrace))
}

// operatorLease owns everything one operator operation holds.
type operatorLease struct {
	target      operatorTarget
	tag         int
	releaseSlot func()
	process     operatorProcess
}

// finishOperation kills and reaps the operation, then releases its tag,
// registration, and concurrency slot. If termination cannot be proven it
// fails closed: the session and pool are poisoned and the registration, tag,
// and slot stay held so no barrier, tag reuse, or new operation can proceed.
func (s *Server) finishOperation(lease *operatorLease) {
	if lease.process != nil {
		// Retire fences off late Kill calls (for example from the input
		// relay) before the tag is released for reuse below.
		proven := lease.process.Retire()
		_, _, _ = lease.process.Wait()
		for _, closer := range []io.Closer{lease.process.Stdin(), lease.process.Stdout(), lease.process.Stderr()} {
			if closer != nil {
				_ = closer.Close()
			}
		}
		if !proven {
			slog.Error("operator process termination could not be proven; failing the runtime closed",
				"runtimeSessionID", string(lease.target.sessionID))
			if lease.target.state != nil {
				s.poisonSession(lease.target.state, "operator process termination could not be proven")
			}
			s.poisonPool("operator_cleanup_unproven")
			return
		}
	}
	if lease.tag != 0 {
		s.operatorState().releaseTag(lease.tag)
	}
	s.mu.Lock()
	if lease.target.state != nil {
		delete(lease.target.state.operators, lease.target.token)
	}
	s.mu.Unlock()
	if lease.releaseSlot != nil {
		lease.releaseSlot()
	}
}

// operatorLaunch is one operator process request.
type operatorLaunch struct {
	UID   int
	GID   int
	Tag   int
	Argv  []string
	Env   []string
	Dir   string
	TTY   bool
	Rows  uint16
	Cols  uint16
	Stdin bool
}

// operatorProcess is a started operator command.
type operatorProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Resize(rows, cols uint16) error
	Signal(name string) error
	Wait() (exitCode int, signaled bool, err error)
	// Kill terminates the whole operation and reports whether no process of
	// it remains. It is safe to call repeatedly and concurrently.
	Kill() bool
	// Retire performs the final Kill, waiting for any in-flight one, and
	// turns every later Kill into a no-op, so a late caller cannot act on the
	// operation's tag group after it is released for reuse.
	Retire() bool
}

type operatorStarter func(operatorLaunch) (operatorProcess, error)

type operatorAccessState struct {
	once    sync.Once
	slots   chan struct{}
	starter operatorStarter
	// used makes every signed operator capability single-use within its
	// validity window, so a captured request cannot be replayed.
	usedMu sync.Mutex
	used   map[harnessv2.OperationID]time.Time
	// runtimeExecutable is the absolute orka-acp-runtime binary that hosts the
	// file helper.
	runtimeExecutable string
	// tags are the operator tag groups held by live operations.
	tagMu   sync.Mutex
	tags    map[int]struct{}
	nextTag int
}

func (s *Server) operatorState() *operatorAccessState {
	s.operator.once.Do(func() {
		if s.operator.slots == nil {
			s.operator.slots = make(chan struct{}, maxConcurrentOperatorOperations)
		}
		if s.operator.starter == nil {
			s.operator.starter = startOperatorProcess
		}
		if s.operator.runtimeExecutable == "" {
			if executable, err := os.Executable(); err == nil {
				s.operator.runtimeExecutable = executable
			}
		}
	})
	return &s.operator
}

// operatorTagInUse is replaceable in tests.
var operatorTagInUse = acp.OperatorTagInUse

// allocateTag reserves a tag group that no live process carries.
func (o *operatorAccessState) allocateTag() (int, bool) {
	o.tagMu.Lock()
	defer o.tagMu.Unlock()
	if o.tags == nil {
		o.tags = map[int]struct{}{}
	}
	size := acp.OperatorTagLastGID - acp.OperatorTagFirstGID + 1
	for range size {
		tag := acp.OperatorTagFirstGID + o.nextTag%size
		o.nextTag++
		if _, held := o.tags[tag]; held || operatorTagInUse(tag) {
			continue
		}
		o.tags[tag] = struct{}{}
		return tag, true
	}
	return 0, false
}

func (o *operatorAccessState) releaseTag(tag int) {
	o.tagMu.Lock()
	defer o.tagMu.Unlock()
	delete(o.tags, tag)
}

func (s *Server) registerOperatorRoutes() {
	s.mux.HandleFunc("GET "+harnessv2.OperatorExecPathTemplate, s.handleOperatorExec)
	s.mux.HandleFunc("GET "+harnessv2.OperatorFilesPathTemplate, s.handleOperatorFiles)
	s.mux.HandleFunc("PUT "+harnessv2.OperatorFilesPathTemplate, s.handleOperatorFiles)
}

func writeOperatorError(w http.ResponseWriter, status int, message string) {
	code := harnessv2.ErrorCodeInvalidRequest
	switch status {
	case http.StatusUnauthorized:
		code = harnessv2.ErrorCodeUnauthenticated
	case http.StatusForbidden:
		code = harnessv2.ErrorCodeForbidden
	case http.StatusConflict, http.StatusNotFound, http.StatusGone:
		code = harnessv2.ErrorCodeSessionPoisoned
	}
	writeError(w, status, code, message, nil, false)
}

// operatorTargetFor resolves the exact live session under the request's fence.
func (s *Server) operatorTargetFor(sessionID harnessv2.RuntimeSessionID, metadata harnessv2.MutationMetadata) (operatorTarget, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[sessionID]
	if state == nil || state.runtime == nil {
		return operatorTarget{}, http.StatusNotFound, "runtime session not found"
	}
	if state.creating {
		return operatorTarget{}, http.StatusConflict, "runtime session is still being created"
	}
	if mismatch := s.sessionFenceMismatch(state, metadata); mismatch != harnessv2.FenceMatch {
		return operatorTarget{}, http.StatusConflict, "runtime session fence does not match the requested Task session"
	}
	if _, ok := operatorAccessStates[state.descriptor.State]; !ok || state.operatorsClosed {
		return operatorTarget{}, http.StatusConflict, fmt.Sprintf(
			"runtime session is %s; operator access is refused while the workspace is frozen, prepared for publication, publishing, finalizing, or being deleted",
			state.descriptor.State)
	}
	uid, gid := operatorChildIdentity(state)
	if uid <= 0 || gid <= 0 {
		return operatorTarget{}, http.StatusConflict, "runtime session child identity is unavailable"
	}
	if state.operators == nil {
		state.operators = map[uint64]*operatorRegistration{}
	}
	state.operatorSeq++
	state.operators[state.operatorSeq] = &operatorRegistration{}
	return operatorTarget{
		uid: uid, gid: gid, paths: state.paths,
		childPath: harnessv2.RuntimeToolboxChildPath(acp.DefaultChildPath, s.cfg.Toolboxes),
		sessionID: sessionID, state: state, token: state.operatorSeq,
	}, 0, ""
}

// authorizeOperatorRequest authenticates the bearer on headers alone, then
// decodes, validates, and capability-verifies the signed request header.
func (s *Server) authorizeOperatorRequest(w http.ResponseWriter, r *http.Request, target interface {
	ValidateAt(time.Time) error
}, metadata func() harnessv2.MutationMetadata) bool {
	if !s.authorizeController(w, r) {
		return false
	}
	if !s.cfg.Capabilities.SupportsOperatorAccess {
		writeOperatorError(w, http.StatusNotFound, "operator access is not supported by this supervisor")
		return false
	}
	if err := harnessv2.DecodeOperatorRequestHeader(r.Header.Get(harnessv2.OperatorRequestHeader), target); err != nil {
		writeOperatorError(w, http.StatusBadRequest, "invalid operator request")
		return false
	}
	if err := target.ValidateAt(time.Now().UTC()); err != nil {
		writeOperatorError(w, http.StatusBadRequest, "invalid operator request: "+err.Error())
		return false
	}
	// Operator access always requires the signed operation capability,
	// regardless of RequireCapabilities: only the controller-side API holds
	// the pool capability secret.
	if len(s.cfg.CapabilitySecret) < harnessv2.MinCapabilitySecretBytes ||
		harnessv2.VerifyOperationCapability(s.cfg.CapabilitySecret, r.Header.Get(OperationCapabilityHeader), metadata(), true, time.Now().UTC()) != nil {
		writeOperatorError(w, http.StatusForbidden, "operation capability authorization failed")
		return false
	}
	if !s.operatorState().consumeCapability(metadata().OperationID, metadata().ExpiresAt) {
		writeOperatorError(w, http.StatusConflict, "operator capability was already used")
		return false
	}
	return true
}

func (o *operatorAccessState) consumeCapability(operationID harnessv2.OperationID, expiresAt time.Time) bool {
	o.usedMu.Lock()
	defer o.usedMu.Unlock()
	now := time.Now().UTC()
	if o.used == nil {
		o.used = map[harnessv2.OperationID]time.Time{}
	}
	for id, expiry := range o.used {
		if !expiry.After(now) {
			delete(o.used, id)
		}
	}
	if _, seen := o.used[operationID]; seen {
		return false
	}
	o.used[operationID] = expiresAt
	return true
}

func (s *Server) acquireOperatorSlot(w http.ResponseWriter) (func(), bool) {
	state := s.operatorState()
	select {
	case state.slots <- struct{}{}:
		return func() { <-state.slots }, true
	default:
		writeOperatorError(w, http.StatusTooManyRequests, "too many concurrent operator operations")
		return nil, false
	}
}

// resolveOperatorCommand never echoes argv0 in its errors: they are relayed
// into orka-api audit records and Task Events, which must not carry raw argv.
func resolveOperatorCommand(argv0, childPath string) (string, error) {
	if filepath.IsAbs(argv0) {
		info, err := os.Stat(argv0)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("the command was not found or is not executable")
		}
		return filepath.Clean(argv0), nil
	}
	if strings.Contains(argv0, "/") {
		return "", fmt.Errorf("command must be a bare name resolved through PATH or an absolute path")
	}
	for dir := range strings.SplitSeq(childPath, ":") {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, argv0)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("the command was not found in the session PATH")
}

func (s *Server) handleOperatorExec(w http.ResponseWriter, r *http.Request) {
	var request harnessv2.OperatorExecRequest
	if !s.authorizeOperatorRequest(w, r, &request, func() harnessv2.MutationMetadata { return request.Metadata }) {
		return
	}
	sessionID := harnessv2.RuntimeSessionID(r.PathValue("sessionID"))
	target, status, message := s.operatorTargetFor(sessionID, request.Metadata)
	if status != 0 {
		writeOperatorError(w, status, message)
		return
	}
	lease := &operatorLease{target: target}
	defer s.finishOperation(lease)
	command, err := resolveOperatorCommand(request.Exec.Argv[0], target.childPath)
	if err != nil {
		writeOperatorError(w, http.StatusBadRequest, err.Error())
		return
	}
	release, ok := s.acquireOperatorSlot(w)
	if !ok {
		return
	}
	lease.releaseSlot = release
	values := map[string]string{}
	if request.Exec.TTY {
		values["TERM"] = request.Exec.Term
	}
	env, err := acp.BuildChildEnvironment(target.paths, acp.EnvironmentConfig{PATH: target.childPath, Values: values})
	if err != nil {
		writeOperatorError(w, http.StatusConflict, "operator environment is unavailable")
		return
	}
	tag, ok := s.operatorState().allocateTag()
	if !ok {
		writeOperatorError(w, http.StatusTooManyRequests, "no operator tag group is available")
		return
	}
	lease.tag = tag
	argv := append([]string{command}, request.Exec.Argv[1:]...)
	process, err := s.operatorState().starter(operatorLaunch{
		UID: target.uid, GID: target.gid, Tag: tag, Argv: argv, Env: env, Dir: target.paths.Workspace,
		TTY: request.Exec.TTY, Rows: request.Exec.Rows, Cols: request.Exec.Cols, Stdin: request.Exec.Stdin,
	})
	if err != nil {
		writeOperatorError(w, http.StatusConflict, "start operator command: "+err.Error())
		return
	}
	lease.process = process
	if !s.bindOperatorProcess(target, process) {
		// finishOperation kills and reaps it before the registration goes.
		writeOperatorError(w, http.StatusConflict, "runtime session left an operator-admitting state")
		return
	}
	// The listener's whole-request read deadline protects small control
	// requests; an authenticated interactive stream lives until its own
	// operation timeout instead.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{operatoraccess.WebSocketSubprotocol},
		// The caller is the authenticated controller-side API, never a
		// browser; there is no ambient cookie authority to protect.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(operatoraccess.MaxFrameBytes + 1)
	started := time.Now()
	final := runOperatorExec(r.Context(), conn, process, request.Exec)
	slog.Info("operator exec finished", "auditID", request.AuditID, "runtimeSessionID", string(sessionID),
		"exitCode", final.ExitCode, "reason", final.Reason, "durationMillis", time.Since(started).Milliseconds())
}

type operatorOutputBudget struct {
	limit int64
	used  atomic.Int64
}

func (b *operatorOutputBudget) take(n int) bool {
	return b.used.Add(int64(n)) <= b.limit
}

// runOperatorExec relays one operator process over the WebSocket until the
// process exits, its timeout or output limit is reached, or the caller
// disconnects. It always kills the process group before returning.
func runOperatorExec(parent context.Context, conn *websocket.Conn, process operatorProcess, options operatoraccess.ExecOptions) operatoraccess.Status {
	// The stream context outlives the command deadline so the final status
	// can still be delivered: the WebSocket library closes the connection
	// when an I/O context is cancelled.
	streamCtx, streamCancel := context.WithCancel(context.WithoutCancel(parent))
	defer streamCancel()
	ctx, cancel := context.WithTimeout(streamCtx, time.Duration(options.TimeoutSeconds)*time.Second)
	defer cancel()
	var writeMu sync.Mutex
	write := func(frame []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeCtx, writeCancel := context.WithTimeout(streamCtx, 30*time.Second)
		defer writeCancel()
		return conn.Write(writeCtx, websocket.MessageBinary, frame)
	}
	var reasonMu sync.Mutex
	reason := ""
	setReason := func(value string) {
		reasonMu.Lock()
		if reason == "" {
			reason = value
		}
		reasonMu.Unlock()
		process.Kill()
	}

	budget := &operatorOutputBudget{limit: options.MaxOutputBytes}
	var copiers sync.WaitGroup
	pump := func(channel byte, reader io.ReadCloser) {
		if reader == nil {
			return
		}
		copiers.Go(func() {
			buffer := make([]byte, 32<<10)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					if !budget.take(n) {
						setReason(operatoraccess.ExitReasonOutputLimit)
						return
					}
					if write(operatoraccess.Frame(channel, buffer[:n])) != nil {
						setReason(operatoraccess.ExitReasonDisconnect)
						return
					}
				}
				if err != nil {
					return
				}
			}
		})
	}
	pump(operatoraccess.ChannelStdout, process.Stdout())
	pump(operatoraccess.ChannelStderr, process.Stderr())

	go relayOperatorInput(streamCtx, conn, process, options, setReason)

	waited := make(chan struct{})
	var exitCode int
	var signaled bool
	var waitErr error
	go func() {
		exitCode, signaled, waitErr = process.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		setReason(operatoraccess.ExitReasonTimeout)
		<-waited
	}
	// Reap the process group, then give the output pumps a bounded grace to
	// flush. Readers are closed afterwards so a descendant that escaped the
	// group and still holds the pipe cannot pin the stream open.
	process.Kill()
	drained := make(chan struct{})
	go func() { copiers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(operatorOutputDrainGrace):
	}
	for _, closer := range []io.Closer{process.Stdin(), process.Stdout(), process.Stderr()} {
		if closer != nil {
			// Closing also interrupts a stdin write blocked on a command that
			// never read its input.
			_ = closer.Close()
		}
	}

	reasonMu.Lock()
	final := operatorFinalStatus(exitCode, signaled, waitErr, reason)
	reasonMu.Unlock()
	if final.Reason != operatoraccess.ExitReasonDisconnect {
		statusCtx, statusCancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		writeMu.Lock()
		_ = conn.Write(statusCtx, websocket.MessageBinary, operatoraccess.StatusFrame(final))
		writeMu.Unlock()
		statusCancel()
	}
	_ = conn.Close(websocket.StatusNormalClosure, final.Reason)
	return final
}

func operatorFinalStatus(exitCode int, signaled bool, waitErr error, reason string) operatoraccess.Status {
	final := operatoraccess.Status{ExitCode: exitCode, Reason: operatoraccess.ExitReasonExited}
	if signaled {
		final.Reason = operatoraccess.ExitReasonSignaled
	}
	if waitErr != nil {
		final = operatoraccess.Status{ExitCode: -1, Reason: operatoraccess.ExitReasonError, Message: "operator command wait failed"}
	}
	if reason != "" {
		final.Reason = reason
		if final.ExitCode == 0 {
			final.ExitCode = -1
		}
	}
	return final
}

// maxOperatorStdinBuffered bounds stdin accepted ahead of a command that is
// not reading it. The WebSocket reader never blocks on the child's stdin, so
// disconnects and control frames are always observed.
const maxOperatorStdinBuffered = 8 << 20

// relayOperatorInput forwards stdin and control frames until the stream ends.
func relayOperatorInput(
	streamCtx context.Context, conn *websocket.Conn, process operatorProcess,
	options operatoraccess.ExecOptions, setReason func(string),
) {
	queue := make(chan []byte, 1024)
	var buffered atomic.Int64
	go func() {
		stdin := process.Stdin()
		defer func() {
			if stdin != nil {
				_ = stdin.Close()
			}
		}()
		for chunk := range queue {
			if chunk == nil {
				return // ordered EOF
			}
			buffered.Add(-int64(len(chunk)))
			if stdin == nil {
				continue
			}
			if _, err := stdin.Write(chunk); err != nil {
				_ = stdin.Close()
				stdin = nil
			}
		}
	}()
	eof := false
	defer func() {
		if !eof {
			close(queue)
		}
	}()
	for {
		kind, payload, err := conn.Read(streamCtx)
		if err != nil {
			if streamCtx.Err() == nil {
				setReason(operatoraccess.ExitReasonDisconnect)
			}
			return
		}
		if kind != websocket.MessageBinary || len(payload) == 0 {
			continue
		}
		switch payload[0] {
		case operatoraccess.ChannelStdin:
			if eof || len(payload) == 1 {
				continue
			}
			chunk := append([]byte(nil), payload[1:]...)
			if buffered.Add(int64(len(chunk))) > maxOperatorStdinBuffered {
				setReason(operatoraccess.ExitReasonStdinOverflow)
				return
			}
			select {
			case queue <- chunk:
			default:
				setReason(operatoraccess.ExitReasonStdinOverflow)
				return
			}
		case operatoraccess.ChannelControl:
			var control operatoraccess.Control
			if json.Unmarshal(payload[1:], &control) != nil {
				continue
			}
			if control.Type == operatoraccess.ControlEOF {
				if !eof {
					eof = true
					select {
					case queue <- nil:
					default:
						setReason(operatoraccess.ExitReasonStdinOverflow)
						return
					}
					close(queue)
				}
				continue
			}
			applyOperatorControl(process, options, control)
		}
	}
}

// applyOperatorControl applies an allowed resize or signal request.
func applyOperatorControl(process operatorProcess, options operatoraccess.ExecOptions, control operatoraccess.Control) {
	switch control.Type {
	case operatoraccess.ControlResize:
		if options.TTY && control.Rows > 0 && control.Cols > 0 &&
			control.Rows <= operatoraccess.MaxTerminalDimension && control.Cols <= operatoraccess.MaxTerminalDimension {
			_ = process.Resize(control.Rows, control.Cols)
		}
	case operatoraccess.ControlSignal:
		if _, allowed := operatoraccess.AllowedSignals[control.Signal]; allowed {
			_ = process.Signal(control.Signal)
		}
	}
}

// handleOperatorFiles copies one regular file out of (GET) or into (PUT) the
// session workspace through a helper running as the session child identity.
func (s *Server) handleOperatorFiles(w http.ResponseWriter, r *http.Request) {
	var request harnessv2.OperatorFileRequest
	if !s.authorizeOperatorRequest(w, r, &request, func() harnessv2.MutationMetadata { return request.Metadata }) {
		return
	}
	if request.Write != (r.Method == http.MethodPut) {
		writeOperatorError(w, http.StatusBadRequest, "operator file request direction does not match the method")
		return
	}
	sessionID := harnessv2.RuntimeSessionID(r.PathValue("sessionID"))
	target, status, message := s.operatorTargetFor(sessionID, request.Metadata)
	if status != 0 {
		writeOperatorError(w, status, message)
		return
	}
	lease := &operatorLease{target: target}
	defer s.finishOperation(lease)
	helper := s.operatorState().runtimeExecutable
	if helper == "" || !filepath.IsAbs(helper) {
		writeOperatorError(w, http.StatusServiceUnavailable, "operator file helper is unavailable")
		return
	}
	release, ok := s.acquireOperatorSlot(w)
	if !ok {
		return
	}
	lease.releaseSlot = release
	mode := OperatorFileHelperRead
	if request.Write {
		mode = OperatorFileHelperWrite
	}
	// The recorded numbers name the workspace only while the supervisor still
	// holds that directory open; see acp.SessionPaths.
	workspaceDevice, workspaceInode, err := target.paths.PinnedWorkspaceIdentity()
	if err != nil {
		writeOperatorError(w, http.StatusConflict, "runtime session workspace identity is unavailable")
		return
	}
	identity := strconv.FormatUint(workspaceDevice, 10) + ":" + strconv.FormatUint(workspaceInode, 10)
	env, err := acp.BuildChildEnvironment(target.paths, acp.EnvironmentConfig{PATH: target.childPath})
	if err != nil {
		writeOperatorError(w, http.StatusConflict, "operator environment is unavailable")
		return
	}
	tag, ok := s.operatorState().allocateTag()
	if !ok {
		writeOperatorError(w, http.StatusTooManyRequests, "no operator tag group is available")
		return
	}
	lease.tag = tag
	process, err := s.operatorState().starter(operatorLaunch{
		UID: target.uid, GID: target.gid, Tag: tag, Env: env, Dir: target.paths.Workspace, Stdin: request.Write,
		Argv: []string{helper, OperatorFileHelperSubcommand, mode, target.paths.Workspace, identity, request.Path,
			strconv.FormatInt(operatorFileHelperBound(request), 10)},
	})
	if err != nil {
		writeOperatorError(w, http.StatusConflict, "start operator file helper: "+err.Error())
		return
	}
	lease.process = process
	if !s.bindOperatorProcess(target, process) {
		writeOperatorError(w, http.StatusConflict, "runtime session left an operator-admitting state")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), operatorFileTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { process.Kill() })
	defer stop()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(operatorFileTimeout))
	_ = controller.SetWriteDeadline(time.Now().Add(operatorFileTimeout))

	if request.Write {
		// The helper commits only after reading exactly request.Size bytes
		// followed by EOF, so a truncated or oversized upload never replaces
		// the target. One extra byte is forwarded to prove oversize.
		body := http.MaxBytesReader(w, r.Body, request.Size+1)
		stdin := process.Stdin()
		copied := make(chan struct{})
		go func() {
			defer close(copied)
			if operatorUploadCopyStarted != nil {
				operatorUploadCopyStarted()
			}
			_, _ = io.Copy(stdin, body)
			_ = stdin.Close()
		}()
		// The handler must not return while the copy still reads r.Body or
		// uses w through MaxBytesReader: a helper that fails before reading
		// its stdin would otherwise leave it blocked on the client.
		defer func() {
			select {
			case <-copied:
				return
			default:
			}
			_ = controller.SetReadDeadline(time.Now())
			_ = stdin.Close()
			process.Kill()
			<-copied
		}()
		result, ok := readOperatorFileStatus(process.Stdout())
		if !ok {
			writeOperatorError(w, http.StatusBadGateway, "operator file helper failed")
			return
		}
		if !result.OK {
			writeOperatorError(w, result.httpStatus(), result.Error)
			return
		}
		writeJSON(w, http.StatusOK, harnessv2.OperatorFileResult{Protocol: harnessv2.ProtocolVersion, Bytes: result.Size})
		return
	}

	stdout := bufio.NewReaderSize(process.Stdout(), 64<<10)
	result, ok := readOperatorFileStatus(stdout)
	if !ok {
		writeOperatorError(w, http.StatusBadGateway, "operator file helper failed")
		return
	}
	if !result.OK {
		writeOperatorError(w, result.httpStatus(), result.Error)
		return
	}
	if result.Size < 0 || result.Size > request.MaxBytes {
		writeOperatorError(w, http.StatusRequestEntityTooLarge, "file exceeds the size bound")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	w.WriteHeader(http.StatusOK)
	if copied, err := io.CopyN(w, stdout, result.Size); err != nil || copied != result.Size {
		// The declared length cannot be met; abort so the caller observes a
		// truncated transfer instead of a short file.
		panic(http.ErrAbortHandler)
	}
}

func operatorFileHelperBound(request harnessv2.OperatorFileRequest) int64 {
	if request.Write {
		return request.Size
	}
	return request.MaxBytes
}

// operatorUploadCopyStarted is a test hook run when an upload body copy
// starts.
var operatorUploadCopyStarted func()

// OperatorFileHelperSubcommand is the hidden orka-acp-runtime subcommand that
// implements operator file copies as the session child identity.
const (
	OperatorFileHelperSubcommand = "__orka_operator_file"
	OperatorFileHelperRead       = "read"
	OperatorFileHelperWrite      = "write"
)

type operatorFileStatus struct {
	OK    bool   `json:"ok"`
	Size  int64  `json:"size,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

const (
	operatorFileCodeNotFound   = "not_found"
	operatorFileCodeNotRegular = "not_regular"
	operatorFileCodeTooLarge   = "too_large"
	operatorFileCodeInvalid    = "invalid"
	operatorFileCodeDenied     = "denied"
	operatorFileCodeIO         = "io"
)

func (s operatorFileStatus) httpStatus() int {
	switch s.Code {
	case operatorFileCodeNotFound:
		return http.StatusNotFound
	case operatorFileCodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case operatorFileCodeDenied:
		return http.StatusForbidden
	case operatorFileCodeNotRegular, operatorFileCodeInvalid:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func readOperatorFileStatus(reader io.Reader) (operatorFileStatus, bool) {
	line := make([]byte, 0, 128)
	one := make([]byte, 1)
	for len(line) < operatorFileStatusLineBytes {
		n, err := reader.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				var status operatorFileStatus
				if json.Unmarshal(line, &status) != nil {
					return operatorFileStatus{}, false
				}
				return status, true
			}
			line = append(line, one[0])
		}
		if err != nil {
			return operatorFileStatus{}, false
		}
	}
	return operatorFileStatus{}, false
}

// realOperatorProcess runs through the ACP exec helper.
type realOperatorProcess struct {
	waitOnce sync.Once
	exitCode int
	signaled bool
	waitErr  error
	uid      int
	tag      int
	// reapMu fences process-group signals against reaping the leader: the
	// group ID is the leader's PID, which stays allocated (alive or zombie)
	// until it is reaped, so a signal sent under reapMu before reaped is set
	// can only reach this operation's group. That holds only if nothing else
	// reaps the leader, so it is withheld from the session-wide reaper until
	// releaseLeader is called, after wait has reaped it.
	reapMu        sync.Mutex
	reaped        bool
	releaseLeader func()
	// killMu serializes kills; once retired, the tag group may be reused by
	// a later operation, so later Kill calls must not scan for it.
	killMu  sync.Mutex
	retired bool
	cmd     *exec.Cmd
	master  *os.File
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
}

func startOperatorProcess(launch operatorLaunch) (operatorProcess, error) {
	cfg := acp.OperatorCommandConfig{UID: launch.UID, GID: launch.GID, TagGID: launch.Tag, Argv: launch.Argv, Environment: launch.Env, Dir: launch.Dir}
	process := &realOperatorProcess{uid: launch.UID, tag: launch.Tag}
	var closeAfterStart []*os.File
	closeAll := func() {
		for _, file := range closeAfterStart {
			_ = file.Close()
		}
	}
	if launch.TTY {
		master, slave, err := acp.OpenOperatorPTY(launch.UID, launch.GID, launch.Rows, launch.Cols)
		if err != nil {
			return nil, err
		}
		cfg.TTY = slave
		process.master = master
		process.stdin, process.stdout = master, master
		closeAfterStart = append(closeAfterStart, slave)
	}
	cmd, err := acp.NewOperatorCommand(cfg)
	if err != nil {
		closeAll()
		if process.master != nil {
			_ = process.master.Close()
		}
		return nil, err
	}
	if !launch.TTY {
		stdoutRead, stdoutWrite, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		stderrRead, stderrWrite, err := os.Pipe()
		if err != nil {
			_ = stdoutRead.Close()
			_ = stdoutWrite.Close()
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = stdoutWrite, stderrWrite
		process.stdout, process.stderr = stdoutRead, stderrRead
		closeAfterStart = append(closeAfterStart, stdoutWrite, stderrWrite)
		if launch.Stdin {
			stdinRead, stdinWrite, err := os.Pipe()
			if err != nil {
				closeAll()
				_ = stdoutRead.Close()
				_ = stderrRead.Close()
				return nil, err
			}
			cmd.Stdin = stdinRead
			process.stdin = stdinWrite
			closeAfterStart = append(closeAfterStart, stdinRead)
		}
	}
	releaseLeader, err := startOperatorCommand(cmd)
	if err != nil {
		closeAll()
		for _, file := range []io.Closer{process.stdin, process.stdout, process.stderr} {
			if file != nil {
				_ = file.Close()
			}
		}
		return nil, err
	}
	closeAll()
	process.cmd, process.releaseLeader = cmd, releaseLeader
	return process, nil
}

func (p *realOperatorProcess) Stdin() io.WriteCloser {
	if p.stdin == nil {
		return nil
	}
	if p.master != nil {
		// Closing the terminal master would hang up the session; EOF on a
		// terminal is the operator's own ^D.
		return nopWriteCloser{p.master}
	}
	return p.stdin
}
func (p *realOperatorProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *realOperatorProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *realOperatorProcess) Resize(rows, cols uint16) error {
	if p.master == nil {
		return nil
	}
	return acp.ResizeOperatorPTY(p.master, rows, cols)
}

// Replaceable in tests.
var (
	startOperatorCommand     = acp.StartOperatorCommand
	signalOperatorGroup      = acp.SignalOperatorCommand
	killOperatorTagProcesses = acp.KillOperatorTag
)

func (p *realOperatorProcess) Signal(name string) error {
	if name == "KILL" {
		// Kill the whole tagged operation, not just the process group.
		p.Kill()
		return nil
	}
	signal, ok := acp.OperatorSignal(name)
	if !ok {
		return fmt.Errorf("signal %q is not allowed", name)
	}
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.reaped {
		// The leader's PID, and with it the group ID, may already belong to
		// an unrelated process group.
		return nil
	}
	return signalOperatorGroup(p.cmd, signal)
}

func (p *realOperatorProcess) Wait() (int, bool, error) {
	p.waitOnce.Do(func() { p.exitCode, p.signaled, p.waitErr = p.wait() })
	return p.exitCode, p.signaled, p.waitErr
}

func (p *realOperatorProcess) wait() (int, bool, error) {
	// Wait for the leader to exit without reaping it, then reap only while
	// no group signal is in flight; see reapMu.
	if p.cmd.Process != nil {
		waitOperatorLeaderExit(p.cmd.Process.Pid)
	}
	p.reapMu.Lock()
	p.reaped = true
	err := p.cmd.Wait()
	p.reapMu.Unlock()
	// Only now may the session-wide reaper see this PID again: it is reaped,
	// and no signal can be sent to its group any more.
	if p.releaseLeader != nil {
		p.releaseLeader()
	}
	state := p.cmd.ProcessState
	if state == nil {
		return -1, false, err
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal()), true, nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return -1, false, err
	}
	return state.ExitCode(), false, nil
}

func (p *realOperatorProcess) Kill() bool {
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.retired {
		return true
	}
	return p.killLocked()
}

func (p *realOperatorProcess) Retire() bool {
	p.killMu.Lock()
	defer p.killMu.Unlock()
	if p.retired {
		return true
	}
	proven := p.killLocked()
	p.retired = true
	return proven
}

func (p *realOperatorProcess) killLocked() bool {
	if p.cmd == nil || p.cmd.Process == nil {
		return true
	}
	// The tag reaches every descendant, wherever it moved; killing the
	// leader directly also unblocks Wait if it left its own group.
	proven := killOperatorTagProcesses(p.uid, p.tag)
	_ = p.cmd.Process.Kill()
	return proven
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
