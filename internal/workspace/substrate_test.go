/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	substrateTestBearer          = "Bearer token"
	substrateTestBootstrapBearer = "Bearer bootstrap-token"
	substrateTestFilesPath       = "/v1/files"
	substrateTestScrubPath       = "/v1/scrub"
	substrateTestToken           = "token"
)

func TestNewSubstrateExecutorDefaultHTTPClientHasNoTimeout(t *testing.T) {
	executor, err := NewSubstrateExecutor(SubstrateConfig{
		RouterURL:      "http://router.test",
		ActorDNSSuffix: "actors.test",
		ControlClient:  &recordingSubstrateControlClient{},
	})
	if err != nil {
		t.Fatalf("NewSubstrateExecutor() error = %v", err)
	}
	if executor.httpClient == nil {
		t.Fatal("httpClient = nil, want default client")
	}
	if executor.httpClient.Timeout != 0 {
		t.Fatalf("default HTTP timeout = %s, want operation context controlled timeout", executor.httpClient.Timeout)
	}
}

func TestSubstrateTransportCredentialsRequireExplicitTrust(t *testing.T) {
	_, err := substrateTransportCredentials(SubstrateConfig{})
	if err == nil {
		t.Fatal("expected missing API trust error")
	}
	if !IsKind(err, ErrorKindInvalidArgument) {
		t.Fatalf("substrateTransportCredentials() error kind = %s, want %s", KindOf(err), ErrorKindInvalidArgument)
	}
	if !strings.Contains(err.Error(), "Substrate API trust") {
		t.Fatalf("substrateTransportCredentials() error = %q, want API trust context", err.Error())
	}
}

func TestSubstrateClaimReattachesAfterConcurrentCreateAlreadyExists(t *testing.T) {
	const (
		actorID  = "actor-1"
		reuseKey = "session-1"
	)
	control := &recordingSubstrateControlClient{
		getErrs: []error{
			NewError("get actor", ErrorKindNotFound, "actor not found", false, nil),
			nil,
		},
		createErr: NewError("create actor", ErrorKindAlreadyExists, "actor already exists", false, nil),
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     http.DefaultClient,
		routerURL:      "http://router.test",
		actorDNSSuffix: "actors.test",
		now:            time.Now,
		retained:       map[string]bool{},
	}

	got, err := executor.Claim(t.Context(), ClaimRequest{
		Namespace:       "ate-demo",
		ClaimName:       actorID,
		CreateIfMissing: true,
		Template:        TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"},
		ReuseKey:        reuseKey,
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if control.createCalls != 1 {
		t.Fatalf("CreateActor calls = %d, want 1", control.createCalls)
	}
	if control.getCalls != 2 {
		t.Fatalf("GetActor calls = %d, want 2", control.getCalls)
	}
	if !got.Reused || got.Created {
		t.Fatalf("Claim() reused=%t created=%t, want reused existing actor", got.Reused, got.Created)
	}
	if got.Ref.ID != actorID || got.ReuseKey != reuseKey {
		t.Fatalf("Claim() ref=%#v reuseKey=%q, want %s/%s", got.Ref, got.ReuseKey, actorID, reuseKey)
	}
}

func TestSubstrateExecUsesDetachedPolling(t *testing.T) {
	var sawDetached bool
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "actor-1.actors.test" {
			t.Errorf("Host = %q, want actor-1.actors.test", r.Host)
		}
		if got := r.Header.Get("Authorization"); got != substrateTestBearer {
			t.Errorf("Authorization = %q, want bearer token", got)
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/exec":
			var req substrateExecRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode exec request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sawDetached = req.Detach
			_ = json.NewEncoder(w).Encode(substrateExecResponse{ExecID: "exec-1", Running: true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/exec/exec-1":
			polls++
			_ = json.NewEncoder(w).Encode(substrateExecResponse{
				ExecID:   "exec-1",
				Stdout:   "ok",
				Stderr:   "warn",
				ExitCode: 0,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := &SubstrateWorkspaceExecutor{
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
	}

	got, err := executor.Exec(t.Context(), ExecRequest{
		Ref:     WorkspaceRef{ID: "actor-1"},
		Command: []string{"echo", "ok"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if !sawDetached {
		t.Fatal("Exec() did not request detached execution")
	}
	if polls == 0 {
		t.Fatal("Exec() did not poll detached execution")
	}
	if got.Stdout != "ok" || got.Stderr != "warn" || got.ExitCode != 0 {
		t.Fatalf("Exec() result = stdout %q stderr %q exit %d, want ok/warn/0", got.Stdout, got.Stderr, got.ExitCode)
	}
}

func TestSubstrateBootstrapHandoffUploadUsesBootstrapToken(t *testing.T) {
	var uploaded bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != substrateTestBootstrapBearer {
			t.Errorf("Authorization = %q, want bootstrap bearer", got)
		}
		if r.Method != http.MethodPut || r.URL.Path != substrateTestFilesPath {
			http.NotFound(w, r)
			return
		}
		var req substrateUploadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode upload request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(req.Files) != 1 {
			t.Errorf("upload files len = %d, want 1", len(req.Files))
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		file := req.Files[0]
		if file.Path != substrateHandoffTokenUploadPath || string(file.Data) != substrateTestToken {
			t.Errorf("upload file = path %q data %q, want handoff token", file.Path, string(file.Data))
		}
		uploaded = true
		_ = json.NewEncoder(w).Encode(substrateUploadResponse{})
	}))
	defer server.Close()

	executor := &SubstrateWorkspaceExecutor{
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		bootstrapToken: "bootstrap-token",
	}

	_, err := executor.Upload(t.Context(), UploadRequest{
		Ref:              WorkspaceRef{ID: "actor-1"},
		BootstrapHandoff: true,
		Artifacts: []UploadArtifact{{
			Path: substrateHandoffTokenUploadPath,
			Data: []byte(substrateTestToken),
			Mode: 0o600,
		}},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if !uploaded {
		t.Fatal("Upload() did not send handoff token")
	}
}

func TestSubstrateWaitReadyFailsFastOnNonRetryableActorError(t *testing.T) {
	control := &recordingSubstrateControlClient{
		getErrs: []error{NewError("get actor", ErrorKindNotFound, "actor missing", false, nil)},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     http.DefaultClient,
		routerURL:      "http://router.test",
		actorDNSSuffix: "actors.test",
		now:            time.Now,
	}

	_, err := executor.WaitReady(t.Context(), WaitReadyRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("WaitReady() error = nil, want not found error")
	}
	if !IsKind(err, ErrorKindNotFound) {
		t.Fatalf("WaitReady() error kind = %s, want %s", KindOf(err), ErrorKindNotFound)
	}
	if control.getCalls != 1 {
		t.Fatalf("GetActor calls = %d, want fail-fast single call", control.getCalls)
	}
}

func TestSubstrateWaitReadyFailsFastOnNonRetryableDaemonError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
	}

	_, err := executor.WaitReady(t.Context(), WaitReadyRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("WaitReady() error = nil, want daemon auth error")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("WaitReady() error = %q, want HTTP 401", err.Error())
	}
	if control.getCalls != 1 {
		t.Fatalf("GetActor calls = %d, want fail-fast single call", control.getCalls)
	}
}

func TestSubstrateDeleteWaitsForSuspendedAfterSuspend(t *testing.T) {
	var scrubbed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			scrubbed = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses:   []string{substrateStatusRunning, substrateStatusSuspending, substrateStatusSuspended},
		suspendStatus: substrateStatusSuspending,
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
		retained:       map[string]bool{},
	}

	got, err := executor.Delete(t.Context(), DeleteRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !scrubbed {
		t.Fatal("Delete() did not scrub the running actor")
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", control.suspendCalls)
	}
	if !control.deleted {
		t.Fatal("DeleteActor was not called")
	}
	if !got.Deleted || got.Phase != PhaseDeleted {
		t.Fatalf("Delete() = %#v, want deleted phase", got)
	}
}

func TestSubstrateDeleteWaitsWhenSuspendReturnsAfterStartingTransition(t *testing.T) {
	var scrubbed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			scrubbed = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning, substrateStatusSuspending, substrateStatusSuspended},
		suspendErr:  fmt.Errorf("suspend transition still in progress"),
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
		retained:       map[string]bool{},
	}

	got, err := executor.Delete(t.Context(), DeleteRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !scrubbed {
		t.Fatal("Delete() did not scrub the running actor")
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", control.suspendCalls)
	}
	if !control.deleted {
		t.Fatal("DeleteActor was not called")
	}
	if !got.Deleted || got.Phase != PhaseDeleted {
		t.Fatalf("Delete() = %#v, want deleted phase", got)
	}
}

func TestSubstrateDeleteSkipScrubDeletesRunningActor(t *testing.T) {
	var scrubbed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			scrubbed = true
			http.Error(w, "scrub should be skipped", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
		retained:       map[string]bool{},
	}

	got, err := executor.Delete(t.Context(), DeleteRequest{
		Ref:       WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout:   time.Second,
		SkipScrub: true,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if scrubbed {
		t.Fatal("Delete() scrubbed despite SkipScrub")
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", control.suspendCalls)
	}
	if !control.deleted {
		t.Fatal("DeleteActor was not called")
	}
	if !got.Deleted || got.Phase != PhaseDeleted {
		t.Fatalf("Delete() = %#v, want deleted phase", got)
	}
}

func TestSubstrateDeleteFailsClosedWhenRunningScrubFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			http.Error(w, "scrub failed", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
		retained:       map[string]bool{},
	}

	_, err := executor.Delete(t.Context(), DeleteRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("Delete() error = nil, want scrub failure")
	}
	if !IsKind(err, ErrorKindFailedPrecondition) {
		t.Fatalf("Delete() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
	}
	if control.suspendCalls != 0 {
		t.Fatalf("SuspendActor calls = %d, want 0 after scrub failure", control.suspendCalls)
	}
	if control.deleted {
		t.Fatal("DeleteActor was called after scrub failure")
	}
}

func TestSubstrateDeleteRestoresHandoffTokenWhenSuspendFailsAfterScrub(t *testing.T) {
	var restored bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath:
			if got := r.Header.Get("Authorization"); got != substrateTestBearer {
				t.Errorf("scrub Authorization = %q, want handoff bearer", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && r.URL.Path == substrateTestFilesPath:
			if got := r.Header.Get("Authorization"); got != substrateTestBootstrapBearer {
				t.Errorf("restore Authorization = %q, want bootstrap bearer", got)
			}
			var req substrateUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode restore request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(req.Files) != 1 {
				t.Errorf("restore files len = %d, want 1", len(req.Files))
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			file := req.Files[0]
			if file.Path != substrateHandoffTokenUploadPath ||
				string(file.Data) != substrateTestToken ||
				file.Mode != 0o600 {
				t.Errorf("restore file = path %q data %q mode %#o, want handoff token", file.Path, string(file.Data), file.Mode)
			}
			restored = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning, substrateStatusRunning},
		suspendErr:  fmt.Errorf("suspend failed"),
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		bootstrapToken: "bootstrap-token",
		now:            time.Now,
		retained:       map[string]bool{},
	}

	_, err := executor.Delete(t.Context(), DeleteRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("Delete() error = nil, want suspend failure")
	}
	if !restored {
		t.Fatal("Delete() did not restore handoff token after suspend failure")
	}
	if control.deleted {
		t.Fatal("DeleteActor was called after suspend failure")
	}
}

func TestSubstrateReleaseRestoresHandoffTokenWhenSuspendFailsAfterScrub(t *testing.T) {
	var scrubbed bool
	var restored bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath:
			if got := r.Header.Get("Authorization"); got != substrateTestBearer {
				t.Errorf("scrub Authorization = %q, want handoff bearer", got)
			}
			scrubbed = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && r.URL.Path == substrateTestFilesPath:
			if got := r.Header.Get("Authorization"); got != substrateTestBootstrapBearer {
				t.Errorf("restore Authorization = %q, want bootstrap bearer", got)
			}
			var req substrateUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode restore request: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(req.Files) != 1 {
				t.Errorf("restore files len = %d, want 1", len(req.Files))
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			file := req.Files[0]
			if file.Path != substrateHandoffTokenUploadPath ||
				string(file.Data) != substrateTestToken ||
				file.Mode != 0o600 {
				t.Errorf("restore file = path %q data %q mode %#o, want handoff token", file.Path, string(file.Data), file.Mode)
			}
			restored = true
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning},
		suspendErr:  fmt.Errorf("suspend failed"),
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		bootstrapToken: "bootstrap-token",
		now:            time.Now,
		retained:       map[string]bool{},
	}

	_, err := executor.Release(t.Context(), ReleaseRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Retain:  true,
		Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("Release() error = nil, want suspend failure")
	}
	if !scrubbed {
		t.Fatal("Release() did not scrub before suspend")
	}
	if !restored {
		t.Fatal("Release() did not restore handoff token after suspend failure")
	}
}

func TestSubstrateReleaseWaitsForSuspendedAfterSuspend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses:   []string{substrateStatusSuspending, substrateStatusSuspended},
		suspendStatus: substrateStatusSuspending,
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		now:            time.Now,
		retained:       map[string]bool{},
	}

	got, err := executor.Release(t.Context(), ReleaseRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Retain:  true,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", control.suspendCalls)
	}
	if !got.Retained || got.Phase != PhaseRetained {
		t.Fatalf("Release() = %#v, want retained phase", got)
	}
}

func TestSubstrateReleaseWaitsWhenSuspendReturnsAfterStartingTransition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusSuspending, substrateStatusSuspended},
		suspendErr:  fmt.Errorf("suspend transition still in progress"),
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		now:            time.Now,
		retained:       map[string]bool{},
	}

	got, err := executor.Release(t.Context(), ReleaseRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Retain:  true,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", control.suspendCalls)
	}
	if !got.Retained || got.Phase != PhaseRetained {
		t.Fatalf("Release() = %#v, want retained phase", got)
	}
}

func TestDefaultSubstrateScrubPathsIncludesStagedWorker(t *testing.T) {
	paths := defaultSubstrateScrubPaths()
	if slices.Contains(paths, "/app/orka-agent-worker") {
		return
	}
	t.Fatalf("defaultSubstrateScrubPaths() = %#v, want staged worker path", paths)
}

type recordingSubstrateControlClient struct {
	getStatuses   []string
	getErrs       []error
	getCalls      int
	createErr     error
	createCalls   int
	resumeErr     error
	resumeCalls   int
	suspendStatus string
	suspendErr    error
	suspendCalls  int
	deleted       bool
}

func (c *recordingSubstrateControlClient) GetActor(ctx context.Context, actorID string) (*substrateActor, error) {
	if actorID == "" {
		return nil, fmt.Errorf("actor id required")
	}
	call := c.getCalls
	c.getCalls++
	if call < len(c.getErrs) && c.getErrs[call] != nil {
		return nil, c.getErrs[call]
	}
	status := substrateStatusSuspended
	if call < len(c.getStatuses) {
		status = c.getStatuses[call]
	}
	return &substrateActor{
		ActorID:           actorID,
		TemplateNamespace: "ate-demo",
		TemplateName:      "orka-codex-ci",
		Status:            status,
		PodIP:             "10.244.0.10",
	}, nil
}

func (c *recordingSubstrateControlClient) CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*substrateActor, error) {
	c.createCalls++
	if c.createErr != nil {
		return nil, c.createErr
	}
	return nil, fmt.Errorf("unexpected CreateActor")
}

func (c *recordingSubstrateControlClient) ResumeActor(ctx context.Context, actorID string) (*substrateActor, error) {
	c.resumeCalls++
	if c.resumeErr != nil {
		return nil, c.resumeErr
	}
	return &substrateActor{
		ActorID:           actorID,
		TemplateNamespace: "ate-demo",
		TemplateName:      "orka-codex-ci",
		Status:            substrateStatusResuming,
		PodIP:             "10.244.0.10",
	}, nil
}

func (c *recordingSubstrateControlClient) SuspendActor(ctx context.Context, actorID string) (*substrateActor, error) {
	c.suspendCalls++
	if c.suspendErr != nil {
		return nil, c.suspendErr
	}
	status := c.suspendStatus
	if status == "" {
		status = substrateStatusSuspended
	}
	return &substrateActor{
		ActorID:           actorID,
		TemplateNamespace: "ate-demo",
		TemplateName:      "orka-codex-ci",
		Status:            status,
		PodIP:             "10.244.0.10",
	}, nil
}

func (c *recordingSubstrateControlClient) DeleteActor(ctx context.Context, actorID string) error {
	c.deleted = true
	return nil
}
