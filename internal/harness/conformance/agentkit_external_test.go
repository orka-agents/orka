//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package conformance

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness"
)

const agentKitObservedFixture = `
from __future__ import annotations

import argparse
from types import TracebackType
import uvicorn

from agentkit_serve_common.config import AgentSpec
from agentkit_serve_common.conversation import RunRequest
from agentkit_serve_common.orka import create_orka_app
from agentkit_serve_common.runtime import OfflineEchoRuntimeFactory, RunResult, RuntimeSession


class EchoRuntime:
    async def __aenter__(self) -> RuntimeSession:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> bool | None:
        return None

    async def run(self, request: RunRequest) -> RunResult:
        return RunResult(
            text=f"agentkit observed conformance ok: {request.prompt}",
            usage={"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
        )


class EchoFactory:
    def build_runtime(self, spec: AgentSpec) -> RuntimeSession:  # noqa: ARG002 - fixture runtime is spec-independent.
        return EchoRuntime()


def spec() -> AgentSpec:
    return AgentSpec.model_validate(
        {
            "abiVersion": "v0",
            "metadata": {"name": "agentkit-orka-conformance"},
            "model": {
                "provider": "openai-compatible",
                "baseURL": "https://api.openai.com/v1",
                "name": "gpt-4o-mini",
            },
            "instructions": "Reply to Orka conformance probes.",
            "tools": [],
            "env": [],
            "expose": {"openai": True, "port": 8080},
        }
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--token", required=True)
    parser.add_argument("--brokered-read", action="store_true")
    parser.add_argument("--brokered-write", action="store_true")
    parser.add_argument("--brokered-coordination", action="store_true")
    args = parser.parse_args()
    factory = OfflineEchoRuntimeFactory() if args.brokered_read or args.brokered_write or args.brokered_coordination else EchoFactory()
    app = create_orka_app(
        spec(),
        factory,
        args.token,
        enable_brokered_read=args.brokered_read,
        enable_brokered_write=args.brokered_write,
        enable_brokered_coordination=args.brokered_coordination,
    )
    uvicorn.run(app, host=args.host, port=args.port, log_level="warning", access_log=False)


if __name__ == "__main__":
    main()
`

// TestExternalAgentKitServeObservedConformance proves the current AgentKit Serve
// Orka protocol skin against Orka's reusable conformance runner. It is path gated
// so Orka-only CI does not need a sibling AgentKit checkout; set
// AGENTKIT_SERVE_ROOT=/path/to/agentkit.serve to enable it.
func TestExternalAgentKitServeObservedConformance(t *testing.T) {
	agentKitRoot := os.Getenv("AGENTKIT_SERVE_ROOT")
	if agentKitRoot == "" {
		t.Skip("set AGENTKIT_SERVE_ROOT=/path/to/agentkit.serve to run AgentKit Serve observed conformance")
	}
	commonRoot := filepath.Join(agentKitRoot, "runtimes", "common")
	if _, err := os.Stat(filepath.Join(commonRoot, "pyproject.toml")); err != nil {
		t.Fatalf("AGENTKIT_SERVE_ROOT does not look like an agentkit.serve checkout: %v", err)
	}

	port := freeLocalPort(t)
	fixturePath := filepath.Join(t.TempDir(), "agentkit_observed_fixture.py")
	if err := os.WriteFile(fixturePath, []byte(agentKitObservedFixture), 0o600); err != nil {
		t.Fatalf("write AgentKit fixture: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"uv",
		"run",
		"--no-project",
		"--with-editable", commonRoot,
		"python", fixturePath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--token", "x",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start AgentKit Orka fixture with uv: %v", err)
	}
	defer stopAgentKitFixture(cmd, cancel)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForAgentKitHealth(t, baseURL, &stderr)

	result := Check(context.Background(), Target{
		BaseURL:        baseURL,
		BearerToken:    "x",
		RequireAuth:    true,
		ProbeTurn:      true,
		ControlTimeout: 10 * time.Second,
	})
	if !result.Passed {
		t.Fatalf("AgentKit observed conformance failed: %s failures=%v stderr=%s", result.Message, result.Failures, stderr.String())
	}
	caps := result.ObservedCapabilities
	if caps == nil {
		t.Fatal("ObservedCapabilities = nil")
	}
	if !slices.Equal(caps.ToolExecutionModes, []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}) {
		t.Fatalf("ToolExecutionModes = %#v, want observed only", caps.ToolExecutionModes)
	}
	if len(caps.BrokeredToolClasses) != 0 {
		t.Fatalf("BrokeredToolClasses = %#v, want none", caps.BrokeredToolClasses)
	}
	if caps.SupportsContinuation {
		t.Fatal("SupportsContinuation = true, want false until brokered conformance passes")
	}
}

func TestExternalAgentKitServeBrokeredReadConformance(t *testing.T) {
	baseURL, stderr, cleanup := startAgentKitFixture(t, "--brokered-read")
	defer cleanup()

	result := Check(context.Background(), Target{
		BaseURL:           baseURL,
		BearerToken:       "x",
		RequireAuth:       true,
		ProbeBrokeredRead: true,
		ControlTimeout:    10 * time.Second,
	})
	if !result.Passed {
		t.Fatalf("AgentKit brokered read conformance failed: %s failures=%v stderr=%s", result.Message, result.Failures, stderr.String())
	}
	caps := result.ObservedCapabilities
	if caps == nil {
		t.Fatal("ObservedCapabilities = nil")
	}
	if !slices.Equal(caps.ToolExecutionModes, []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered}) {
		t.Fatalf("ToolExecutionModes = %#v, want observed+brokered", caps.ToolExecutionModes)
	}
	if !slices.Equal(caps.BrokeredToolClasses, []harness.BrokeredToolClass{harness.BrokeredToolClassRead}) {
		t.Fatalf("BrokeredToolClasses = %#v, want read", caps.BrokeredToolClasses)
	}
	if !caps.SupportsContinuation {
		t.Fatal("SupportsContinuation = false, want true for brokered read fixture")
	}
}

func TestExternalAgentKitServeBrokeredWriteConformance(t *testing.T) {
	baseURL, stderr, cleanup := startAgentKitFixture(t, "--brokered-write")
	defer cleanup()

	result := Check(context.Background(), Target{
		BaseURL:            baseURL,
		BearerToken:        "x",
		RequireAuth:        true,
		ProbeBrokeredWrite: true,
		ControlTimeout:     10 * time.Second,
	})
	if !result.Passed {
		t.Fatalf("AgentKit brokered write conformance failed: %s failures=%v stderr=%s", result.Message, result.Failures, stderr.String())
	}
	caps := result.ObservedCapabilities
	if caps == nil {
		t.Fatal("ObservedCapabilities = nil")
	}
	if !slices.Equal(caps.ToolExecutionModes, []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered}) {
		t.Fatalf("ToolExecutionModes = %#v, want observed+brokered", caps.ToolExecutionModes)
	}
	if !slices.Equal(caps.BrokeredToolClasses, []harness.BrokeredToolClass{harness.BrokeredToolClassWrite}) {
		t.Fatalf("BrokeredToolClasses = %#v, want write", caps.BrokeredToolClasses)
	}
	if !caps.SupportsContinuation {
		t.Fatal("SupportsContinuation = false, want true for brokered write fixture")
	}
}

func TestExternalAgentKitServeBrokeredCoordinationConformance(t *testing.T) {
	baseURL, stderr, cleanup := startAgentKitFixture(t, "--brokered-coordination")
	defer cleanup()

	result := Check(context.Background(), Target{
		BaseURL:                   baseURL,
		BearerToken:               "x",
		RequireAuth:               true,
		ProbeBrokeredCoordination: true,
		ControlTimeout:            10 * time.Second,
	})
	if !result.Passed {
		t.Fatalf("AgentKit brokered coordination conformance failed: %s failures=%v stderr=%s", result.Message, result.Failures, stderr.String())
	}
	caps := result.ObservedCapabilities
	if caps == nil {
		t.Fatal("ObservedCapabilities = nil")
	}
	if !slices.Equal(caps.ToolExecutionModes, []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered}) {
		t.Fatalf("ToolExecutionModes = %#v, want observed+brokered", caps.ToolExecutionModes)
	}
	if !slices.Equal(caps.BrokeredToolClasses, []harness.BrokeredToolClass{harness.BrokeredToolClassCoordination}) {
		t.Fatalf("BrokeredToolClasses = %#v, want coordination", caps.BrokeredToolClasses)
	}
	if !caps.SupportsContinuation {
		t.Fatal("SupportsContinuation = false, want true for brokered coordination fixture")
	}
}

func startAgentKitFixture(t *testing.T, extraArgs ...string) (string, *bytes.Buffer, func()) {
	t.Helper()
	agentKitRoot := os.Getenv("AGENTKIT_SERVE_ROOT")
	if agentKitRoot == "" {
		t.Skip("set AGENTKIT_SERVE_ROOT=/path/to/agentkit.serve to run AgentKit Serve conformance")
	}
	commonRoot := filepath.Join(agentKitRoot, "runtimes", "common")
	if _, err := os.Stat(filepath.Join(commonRoot, "pyproject.toml")); err != nil {
		t.Fatalf("AGENTKIT_SERVE_ROOT does not look like an agentkit.serve checkout: %v", err)
	}

	port := freeLocalPort(t)
	fixturePath := filepath.Join(t.TempDir(), "agentkit_observed_fixture.py")
	if err := os.WriteFile(fixturePath, []byte(agentKitObservedFixture), 0o600); err != nil {
		t.Fatalf("write AgentKit fixture: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"run",
		"--no-project",
		"--with-editable", commonRoot,
		"python", fixturePath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--token", "x",
	}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "uv", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start AgentKit Orka fixture with uv: %v", err)
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() { stopAgentKitFixture(cmd, cancel) })
	}
	t.Cleanup(cleanup)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForAgentKitHealth(t, baseURL, &stderr)
	return baseURL, &stderr, cleanup
}

func stopAgentKitFixture(cmd *exec.Cmd, cancel context.CancelFunc) {
	cancel()
	if cmd.Process != nil {
		// uv may spawn or exec Python; kill the whole process group so the server
		// cannot inherit stderr and keep Cmd.Wait blocked after the test completes.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate local port: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForAgentKitHealth(t *testing.T, baseURL string, stderr *bytes.Buffer) {
	t.Helper()
	client := http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + harness.HealthPath)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("AgentKit Orka fixture did not become healthy: lastErr=%v stderr=%s", lastErr, stderr.String())
}
