//go:build linux && hyperlight_e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/hyperlight"
)

const (
	hyperlightE2ESessionUID = 20001
	hyperlightE2ESessionGID = 20002
)

// Only the dedicated KVM workflow enables these tests. Missing prerequisites
// are failures, not skips; no fake executable or host interpreter is used.
func TestHyperlightE2ESandboxExecPublicMCPLanguages(t *testing.T) {
	fixture := newHyperlightE2ESandboxFixture(t)
	for _, tc := range []struct {
		name, language, code, output string
	}{
		{
			name: "bash", language: "bash", output: "bash-ok\n",
			code: `set -eu
[ "$PWD" = /workspace ]
printf 'bash-workspace' > bash.txt
printf 'bash-ok\n'`,
		},
		{
			name: "python future import", language: "python", output: "python-ok\n",
			code: `"sandbox module documentation"
from __future__ import annotations
import os
assert __doc__ == 'sandbox module documentation'
assert os.getcwd() == '/workspace'
def annotated(value: NotDefinedYet) -> NotDefinedYet:
    return value
assert annotated.__annotations__['value'] == 'NotDefinedYet'
with open('bash.txt') as previous:
    assert previous.read() == 'bash-workspace'
with open('python.txt', 'w') as result:
    result.write('python-workspace')
print('python-ok')`,
		},
		{
			name: "javascript strict main and workspace modules", language: "javascript", output: "javascript-ok\n",
			code: `"use strict";
const assert = require('assert');
assert.throws(() => { hyperlightE2EUndeclared = 1; }, ReferenceError);
assert.strictEqual(typeof globalThis.hyperlightE2EUndeclared, 'undefined');
assert.strictEqual(process.cwd(), '/workspace');
assert.strictEqual(__dirname, '/workspace');
assert.strictEqual(__filename, '/workspace/sandbox_exec.js');
assert.strictEqual(require.main, module);
assert.strictEqual(process.mainModule, module);
assert.strictEqual(module.filename, __filename);
assert.strictEqual(require('./package.json').name, 'workspace-package');
assert.strictEqual(module.require('./package.json').name, 'workspace-package');
assert.strictEqual(require('workspace-dependency'), 'dependency-loaded');
assert.strictEqual(module.require('workspace-dependency'), 'dependency-loaded');
assert.strictEqual(require.resolve('./package.json'), '/workspace/package.json');
assert.ok(module.paths.includes('/workspace/node_modules'));
const fs = require('fs');
assert.strictEqual(fs.readFileSync('python.txt', 'utf8'), 'python-workspace');
if (require.main === module) {
    fs.writeFileSync('javascript.txt', 'javascript-workspace');
    console.log('javascript-ok');
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, endpoint := fixture.session(t, fixture.config)
			listed := hyperlightE2ESandboxRPC(t, endpoint, `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`)
			if listed.Error != nil || responseToolCount(listed.Result) != 1 || !strings.Contains(fmt.Sprint(listed.Result), acp.SandboxExecToolName) {
				t.Fatalf("public tools/list did not expose sandbox_exec: %#v", listed)
			}
			response := hyperlightE2ESandboxRPC(t, endpoint, hyperlightE2ESandboxCall(t, tc.language, tc.code, 30))
			result := hyperlightE2ESandboxResult(t, response)
			if result.Stdout != tc.output {
				t.Fatalf("real %s guest output = %q, want %q; stderr = %q", tc.language, result.Stdout, tc.output, result.Stderr)
			}
		})
	}

	// Root has no DAC_OVERRIDE in CI. Reclaim only after every guest is gone,
	// then inspect the files written through the real workspace mount.
	fixture.reclaim(t)
	for name, want := range map[string]string{
		"bash.txt": "bash-workspace", "python.txt": "python-workspace", "javascript.txt": "javascript-workspace",
	} {
		data, err := os.ReadFile(filepath.Join(fixture.workspace, name))
		if err != nil || string(data) != want {
			t.Fatalf("persisted workspace file %s = %q, error = %v, want %q", name, data, err, want)
		}
	}
	fixture.checkCache(t)
}

func TestHyperlightE2ESandboxExecHostIsolation(t *testing.T) {
	fixture := newHyperlightE2ESandboxFixture(t)
	const sentinel = "hyperlight-acp-e2e-host-only"
	t.Setenv("ORKA_HYPERLIGHT_E2E_HOST_ONLY", sentinel)
	hostPath := filepath.Join(fixture.root, "host-sentinel.txt")
	if err := os.WriteFile(hostPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the sentinel readable to the session UID on the host. Its absence
	// in the guest must come from the mount boundary, not Unix permissions.
	if err := os.Chmod(hostPath, 0o644); err != nil {
		t.Fatal(err)
	}
	_, endpoint := fixture.session(t, fixture.config)
	code := `import errno, os, socket
assert 'ORKA_HYPERLIGHT_E2E_HOST_ONLY' not in os.environ
for path in (` + strconv.Quote(hostPath) + `, '/workspace/../host-sentinel.txt'):
    try:
        with open(path) as leaked:
            leaked.read()
    except OSError:
        pass
    else:
        raise AssertionError('unmounted host file was readable: ' + path)
# A policy denial is stronger evidence than a failed connection in Docker's
# --network none container. The production guest refuses socket creation.
for kind in (socket.SOCK_STREAM, socket.SOCK_DGRAM):
    try:
        connection = socket.socket(socket.AF_INET, kind)
    except OSError as failure:
        assert failure.errno == errno.EACCES, failure
    else:
        connection.close()
        raise AssertionError('guest networking was enabled')
print('isolated')`
	result := hyperlightE2ESandboxResult(t, hyperlightE2ESandboxRPC(t, endpoint, hyperlightE2ESandboxCall(t, "python", code, 30)))
	if result.Stdout != "isolated\n" || strings.Contains(result.Stdout+result.Stderr, sentinel) {
		t.Fatalf("real guest host isolation result: %+v", result)
	}
	data, err := os.ReadFile(hostPath)
	if err != nil || string(data) != sentinel {
		t.Fatalf("host sentinel changed: %q, error = %v", data, err)
	}
	fixture.checkCache(t)
}

func TestHyperlightE2ESandboxExecMissingPrerequisitesFailClosed(t *testing.T) {
	fixture := newHyperlightE2ESandboxFixture(t)
	for _, prerequisite := range []string{"binary", "rootfs", "device"} {
		t.Run(prerequisite, func(t *testing.T) {
			config := fixture.config
			missing := filepath.Join(fixture.root, "missing-"+prerequisite)
			switch prerequisite {
			case "binary":
				config.Hyperlight.Binary = missing
			case "rootfs":
				config.Hyperlight.RootfsDir = missing
			case "device":
				config.Hyperlight.DevicePaths = []string{missing}
			}
			_, endpoint := fixture.session(t, config)
			response := hyperlightE2ESandboxRPC(t, endpoint, hyperlightE2ESandboxCall(t, "bash", "echo fallback > must-not-run.txt", 30))
			text, isError := sandboxToolResult(t, response)
			if !isError || !strings.Contains(text, "sandbox_exec is unavailable") {
				t.Fatalf("missing %s did not fail closed through MCP: %q, isError = %t", prerequisite, text, isError)
			}
			if result := response.Result.(map[string]any); result["structuredContent"] != nil {
				t.Fatalf("missing %s returned an execution result: %#v", prerequisite, result)
			}
		})
	}
	fixture.reclaim(t)
	if _, err := os.Stat(filepath.Join(fixture.workspace, "must-not-run.txt")); !os.IsNotExist(err) {
		t.Fatalf("unavailable sandbox ran a fallback: %v", err)
	}
	entries, err := os.ReadDir(fixture.config.Hyperlight.CacheDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed preflight created execution files: entries = %v, error = %v", entries, err)
	}
	fixture.checkCache(t)
}

func TestHyperlightE2ESandboxExecSettlementCancelsRunningVM(t *testing.T) {
	fixture := newHyperlightE2ESandboxFixture(t)
	// A hard link lets root observe guest readiness without opening the
	// session's 0700 directory or granting the VM another host mount.
	ready := filepath.Join(fixture.root, "ready")
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ready, 0o644); err != nil {
		t.Fatal(err)
	}
	// Temporarily reclaim to prepare the link, then restore the same identity
	// before the public MCP call. No VM is running during either operation.
	fixture.reclaim(t)
	// Link while root still owns the file: protected_hardlinks forbids linking
	// a session-owned 0644 file without the deliberately absent FOWNER cap.
	if err := os.Link(ready, filepath.Join(fixture.workspace, "vm-ready.txt")); err != nil {
		t.Fatal(err)
	}
	if err := acp.FinalizeSessionOwnership(fixture.workspace, hyperlightE2ESessionUID, hyperlightE2ESessionGID); err != nil {
		t.Fatal(err)
	}
	session, endpoint := fixture.session(t, fixture.config)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Second)
	defer cancel()
	defer session.deactivate(testPromptOneID, harnessv2.RuntimeSessionStateValidating)
	type requestResult struct {
		response *http.Response
		err      error
	}
	responseDone := make(chan requestResult, 1)
	payload := hyperlightE2ESandboxCall(t, "python", "with open('vm-ready.txt', 'w') as ready:\n    ready.write('guest-running')\nwhile True:\n    pass", 60)
	go func() {
		response, err := hyperlightE2ESandboxHTTPRequest(ctx, endpoint, payload)
		responseDone <- requestResult{response: response, err: err}
	}()
	// Always join the HTTP goroutine, including a readiness assertion failure.
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case got := <-responseDone:
				if got.response != nil {
					_ = got.response.Body.Close()
				}
			case <-time.After(5 * time.Second):
				t.Error("cancelled HTTP request did not finish during cleanup")
			}
		}
	}()

	deadline := time.Now().Add(25 * time.Second)
	for {
		select {
		case got := <-responseDone:
			joined = true
			if got.err != nil {
				t.Fatalf("MCP request ended before guest readiness: %v", got.err)
			}
			t.Fatalf("VM ended before readiness: %#v", decodeMCPResponse(t, got.response))
		default:
		}
		data, err := os.ReadFile(ready)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) == "guest-running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real guest did not write its readiness marker within 25 seconds")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid := fixture.runningProcess(t)
	settledAt := time.Now()
	session.deactivate(testPromptOneID, harnessv2.RuntimeSessionStateValidating)
	select {
	case got := <-responseDone:
		joined = true
		if got.err != nil {
			t.Fatalf("settled MCP request failed at HTTP layer: %v", got.err)
		}
		response := decodeMCPResponse(t, got.response)
		if response.Error == nil || response.Error.Code != -32001 || response.Result != nil {
			t.Fatalf("settled MCP call returned guest output instead of revoking authorization: %#v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt settlement did not stop the real VM and return its MCP call within 5 seconds")
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); !os.IsNotExist(err) {
		t.Fatalf("hluk PID %d survived its completed cancelled call: %v", pid, err)
	}
	t.Logf("settlement stopped session UID %d hluk PID %d and returned MCP in %s", hyperlightE2ESessionUID, pid, time.Since(settledAt))
	response := hyperlightE2ESandboxRPC(t, endpoint, hyperlightE2ESandboxCall(t, "bash", "echo late > late.txt", 30))
	if response.Error == nil || response.Error.Code != -32001 {
		t.Fatalf("settled prompt admitted a new sandbox_exec call: %#v", response)
	}
	fixture.reclaim(t)
	if _, err := os.Stat(filepath.Join(fixture.workspace, "late.txt")); !os.IsNotExist(err) {
		t.Fatalf("a call after settlement wrote to the workspace: %v", err)
	}
	fixture.checkCache(t)
}

type hyperlightE2ESandboxFixture struct {
	root, workspace string
	config          SandboxExecConfig
}

func newHyperlightE2ESandboxFixture(t *testing.T) *hyperlightE2ESandboxFixture {
	t.Helper()
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		t.Fatal("ACP Hyperlight E2E requires root with CHOWN, KILL, SETUID and SETGID, but no DAC_OVERRIDE")
	}
	device, err := os.Stat("/dev/kvm")
	if err != nil || device.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("ACP Hyperlight E2E requires the real /dev/kvm character device: %v", err)
	}
	gid, err := strconv.ParseUint(os.Getenv(EnvHyperlightDeviceGID), 10, 32)
	if err != nil || uint32(gid) != device.Sys().(*syscall.Stat_t).Gid {
		t.Fatalf("%s must be the numeric GID of /dev/kvm: %v", EnvHyperlightDeviceGID, err)
	}
	config := SandboxExecConfig{
		DeviceGID: uint32(gid),
		Hyperlight: hyperlight.Config{
			Binary: "/opt/orka/hyperlight/bin/hluk", RootfsDir: "/opt/orka/hyperlight/rootfs",
			DevicePaths: []string{"/dev/kvm"}, SharedPod: true,
		},
	}
	for _, path := range []string{config.Hyperlight.Binary,
		filepath.Join(config.Hyperlight.RootfsDir, "bash.cpio"),
		filepath.Join(config.Hyperlight.RootfsDir, "python.cpio"),
		filepath.Join(config.Hyperlight.RootfsDir, "node.cpio")} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			t.Fatalf("real production Hyperlight bundle prerequisite %s: %v", path, err)
		}
		if path == config.Hyperlight.Binary && info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("production hluk is not executable: %s", path)
		}
	}
	// Umask is process-wide; these tests deliberately do not run in parallel.
	oldUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })
	// Avoid t.TempDir's private ancestor. Change only the directory we own,
	// never /tmp or another test's directories, to permit session traversal.
	root, err := os.MkdirTemp("/tmp", "orka-acp-hyperlight-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &hyperlightE2ESandboxFixture{root: root, workspace: filepath.Join(root, "workspace"), config: config}
	t.Cleanup(func() {
		if err := acp.ReclaimSessionOwnership(fixture.workspace); err != nil {
			t.Errorf("reclaim fixture workspace: %v", err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove fixture: %v", err)
		}
	})
	fixture.config.Hyperlight.CacheDir = filepath.Join(root, "cache")
	for _, path := range []string{root, fixture.config.Hyperlight.CacheDir} {
		if path != root {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		// The child must traverse to scripts/snapshots, but cannot list or
		// modify the root-owned cache. A 0700 cache would prevent real runs.
		if err := os.Chmod(path, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	dependency := filepath.Join(fixture.workspace, "node_modules", "workspace-dependency")
	if err := os.MkdirAll(dependency, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"package.json": `{"name":"workspace-package"}`,
		"node_modules/workspace-dependency/index.js": `module.exports = 'dependency-loaded';`,
	} {
		if err := os.WriteFile(filepath.Join(fixture.workspace, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := acp.FinalizeSessionOwnership(fixture.workspace, hyperlightE2ESessionUID, hyperlightE2ESessionGID); err != nil {
		t.Fatal(err)
	}
	fixture.checkCache(t)
	return fixture
}

func (f *hyperlightE2ESandboxFixture) session(t *testing.T, config SandboxExecConfig) (*mcpProxySession, string) {
	t.Helper()
	session, endpoint := newSandboxTestSession(t, newSandboxExecutor(&config))
	session.bindSandboxWorkspace(f.workspace, hyperlightE2ESessionUID, hyperlightE2ESessionGID)
	return session, endpoint
}

func (f *hyperlightE2ESandboxFixture) reclaim(t *testing.T) {
	t.Helper()
	if err := acp.ReclaimSessionOwnership(f.workspace); err != nil {
		t.Fatal(err)
	}
}

func (f *hyperlightE2ESandboxFixture) checkCache(t *testing.T) {
	t.Helper()
	for _, path := range []string{f.root, f.config.Hyperlight.CacheDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		owner := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode().Perm() != 0o711 || owner.Uid != 0 || owner.Gid != 0 {
			t.Fatalf("fixture/cache %s is not root-owned and traverse-only for sessions: mode = %v, uid = %d, gid = %d", path, info.Mode(), owner.Uid, owner.Gid)
		}
	}
}

func (f *hyperlightE2ESandboxFixture) runningProcess(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := bytes.Split(cmdline, []byte{0})
		if len(args) == 0 || string(args[0]) != f.config.Hyperlight.Binary {
			continue
		}
		found := false
		for _, arg := range args {
			if string(arg) == f.workspace+":"+acp.SandboxExecWorkspacePath {
				found = true
			}
		}
		if !found {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			t.Fatal(err)
		}
		fields := make(map[string][]string)
		for line := range strings.SplitSeq(string(status), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok {
				fields[key] = strings.Fields(value)
			}
		}
		for key, want := range map[string]int{"Uid": hyperlightE2ESessionUID, "Gid": hyperlightE2ESessionGID} {
			if len(fields[key]) < 2 || fields[key][1] != strconv.Itoa(want) {
				t.Fatalf("running hluk PID %d %s = %v, want effective identity %d", pid, key, fields[key], want)
			}
		}
		if f.config.DeviceGID != 0 {
			groups := " " + strings.Join(fields["Groups"], " ") + " "
			if !strings.Contains(groups, " "+strconv.FormatUint(uint64(f.config.DeviceGID), 10)+" ") {
				t.Fatalf("running hluk PID %d lacks KVM supplementary GID %d: %v", pid, f.config.DeviceGID, fields["Groups"])
			}
		}
		return pid
	}
	t.Fatal("guest wrote readiness, but no hluk process with this workspace mount is running")
	return 0
}

func hyperlightE2ESandboxCall(t *testing.T, language, code string, timeout int) string {
	t.Helper()
	arguments, err := json.Marshal(map[string]any{"language": language, "code": code, "timeout": timeout})
	if err != nil {
		t.Fatal(err)
	}
	return sandboxCall("e2e", string(arguments))
}

// Unlike doMCPRequest, this helper bounds the request and returns errors, so
// the cancellation test's goroutine never calls testing.Fatal/FailNow.
func hyperlightE2ESandboxHTTPRequest(ctx context.Context, endpoint, payload string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer credential")
	return http.DefaultClient.Do(request)
}

func hyperlightE2ESandboxRPC(t *testing.T, endpoint, payload string) mcpJSONRPCResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	response, err := hyperlightE2ESandboxHTTPRequest(ctx, endpoint, payload)
	if err != nil {
		t.Fatal(err)
	}
	return decodeMCPResponse(t, response)
}

func hyperlightE2ESandboxResult(t *testing.T, response mcpJSONRPCResponse) acp.SandboxExecResult {
	t.Helper()
	text, isError := sandboxToolResult(t, response)
	var result acp.SandboxExecResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("real guest returned no execution result: %q, error = %v", text, err)
	}
	if isError || result.ExitCode != 0 || result.TimedOut || result.StdoutTruncated || result.StderrTruncated {
		t.Fatalf("real sandbox_exec guest failed: %+v, isError = %t", result, isError)
	}
	structured, err := json.Marshal(response.Result.(map[string]any)["structuredContent"])
	if err != nil {
		t.Fatal(err)
	}
	var structuredResult acp.SandboxExecResult
	if err := json.Unmarshal(structured, &structuredResult); err != nil || structuredResult != result {
		t.Fatalf("public MCP structured result differs from text: %s, text = %s, error = %v", structured, text, err)
	}
	return result
}
