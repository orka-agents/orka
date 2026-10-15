package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/hyperlight"
)

const (
	// EnvSandboxExecEnabled turns on the runtime-local sandbox_exec tool. The
	// controller sets it with the Hyperlight device and bundle it adds to the
	// Pod (--acp-sandbox-exec).
	EnvSandboxExecEnabled = "ORKA_ACP_SANDBOX_EXEC_ENABLED"
	// EnvHyperlightDeviceGID is the group that owns the hypervisor device.
	// Session users get it only for the micro-VM a sandbox_exec call runs.
	EnvHyperlightDeviceGID = "ORKA_HYPERLIGHT_DEVICE_GID"
)

// SandboxExecConfig configures the runtime-local sandbox_exec tool.
type SandboxExecConfig struct {
	Hyperlight hyperlight.Config
	// DeviceGID is the hypervisor device's group; 0 adds none.
	DeviceGID uint32
}

func sandboxExecConfigFromEnv() (*SandboxExecConfig, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(EnvSandboxExecEnabled)), "true") {
		return nil, nil
	}
	cfg := &SandboxExecConfig{Hyperlight: hyperlight.ConfigFromEnv()}
	// Session users share this Pod: the snapshot cache must be closed to them.
	cfg.Hyperlight.SharedPod = true
	if value := strings.TrimSpace(os.Getenv(EnvHyperlightDeviceGID)); value != "" {
		gid, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvHyperlightDeviceGID, err)
		}
		cfg.DeviceGID = uint32(gid)
	}
	return cfg, nil
}

// protectSandboxSessionRoot keeps the workspace entry under a parent the
// provider child can traverse but cannot rename or populate. Call only before
// child startup or while the session is frozen, after ownership finalization.
func (s *Server) protectSandboxSessionRoot(root string, uid, gid int) error {
	if s.cfg.SandboxExec == nil {
		return nil
	}
	if uid == os.Geteuid() || uid <= 0 || gid <= 0 {
		return fmt.Errorf("sandbox_exec requires a session identity distinct from the supervisor")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("sandbox session root must be a real directory")
	}
	// Re-own before opening or chmod: the supervisor has CHOWN, but not
	// DAC_OVERRIDE or FOWNER, and the child-owned root currently has mode 0700.
	if err := os.Lchown(root, os.Geteuid(), gid); err != nil {
		return fmt.Errorf("reclaim sandbox session parent: %w", err)
	}
	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("open sandbox session parent: %w", err)
	}
	defer directory.Close() //nolint:errcheck
	opened, err := directory.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return fmt.Errorf("sandbox session parent changed while opening")
	}
	if err := directory.Chmod(0o710); err != nil {
		return fmt.Errorf("protect sandbox session parent: %w", err)
	}
	return nil
}

// sandboxRunner runs one authorized sandbox_exec call for a session and
// reports how long it waited for the Pod's guest slot.
type sandboxRunner interface {
	run(ctx context.Context, request acp.SandboxExecRequest, workspace sandboxWorkspace) (acp.SandboxExecResult, time.Duration, error)
}

type hyperlightRunner interface {
	Run(context.Context, hyperlight.Request) (hyperlight.Result, error)
}

// sandboxExecutor is shared by every session in the runtime Pod. Serialize
// guests so MCP prompt concurrency cannot multiply their fixed memory sizes.
type sandboxExecutor struct {
	runner hyperlightRunner
	groups []uint32
	slot   chan struct{}
}

func newSandboxExecutor(cfg *SandboxExecConfig) *sandboxExecutor {
	if cfg == nil {
		return nil
	}
	executor := &sandboxExecutor{runner: hyperlight.NewRunner(cfg.Hyperlight), slot: make(chan struct{}, 1)}
	if cfg.DeviceGID != 0 {
		executor.groups = []uint32{cfg.DeviceGID}
	}
	return executor
}

// sandboxWorkspace is the session a sandbox_exec call runs for: its
// workspace, mounted in the micro-VM, and the user that owns it. A session
// that may not change its workspace gets it read-only.
type sandboxWorkspace struct {
	dir      string
	uid      uint32
	gid      uint32
	readOnly bool
}

// run executes one call as the session's user, with only its workspace. The
// timeout bounds the guest's run alone: waiting for another session's guest
// to free the slot is bounded separately, by the same length, so a queued
// call is never killed for time it spent waiting.
func (e *sandboxExecutor) run(ctx context.Context, request acp.SandboxExecRequest, workspace sandboxWorkspace) (acp.SandboxExecResult, time.Duration, error) {
	start := time.Now()
	wait, cancelWait := context.WithTimeout(ctx, request.Timeout)
	select {
	case e.slot <- struct{}{}:
		defer func() { <-e.slot }()
	case <-wait.Done():
	}
	waitErr := wait.Err()
	cancelWait()
	queued := time.Since(start)
	if waitErr != nil {
		if errors.Is(waitErr, context.DeadlineExceeded) {
			return acp.SandboxExecResult{ExitCode: -1, TimedOut: true, Stderr: fmt.Sprintf(
				"execution did not start: other sandbox_exec calls kept the micro-VM busy for the whole %ds timeout; nothing ran",
				int64(request.Timeout.Seconds()))}, queued, nil
		}
		return acp.SandboxExecResult{ExitCode: -1}, queued, waitErr
	}
	ctx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	stdout := newSandboxOutput(acp.SandboxExecOutputLimitBytes)
	stderr := newSandboxOutput(acp.SandboxExecOutputLimitBytes)
	run, err := e.runner.Run(ctx, hyperlight.Request{
		Runtime: request.Runtime,
		Script:  request.Script,
		Mounts:  []hyperlight.Mount{{Host: workspace.dir, Guest: acp.SandboxExecWorkspacePath, ReadOnly: workspace.readOnly}},
		Stdout:  stdout,
		Stderr:  stderr,
		Credential: &hyperlight.Credential{
			UID: workspace.uid, GID: workspace.gid, Groups: e.groups,
		},
	})
	result := acp.SandboxExecResult{
		Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: run.ExitCode, TimedOut: run.TimedOut,
		StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated,
	}
	switch {
	case run.OutputExceeded:
		result.ExitCode = -1
		result.Stderr = appendSandboxNote(result.Stderr, fmt.Sprintf("execution stopped: output passed %d bytes", hyperlight.DefaultOutputBudget))
	case run.TimedOut:
		result.Stderr = appendSandboxNote(result.Stderr, "execution timed out")
	}
	return result, queued, err
}

// handleRuntimeLocalCall serves a runtime-local tool call that the session's
// prompt grant has already authorized; gate revocation cancels it.
func (s *mcpProxySession) handleRuntimeLocalCall(w http.ResponseWriter, r *http.Request, rpc mcpJSONRPCRequest, gate context.Context, params mcpToolsCallParams) {
	if params.Name != acp.SandboxExecToolName {
		writeMCPRPCError(w, rpc.ID, -32002, "runtime-local MCP tool is not supported")
		return
	}
	// Brokered calls get this bound from MCPToolCall validation; local ones
	// never build that call.
	if len(params.Arguments) > harnessv2.MaxMCPArgumentsBytes {
		writeMCPToolError(w, rpc.ID, fmt.Sprintf("sandbox_exec arguments must be no larger than %d bytes", harnessv2.MaxMCPArgumentsBytes))
		return
	}
	request, err := acp.ParseSandboxExecArguments(params.Arguments)
	if err != nil {
		writeMCPToolError(w, rpc.ID, err.Error())
		return
	}
	s.mu.Lock()
	workspace := s.sandbox
	s.mu.Unlock()
	if s.proxy.sandbox == nil || workspace.dir == "" {
		writeMCPToolError(w, rpc.ID, "sandbox_exec is not enabled in this runtime: the controller runs it with --acp-sandbox-exec")
		return
	}
	// The call may run for minutes and its result comes only at the end. A
	// client that accepts an event stream gets one, kept alive meanwhile:
	// OpenCode's fetch (Bun 1.3.14) drops a response silent for 300 s.
	out := w
	if stream := startMCPEventStream(w, r); stream != nil {
		defer stream.close()
		out = stream
	}

	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(gate, cancel)
	defer func() {
		stop()
		cancel()
	}()
	start := time.Now()
	result, queued, err := s.proxy.sandbox.run(ctx, request, workspace)
	codeSum := sha256.Sum256([]byte(request.Script))
	slog.Info("sandbox_exec audit",
		"runtime", request.Runtime, "code_sha256", hex.EncodeToString(codeSum[:]), "code_bytes", len(request.Script),
		"exit_code", result.ExitCode, "timed_out", result.TimedOut, "duration_ms", time.Since(start).Milliseconds(),
		"queue_ms", queued.Milliseconds(), "read_only", workspace.readOnly,
		"stdout_bytes", len(result.Stdout), "stderr_bytes", len(result.Stderr), "uid", workspace.uid, "error", err)
	if gate.Err() != nil {
		waitForPromptGateCancellation(r.Context(), gate)
		writeMCPRPCError(out, rpc.ID, -32001, "MCP tool call is not authorized")
		return
	}
	if err != nil {
		message := "sandbox_exec failed: " + err.Error()
		if errors.Is(err, hyperlight.ErrUnavailable) {
			message = "sandbox_exec is unavailable in this runtime: " + err.Error()
		}
		writeMCPToolError(out, rpc.ID, message)
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		writeMCPRPCError(out, rpc.ID, -32002, "sandbox_exec result could not be encoded")
		return
	}
	var structured map[string]any
	_ = json.Unmarshal(encoded, &structured)
	response := mcpToolTextResult(string(encoded), result.ExitCode != 0 || result.TimedOut)
	response["structuredContent"] = structured
	writeMCPRPCResult(out, rpc.ID, response)
}

// bindSandboxWorkspace records the session's workspace and user for
// sandbox_exec once the supervisor has prepared them. Only a write-intent
// session's guests may change the workspace.
func (s *mcpProxySession) bindSandboxWorkspace(dir string, uid, gid int, intent harnessv2.WorkspaceIntent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandbox = sandboxWorkspace{
		dir: dir, uid: uint32(uid), gid: uint32(gid), //nolint:gosec // session IDs come from the bounded UID allocator
		readOnly: intent != harnessv2.WorkspaceIntentWrite,
	}
}

// sandboxExecKeepalive is how often a streamed sandbox_exec call sends a
// comment while it runs.
var sandboxExecKeepalive = 30 * time.Second

// mcpEventStream answers one MCP request with an event stream, which the
// Streamable HTTP transport lets a server choose for any request. It sends a
// comment every sandboxExecKeepalive; the JSON-RPC response written to it is
// sent as the stream's one event on close.
type mcpEventStream struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	header   http.Header
	response bytes.Buffer
	stop     chan struct{}
	stopped  chan struct{}
}

// startMCPEventStream starts the stream if the client accepts one, and
// returns nil if not.
func startMCPEventStream(w http.ResponseWriter, r *http.Request) *mcpEventStream {
	flusher, ok := w.(http.Flusher)
	if !ok || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	stream := &mcpEventStream{w: w, flusher: flusher, header: http.Header{}, stop: make(chan struct{}), stopped: make(chan struct{})}
	go stream.keepalive()
	return stream
}

func (s *mcpEventStream) keepalive() {
	defer close(s.stopped)
	ticker := time.NewTicker(sandboxExecKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_, _ = io.WriteString(s.w, ": keepalive\n\n")
			s.flusher.Flush()
		}
	}
}

// Header, WriteHeader and Write collect the JSON-RPC response for close.
func (s *mcpEventStream) Header() http.Header         { return s.header }
func (s *mcpEventStream) WriteHeader(int)             {}
func (s *mcpEventStream) Write(p []byte) (int, error) { return s.response.Write(p) }

func (s *mcpEventStream) close() {
	close(s.stop)
	<-s.stopped
	_, _ = fmt.Fprintf(s.w, "event: message\ndata: %s\n\n", bytes.TrimSpace(s.response.Bytes()))
	s.flusher.Flush()
}

func writeMCPToolError(w http.ResponseWriter, id json.RawMessage, text string) {
	writeMCPRPCResult(w, id, mcpToolTextResult(text, true))
}

// mcpToolTextResult is an MCP tools/call result with one text content item.
func mcpToolTextResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// sandboxOutput keeps the first limit bytes of a stream.
type sandboxOutput struct {
	buf       bytes.Buffer
	limit     int
	total     int
	truncated bool
}

func newSandboxOutput(limit int) *sandboxOutput {
	return &sandboxOutput{limit: limit}
}

func (o *sandboxOutput) Write(p []byte) (int, error) {
	o.total += len(p)
	if room := o.limit - o.buf.Len(); room > 0 {
		o.buf.Write(p[:min(len(p), room)])
	}
	if o.total > o.buf.Len() {
		o.truncated = true
	}
	return len(p), nil
}

func (o *sandboxOutput) String() string {
	if !o.truncated {
		return o.buf.String()
	}
	return o.buf.String() + fmt.Sprintf("\n[truncated after %d bytes; %d bytes omitted]", o.limit, o.total-o.buf.Len())
}

func appendSandboxNote(text, note string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return text + note
	}
	return text + "\n" + note
}
