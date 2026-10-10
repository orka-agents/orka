//go:build linux && hyperlight_e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/hyperlight"
	"github.com/orka-agents/orka/internal/workerenv"
)

// These tests deliberately fail instead of skipping when their real bundle or
// device is missing. Only the dedicated KVM workflow enables hyperlight_e2e.
func TestHyperlightE2ECodeExec(t *testing.T) {
	cfg := hyperlight.ConfigFromEnv()
	for _, path := range []string{cfg.Binary, "/dev/kvm", filepath.Join(cfg.RootfsDir, "python.cpio"), filepath.Join(cfg.RootfsDir, "node.cpio"), filepath.Join(cfg.RootfsDir, "bash.cpio")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Hyperlight E2E prerequisite %s: %v", path, err)
		}
	}
	t.Setenv(workerenv.CodeExecBackend, codeExecBackendHyperlight)
	t.Setenv(workerenv.CodeExecBackendEnforced, "true")
	t.Setenv(hyperlight.EnvCacheDir, t.TempDir())
	tool := NewCodeExecTool()

	for _, tc := range []struct {
		language string
		code     string
	}{
		{"bash", "echo hyperlight-bash"},
		{"python", "print('hyperlight-python')"},
		{"javascript", "console.log('hyperlight-javascript')"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			// Run twice through the public tool API to exercise snapshot reuse.
			for range 2 {
				got := hyperlightE2ECodeExec(t, tool, tc.language, tc.code, 30)
				if got.ExitCode != 0 || got.TimedOut || got.Error != "" || got.Output != "hyperlight-"+tc.language+"\n" {
					t.Fatalf("%s guest result: %+v", tc.language, got)
				}
			}
		})
	}

	t.Run("streams and guest exit", func(t *testing.T) {
		got := hyperlightE2ECodeExec(t, tool, "bash", "echo stdout; echo stderr >&2; exit 7", 30)
		if got.ExitCode != 7 || got.Output != "stdout\n" || got.Error != "stderr\n" || got.TimedOut {
			t.Fatalf("guest streams and exit: %+v", got)
		}
	})
	t.Run("output limit", func(t *testing.T) {
		got := hyperlightE2ECodeExec(t, tool, "python", "print('x' * 100000)", 30)
		if got.ExitCode != 0 || !got.OutputTruncated || len(got.Output) > 66<<10 {
			t.Fatalf("output was not bounded: exit=%d truncated=%t bytes=%d", got.ExitCode, got.OutputTruncated, len(got.Output))
		}
	})
	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		got := hyperlightE2ECodeExec(t, tool, "python", "while True: pass", 1)
		if !got.TimedOut || got.ExitCode != -1 || time.Since(start) > 10*time.Second {
			t.Fatalf("unbounded guest timeout: %+v elapsed=%s", got, time.Since(start))
		}
	})
	t.Run("host environment and files are absent", func(t *testing.T) {
		const sentinel = "hyperlight-e2e-host-only"
		t.Setenv("ORKA_HYPERLIGHT_E2E_HOST_ONLY", sentinel)
		hostPath := filepath.Join(t.TempDir(), "host-only.txt")
		if err := os.WriteFile(hostPath, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		code := "import os\nassert 'ORKA_HYPERLIGHT_E2E_HOST_ONLY' not in os.environ\nassert not os.path.exists(" + strconv.Quote(hostPath) + ")\nprint('isolated')"
		got := hyperlightE2ECodeExec(t, tool, "python", code, 30)
		if got.ExitCode != 0 || got.Output != "isolated\n" {
			t.Fatalf("host isolation failed: %+v", got)
		}
	})
	t.Run("guest state is reset", func(t *testing.T) {
		got := hyperlightE2ECodeExec(t, tool, "python", "open('/tmp/guest-only.txt', 'w').write('guest')\nprint('created')", 30)
		if got.ExitCode != 0 || got.Output != "created\n" {
			t.Fatalf("guest state setup: %+v", got)
		}
		got = hyperlightE2ECodeExec(t, tool, "python", "import os\nassert not os.path.exists('/tmp/guest-only.txt')\nprint('fresh')", 30)
		if got.ExitCode != 0 || got.Output != "fresh\n" {
			t.Fatalf("guest state survived a new call: %+v", got)
		}
	})
}

func hyperlightE2ECodeExec(t *testing.T, tool *CodeExecTool, language, code string, timeout int) CodeExecResult {
	t.Helper()
	args, err := json.Marshal(CodeExecArgs{Language: language, Code: code, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	encoded, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("code_exec: %v", err)
	}
	var result CodeExecResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatalf("code_exec returned invalid JSON: %v", err)
	}
	return result
}
