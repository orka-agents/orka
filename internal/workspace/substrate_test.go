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
	"testing"
	"time"
)

const substrateTestScrubPath = "/v1/scrub"

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

func TestSubstrateExecUsesDetachedPolling(t *testing.T) {
	var sawDetached bool
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "actor-1.actors.test" {
			t.Errorf("Host = %q, want actor-1.actors.test", r.Host)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
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
		handoffToken:   "token",
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
		handoffToken:   "token",
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
		handoffToken:   "token",
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
	getCalls      int
	suspendStatus string
	suspendErr    error
	suspendCalls  int
	deleted       bool
}

func (c *recordingSubstrateControlClient) GetActor(ctx context.Context, actorID string) (*substrateActor, error) {
	if actorID == "" {
		return nil, fmt.Errorf("actor id required")
	}
	status := substrateStatusSuspended
	if c.getCalls < len(c.getStatuses) {
		status = c.getStatuses[c.getCalls]
	}
	c.getCalls++
	return &substrateActor{
		ActorID:           actorID,
		TemplateNamespace: "ate-demo",
		TemplateName:      "orka-codex-ci",
		Status:            status,
		PodIP:             "10.244.0.10",
	}, nil
}

func (c *recordingSubstrateControlClient) CreateActor(ctx context.Context, actorID, templateNamespace, templateName string) (*substrateActor, error) {
	return nil, fmt.Errorf("unexpected CreateActor")
}

func (c *recordingSubstrateControlClient) ResumeActor(ctx context.Context, actorID string) (*substrateActor, error) {
	return nil, fmt.Errorf("unexpected ResumeActor")
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
