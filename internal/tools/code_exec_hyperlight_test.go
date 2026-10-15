//go:build !windows

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
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/hyperlight"
	"github.com/orka-agents/orka/internal/workerenv"
)

// fakeHlukForCodeExec stands in for hluk: a bash guest runs its script with
// sh, any other guest echoes its script, and every call is logged to
// $HOME/calls.log ($HOME is the runner's cache dir).
const fakeHlukForCodeExec = `#!/bin/sh
echo "$*" >> "$HOME/calls.log"
case "$1 $2" in
"snapshot key") echo "k0-c4"; exit 0 ;;
"snapshot save")
	while [ $# -gt 0 ]; do [ "$1" = "--output" ] && out="$2"; shift; done
	mkdir "$out"; exit 0 ;;
"snapshot run") snapshot="$3"; script="$4" ;;
esac
case "$snapshot" in
*/bash-*) . "$script" ;;
*) cat "$script" ;;
esac
`

func newHyperlightTestExecutor(t *testing.T) (*HyperlightCodeExecutor, string, string) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "hluk")
	if err := os.WriteFile(binary, []byte(fakeHlukForCodeExec), 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"python", "node", "bash"} {
		if err := os.WriteFile(filepath.Join(rootfs, runtime+".cpio"), []byte("cpio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	device := filepath.Join(dir, "kvm")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := hyperlight.NewRunner(hyperlight.Config{Binary: binary, RootfsDir: rootfs, CacheDir: cache, DevicePaths: []string{device}})
	return &HyperlightCodeExecutor{runner: runner}, cache, device
}

func hyperlightRequest(language, code string) CodeExecutionRequest {
	return CodeExecutionRequest{
		Backend:          codeExecBackendHyperlight,
		Language:         language,
		Code:             code,
		Timeout:          10 * time.Second,
		DenyPatterns:     defaultDenyPatterns,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	}
}

func TestHyperlightCodeExecutorMapsTheRun(t *testing.T) {
	executor, cache, _ := newHyperlightTestExecutor(t)
	result := executor.Execute(context.Background(), hyperlightRequest(codeLanguageBash, "echo out; echo err >&2; exit 4"))
	if result.Output != "out\n" || result.Error != "err\n" || result.ExitCode != 4 || result.TimedOut {
		t.Fatalf("result = %+v, want stdout out, stderr err, exit 4", result)
	}

	result = executor.Execute(context.Background(), hyperlightRequest(python3BinaryName, "print('hi')"))
	if result.Output != "print('hi')" || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want the python guest to get the code", result)
	}
	calls, err := os.ReadFile(filepath.Join(cache, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "python.cpio") {
		t.Fatalf("python ran on another image; calls:\n%s", calls)
	}
	if strings.Contains(string(calls), "--mount") || strings.Contains(string(calls), "--net") {
		t.Fatalf("code_exec granted the guest host files or network; calls:\n%s", calls)
	}
}

func TestHyperlightCodeExecutorCapsOutputAndTimesOut(t *testing.T) {
	executor, _, _ := newHyperlightTestExecutor(t)
	req := hyperlightRequest(codeLanguageBash, "i=0; while [ $i -lt 200 ]; do echo 0123456789; i=$((i+1)); done")
	req.OutputLimitBytes = 100
	result := executor.Execute(context.Background(), req)
	if !result.OutputTruncated || !strings.Contains(result.Output, "[truncated after 100 bytes") {
		t.Fatalf("result = %+v, want truncated output", result)
	}

	req = hyperlightRequest(codeLanguageBash, "sleep 30")
	req.Timeout = 200 * time.Millisecond
	start := time.Now()
	result = executor.Execute(context.Background(), req)
	if !result.TimedOut || result.ExitCode != -1 || !strings.Contains(result.Error, "execution timed out") {
		t.Fatalf("result = %+v, want a timeout", result)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timed-out run took %s", elapsed)
	}
}

func TestHyperlightCodeExecutorFailsClosed(t *testing.T) {
	executor, _, device := newHyperlightTestExecutor(t)
	if err := os.Remove(device); err != nil {
		t.Fatal(err)
	}
	result := executor.Execute(context.Background(), hyperlightRequest(codeLanguageBash, "echo should-not-run"))
	if result.ExitCode != -1 || result.Output != "" || !strings.Contains(result.Error, "hyperlight backend unavailable") {
		t.Fatalf("result = %+v, want an unavailable error and no run", result)
	}

	executor, _, _ = newHyperlightTestExecutor(t)
	result = executor.Execute(context.Background(), hyperlightRequest(codeLanguageBash, "rm -rf /"))
	if result.ExitCode != -1 || !strings.Contains(result.Error, "safety guard") {
		t.Fatalf("result = %+v, want the deny guard to stop the command", result)
	}
}

func TestCodeExecTool_BackendSelector_Hyperlight(t *testing.T) {
	t.Setenv("ORKA_WORK_DIR", t.TempDir())
	t.Setenv(codeExecBackendEnv, "Hyperlight")
	tool := NewCodeExecTool()
	if tool.backend != codeExecBackendHyperlight {
		t.Fatalf("backend = %q, want %q", tool.backend, codeExecBackendHyperlight)
	}
	if _, ok := tool.executor.(*HyperlightCodeExecutor); !ok {
		t.Fatalf("executor type = %T, want *HyperlightCodeExecutor", tool.executor)
	}
}

func TestCodeExecTool_EnforcedBackendIgnoresScopedOverrides(t *testing.T) {
	provider, tenant := "enforced-provider", "enforced-tenant"
	t.Setenv(codeExecBackendEnv, codeExecBackendHyperlight)
	t.Setenv(workerenv.CodeExecBackendEnforced, "true")
	t.Setenv(codeExecBackendEnv+"_TENANT_ENFORCED_TENANT", "in-process")
	t.Setenv(codeExecBackendEnv+"_PROVIDER_ENFORCED_PROVIDER", "in-process")
	tool := &CodeExecTool{backend: codeExecBackendHyperlight}
	if got := tool.resolveCodeExecBackend(provider, "", tenant); got != codeExecBackendHyperlight {
		t.Fatalf("enforced backend resolved to %q, want %q", got, codeExecBackendHyperlight)
	}

	t.Setenv(workerenv.CodeExecBackendEnforced, "")
	if got := tool.resolveCodeExecBackend(provider, "", tenant); got != codeExecBackendInProcess {
		t.Fatalf("unenforced backend resolved to %q, want the scoped %q", got, codeExecBackendInProcess)
	}
}

func TestCodeExecTool_HyperlightAuditNamesTheGuest(t *testing.T) {
	req := hyperlightRequest(codeLanguageJavaScript, "1")
	if err := populateCodeExecRequestResourceAudit(&req); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"network": "none", "host_files": "none", "runtime": "node", "scratch_mb": "512"}
	got, _ := json.Marshal(req.ResourceAudit)
	for key, value := range want {
		if req.ResourceAudit[key] != value {
			t.Fatalf("audit = %s, want %s=%s", got, key, value)
		}
	}
}
