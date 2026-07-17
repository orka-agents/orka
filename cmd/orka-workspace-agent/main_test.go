/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	workspaceagent "github.com/orka-agents/orka/pkg/workspaceagent"
)

const (
	testBootstrapHandoffToken = "secret"
	testWorkspaceUID          = "workspace-uid"
)

func TestWorkspaceAgentRejectsUnauthenticatedExec(t *testing.T) {
	t.Setenv(envHandoffAuth, "secret")
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
	t.Setenv(envHandoffAuth, "secret")
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
	t.Setenv(envHandoffAuth, "secret")
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
	t.Setenv(envHandoffAuth, "secret")
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
	if statusResp.Code != http.StatusOK {
		t.Fatalf("completed detached exec status after poll = %d, want %d", statusResp.Code, http.StatusOK)
	}
	var retained execResponse
	if err := json.NewDecoder(statusResp.Body).Decode(&retained); err != nil {
		t.Fatalf("decode retained response: %v", err)
	}
	if retained.Running || retained.Stdout != got.Stdout || retained.ExitCode != got.ExitCode {
		t.Fatalf("retained response = %#v, want original terminal result %#v", retained, got)
	}
}

func TestWorkspaceAgentRejectsResidentExec(t *testing.T) {
	t.Setenv(envHandoffAuth, "secret")
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
	t.Setenv(envHandoffAuth, "secret")
	t.Setenv(envBootstrapAuth, "bootstrap-secret")
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
	t.Setenv(envHandoffAuth, "secret")
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

func TestWorkspaceAgentRejectsSpecialUploadModes(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	t.Setenv(envHandoffAuth, "secret")
	server := newWorkspaceAgentServer()
	body := mustJSON(t, uploadRequest{Files: []uploadFile{{
		Path: filepath.Join(dir, "setuid-worker"),
		Data: []byte("binary"),
		Mode: uint32(os.ModeSetuid | 0o700),
	}}})
	request := httptest.NewRequest(http.MethodPut, workspaceagent.FilesPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("special mode upload status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestWorkspaceAgentUploadOwnsFilesForCommandIdentity(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	server.controlAuthConfigured = true
	server.commandUID = uint32(os.Getuid())
	server.commandGID = uint32(os.Getgid())
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	path := filepath.Join(dir, "nested", "worker")
	body := mustJSON(t, uploadRequest{
		Versioned: workspaceagent.NewVersioned(),
		Files: []uploadFile{{
			Path: path,
			Data: []byte("binary"),
			Mode: 0o700,
		}},
	})
	response := doAttachmentRequest(
		t, server, http.MethodPut, workspaceagent.FilesPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", response.Code, response.Body.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("uploaded mode = %o, want 700", info.Mode().Perm())
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

func TestSecureFileOperationsRejectSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	outsideFile := filepath.Join(outside, "sensitive")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := secureWriteFile(
		filepath.Join(link, "written"), []byte("bad"), 0o600, false, 0, 0, time.Time{},
	); err == nil {
		t.Fatal("secureWriteFile followed a symlinked parent")
	}
	if _, _, err := secureReadFile(filepath.Join(link, "sensitive"), defaultMaxDownloadBytes); err == nil {
		t.Fatal("secureReadFile followed a symlinked parent")
	}
	if err := secureRemoveAll(link); err != nil {
		t.Fatalf("secureRemoveAll symlink: %v", err)
	}
	if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "outside" {
		t.Fatalf("secureRemoveAll modified symlink target: data=%q err=%v", data, err)
	}
}

func TestV1DataPathsIncludeEveryResettableRoot(t *testing.T) {
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{"/app", "/workspace", "/tmp", "/dev/shm"}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	for _, path := range []string{"/app/task-file", "/workspace/task-file", "/tmp/task-file", "/dev/shm/task-file"} {
		if !v1DataPathAllowed(path) {
			t.Fatalf("v1 data path rejected resettable path %q", path)
		}
	}
}

func TestCommandWritableRootsAreResettable(t *testing.T) {
	for _, writable := range commandWritableRoots {
		found := false
		for _, resettable := range allowedRoots {
			if filepath.Clean(writable) == filepath.Clean(resettable) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("command writable root %q is not resettable", writable)
		}
	}
}

func TestValidateCommandWriteConfinementFailsClosed(t *testing.T) {
	if err := validateCommandWriteConfinement(false, false); err != nil {
		t.Fatalf("legacy confinement validation: %v", err)
	}
	if err := validateCommandWriteConfinement(true, true); err != nil {
		t.Fatalf("secured confinement validation: %v", err)
	}
	if err := validateCommandWriteConfinement(true, false); !errors.Is(
		err, errCommandWriteConfinementUnavailable,
	) {
		t.Fatalf("missing confinement error = %v, want %v", err, errCommandWriteConfinementUnavailable)
	}
}

func TestWorkspaceAgentRejectsExecWithoutCommandWriteConfinement(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	server.controlAuthConfigured = true
	server.writeConfinement = false
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, execRequest{
		Versioned: workspaceagent.NewVersioned(), OperationID: "unconfined", Command: []string{"true"},
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"unconfined exec status = %d, want %d: %s",
			response.Code,
			http.StatusServiceUnavailable,
			response.Body.String(),
		)
	}
}

func TestWorkspaceAgentResetRequiresCommandWriteConfinement(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	server.controlAuthConfigured = true
	server.writeConfinement = false
	server.boundWorkspaceUID = testWorkspaceUID
	body := resetBodyForServer(t, server, "reset-without-confinement", []string{dir})
	response := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, body)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("reset status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
}

func TestWorkspaceAgentAdvertisesResetOnlyWithCommandWriteConfinement(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	server.controlAuthConfigured = true
	request := httptest.NewRequest(http.MethodGet, workspaceagent.CapabilitiesPath, nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d: %s", response.Code, response.Body.String())
	}
	var capabilities workspaceagent.CapabilitiesResponse
	if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if slices.Contains(capabilities.Features, "reset") {
		t.Fatal("reset advertised without command write confinement")
	}

	server.writeConfinement = true
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
		t.Fatalf("decode confined capabilities: %v", err)
	}
	if !slices.Contains(capabilities.Features, "reset") {
		t.Fatal("reset not advertised with command write confinement")
	}
	if capabilities.MaxOperationIDsPerEpoch != defaultMaxOperationIDsPerEpoch {
		t.Fatalf(
			"max operation IDs per epoch = %d, want %d",
			capabilities.MaxOperationIDsPerEpoch,
			defaultMaxOperationIDsPerEpoch,
		)
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

func TestWorkspaceAgentBootstrapDefaultUploadHonorsConfiguredTokenFile(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() {
		allowedRoots = previousAllowedRoots
	})
	tokenFile := filepath.Join(dir, "custom-handoff-token")
	resp := exerciseHandoffBootstrap(t, tokenFile, defaultHandoffUploadAlias)

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
	t.Setenv(envHandoffAuthFile, "custom-handoff-token")

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
	t.Setenv(envHandoffAuthFile, tokenFile)
	t.Setenv(envBootstrapAuth, "bootstrap-secret")
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
	t.Setenv(envHandoffAuthFile, filepath.Join(dir, "handoff-token"))
	t.Setenv(envBootstrapAuth, "bootstrap-secret")
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
	t.Setenv(envHandoffAuthFile, tokenFile)
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

func TestWorkspaceAgentAttachmentActivationRevocationAndEpochRollover(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task-1", 1, "token-one", time.Now().Add(time.Minute))

	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "epoch-one",
		Command:     []string{"sh", "-c", "printf one"},
		WorkDir:     "/tmp",
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token-one",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("epoch one exec status = %d: %s", response.Code, response.Body.String())
	}

	rolloverBody := mustJSON(t, workspaceagent.AttachmentControlRequest{
		Versioned:         workspaceagent.NewVersioned(),
		WorkspaceUID:      testWorkspaceUID,
		BindingGeneration: server.bindingGeneration,
		TaskUID:           "task-2",
		Epoch:             2,
		TokenSHA256:       digest([]byte("token-two")),
		ExpiresAt:         time.Now().Add(time.Minute),
	})
	rollover := doControlRequest(t, server, http.MethodPut, workspaceagent.AttachmentControlPath, rolloverBody)
	if rollover.Code != http.StatusConflict {
		t.Fatalf("active epoch rollover status = %d, want %d", rollover.Code, http.StatusConflict)
	}

	for _, tc := range []struct {
		name         string
		workspaceUID string
		epoch        int64
		token        string
	}{
		{name: "wrong workspace", workspaceUID: "other", epoch: 1, token: "token-one"},
		{name: "wrong epoch", workspaceUID: testWorkspaceUID, epoch: 2, token: "token-one"},
		{name: "wrong token", workspaceUID: testWorkspaceUID, epoch: 1, token: "token-two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := doAttachmentRequest(
				t, server, http.MethodPost, workspaceagent.ExecPath, body, tc.workspaceUID, tc.epoch, tc.token,
			)
			if got.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d: %s", got.Code, http.StatusUnauthorized, got.Body.String())
			}
		})
	}

	revokeWorkspaceAttachment(t, server)
	stale := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token-one",
	)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("stale token status after revocation = %d, want %d", stale.Code, http.StatusUnauthorized)
	}

	activateWorkspaceAttachment(t, server, "task-2", 2, "token-two", time.Now().Add(time.Minute))
	oldToken := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token-one",
	)
	if oldToken.Code != http.StatusUnauthorized {
		t.Fatalf("old epoch status = %d, want %d", oldToken.Code, http.StatusUnauthorized)
	}
	body = mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "epoch-two",
		Command:     []string{"sh", "-c", "printf two"},
		WorkDir:     "/tmp",
	})
	newToken := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 2, "token-two",
	)
	if newToken.Code != http.StatusOK {
		t.Fatalf("new epoch status = %d: %s", newToken.Code, newToken.Body.String())
	}
}

func TestWorkspaceAgentSameEpochActivationIsExactlyIdempotent(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	expiresAt := time.Now().Add(time.Minute).UTC()
	first := requestWorkspaceAttachment(
		t, server, testWorkspaceUID, "task", 1, "token", expiresAt,
	)
	if first.Code != http.StatusOK {
		t.Fatalf("first activation status = %d: %s", first.Code, first.Body.String())
	}
	retry := requestWorkspaceAttachment(
		t, server, testWorkspaceUID, "task", 1, "token", expiresAt,
	)
	if retry.Code != http.StatusOK {
		t.Fatalf("idempotent activation status = %d: %s", retry.Code, retry.Body.String())
	}
	stale := requestWorkspaceAttachment(
		t, server, testWorkspaceUID, "task", 1, "token", expiresAt.Add(time.Minute),
	)
	if stale.Code != http.StatusConflict {
		t.Fatalf("different same-epoch activation status = %d, want %d", stale.Code, http.StatusConflict)
	}
}

func TestWorkspaceAgentExpiredAttachmentRejectsDataRequests(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(20*time.Millisecond))
	time.Sleep(30 * time.Millisecond)
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "expired",
		Command:     []string{"true"},
		WorkDir:     "/tmp",
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "expired") {
		t.Fatalf("expired response = %d %q", response.Code, response.Body.String())
	}
}

func TestWorkspaceAgentRunningOperationStopsAtAttachmentExpiry(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	terminated := filepath.Join(dir, "terminated")
	server.processTerminator = func(context.Context) error {
		return os.WriteFile(terminated, []byte("terminated"), 0o600)
	}
	expiresAt := time.Now().Add(300 * time.Millisecond)
	activateWorkspaceAttachment(t, server, "task", 1, "token", expiresAt)
	output := filepath.Join(dir, "after-expiry")
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "attachment-expiry",
		Command: []string{
			"sh",
			"-c",
			fmt.Sprintf("sleep 1; test -e %q || touch %q", terminated, output),
		},
		WorkDir: dir,
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", response.Code, response.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		server.mu.Lock()
		autoRevoked := server.lastRevokedEpoch == 1 &&
			server.revokingEpoch == 0 &&
			server.activeAttachment == nil
		running := server.runningByEpoch[1]
		operationRunning := server.operationRunning["attachment-expiry"]
		server.mu.Unlock()
		if autoRevoked && running == 0 && !operationRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation and revocation did not complete after attachment expiry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(terminated); err != nil {
		t.Fatalf("attachment expiry did not invoke the process terminator: %v", err)
	}
	if wait := time.Until(expiresAt.Add(900 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("operation mutated workspace after attachment expiry: %v", err)
	}
}

func TestWorkspaceAgentDispatchRacePreservesExpiryStatus(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	server.activeAttachment = &attachmentState{epoch: 1, expiresAt: time.Now().Add(-time.Second)}
	request := execRequest{OperationID: "expired-dispatch", Command: []string{"true"}}
	_, err := server.startExecution(
		request,
		normalizedExecRequest{workDir: "/tmp", timeout: time.Second, maxOutput: 1024},
		1,
	)
	if !errors.Is(err, errAttachmentExpired) {
		t.Fatalf("expired dispatch error = %v, want %v", err, errAttachmentExpired)
	}
	if status := executionStartErrorStatus(err); status != http.StatusUnauthorized {
		t.Fatalf("expired dispatch status = %d, want %d", status, http.StatusUnauthorized)
	}

	server.activeAttachment = nil
	server.expiringEpoch = 1
	request.OperationID = "expiring-dispatch"
	_, err = server.startExecution(
		request,
		normalizedExecRequest{workDir: "/tmp", timeout: time.Second, maxOutput: 1024},
		1,
	)
	if !errors.Is(err, errAttachmentExpired) || executionStartErrorStatus(err) != http.StatusUnauthorized {
		t.Fatalf("expiring dispatch = error %v status %d", err, executionStartErrorStatus(err))
	}
}

func TestUploadWritePolicyDropsOrdinaryFilesToCommandIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envHandoffAuthFile, filepath.Join(dir, "handoff"))
	mode, setOwner := uploadWritePolicy(filepath.Join(dir, "ordinary"), true, true, 0o640)
	if mode != 0o640 || !setOwner {
		t.Fatalf("ordinary upload policy = mode %o setOwner=%t", mode, setOwner)
	}
	mode, setOwner = uploadWritePolicy(handoffTokenFilePath(), true, true, 0o644)
	if mode != 0o600 || setOwner {
		t.Fatalf("handoff upload policy = mode %o setOwner=%t", mode, setOwner)
	}
	_, setOwner = uploadWritePolicy(filepath.Join(dir, "ordinary"), true, false, 0o640)
	if setOwner {
		t.Fatal("non-root supervisor requested ownership change")
	}
}

func TestWorkspaceAgentDownloadFiltersProtectedFiles(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	protected := filepath.Join(root, "control-auth")
	normal := filepath.Join(root, "result.txt")
	if err := os.WriteFile(protected, []byte("protected"), 0o600); err != nil {
		t.Fatalf("write protected file: %v", err)
	}
	if err := os.WriteFile(normal, []byte("result"), 0o600); err != nil {
		t.Fatalf("write normal file: %v", err)
	}
	server := newFencedWorkspaceAgentServer()
	server.controlAuthPath = protected
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))

	directBody := mustJSON(t, downloadRequest{
		Versioned: workspaceagent.NewVersioned(),
		Paths:     []string{protected},
	})
	direct := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.FilesDownloadPath, directBody, testWorkspaceUID, 1, "token",
	)
	if direct.Code != http.StatusBadRequest {
		t.Fatalf("protected download status = %d, want %d", direct.Code, http.StatusBadRequest)
	}

	allBody := mustJSON(t, downloadRequest{Versioned: workspaceagent.NewVersioned()})
	all := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.FilesDownloadPath, allBody, testWorkspaceUID, 1, "token",
	)
	if all.Code != http.StatusOK {
		t.Fatalf("tree download status = %d: %s", all.Code, all.Body.String())
	}
	var response downloadResponse
	if err := json.NewDecoder(all.Body).Decode(&response); err != nil {
		t.Fatalf("decode tree download: %v", err)
	}
	if len(response.Artifacts) != 1 || string(response.Artifacts[0].Data) != "result" {
		t.Fatalf("tree download artifacts = %#v", response.Artifacts)
	}
}

func TestWorkspaceAgentDownloadEnforcesCumulativeLimit(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("abc"), 0o600); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	if err := os.WriteFile(second, []byte("def"), 0o600); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	server := newFencedWorkspaceAgentServer()
	server.maxDownloadBytes = 5
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, downloadRequest{
		Versioned: workspaceagent.NewVersioned(),
		Paths:     []string{first, second},
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.FilesDownloadPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("download status = %d, want %d: %s", response.Code, http.StatusRequestEntityTooLarge, response.Body.String())
	}
}

func TestWorkspaceAgentDownloadMapsBoundedTreeFailure(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	deepest := root
	for range maxWorkspaceTreeDepth + 1 {
		deepest = filepath.Join(deepest, "d")
	}
	if err := os.MkdirAll(deepest, 0o755); err != nil {
		t.Fatalf("create deep workspace tree: %v", err)
	}

	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, downloadRequest{Versioned: workspaceagent.NewVersioned()})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.FilesDownloadPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf(
			"bounded tree download status = %d, want %d: %s",
			response.Code,
			http.StatusRequestEntityTooLarge,
			response.Body.String(),
		)
	}
}

func TestWorkspaceAgentRejectsRequestBodyCompletedAfterExpiry(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(30*time.Millisecond))
	path := filepath.Join(dir, "late-upload")
	body := mustJSON(t, uploadRequest{
		Versioned: workspaceagent.NewVersioned(),
		Files:     []uploadFile{{Path: path, Data: []byte("late")}},
	})
	reader := newGatedReader(body)
	request := httptest.NewRequest(http.MethodPut, workspaceagent.FilesPath, reader)
	setAttachmentHeaders(request, testWorkspaceUID, 1, "token")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(response, request)
		close(done)
	}()
	<-reader.started
	time.Sleep(50 * time.Millisecond)
	close(reader.release)
	<-done
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("late upload status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("late upload mutated workspace: %v", err)
	}
}

func TestWorkspaceAgentStaleTokenCannotUseAnyDataOperation(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	revokeWorkspaceAttachment(t, server)

	execBody := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "stale",
		Command:     []string{"true"},
	})
	cancelBody := mustJSON(t, workspaceagent.NewVersioned())
	uploadBody := mustJSON(t, uploadRequest{
		Versioned: workspaceagent.NewVersioned(),
		Files:     []uploadFile{{Path: "/tmp/stale", Data: []byte("x")}},
	})
	downloadBody := mustJSON(t, downloadRequest{
		Versioned: workspaceagent.NewVersioned(),
		Paths:     []string{"/tmp/stale"},
	})
	scrubBody := mustJSON(t, scrubRequest{
		Versioned: workspaceagent.NewVersioned(),
		Paths:     []string{"/tmp/stale"},
	})
	resetBody := mustJSON(t, workspaceagent.ResetRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "reset-operation",
		Paths:       []string{"/tmp"},
	})
	requests := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{name: "exec", method: http.MethodPost, path: workspaceagent.ExecPath, body: execBody},
		{name: "status", method: http.MethodGet, path: workspaceagent.ExecStatusPath("stale")},
		{name: "cancel", method: http.MethodPost, path: workspaceagent.ExecCancelPath("stale"), body: cancelBody},
		{name: "upload", method: http.MethodPut, path: workspaceagent.FilesPath, body: uploadBody},
		{name: "download", method: http.MethodPost, path: workspaceagent.FilesDownloadPath, body: downloadBody},
		{name: "scrub", method: http.MethodPost, path: workspaceagent.ScrubPath, body: scrubBody},
		{name: "reset", method: http.MethodPost, path: workspaceagent.ResetPath, body: resetBody},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			response := doAttachmentRequest(t, server, tc.method, tc.path, tc.body, testWorkspaceUID, 1, "token")
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusUnauthorized, response.Body.String())
			}
		})
	}
}

func TestWorkspaceAgentDuplicateOperationIDExecutesOnce(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	output := filepath.Join(dir, "count")
	request := execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "idempotent-operation",
		Command:     []string{"sh", "-c", fmt.Sprintf("printf x >> %q", output)},
		WorkDir:     dir,
	}
	body := mustJSON(t, request)
	first := doAttachmentRequest(t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token")
	if first.Code != http.StatusOK {
		t.Fatalf("first exec status = %d: %s", first.Code, first.Body.String())
	}
	waitForOperation(t, server, testWorkspaceUID, 1, "token", request.OperationID, workspaceagent.OperationStateSucceeded)

	second := doAttachmentRequest(t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token")
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate exec status = %d: %s", second.Code, second.Body.String())
	}
	time.Sleep(20 * time.Millisecond)
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "x" {
		t.Fatalf("output = %q, want one execution", data)
	}
}

func TestWorkspaceAgentTimeoutStatusIsRetained(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, execRequest{
		Versioned:      workspaceagent.NewVersioned(),
		OperationID:    "timeout-operation",
		Command:        []string{"sh", "-c", "sleep 2"},
		WorkDir:        "/tmp",
		TimeoutSeconds: 1,
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", response.Code, response.Body.String())
	}
	first := waitForOperation(
		t, server, testWorkspaceUID, 1, "token", "timeout-operation", workspaceagent.OperationStateFailed,
	)
	second := getOperation(t, server, testWorkspaceUID, 1, "token", "timeout-operation")
	retained := second.ExitCode == first.ExitCode &&
		second.State == first.State && second.FinishedAt.Equal(first.FinishedAt)
	if first.ExitCode != 124 || !retained {
		t.Fatalf("retained timeout mismatch: first=%#v second=%#v", first, second)
	}
}

func TestWorkspaceAgentCancelReachesTerminalCancelledState(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "cancel-operation",
		Command:     []string{"sh", "-c", "sleep 10"},
		WorkDir:     "/tmp",
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", response.Code, response.Body.String())
	}
	cancel := doAttachmentRequest(
		t,
		server,
		http.MethodPost,
		workspaceagent.ExecCancelPath("cancel-operation"),
		mustJSON(t, workspaceagent.NewVersioned()),
		testWorkspaceUID,
		1,
		"token",
	)
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel status = %d: %s", cancel.Code, cancel.Body.String())
	}
	result := waitForOperation(
		t, server, testWorkspaceUID, 1, "token", "cancel-operation", workspaceagent.OperationStateCancelled,
	)
	if result.ExitCode != 130 || result.Running {
		t.Fatalf("cancel result = %#v", result)
	}
}

func TestWorkspaceAgentPrivilegedScrubRequiresRevocation(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	scrubBody := scrubBodyForServer(t, server, []string{path})
	before := doControlRequest(t, server, http.MethodPost, workspaceagent.ScrubPath, scrubBody)
	if before.Code != http.StatusConflict {
		t.Fatalf("scrub before revoke = %d, want %d: %s", before.Code, http.StatusConflict, before.Body.String())
	}
	revokeWorkspaceAttachment(t, server)
	after := doControlRequest(t, server, http.MethodPost, workspaceagent.ScrubPath, scrubBody)
	if after.Code != http.StatusOK {
		t.Fatalf("scrub after revoke = %d: %s", after.Code, after.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("scrub left file: %v", err)
	}
}

func newFencedWorkspaceAgentServer() *workspaceAgentServer {
	server := newWorkspaceAgentServer()
	server.controlAuth = "control-token"
	server.legacyAuthEnabled = false
	return server
}

func activateWorkspaceAttachment(
	t *testing.T,
	server *workspaceAgentServer,
	taskUID string,
	epoch int64,
	token string,
	expiresAt time.Time,
) {
	t.Helper()
	response := requestWorkspaceAttachment(
		t, server, testWorkspaceUID, taskUID, epoch, token, expiresAt,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("activate attachment status = %d: %s", response.Code, response.Body.String())
	}
}

func requestWorkspaceAttachment(
	t *testing.T,
	server *workspaceAgentServer,
	workspaceUID, taskUID string,
	epoch int64,
	auth string,
	expiresAt time.Time,
) *httptest.ResponseRecorder {
	t.Helper()
	body := mustJSON(t, workspaceagent.AttachmentControlRequest{
		Versioned:         workspaceagent.NewVersioned(),
		WorkspaceUID:      workspaceUID,
		BindingGeneration: server.bindingGeneration,
		TaskUID:           taskUID,
		Epoch:             epoch,
		TokenSHA256:       digest([]byte(auth)),
		ExpiresAt:         expiresAt.UTC(),
	})
	return doControlRequest(t, server, http.MethodPut, workspaceagent.AttachmentControlPath, body)
}

func revokeWorkspaceAttachment(t *testing.T, server *workspaceAgentServer) {
	t.Helper()
	response := requestWorkspaceAttachmentRevocation(t, server, 1)
	if response.Code != http.StatusOK {
		t.Fatalf("revoke attachment status = %d: %s", response.Code, response.Body.String())
	}
}

func requestWorkspaceAttachmentRevocation(
	t *testing.T,
	server *workspaceAgentServer,
	epoch int64,
) *httptest.ResponseRecorder {
	t.Helper()
	body := mustJSON(t, workspaceagent.AttachmentRevocationRequest{
		Versioned:         workspaceagent.NewVersioned(),
		WorkspaceUID:      server.boundWorkspaceUID,
		BindingGeneration: server.bindingGeneration,
	})
	return doControlRequest(
		t,
		server,
		http.MethodDelete,
		workspaceagent.AttachmentRevocationPath(strconv.FormatInt(epoch, 10)),
		body,
	)
}

func scrubBodyForServer(t *testing.T, server *workspaceAgentServer, paths []string) []byte {
	t.Helper()
	return mustJSON(t, scrubRequest{
		Versioned:         workspaceagent.NewVersioned(),
		WorkspaceUID:      server.boundWorkspaceUID,
		BindingGeneration: server.bindingGeneration,
		Paths:             paths,
	})
}

func resetBodyForServer(
	t *testing.T,
	server *workspaceAgentServer,
	operationID string,
	paths []string,
) []byte {
	t.Helper()
	return mustJSON(t, workspaceagent.ResetRequest{
		Versioned:         workspaceagent.NewVersioned(),
		OperationID:       operationID,
		WorkspaceUID:      server.boundWorkspaceUID,
		BindingGeneration: server.bindingGeneration,
		Paths:             paths,
	})
}

func doControlRequest(
	t *testing.T,
	server *workspaceAgentServer,
	method, path string,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer control-token")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, req)
	return response
}

func setAttachmentHeaders(req *http.Request, workspaceUID string, epoch int64, auth string) {
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set(workspaceagent.WorkspaceUIDHeader, workspaceUID)
	req.Header.Set(workspaceagent.AttachmentEpochHeader, strconv.FormatInt(epoch, 10))
}

type gatedReader struct {
	payload []byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
	read    bool
}

func newGatedReader(payload []byte) *gatedReader {
	return &gatedReader{
		payload: payload,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *gatedReader) Read(buffer []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	return copy(buffer, r.payload), nil
}

func doAttachmentRequest(
	t *testing.T,
	server *workspaceAgentServer,
	method, path string,
	body []byte,
	workspaceUID string,
	epoch int64,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	setAttachmentHeaders(req, workspaceUID, epoch, token)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, req)
	return response
}

func waitForOperation(
	t *testing.T,
	server *workspaceAgentServer,
	workspaceUID string,
	epoch int64,
	token, operationID string,
	state workspaceagent.OperationState,
) execResponse {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result := getOperation(t, server, workspaceUID, epoch, token, operationID)
		if result.State == state && !result.Running {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %q did not reach state %s", operationID, state)
	return execResponse{}
}

func getOperation(
	t *testing.T,
	server *workspaceAgentServer,
	workspaceUID string,
	epoch int64,
	token, operationID string,
) execResponse {
	t.Helper()
	response := doAttachmentRequest(
		t, server, http.MethodGet, workspaceagent.ExecStatusPath(operationID), nil, workspaceUID, epoch, token,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status operation %q = %d: %s", operationID, response.Code, response.Body.String())
	}
	var result execResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode operation %q: %v", operationID, err)
	}
	return result
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return data
}

func TestWorkspaceAgentV1DetachRequiresCallerOperationID(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, execRequest{
		Versioned: workspaceagent.NewVersioned(),
		Command:   []string{"true"},
		Detach:    true,
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("detach without operationID status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	server.mu.Lock()
	operations := len(server.executions)
	server.mu.Unlock()
	if operations != 0 {
		t.Fatalf("detach without operationID started %d operations", operations)
	}
}

func TestWorkspaceAgentCleanupBlocksConcurrentActivation(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "scrub", path: workspaceagent.ScrubPath},
		{name: "reset", path: workspaceagent.ResetPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newFencedWorkspaceAgentServer()
			activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
			revokeWorkspaceAttachment(t, server)

			body := scrubBodyForServer(t, server, []string{filepath.Join(dir, "scratch")})
			if tc.name == "reset" {
				body = resetBodyForServer(t, server, "reset-operation", []string{dir})
			}
			reader := newGatedReader(body)
			request := httptest.NewRequest(http.MethodPost, tc.path, reader)
			request.Header.Set("Authorization", "Bearer control-token")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				server.routes().ServeHTTP(response, request)
				close(done)
			}()
			<-reader.started

			activation := requestWorkspaceAttachment(
				t,
				server,
				testWorkspaceUID,
				"task-2",
				2,
				"token-2",
				time.Now().Add(time.Minute),
			)
			close(reader.release)
			<-done
			serialized := (activation.Code == http.StatusConflict && response.Code == http.StatusOK) ||
				(activation.Code == http.StatusOK && response.Code == http.StatusConflict)
			if !serialized {
				t.Fatalf(
					"activation/cleanup were not serialized for %s: activation=%d cleanup=%d body=%s",
					tc.name, activation.Code, response.Code, response.Body.String(),
				)
			}
		})
	}
}

func TestSecureWriteFileIsIdempotentForMatchingContent(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	path := filepath.Join(dir, "artifact")
	data := bytes.Repeat([]byte("a"), 1<<20)
	originalTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if err := os.Chtimes(path, originalTime, originalTime); err != nil {
		t.Fatalf("set artifact time: %v", err)
	}
	metadata, err := secureWriteFile(path, data, 0o600, false, 0, 0, time.Time{})
	if err != nil {
		t.Fatalf("secureWriteFile identical content: %v", err)
	}
	if !metadata.ModTime.Equal(originalTime) {
		t.Fatalf("identical write changed modtime from %v to %v", originalTime, metadata.ModTime)
	}
	updated := []byte("updated")
	if _, err := secureWriteFile(path, updated, 0o600, false, 0, 0, time.Time{}); err != nil {
		t.Fatalf("secureWriteFile updated content: %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(updated) {
		t.Fatalf("updated content = %q, err=%v", stored, err)
	}
}

func TestWorkspaceAgentRejectsOutOfRangeCommandIdentity(t *testing.T) {
	t.Setenv(envCommandUID, "4294967296")
	server := newWorkspaceAgentServer()
	if server.startupErr == nil {
		t.Fatal("workspace-agent accepted a command UID that wraps to root")
	}
}

func TestWorkspaceAgentControlAuthRequiresIsolatedFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root isolation failure is specific to non-root test execution")
	}
	path := filepath.Join(t.TempDir(), "control-auth")
	if err := os.WriteFile(path, []byte("example-control-auth"), 0o600); err != nil {
		t.Fatalf("write control auth: %v", err)
	}
	t.Setenv(envControlAuthFile, path)
	server := newWorkspaceAgentServer()
	if server.startupErr == nil {
		t.Fatal("non-root workspace-agent accepted a privileged control auth file")
	}
	if server.legacyAuthEnabled {
		t.Fatal("configured but invalid control auth enabled legacy fallback")
	}
}

func TestWorkspaceAgentSecuredOperationsSerialize(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	server.controlAuthConfigured = true
	server.activeAttachment = &attachmentState{epoch: 1, expiresAt: time.Now().Add(time.Minute)}
	server.executions["already-running"] = execResponse{
		Versioned: workspaceagent.NewVersioned(), OperationID: "already-running", Running: true,
	}
	server.executionEpochs["already-running"] = 1
	server.operationRunning["already-running"] = true
	server.runningByEpoch[1] = 1

	request := execRequest{OperationID: "second-operation", Command: []string{"true"}}
	_, err := server.startExecution(
		request,
		normalizedExecRequest{workDir: "/tmp", timeout: time.Second, maxOutput: 1024},
		1,
	)
	if !errors.Is(err, errOperationBusy) {
		t.Fatalf("concurrent secured operation error = %v, want %v", err, errOperationBusy)
	}
}

func TestWorkspaceAgentBackgroundDescendantPreservesSuccessfulRootOutcome(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	output := filepath.Join(dir, "late-background-write")
	server := newFencedWorkspaceAgentServer()
	response := server.runExec(
		context.Background(),
		execRequest{Command: []string{
			"sh", "-c", fmt.Sprintf("(sleep 1; touch %q) >/dev/null 2>&1 & exit 0", output),
		}},
		normalizedExecRequest{workDir: dir, timeout: 3 * time.Second, maxOutput: 1024},
	)
	if response.State != workspaceagent.OperationStateSucceeded || response.ExitCode != 0 || response.IsolationFailed {
		t.Fatalf(
			"background descendant response = state %q exit %d isolationFailed=%t",
			response.State,
			response.ExitCode,
			response.IsolationFailed,
		)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("background descendant survived successful command cleanup: %v", err)
	}
}

func TestWorkspaceAgentSecuredCompletionTerminatesNamespaceProcesses(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	calls := 0
	server.processTerminator = func(context.Context) error {
		calls++
		return nil
	}
	response := server.runExec(
		context.Background(),
		execRequest{Command: []string{"true"}},
		normalizedExecRequest{workDir: "/tmp", timeout: time.Second, maxOutput: 1024},
	)
	if response.State != workspaceagent.OperationStateSucceeded || calls != 1 {
		t.Fatalf("secured completion = state %q terminator calls %d", response.State, calls)
	}

	server.processTerminator = func(context.Context) error {
		return errors.New("namespace drain failed")
	}
	response = server.runExec(
		context.Background(),
		execRequest{Command: []string{"true"}},
		normalizedExecRequest{workDir: "/tmp", timeout: time.Second, maxOutput: 1024},
	)
	if response.State != workspaceagent.OperationStateSucceeded || !response.IsolationFailed {
		t.Fatalf("failed namespace drain response = state %q isolationFailed=%t", response.State, response.IsolationFailed)
	}
}

func TestWorkspaceAgentBoundsRetainedOperationResults(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	finished := time.Now().UTC()
	server.mu.Lock()
	server.activeAttachment = &attachmentState{epoch: 1, expiresAt: time.Now().Add(time.Minute)}
	for i := range defaultMaxRetainedOperations {
		id := fmt.Sprintf("operation-%03d", i)
		server.executions[id] = execResponse{
			Versioned:   workspaceagent.NewVersioned(),
			OperationID: id,
			ExecID:      id,
			State:       workspaceagent.OperationStateSucceeded,
			FinishedAt:  finished,
		}
		server.executionEpochs[id] = 1
	}
	server.evictCompletedExecutionsLocked(finished.Add(time.Minute))
	retained := len(server.executions)
	server.mu.Unlock()
	if retained != defaultMaxRetainedOperations {
		t.Fatalf("retained operations = %d, want %d", retained, defaultMaxRetainedOperations)
	}

	request := execRequest{OperationID: "new-operation", Command: []string{"true"}}
	_, err := server.startExecution(request, normalizedExecRequest{workDir: "/tmp", timeout: time.Second}, 1)
	if !errors.Is(err, errOperationCapacity) {
		t.Fatalf("start beyond capacity error = %v, want %v", err, errOperationCapacity)
	}

	request.OperationID = "operation-000"
	normalized := normalizedExecRequest{workDir: "/tmp", timeout: time.Second}
	fingerprint, err := executionRequestFingerprint(request, normalized)
	if err != nil {
		t.Fatalf("fingerprint retained operation: %v", err)
	}
	server.mu.Lock()
	server.executionFingerprints[request.OperationID] = fingerprint
	server.mu.Unlock()
	if _, err := server.startExecution(
		request, normalized, 1,
	); err != nil {
		t.Fatalf("duplicate retained operation rejected: %v", err)
	}

	server.mu.Lock()
	server.evictCompletedExecutionsLocked(finished.Add(completedExecutionRetention + time.Second))
	results := len(server.executions)
	tombstones := len(server.executionTombstones)
	server.mu.Unlock()
	if results != 0 || tombstones != defaultMaxRetainedOperations {
		t.Fatalf("post-expiry results=%d tombstones=%d", results, tombstones)
	}
	if _, err := server.startExecution(
		request, normalized, 1,
	); !errors.Is(err, errOperationResultExpired) {
		t.Fatalf("expired operation retry error = %v, want %v", err, errOperationResultExpired)
	}
	newRequest := execRequest{OperationID: "after-result-expiry", Command: []string{"true"}}
	if _, err := server.startExecution(newRequest, normalized, 1); err != nil {
		t.Fatalf("new operation rejected by tombstones: %v", err)
	}
	server.mu.Lock()
	server.evictCompletedExecutionsLocked(
		finished.Add(completedExecutionRetention + operationTombstoneRetention + 2*time.Second),
	)
	remainingTombstones := len(server.executionTombstones)
	server.mu.Unlock()
	if remainingTombstones != 0 {
		t.Fatalf("expired tombstones retained = %d", remainingTombstones)
	}
	if _, err := server.startExecution(request, normalized, 1); !errors.Is(err, errOperationResultExpired) {
		t.Fatalf("operation ownership expired within active epoch: %v", err)
	}
}

func TestWorkspaceAgentBoundsOperationIDsPerEpoch(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	server.activeAttachment = &attachmentState{epoch: 1, expiresAt: time.Now().Add(time.Minute)}
	for i := range defaultMaxOperationIDsPerEpoch {
		id := fmt.Sprintf("owned-%04d", i)
		server.executionEpochs[id] = 1
		server.executionFingerprints[id] = "fingerprint"
	}
	_, err := server.startExecution(
		execRequest{OperationID: "beyond-owned-capacity", Command: []string{"true"}},
		normalizedExecRequest{workDir: "/tmp", timeout: time.Second, maxOutput: 1024},
		1,
	)
	if !errors.Is(err, errOperationCapacity) {
		t.Fatalf("operation ID capacity error = %v, want %v", err, errOperationCapacity)
	}
}

func TestWorkspaceAgentRevocationWaitsForAuthenticatedDispatch(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	output := filepath.Join(dir, "should-not-exist")
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "revocation-race",
		Command:     []string{"sh", "-c", fmt.Sprintf("touch %q", output)},
		WorkDir:     dir,
	})
	reader := newGatedReader(body)
	execReq := httptest.NewRequest(http.MethodPost, workspaceagent.ExecPath, reader)
	setAttachmentHeaders(execReq, testWorkspaceUID, 1, "token")
	execResp := httptest.NewRecorder()
	execDone := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(execResp, execReq)
		close(execDone)
	}()
	<-reader.started

	revokeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		revokeDone <- doControlRequest(
			t,
			server,
			http.MethodDelete,
			workspaceagent.AttachmentRevocationPath("1"),
			mustJSON(t, workspaceagent.AttachmentRevocationRequest{
				Versioned: workspaceagent.NewVersioned(), WorkspaceUID: testWorkspaceUID,
				BindingGeneration: server.bindingGeneration,
			}),
		)
	}()
	select {
	case response := <-revokeDone:
		t.Fatalf("revocation returned before authenticated request drained: %d", response.Code)
	case <-time.After(30 * time.Millisecond):
	}

	close(reader.release)
	<-execDone
	response := <-revokeDone
	if response.Code != http.StatusOK {
		t.Fatalf("revocation status = %d: %s", response.Code, response.Body.String())
	}
	if execResp.Code != http.StatusConflict {
		t.Fatalf("racing exec status = %d, want %d: %s", execResp.Code, http.StatusConflict, execResp.Body.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("racing exec mutated workspace after revocation: %v", err)
	}
}

func TestWorkspaceAgentRevocationWaitsForRunningOperationCancellation(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "revoke-running",
		Command:     []string{"sh", "-c", "sleep 10"},
		WorkDir:     "/tmp",
	})
	started := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if started.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", started.Code, started.Body.String())
	}
	revokeWorkspaceAttachment(t, server)
	server.mu.Lock()
	_, retained := server.executions["revoke-running"]
	running := server.runningByEpoch[1]
	server.mu.Unlock()
	if retained || running != 0 {
		t.Fatalf("revoked epoch retained operation state: retained=%t running=%d", retained, running)
	}
}

func TestWorkspaceAgentRevocationKillsOperationProcessGroup(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	output := filepath.Join(dir, "orphan-mutation")
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "process-group",
		Command: []string{
			"sh",
			"-c",
			fmt.Sprintf("(sleep 0.2; touch %q) >/dev/null 2>&1 & exit 0", output),
		},
		WorkDir: dir,
	})
	started := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "token",
	)
	if started.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", started.Code, started.Body.String())
	}
	revokeWorkspaceAttachment(t, server)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("descendant process mutated workspace after revocation: %v", err)
	}
}

func TestWorkspaceAgentRejectsRevocationOlderThanLatestActivation(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task-1", 1, "token-1", time.Now().Add(time.Minute))
	revokeWorkspaceAttachment(t, server)
	activateWorkspaceAttachment(t, server, "task-2", 2, "token-2", time.Now().Add(time.Minute))

	second := requestWorkspaceAttachmentRevocation(t, server, 2)
	if second.Code != http.StatusOK {
		t.Fatalf("epoch 2 revocation status = %d: %s", second.Code, second.Body.String())
	}
	stale := requestWorkspaceAttachmentRevocation(t, server, 1)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale revocation status = %d, want %d: %s", stale.Code, http.StatusConflict, stale.Body.String())
	}
	if server.lastRevokedEpoch != 2 || server.revokingEpoch != 0 || server.activeAttachment != nil {
		t.Fatalf(
			"stale revocation changed fencing state: lastRevoked=%d revoking=%d active=%v",
			server.lastRevokedEpoch,
			server.revokingEpoch,
			server.activeAttachment,
		)
	}
}

func TestWorkspaceAgentRevocationRetryPreservesWorkspaceIdentity(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	attempts := 0
	server.processTerminator = func(context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("transient process termination failure")
		}
		return nil
	}
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))

	first := requestWorkspaceAttachmentRevocation(t, server, 1)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first revocation status = %d, want %d: %s", first.Code, http.StatusInternalServerError, first.Body.String())
	}
	second := requestWorkspaceAttachmentRevocation(t, server, 1)
	if second.Code != http.StatusOK {
		t.Fatalf("retry revocation status = %d: %s", second.Code, second.Body.String())
	}
	var response workspaceagent.AttachmentControlResponse
	if err := json.NewDecoder(second.Body).Decode(&response); err != nil {
		t.Fatalf("decode retry revocation response: %v", err)
	}
	if response.WorkspaceUID != testWorkspaceUID || response.BindingGeneration != server.bindingGeneration ||
		response.ActiveEpoch != 1 || response.Active {
		t.Fatalf("retry revocation response = %#v", response)
	}
	if server.lastRevokedWorkspace != testWorkspaceUID || attempts != 2 {
		t.Fatalf("retry state = workspace %q attempts %d", server.lastRevokedWorkspace, attempts)
	}

	idempotent := requestWorkspaceAttachmentRevocation(t, server, 1)
	if idempotent.Code != http.StatusOK {
		t.Fatalf("idempotent revocation status = %d: %s", idempotent.Code, idempotent.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("idempotent revocation repeated termination: attempts=%d", attempts)
	}
}

func TestWorkspaceAgentRejectsStaleDeleteDuringRevocation(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task-1", 1, "token-1", time.Now().Add(time.Minute))
	revokeWorkspaceAttachment(t, server)
	activateWorkspaceAttachment(t, server, "task-2", 2, "token-2", time.Now().Add(time.Minute))

	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "revoking-two",
		Command:     []string{"true"},
	})
	reader := newGatedReader(body)
	request := httptest.NewRequest(http.MethodPost, workspaceagent.ExecPath, reader)
	setAttachmentHeaders(request, testWorkspaceUID, 2, "token-2")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(response, request)
		close(done)
	}()
	<-reader.started

	revokeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		revokeDone <- doControlRequest(
			t, server, http.MethodDelete, workspaceagent.AttachmentRevocationPath("2"),
			mustJSON(t, workspaceagent.AttachmentRevocationRequest{
				Versioned: workspaceagent.NewVersioned(), WorkspaceUID: testWorkspaceUID,
				BindingGeneration: server.bindingGeneration,
			}),
		)
	}()
	time.Sleep(20 * time.Millisecond)
	stale := doControlRequest(
		t, server, http.MethodDelete, workspaceagent.AttachmentRevocationPath("1"),
		mustJSON(t, workspaceagent.AttachmentRevocationRequest{
			Versioned: workspaceagent.NewVersioned(), WorkspaceUID: testWorkspaceUID,
			BindingGeneration: server.bindingGeneration,
		}),
	)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale DELETE status = %d, want %d", stale.Code, http.StatusConflict)
	}
	close(reader.release)
	<-done
	if final := <-revokeDone; final.Code != http.StatusOK {
		t.Fatalf("epoch 2 revocation status = %d: %s", final.Code, final.Body.String())
	}
}

func TestWorkspaceAgentRejectsPartialResetBeforeRebinding(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{firstRoot, secondRoot}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	server.boundWorkspaceUID = testWorkspaceUID
	beforeGeneration := server.bindingGeneration
	body := resetBodyForServer(t, server, "reset-operation", []string{firstRoot})
	response := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("partial reset status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if server.boundWorkspaceUID != testWorkspaceUID || server.bindingGeneration != beforeGeneration {
		t.Fatal("partial reset released the existing workspace binding")
	}
}

func TestResetWorkspaceBindingClearsExpiredOperationOwnership(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	server.executionEpochs["expired-operation"] = 1
	server.executionFingerprints["expired-operation"] = "fingerprint"
	server.executionTombstones["expired-operation"] = time.Now().UTC()
	if _, err := server.resetWorkspaceBinding("reset-operation", "reset-fingerprint"); err != nil {
		t.Fatalf("resetWorkspaceBinding: %v", err)
	}
	if len(server.executionEpochs) != 0 || len(server.executionFingerprints) != 0 ||
		len(server.executionTombstones) != 0 {
		t.Fatalf(
			"operation metadata after reset = epochs %d fingerprints %d tombstones %d",
			len(server.executionEpochs),
			len(server.executionFingerprints),
			len(server.executionTombstones),
		)
	}
}

func TestWorkspaceAgentResetClearsRootsButPreservesAgentFiles(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	protected := filepath.Join(root, "control-auth")
	stale := filepath.Join(root, "stale-task-data")
	if err := os.WriteFile(protected, []byte("protected"), 0o600); err != nil {
		t.Fatalf("write protected file: %v", err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	server := newFencedWorkspaceAgentServer()
	server.controlAuthPath = protected
	server.boundWorkspaceUID = testWorkspaceUID
	body := resetBodyForServer(t, server, "reset-operation", nil)
	response := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, body)
	if response.Code != http.StatusOK {
		t.Fatalf("reset status = %d: %s", response.Code, response.Body.String())
	}
	if data, err := os.ReadFile(protected); err != nil || string(data) != "protected" {
		t.Fatalf("reset removed protected file: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("reset retained stale task data: %v", err)
	}
}

func TestWorkspaceAgentResetAllowsBindingDifferentWorkspaceUID(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	server := newFencedWorkspaceAgentServer()
	oldGeneration := server.bindingGeneration
	activateWorkspaceAttachment(t, server, "task", 1, "token", time.Now().Add(time.Minute))
	revokeWorkspaceAttachment(t, server)
	resetBody := resetBodyForServer(t, server, "reset-operation-a", []string{dir})
	reset := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, resetBody)
	if reset.Code != http.StatusOK {
		t.Fatalf("reset status = %d: %s", reset.Code, reset.Body.String())
	}
	firstResetGeneration := server.bindingGeneration
	if firstResetGeneration == oldGeneration {
		t.Fatal("reset did not rotate the workspace binding generation")
	}
	retryReset := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, resetBody)
	if retryReset.Code != http.StatusOK || server.bindingGeneration != firstResetGeneration {
		t.Fatal("idempotent reset changed the current binding")
	}
	staleBody := mustJSON(t, workspaceagent.AttachmentControlRequest{
		Versioned:         workspaceagent.NewVersioned(),
		WorkspaceUID:      testWorkspaceUID,
		BindingGeneration: oldGeneration,
		TaskUID:           "stale-task",
		Epoch:             1,
		TokenSHA256:       digest([]byte("stale-token")),
		ExpiresAt:         time.Now().Add(time.Minute),
	})
	stale := doControlRequest(t, server, http.MethodPut, workspaceagent.AttachmentControlPath, staleBody)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale binding activation status = %d, want %d", stale.Code, http.StatusConflict)
	}

	other := requestWorkspaceAttachment(
		t, server, "other-workspace", "task-2", 1, "other-token", time.Now().Add(time.Minute),
	)
	if other.Code != http.StatusOK {
		t.Fatalf("rebind after reset status = %d: %s", other.Code, other.Body.String())
	}
	revokeWorkspaceAttachment(t, server)
	secondResetBody := resetBodyForServer(t, server, "reset-operation-b", []string{dir})
	secondReset := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, secondResetBody)
	if secondReset.Code != http.StatusOK {
		t.Fatalf("second reset status = %d: %s", secondReset.Code, secondReset.Body.String())
	}
	secondGeneration := server.bindingGeneration
	delayedFirst := doControlRequest(t, server, http.MethodPost, workspaceagent.ResetPath, resetBody)
	if delayedFirst.Code != http.StatusOK {
		t.Fatalf("delayed first reset status = %d: %s", delayedFirst.Code, delayedFirst.Body.String())
	}
	if server.bindingGeneration != secondGeneration {
		t.Fatal("delayed reset replay changed the current binding generation")
	}
}

func TestWorkspaceAgentStartupRequiresResetBeforeActivation(t *testing.T) {
	dir := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{dir}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	stale := filepath.Join(dir, "stale-task-data")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale task data: %v", err)
	}

	server := newFencedWorkspaceAgentServer()
	server.resetRequired = true
	before := requestWorkspaceAttachment(
		t, server, "new-workspace", "task", 1, "token", time.Now().Add(time.Minute),
	)
	if before.Code != http.StatusConflict {
		t.Fatalf(
			"activation before startup reset status = %d, want %d: %s",
			before.Code,
			http.StatusConflict,
			before.Body.String(),
		)
	}

	resetRequest := workspaceagent.ResetRequest{
		Versioned:         workspaceagent.NewVersioned(),
		OperationID:       "startup-reset",
		WorkspaceUID:      "new-workspace",
		BindingGeneration: server.bindingGeneration,
		Paths:             []string{dir},
	}
	reset := doControlRequest(
		t, server, http.MethodPost, workspaceagent.ResetPath, mustJSON(t, resetRequest),
	)
	if reset.Code != http.StatusOK {
		t.Fatalf("startup reset status = %d: %s", reset.Code, reset.Body.String())
	}
	if server.resetRequired {
		t.Fatal("successful startup reset left resetRequired set")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("startup reset retained stale task data: %v", err)
	}
	after := requestWorkspaceAttachment(
		t, server, "new-workspace", "task", 1, "token", time.Now().Add(time.Minute),
	)
	if after.Code != http.StatusOK {
		t.Fatalf("activation after startup reset status = %d: %s", after.Code, after.Body.String())
	}
}

func TestWorkspaceAgentRejectsDotSegmentOperationIDs(t *testing.T) {
	for _, operationID := range []string{".", ".."} {
		if validOperationID(operationID) {
			t.Fatalf("validOperationID(%q) = true", operationID)
		}
	}
}

func TestWorkspaceAgentAuthenticationErrorsDoNotEchoTokens(t *testing.T) {
	server := newFencedWorkspaceAgentServer()
	activateWorkspaceAttachment(t, server, "task", 1, "correct-token", time.Now().Add(time.Minute))
	body := mustJSON(t, execRequest{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: "secret-error",
		Command:     []string{"true"},
	})
	response := doAttachmentRequest(
		t, server, http.MethodPost, workspaceagent.ExecPath, body, testWorkspaceUID, 1, "wrong-secret-token",
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	leakedWrong := strings.Contains(response.Body.String(), "wrong-secret-token")
	leakedCorrect := strings.Contains(response.Body.String(), "correct-token")
	if leakedWrong || leakedCorrect {
		t.Fatalf("authentication error leaked token: %q", response.Body.String())
	}
}
