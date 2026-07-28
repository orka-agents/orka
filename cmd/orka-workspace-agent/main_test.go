/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/workspace/daemonprotocol"
)

const testBootstrapHandoffToken = "secret"

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

func TestWorkspaceAgentExecTimeoutReturns124(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	server := newWorkspaceAgentServer()
	body, err := json.Marshal(execRequest{
		Command:        []string{"sh", "-c", "sleep 2"},
		WorkDir:        "/tmp",
		TimeoutSeconds: 1,
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
	if got.ExitCode != 124 {
		t.Fatalf("exitCode = %d, want 124", got.ExitCode)
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

	for poll := 2; poll <= 3; poll++ {
		statusReq := httptest.NewRequest(http.MethodGet, "/v1/exec/"+started.ExecID, nil)
		statusReq.Header.Set("Authorization", "Bearer secret")
		statusResp := httptest.NewRecorder()
		server.routes().ServeHTTP(statusResp, statusReq)
		if statusResp.Code != http.StatusOK {
			t.Fatalf(
				"completed detached exec status poll %d = %d, want %d: %s",
				poll,
				statusResp.Code,
				http.StatusOK,
				statusResp.Body.String(),
			)
		}
		var repeated execResponse
		if err := json.NewDecoder(statusResp.Body).Decode(&repeated); err != nil {
			t.Fatalf("decode repeated status response: %v", err)
		}
		if repeated != got {
			t.Fatalf("completed detached exec poll %d = %#v, want %#v", poll, repeated, got)
		}
	}
}

func TestCompletedExecutionRetainedUntilExpiry(t *testing.T) {
	server := newWorkspaceAgentServer()
	finishedAt := time.Now().UTC()
	want := execResponse{
		ExecID:     "completed-exec",
		Stdout:     "done",
		ExitCode:   0,
		StartedAt:  finishedAt.Add(-time.Second),
		FinishedAt: finishedAt,
	}
	server.storeExecution(want)

	for read := 1; read <= 2; read++ {
		got, ok := server.loadExecution(want.ExecID)
		if !ok {
			t.Fatalf("load %d missing completed execution before retention expires", read)
		}
		if got != want {
			t.Fatalf("load %d = %#v, want %#v", read, got, want)
		}
	}

	server.mu.Lock()
	server.evictCompletedExecutionsLocked(finishedAt.Add(completedExecutionRetention))
	server.mu.Unlock()
	if got, ok := server.loadExecution(want.ExecID); ok {
		t.Fatalf("expired completed execution still present: %#v", got)
	}
}

func TestWorkspaceAgentRejectsResidentExec(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	server := newWorkspaceAgentServer()

	for _, detach := range []bool{false, true} {
		t.Run(fmt.Sprintf("detach=%t", detach), func(t *testing.T) {
			body, err := json.Marshal(execRequest{
				Command:     []string{"sh", "-c", "printf unreachable"},
				WorkDir:     "/tmp",
				Detach:      detach,
				Resident:    true,
				ResidentKey: "session-1",
			})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			resp := httptest.NewRecorder()

			server.routes().ServeHTTP(resp, req)

			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
			}
			if !strings.Contains(resp.Body.String(), "resident exec is not supported yet") {
				t.Fatalf("body = %q, want unsupported resident message", resp.Body.String())
			}
		})
	}
}

func TestWorkspaceAgentExecDoesNotInheritDaemonAuthTokens(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	t.Setenv(envBootstrapToken, "bootstrap-secret")
	server := newWorkspaceAgentServer()
	body, err := json.Marshal(execRequest{
		Command: []string{
			"sh",
			"-c",
			`printf '%s/%s' "${ORKA_WORKSPACE_HANDOFF_TOKEN:-}" "${ORKA_WORKSPACE_BOOTSTRAP_TOKEN:-}"`,
		},
		WorkDir: "/tmp",
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
	if got.Stdout != "/" {
		t.Fatalf("auth env stdout = %q, want empty token values", got.Stdout)
	}
}

func TestWorkspaceAgentUploadUpdatesExistingFileMode(t *testing.T) {
	t.Setenv(envHandoffToken, "secret")
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	path := filepath.Join(dir, "worker")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	server := newWorkspaceAgentServer()
	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: path,
		Data: []byte("new"),
		Mode: 0o700,
	}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/files", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp := httptest.NewRecorder()

	server.routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("uploaded mode = %#o, want 0700", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("uploaded data = %q, want new", string(data))
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
	if strings.TrimSpace(string(data)) != testBootstrapHandoffToken {
		t.Fatalf("token file = %q, want %s", string(data), testBootstrapHandoffToken)
	}
}

func TestWorkspaceAgentSerializesBootstrapMutationWithFileAuthentication(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "handoff-token")
	t.Setenv(envHandoffTokenFile, tokenFile)
	t.Setenv(envBootstrapToken, "bootstrap-secret")
	server := newWorkspaceAgentServer()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseFirst)
		}
	}()

	handler := server.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bootstrap-secret" {
			close(firstEntered)
			<-releaseFirst
			if err := os.WriteFile(tokenFile, []byte("minted-secret"), 0o600); err != nil {
				t.Errorf("write minted token: %v", err)
				http.Error(w, "write failed", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})

	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: tokenFile,
		Data: []byte("minted-secret"),
		Mode: 0o600,
	}}})
	if err != nil {
		t.Fatalf("marshal upload: %v", err)
	}
	firstReq := httptest.NewRequest(http.MethodPut, daemonprotocol.FilesPath, bytes.NewReader(body))
	firstReq.Header.Set("Authorization", "Bearer bootstrap-secret")
	firstResp := httptest.NewRecorder()
	go func() {
		handler(firstResp, firstReq)
		close(firstDone)
	}()
	<-firstEntered

	secondReq := httptest.NewRequest(http.MethodPut, daemonprotocol.FilesPath, bytes.NewReader(body))
	secondReq.Header.Set("Authorization", "Bearer minted-secret")
	secondResp := httptest.NewRecorder()
	go func() {
		handler(secondResp, secondReq)
		close(secondDone)
	}()

	select {
	case <-secondDone:
		t.Fatal("second file request authenticated before bootstrap mutation completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	released = true
	<-firstDone
	<-secondDone
	if firstResp.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d: %s", firstResp.Code, http.StatusNoContent, firstResp.Body.String())
	}
	if secondResp.Code != http.StatusNoContent {
		t.Fatalf("second status = %d, want %d: %s", secondResp.Code, http.StatusNoContent, secondResp.Body.String())
	}
}

func TestWorkspaceAgentSerializesScrubAfterFileMutation(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "handoff-token")
	if err := os.WriteFile(tokenFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write initial token: %v", err)
	}
	t.Setenv(envHandoffToken, "")
	t.Setenv(envHandoffTokenFile, tokenFile)
	server := newWorkspaceAgentServer()

	uploadEntered := make(chan struct{})
	releaseUpload := make(chan struct{})
	uploadDone := make(chan struct{})
	scrubDone := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseUpload)
		}
	}()

	handler := server.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == daemonprotocol.FilesPath:
			close(uploadEntered)
			<-releaseUpload
			if err := os.WriteFile(tokenFile, []byte("secret"), 0o600); err != nil {
				t.Errorf("write handoff token: %v", err)
				http.Error(w, "write failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == daemonprotocol.ScrubPath:
			if err := os.Remove(tokenFile); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove handoff token: %v", err)
				http.Error(w, "scrub failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})

	uploadReq := httptest.NewRequest(http.MethodPut, daemonprotocol.FilesPath, bytes.NewReader([]byte(`{"files":[]}`)))
	uploadReq.Header.Set("Authorization", "Bearer secret")
	uploadResp := httptest.NewRecorder()
	go func() {
		handler(uploadResp, uploadReq)
		close(uploadDone)
	}()
	<-uploadEntered

	scrubReq := httptest.NewRequest(http.MethodPost, daemonprotocol.ScrubPath, bytes.NewReader([]byte(`{"paths":[]}`)))
	scrubReq.Header.Set("Authorization", "Bearer secret")
	scrubResp := httptest.NewRecorder()
	go func() {
		handler(scrubResp, scrubReq)
		close(scrubDone)
	}()

	scrubCompletedBeforeUpload := false
	select {
	case <-scrubDone:
		scrubCompletedBeforeUpload = true
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseUpload)
	released = true
	<-uploadDone
	<-scrubDone

	if scrubCompletedBeforeUpload {
		t.Fatal("scrub completed before the in-flight file mutation")
	}
	if uploadResp.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, want %d: %s", uploadResp.Code, http.StatusNoContent, uploadResp.Body.String())
	}
	if scrubResp.Code != http.StatusNoContent {
		t.Fatalf("scrub status = %d, want %d: %s", scrubResp.Code, http.StatusNoContent, scrubResp.Body.String())
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("handoff token exists after serialized scrub: %v", err)
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
	if strings.TrimSpace(string(data)) != testBootstrapHandoffToken {
		t.Fatalf("configured token file = %q, want %s", string(data), testBootstrapHandoffToken)
	}
}

func TestWorkspaceAgentHandoffBootstrapRepairsEmptyTokenFile(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "handoff-token")
	if err := os.WriteFile(tokenFile, []byte(" \n"), 0o600); err != nil {
		t.Fatalf("write empty token file: %v", err)
	}
	resp := exerciseHandoffBootstrap(t, tokenFile, tokenFile)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if strings.TrimSpace(string(data)) != testBootstrapHandoffToken {
		t.Fatalf("token file = %q, want %s", string(data), testBootstrapHandoffToken)
	}
}

func TestWorkspaceAgentRejectsHandoffBootstrapWithoutBearerProof(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "handoff-token")
	resp := exerciseHandoffBootstrapWithBearer(t, tokenFile, tokenFile, "")

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusUnauthorized, resp.Body.String())
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("unauthorized bootstrap wrote token file: %v", err)
	}
}

func TestWorkspaceAgentRejectsHandoffBootstrapBearerMismatch(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "handoff-token")
	resp := exerciseHandoffBootstrapWithBearer(t, tokenFile, tokenFile, "different")

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusUnauthorized, resp.Body.String())
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("mismatched bootstrap wrote token file: %v", err)
	}
}

func TestWorkspaceAgentRejectsHandoffBootstrapBearerMatchingUploadedToken(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "handoff-token")
	resp := exerciseHandoffBootstrapWithBearer(t, tokenFile, tokenFile, testBootstrapHandoffToken)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusUnauthorized, resp.Body.String())
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("self-proved bootstrap wrote token file: %v", err)
	}
}

func TestWorkspaceAgentRejectsHandoffBootstrapWhenTokenPathUnreadable(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "handoff-token")
	if err := os.Mkdir(tokenFile, 0o700); err != nil {
		t.Fatalf("mkdir token path: %v", err)
	}
	resp := exerciseHandoffBootstrapWithBearer(t, tokenFile, tokenFile, "bootstrap-secret")

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusServiceUnavailable, resp.Body.String())
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
	return exerciseHandoffBootstrapWithBearer(t, tokenFile, uploadPath, "bootstrap-secret")
}

func exerciseHandoffBootstrapWithBearer(
	t *testing.T,
	tokenFile string,
	uploadPath string,
	bearer string,
) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv(envHandoffTokenFile, tokenFile)
	t.Setenv(envBootstrapToken, "bootstrap-secret")
	server := newWorkspaceAgentServer()
	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: uploadPath,
		Data: []byte(testBootstrapHandoffToken),
		Mode: 0o600,
	}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/files", bytes.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
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
	t.Setenv(envBootstrapToken, "bootstrap-secret")
	server := newWorkspaceAgentServer()
	disallowedPath := filepath.Join(dir, "not-the-token")
	body, err := json.Marshal(uploadRequest{Files: []uploadFile{{
		Path: disallowedPath,
		Data: []byte(testBootstrapHandoffToken),
		Mode: 0o600,
	}}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/files", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bootstrap-secret")
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
