package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

const operatorTestToken = "operator-test-controller-token-0123456789abcdef"

var operatorTestSecret = bytes.Repeat([]byte("c"), harnessv2.MinCapabilitySecretBytes)

type fakeOperatorProcess struct {
	stdinR     *io.PipeReader
	stdinW     *io.PipeWriter
	stdoutR    *io.PipeReader
	stdoutW    *io.PipeWriter
	exit       chan int
	killed     atomic.Bool
	mu         sync.Mutex
	signals    []string
	once       sync.Once
	unkillable bool
}

func newFakeOperatorProcess() *fakeOperatorProcess {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	return &fakeOperatorProcess{stdinR: stdinR, stdinW: stdinW, stdoutR: stdoutR, stdoutW: stdoutW, exit: make(chan int, 1)}
}

func (p *fakeOperatorProcess) finish(code int) {
	p.once.Do(func() {
		_ = p.stdoutW.Close()
		p.exit <- code
	})
}

func (p *fakeOperatorProcess) Stdin() io.WriteCloser       { return p.stdinW }
func (p *fakeOperatorProcess) Stdout() io.ReadCloser       { return p.stdoutR }
func (p *fakeOperatorProcess) Stderr() io.ReadCloser       { return nil }
func (p *fakeOperatorProcess) Resize(uint16, uint16) error { return nil }
func (p *fakeOperatorProcess) Kill() bool {
	p.killed.Store(true)
	p.finish(137)
	return !p.unkillable
}
func (p *fakeOperatorProcess) Wait() (int, bool, error) {
	code := <-p.exit
	p.exit <- code
	return code, code == 137, nil
}
func (p *fakeOperatorProcess) Signal(name string) error {
	p.mu.Lock()
	p.signals = append(p.signals, name)
	p.mu.Unlock()
	if name == "INT" {
		p.finish(130)
	}
	return nil
}

func operatorTestFence() harnessv2.Fence {
	return harnessv2.Fence{
		RuntimeInstanceID: "pod-uid.boot", SupervisorBootID: "boot", ControllerEpoch: 3,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1,
		RuntimeProfileDigest:       harnessv2.ProfileDigest("sha256:" + strings.Repeat("b", 64)),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
}

func newOperatorTestServer(t *testing.T, state harnessv2.RuntimeSessionState, starter operatorStarter) (*Server, *httptest.Server, harnessv2.RuntimeSessionID) {
	t.Helper()
	root := t.TempDir()
	paths := acp.SessionPaths{Root: root}
	for name, target := range map[string]*string{"home": &paths.Home, "tmp": &paths.Temp, "workspace": &paths.Workspace,
		"config": &paths.Config, "cache": &paths.Cache, "data": &paths.Data, "state": &paths.State} {
		*target = filepath.Join(root, name)
		if err := os.MkdirAll(*target, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{
		cfg: Config{
			ControllerBearerToken: operatorTestToken, CapabilitySecret: operatorTestSecret, Fence: operatorTestFence(),
			Capabilities: harnessv2.CapabilitiesResponse{SupportsOperatorAccess: true},
		},
		mux:      http.NewServeMux(),
		sessions: map[harnessv2.RuntimeSessionID]*sessionState{},
	}
	server.operator.starter = starter
	server.registerOperatorRoutes()
	sessionID := harnessv2.RuntimeSessionID("runtime-session-uid-g2")
	server.sessions[sessionID] = &sessionState{
		id: sessionID, runtime: &acp.RuntimeSession{}, paths: paths,
		descriptor: harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: "session-uid", Generation: 2, State: state},
	}
	previous := operatorChildIdentity
	operatorChildIdentity = func(*sessionState) (int, int) { return 20001, 20000 }
	t.Cleanup(func() { operatorChildIdentity = previous })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return server, httpServer, sessionID
}

func signedOperatorExecHeaders(t *testing.T, sessionGeneration uint64, options operatoraccess.ExecOptions) http.Header {
	t.Helper()
	fence := operatorTestFence()
	fence.RuntimeSessionUID = "session-uid"
	fence.RuntimeSessionGeneration = sessionGeneration
	if err := options.Normalize(); err != nil {
		t.Fatal(err)
	}
	request := harnessv2.OperatorExecRequest{
		Protocol: harnessv2.ProtocolVersion, AuditID: "op-test",
		Metadata: harnessv2.MutationMetadata{Fence: fence, OperationID: "operator-exec-op-test", ExpiresAt: time.Now().UTC().Add(time.Minute)},
		Exec:     options,
	}
	header, capability, err := harnessv2.SealOperatorRequest(operatorTestSecret, &request, &request.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+operatorTestToken)
	headers.Set(OperationCapabilityHeader, capability)
	headers.Set(harnessv2.OperatorRequestHeader, header)
	return headers
}

func operatorExecURL(server *httptest.Server, sessionID harnessv2.RuntimeSessionID) string {
	path, _ := harnessv2.OperatorExecPath(sessionID)
	return server.URL + path
}

func TestOperatorExecRefusals(t *testing.T) {
	starter := func(operatorLaunch) (operatorProcess, error) {
		t.Fatal("a refused request must never start a process")
		return nil, nil
	}
	options := operatoraccess.ExecOptions{Argv: []string{"sh"}}
	for _, tc := range []struct {
		name       string
		state      harnessv2.RuntimeSessionState
		headers    func() http.Header
		wantStatus int
	}{
		{"missing bearer", harnessv2.RuntimeSessionStateIdle, func() http.Header {
			h := signedOperatorExecHeaders(t, 2, options)
			h.Del("Authorization")
			return h
		}, http.StatusUnauthorized},
		{"missing capability", harnessv2.RuntimeSessionStateIdle, func() http.Header {
			h := signedOperatorExecHeaders(t, 2, options)
			h.Del(OperationCapabilityHeader)
			return h
		}, http.StatusForbidden},
		{"stale session generation", harnessv2.RuntimeSessionStateIdle, func() http.Header {
			return signedOperatorExecHeaders(t, 1, options)
		}, http.StatusConflict},
		{"validating workspace is frozen", harnessv2.RuntimeSessionStateValidating, func() http.Header {
			return signedOperatorExecHeaders(t, 2, options)
		}, http.StatusConflict},
		{"publication prepared", harnessv2.RuntimeSessionStatePublicationPrepared, func() http.Header {
			return signedOperatorExecHeaders(t, 2, options)
		}, http.StatusConflict},
		{"publishing", harnessv2.RuntimeSessionStatePublishing, func() http.Header {
			return signedOperatorExecHeaders(t, 2, options)
		}, http.StatusConflict},
		{"finalizing", harnessv2.RuntimeSessionStateFinalizing, func() http.Header {
			return signedOperatorExecHeaders(t, 2, options)
		}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, server, sessionID := newOperatorTestServer(t, tc.state, starter)
			request, _ := http.NewRequest(http.MethodGet, operatorExecURL(server, sessionID), nil)
			request.Header = tc.headers()
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close() //nolint:errcheck
			if response.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, tc.wantStatus, body)
			}
		})
	}
}

func TestOperatorCapabilityIsHiddenFromOldClients(t *testing.T) {
	server := &Server{cfg: Config{Capabilities: harnessv2.CapabilitiesResponse{SupportsOperatorAccess: true}}}
	for header, want := range map[string]bool{"": false, "1": true} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, harnessv2.CapabilitiesPath, nil)
		if header != "" {
			request.Header.Set(OperatorAccessCapabilityHeader, header)
		}
		server.handleCapabilities(recorder, request)
		var response harnessv2.CapabilitiesResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.SupportsOperatorAccess != want {
			t.Fatalf("header %q: SupportsOperatorAccess = %v", header, response.SupportsOperatorAccess)
		}
	}
}

type operatorStreamResult struct {
	stdout string
	status operatoraccess.Status
}

func runOperatorTestStream(t *testing.T, url string, headers http.Header, send func(context.Context, *websocket.Conn)) operatorStreamResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{
		HTTPHeader: headers, Subprotocols: []string{operatoraccess.WebSocketSubprotocol},
	})
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("dial: %v: %s", err, body)
		}
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow() //nolint:errcheck
	conn.SetReadLimit(operatoraccess.MaxFrameBytes + 1)
	if send != nil {
		send(ctx, conn)
	}
	var result operatorStreamResult
	for {
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("stream ended without a status: %v", err)
		}
		switch payload[0] {
		case operatoraccess.ChannelStdout:
			result.stdout += string(payload[1:])
		case operatoraccess.ChannelStatus:
			if err := json.Unmarshal(payload[1:], &result.status); err != nil {
				t.Fatal(err)
			}
			return result
		}
	}
}

func TestOperatorExecStreamsStdinAndExitStatus(t *testing.T) {
	process := newFakeOperatorProcess()
	var launched operatorLaunch
	go func() {
		// Echo stdin to stdout until EOF, then exit 3.
		_, _ = io.Copy(process.stdoutW, process.stdinR)
		process.finish(3)
	}()
	_, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStatePromptRunning,
		func(launch operatorLaunch) (operatorProcess, error) {
			launched = launch
			return process, nil
		})
	result := runOperatorTestStream(t, operatorExecURL(server, sessionID),
		signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh", "-c", "cat"}, Stdin: true}),
		func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.Frame(operatoraccess.ChannelStdin, []byte("hello")))
			_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.ControlFrame(operatoraccess.Control{Type: operatoraccess.ControlEOF}))
		})
	if result.stdout != "hello" || result.status.ExitCode != 3 || result.status.Reason != operatoraccess.ExitReasonExited {
		t.Fatalf("unexpected result: %+v", result)
	}
	if launched.UID != 20001 || launched.GID != 20000 || !filepath.IsAbs(launched.Argv[0]) || launched.Argv[1] != "-c" ||
		launched.Tag < acp.OperatorTagFirstGID || launched.Tag > acp.OperatorTagLastGID {
		t.Fatalf("operator command must run as the session child identity with a resolved command: %+v", launched)
	}
	if !strings.HasSuffix(launched.Dir, "workspace") {
		t.Fatalf("operator command must start in the session workspace, got %q", launched.Dir)
	}
	for _, entry := range launched.Env {
		if strings.Contains(strings.ToLower(entry), "token") || strings.Contains(strings.ToLower(entry), "secret") {
			t.Fatalf("operator environment leaked a credential-shaped variable: %q", entry)
		}
	}
}

func TestOperatorExecForwardsInterrupt(t *testing.T) {
	process := newFakeOperatorProcess()
	_, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle,
		func(operatorLaunch) (operatorProcess, error) { return process, nil })
	result := runOperatorTestStream(t, operatorExecURL(server, sessionID),
		signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}}),
		func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.ControlFrame(operatoraccess.Control{Type: operatoraccess.ControlSignal, Signal: "INT"}))
			// Disallowed signals are ignored.
			_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.ControlFrame(operatoraccess.Control{Type: operatoraccess.ControlSignal, Signal: "STOP"}))
		})
	if result.status.ExitCode != 130 {
		t.Fatalf("unexpected status: %+v", result.status)
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	if len(process.signals) != 1 || process.signals[0] != "INT" {
		t.Fatalf("forwarded signals = %v", process.signals)
	}
}

func TestOperatorExecEnforcesOutputLimit(t *testing.T) {
	process := newFakeOperatorProcess()
	go func() { _, _ = process.stdoutW.Write(bytes.Repeat([]byte("x"), 64)) }()
	_, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle,
		func(operatorLaunch) (operatorProcess, error) { return process, nil })
	result := runOperatorTestStream(t, operatorExecURL(server, sessionID),
		signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}, MaxOutputBytes: 16}), nil)
	if result.status.Reason != operatoraccess.ExitReasonOutputLimit || !process.killed.Load() {
		t.Fatalf("output limit not enforced: %+v killed=%v", result.status, process.killed.Load())
	}
	if len(result.stdout) > 16 {
		t.Fatalf("relayed %d bytes past the limit", len(result.stdout))
	}
}

func TestOperatorExecEnforcesTimeout(t *testing.T) {
	process := newFakeOperatorProcess()
	_, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle,
		func(operatorLaunch) (operatorProcess, error) { return process, nil })
	result := runOperatorTestStream(t, operatorExecURL(server, sessionID),
		signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}, TimeoutSeconds: 1}), nil)
	if result.status.Reason != operatoraccess.ExitReasonTimeout || !process.killed.Load() {
		t.Fatalf("timeout not enforced: %+v", result.status)
	}
}

func TestOperatorExecLimitsConcurrency(t *testing.T) {
	supervisor, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle,
		func(operatorLaunch) (operatorProcess, error) { return newFakeOperatorProcess(), nil })
	for range maxConcurrentOperatorOperations {
		supervisor.operatorState().slots <- struct{}{}
	}
	request, _ := http.NewRequest(http.MethodGet, operatorExecURL(server, sessionID), nil)
	request.Header = signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.StatusCode)
	}
}

func TestOperatorCapabilityIsSingleUse(t *testing.T) {
	_, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateValidating,
		func(operatorLaunch) (operatorProcess, error) { return newFakeOperatorProcess(), nil })
	headers := signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}})
	var statuses []int
	for range 2 {
		request, _ := http.NewRequest(http.MethodGet, operatorExecURL(server, sessionID), nil)
		request.Header = headers.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		statuses = append(statuses, response.StatusCode)
		if len(statuses) == 2 && !strings.Contains(string(body), "already used") {
			t.Fatalf("replayed capability was not refused as reused: %s", body)
		}
	}
	if statuses[1] != http.StatusConflict {
		t.Fatalf("statuses = %v", statuses)
	}
}

func TestOperatorExecIsKilledWhenTheSessionFreezes(t *testing.T) {
	process := newFakeOperatorProcess()
	started := make(chan struct{})
	supervisor, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStatePromptRunning,
		func(operatorLaunch) (operatorProcess, error) { close(started); return process, nil })
	go func() {
		<-started
		// Wait until the process is bound, then settle the prompt.
		for {
			supervisor.mu.Lock()
			state := supervisor.sessions[sessionID]
			bound := false
			for _, registered := range state.operators {
				bound = bound || registered.process != nil
			}
			if bound {
				setSessionState(state, harnessv2.RuntimeSessionStateValidating)
				supervisor.mu.Unlock()
				return
			}
			supervisor.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	result := runOperatorTestStream(t, operatorExecURL(server, sessionID),
		signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}}), nil)
	if !process.killed.Load() || result.status.ExitCode == 0 {
		t.Fatalf("operator process survived the session freeze: %+v", result.status)
	}
	// The registration is removed only after the handler's proven cleanup.
	deadline := time.Now().Add(5 * time.Second)
	for {
		supervisor.mu.Lock()
		remaining := len(supervisor.sessions[sessionID].operators)
		supervisor.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operator registration leaked after revocation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOperatorExecRefusedWhenSessionFreezesDuringStart(t *testing.T) {
	process := newFakeOperatorProcess()
	var supervisor *Server
	var sessionID harnessv2.RuntimeSessionID
	supervisor, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStatePromptRunning,
		func(operatorLaunch) (operatorProcess, error) {
			supervisor.mu.Lock()
			setSessionState(supervisor.sessions[sessionID], harnessv2.RuntimeSessionStateValidating)
			supervisor.mu.Unlock()
			return process, nil
		})
	request, _ := http.NewRequest(http.MethodGet, operatorExecURL(server, sessionID), nil)
	request.Header = signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusConflict || !process.killed.Load() {
		t.Fatalf("status = %d killed = %v; a process started across a freeze must be killed", response.StatusCode, process.killed.Load())
	}
}

func TestOperatorExecObservesControlsWhileStdinIsBlocked(t *testing.T) {
	// The fake never reads stdin, so every stdin write blocks.
	process := newFakeOperatorProcess()
	_, server, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle,
		func(operatorLaunch) (operatorProcess, error) { return process, nil })
	result := runOperatorTestStream(t, operatorExecURL(server, sessionID),
		signedOperatorExecHeaders(t, 2, operatoraccess.ExecOptions{Argv: []string{"sh"}, Stdin: true}),
		func(ctx context.Context, conn *websocket.Conn) {
			for range 4 {
				_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.Frame(operatoraccess.ChannelStdin, bytes.Repeat([]byte("x"), 1024)))
			}
			_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.ControlFrame(operatoraccess.Control{Type: operatoraccess.ControlSignal, Signal: "INT"}))
		})
	if result.status.ExitCode != 130 {
		t.Fatalf("interrupt was not observed behind blocked stdin: %+v", result.status)
	}
}

func TestOperatorTagsAreUniqueAndSkipLiveTags(t *testing.T) {
	previous := operatorTagInUse
	t.Cleanup(func() { operatorTagInUse = previous })
	operatorTagInUse = func(tag int) bool { return tag == acp.OperatorTagFirstGID }
	var state operatorAccessState
	first, ok := state.allocateTag()
	if !ok || first == acp.OperatorTagFirstGID {
		t.Fatalf("allocated a tag still carried by a live process: %d", first)
	}
	second, ok := state.allocateTag()
	if !ok || second == first {
		t.Fatalf("tags must be unique among live operations: %d %d", first, second)
	}
	state.releaseTag(first)
	operatorTagInUse = func(int) bool { return true }
	if _, ok := state.allocateTag(); ok {
		t.Fatal("allocation must fail closed when every tag is in use")
	}
}

func TestOperatorQuiesceBarrierBlocksLaunchesAndKillsOperations(t *testing.T) {
	supervisor, _, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStatePromptRunning, nil)
	fence := operatorTestFence()
	fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "session-uid", 2
	metadata := harnessv2.MutationMetadata{Fence: fence}
	bound, status, _ := supervisor.operatorTargetFor(sessionID, metadata)
	pending, status2, _ := supervisor.operatorTargetFor(sessionID, metadata)
	if status != 0 || status2 != 0 {
		t.Fatalf("admission failed: %d %d", status, status2)
	}
	running := newFakeOperatorProcess()
	if !supervisor.bindOperatorProcess(bound, running) {
		t.Fatal("bind failed")
	}
	// The bound operation's handler finishes once its process is killed.
	go func() {
		_, _, _ = running.Wait()
		supervisor.finishOperation(&operatorLease{target: bound, process: running})
	}()
	// The pending launch completes after the barrier started; it must be
	// refused and its registration must survive until its cleanup finished.
	late := newFakeOperatorProcess()
	lateFinished := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		if supervisor.bindOperatorProcess(pending, late) {
			t.Error("a launch completing during the barrier was bound")
		}
		supervisor.finishOperation(&operatorLease{target: pending, process: late})
		close(lateFinished)
	}()
	state := supervisor.sessions[sessionID]
	if err := supervisor.quiesceOperators(state, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lateFinished:
	default:
		t.Fatal("the barrier returned before an in-flight launch finished its cleanup")
	}
	if !running.killed.Load() || !late.killed.Load() {
		t.Fatalf("barrier returned with live operator operations: running=%v late=%v", running.killed.Load(), late.killed.Load())
	}
	if _, status, _ := supervisor.operatorTargetFor(sessionID, metadata); status == 0 {
		t.Fatal("admission must stay closed after the barrier")
	}
	// Returning to an admitting state reopens admission.
	supervisor.mu.Lock()
	setSessionState(state, harnessv2.RuntimeSessionStateValidating)
	setSessionState(state, harnessv2.RuntimeSessionStateIdle)
	supervisor.mu.Unlock()
	target, status, message := supervisor.operatorTargetFor(sessionID, metadata)
	if status != 0 {
		t.Fatalf("admission did not reopen: %d %s", status, message)
	}
	supervisor.finishOperation(&operatorLease{target: target})
}

func TestOperatorUnprovenCleanupFailsClosed(t *testing.T) {
	supervisor, _, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle, nil)
	state := supervisor.sessions[sessionID]
	state.drainCleanupScheduled = true
	fence := operatorTestFence()
	fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "session-uid", 2
	target, status, _ := supervisor.operatorTargetFor(sessionID, harnessv2.MutationMetadata{Fence: fence})
	if status != 0 {
		t.Fatal("admission failed")
	}
	process := newFakeOperatorProcess()
	process.unkillable = true
	if !supervisor.bindOperatorProcess(target, process) {
		t.Fatal("bind failed")
	}
	slotReleased := false
	tag, _ := supervisor.operatorState().allocateTag()
	supervisor.finishOperation(&operatorLease{target: target, process: process, tag: tag, releaseSlot: func() { slotReleased = true }})
	supervisor.mu.Lock()
	registrations := len(state.operators)
	poisoned := state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned && supervisor.poisoned
	supervisor.mu.Unlock()
	if slotReleased || registrations != 1 || !poisoned {
		t.Fatalf("unproven cleanup must poison and hold resources: slot=%v registrations=%d poisoned=%v", slotReleased, registrations, poisoned)
	}
	if _, held := supervisor.operatorState().tags[tag]; !held {
		t.Fatal("the tag of an uncontained operation was released for reuse")
	}
	if err := supervisor.quiesceOperators(state, 50*time.Millisecond); err == nil {
		t.Fatal("the barrier must fail closed while an operation is uncontained")
	}
	if _, status, _ := supervisor.operatorTargetFor(sessionID, harnessv2.MutationMetadata{Fence: fence}); status == 0 {
		t.Fatal("a poisoned session must not admit operator access")
	}
}

func TestOperatorQuiesceFailsClosedOnStuckLaunch(t *testing.T) {
	supervisor, _, sessionID := newOperatorTestServer(t, harnessv2.RuntimeSessionStateIdle, nil)
	fence := operatorTestFence()
	fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "session-uid", 2
	if _, status, _ := supervisor.operatorTargetFor(sessionID, harnessv2.MutationMetadata{Fence: fence}); status != 0 {
		t.Fatal("admission failed")
	}
	if err := supervisor.quiesceOperators(supervisor.sessions[sessionID], 50*time.Millisecond); err == nil {
		t.Fatal("a launch that never settles must fail the barrier closed")
	}
}
