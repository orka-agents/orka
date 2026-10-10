package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/acp"
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

// sandboxRunner runs one authorized sandbox_exec call for a session.
type sandboxRunner interface {
	run(ctx context.Context, request acp.SandboxExecRequest, workspace sandboxWorkspace) (acp.SandboxExecResult, error)
}

// sandboxExecutor runs sandbox_exec calls in Hyperlight micro-VMs.
type sandboxExecutor struct {
	runner *hyperlight.Runner
	groups []uint32
}

func newSandboxExecutor(cfg *SandboxExecConfig) *sandboxExecutor {
	if cfg == nil {
		return nil
	}
	executor := &sandboxExecutor{runner: hyperlight.NewRunner(cfg.Hyperlight)}
	if cfg.DeviceGID != 0 {
		executor.groups = []uint32{cfg.DeviceGID}
	}
	return executor
}

// sandboxWorkspace is the session a sandbox_exec call runs for: its
// workspace, mounted in the micro-VM, and the user that owns it.
type sandboxWorkspace struct {
	dir string
	uid uint32
	gid uint32
}

// run executes one call as the session's user, with only its workspace.
func (e *sandboxExecutor) run(ctx context.Context, request acp.SandboxExecRequest, workspace sandboxWorkspace) (acp.SandboxExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	stdout := newSandboxOutput(acp.SandboxExecOutputLimitBytes)
	stderr := newSandboxOutput(acp.SandboxExecOutputLimitBytes)
	run, err := e.runner.Run(ctx, hyperlight.Request{
		Runtime: request.Runtime,
		Script:  request.Script,
		Mounts:  []hyperlight.Mount{{Host: workspace.dir, Guest: acp.SandboxExecWorkspacePath}},
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
	return result, err
}

// handleRuntimeLocalCall serves a runtime-local tool call that the session's
// prompt grant has already authorized; gate revocation cancels it.
func (s *mcpProxySession) handleRuntimeLocalCall(w http.ResponseWriter, r *http.Request, rpc mcpJSONRPCRequest, gate context.Context, params mcpToolsCallParams) {
	if params.Name != acp.SandboxExecToolName {
		writeMCPRPCError(w, rpc.ID, -32002, "runtime-local MCP tool is not supported")
		return
	}
	request, err := acp.ParseSandboxExecArguments(params.Arguments)
	if err != nil {
		writeMCPToolText(w, rpc.ID, err.Error(), true)
		return
	}
	s.mu.Lock()
	workspace := s.sandbox
	s.mu.Unlock()
	if s.proxy.sandbox == nil || workspace.dir == "" {
		writeMCPToolText(w, rpc.ID, "sandbox_exec is not enabled in this runtime: the controller runs it with --acp-sandbox-exec", true)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(gate, cancel)
	defer func() {
		stop()
		cancel()
	}()
	start := time.Now()
	result, err := s.proxy.sandbox.run(ctx, request, workspace)
	codeSum := sha256.Sum256([]byte(request.Script))
	slog.Info("sandbox_exec audit",
		"runtime", request.Runtime, "code_sha256", hex.EncodeToString(codeSum[:]), "code_bytes", len(request.Script),
		"exit_code", result.ExitCode, "timed_out", result.TimedOut, "duration_ms", time.Since(start).Milliseconds(),
		"stdout_bytes", len(result.Stdout), "stderr_bytes", len(result.Stderr), "uid", workspace.uid, "error", err)
	if gate.Err() != nil {
		waitForPromptGateCancellation(r.Context(), gate)
		writeMCPRPCError(w, rpc.ID, -32001, "MCP tool call is not authorized")
		return
	}
	if err != nil {
		message := "sandbox_exec failed: " + err.Error()
		if errors.Is(err, hyperlight.ErrUnavailable) {
			message = "sandbox_exec is unavailable in this runtime: " + err.Error()
		}
		writeMCPToolText(w, rpc.ID, message, true)
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		writeMCPRPCError(w, rpc.ID, -32002, "sandbox_exec result could not be encoded")
		return
	}
	var structured map[string]any
	_ = json.Unmarshal(encoded, &structured)
	response := mcpToolTextResult(string(encoded), result.ExitCode != 0 || result.TimedOut)
	response["structuredContent"] = structured
	writeMCPRPCResult(w, rpc.ID, response)
}

// bindSandboxWorkspace records the session's workspace and user for
// sandbox_exec once the supervisor has prepared them.
func (s *mcpProxySession) bindSandboxWorkspace(dir string, uid, gid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandbox = sandboxWorkspace{dir: dir, uid: uint32(uid), gid: uint32(gid)} //nolint:gosec // session IDs come from the bounded UID allocator
}

func writeMCPToolText(w http.ResponseWriter, id json.RawMessage, text string, isError bool) {
	writeMCPRPCResult(w, id, mcpToolTextResult(text, isError))
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
