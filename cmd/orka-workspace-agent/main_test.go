/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceAgentRejectsUnauthenticatedExec(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	server := newWorkspaceAgentServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", strings.NewReader(`{"command":["echo","ok"]}`))
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}

func TestSafePathRejectsTraversal(t *testing.T) {
	if _, err := safePath("/workspace/../etc/passwd"); err == nil {
		t.Fatal("safePath accepted path traversal")
	}
}

func TestWorkspaceAgentExecTruncatesOutput(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	server := newWorkspaceAgentServer()
	server.defaultCommandTimeout = time.Second
	server.defaultMaxOutputBytes = 4
	body, err := json.Marshal(execRequest{
		Command:        []string{"sh", "-c", "printf abcdef"},
		WorkDir:        "/tmp",
		MaxOutputBytes: 4,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	var got execResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Stdout != "abcd" || !got.StdoutTruncated {
		t.Fatalf("stdout = %q truncated=%t, want abcd/true", got.Stdout, got.StdoutTruncated)
	}
}

func TestWorkspaceAgentDetachedExecCanBePolled(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	server := newWorkspaceAgentServer()
	body, err := json.Marshal(execRequest{
		Command: []string{"sh", "-c", "printf detached-ok"},
		WorkDir: "/tmp",
		Detach:  true,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	var started execResponse
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.ExecID == "" || !started.Running {
		t.Fatalf("start response execID=%q running=%t, want id/running", started.ExecID, started.Running)
	}

	var got execResponse
	for range 100 {
		statusReq := httptest.NewRequest(http.MethodGet, "/v1/exec/"+started.ExecID, nil)
		statusReq.Header.Set("Authorization", "Bearer secret")
		statusResp := httptest.NewRecorder()
		server.routes().ServeHTTP(statusResp, statusReq)
		if statusResp.Code != http.StatusOK {
			t.Fatalf("status poll code = %d, want %d: %s", statusResp.Code, http.StatusOK, statusResp.Body.String())
		}
		var status execResponse
		if err := json.NewDecoder(statusResp.Body).Decode(&status); err != nil {
			t.Fatalf("decode status response: %v", err)
		}
		got = status
		if !got.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Running || got.Stdout != "detached-ok" || got.ExitCode != 0 {
		t.Fatalf(
			"detached result running=%t stdout=%q exit=%d, want done/detached-ok/0",
			got.Running,
			got.Stdout,
			got.ExitCode,
		)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/v1/exec/"+started.ExecID, nil)
	statusReq.Header.Set("Authorization", "Bearer secret")
	statusResp := httptest.NewRecorder()
	server.routes().ServeHTTP(statusResp, statusReq)
	if statusResp.Code != http.StatusNotFound {
		t.Fatalf("completed detached exec status after poll = %d, want %d", statusResp.Code, http.StatusNotFound)
	}
}

func TestWorkspaceAgentDecodesLargeUploadRequest(t *testing.T) {
	server := newWorkspaceAgentServer()
	payload := bytes.Repeat([]byte("a"), 8<<20)
	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: "/tmp/large-worker",
		Data: payload,
		Mode: 0o700,
	}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if len(body) <= 10<<20 {
		t.Fatalf("test body length = %d, want above prior 10MiB limit", len(body))
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/files", bytes.NewReader(body))

	var got uploadRequest
	if err := server.decodeJSON(req, &got); err != nil {
		t.Fatalf("decodeJSON() error = %v", err)
	}
	decodedLen := 0
	if len(got.Files) == 1 {
		decodedLen = len(got.Files[0].Data)
	}
	if len(got.Files) != 1 || decodedLen != len(payload) {
		t.Fatalf("decoded payload length = %d, want %d", decodedLen, len(payload))
	}
}

func TestWorkspaceAgentMaxRequestBytesFromEnv(t *testing.T) {
	t.Setenv(envMaxRequestBytes, "1024")
	server := newWorkspaceAgentServer()
	if server.maxRequestBytes != 1024 {
		t.Fatalf("maxRequestBytes = %d, want 1024", server.maxRequestBytes)
	}
}

func TestWorkspaceAgentAllowsOnlyHandoffTokenBootstrap(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "handoff-token")
	resp := exerciseHandoffBootstrap(t, tokenFile, tokenFile)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "secret" {
		t.Fatalf("token file = %q, want secret", string(data))
	}
}

func TestWorkspaceAgentBootstrapDefaultUploadHonorsConfiguredTokenFile(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "custom-handoff-token")
	resp := exerciseHandoffBootstrap(t, tokenFile, defaultHandoffTokenUpload)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read configured token file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "secret" {
		t.Fatalf("configured token file = %q, want secret", string(data))
	}
}

func TestHandoffTokenFilePathNormalizesRelativeEnv(t *testing.T) {
	t.Setenv(envHandoffTokenFile, "custom-handoff-token")

	if got, want := handoffTokenFilePath(), "/app/custom-handoff-token"; got != want {
		t.Fatalf("handoffTokenFilePath() = %q, want %q", got, want)
	}
}

func exerciseHandoffBootstrap(t *testing.T, tokenFile, uploadPath string) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv(envHandoffTokenFile, tokenFile)
	server := newWorkspaceAgentServer()
	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: uploadPath,
		Data: []byte("secret"),
		Mode: 0o600,
	}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/files", bytes.NewReader(body))
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)
	return resp
}

func TestWorkspaceAgentRejectsInvalidHandoffBootstrapWithoutWritingFile(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	t.Setenv(envHandoffTokenFile, filepath.Join(dir, "handoff-token"))
	server := newWorkspaceAgentServer()
	disallowedPath := filepath.Join(dir, "not-the-token")
	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: disallowedPath,
		Data: []byte("secret"),
		Mode: 0o600,
	}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/files", bytes.NewReader(body))
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusUnauthorized, resp.Body.String())
	}
	if _, err := os.Stat(disallowedPath); !os.IsNotExist(err) {
		t.Fatalf("invalid bootstrap wrote %s: %v", disallowedPath, err)
	}
}

func TestWorkspaceAgentScrubRemovesConfiguredHandoffTokenFile(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "custom-handoff-token")
	otherFile := filepath.Join(dir, "scratch")
	if err := os.WriteFile(tokenFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if err := os.WriteFile(otherFile, []byte("scratch"), 0o600); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}
	t.Setenv(envHandoffTokenFile, tokenFile)
	server := newWorkspaceAgentServer()
	body, err := json.Marshal(scrubRequest{Paths: []string{otherFile}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/scrub", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNoContent, resp.Body.String())
	}
	for _, path := range []string{tokenFile, otherFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("scrub left %s: %v", path, err)
		}
	}
}

func TestSafePathRejectsSymlinkTargetsOutsideAllowedRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	outsideFile := filepath.Join(outside, "secret")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	linkFile := filepath.Join(root, "file-link")
	if err := os.Symlink(outsideFile, linkFile); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := safePath(linkFile); err == nil {
		t.Fatal("safePath accepted symlink file escaping allowed root")
	}

	linkDir := filepath.Join(root, "dir-link")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Skipf("symlink directory unavailable: %v", err)
	}
	if _, err := safePath(filepath.Join(linkDir, "new-file")); err == nil {
		t.Fatal("safePath accepted path through symlink directory escaping allowed root")
	}
}

func TestSafePathRejectsDanglingSymlinkTargetsOutsideAllowedRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "missing")
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	linkFile := filepath.Join(root, "dangling-file")
	if err := os.Symlink(outside, linkFile); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := safePath(linkFile); err == nil {
		t.Fatal("safePath accepted dangling symlink escaping allowed root")
	}
	if _, err := safePath(filepath.Join(linkFile, "new-file")); err == nil {
		t.Fatal("safePath accepted path through dangling symlink escaping allowed root")
	}
}
