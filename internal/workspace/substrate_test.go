/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/workspace/daemonprotocol"
)

const (
	substrateTestBearer          = "Bearer token"
	substrateTestBootstrapBearer = "Bearer bootstrap-token"
	substrateTestFilesPath       = "/v1/files"
	substrateTestScrubPath       = "/v1/scrub"
	substrateTestExecPath        = "/v1/exec"
	substrateTestHealthPath      = "/healthz"
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

func TestNewSubstrateExecutorRejectsSessionIdentityCertificateMinting(t *testing.T) {
	_, err := NewSubstrateExecutor(SubstrateConfig{
		RouterURL:               "http://router.test",
		ActorDNSSuffix:          "actors.test",
		ControlClient:           &recordingSubstrateControlClient{},
		SessionIdentityMintCert: true,
	})
	if err == nil {
		t.Fatal("NewSubstrateExecutor() error = nil, want unsupported certificate minting error")
	}
	if !IsKind(err, ErrorKindFailedPrecondition) {
		t.Fatalf("NewSubstrateExecutor() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
	}
	if !strings.Contains(err.Error(), "certificate minting is not supported yet") {
		t.Fatalf("NewSubstrateExecutor() error = %q, want unsupported certificate minting context", err.Error())
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

func TestSubstrateClaimRejectsReattachedActorTemplateMismatch(t *testing.T) {
	tests := []struct {
		name            string
		getErrs         []error
		createErr       error
		wantGetCalls    int
		wantCreateCalls int
	}{
		{
			name:         "direct reattach",
			wantGetCalls: 1,
		},
		{
			name: "concurrent create already exists",
			getErrs: []error{
				NewError("get actor", ErrorKindNotFound, "actor not found", false, nil),
				nil,
			},
			createErr:       NewError("create actor", ErrorKindAlreadyExists, "actor already exists", false, nil),
			wantGetCalls:    2,
			wantCreateCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			control := &recordingSubstrateControlClient{
				getErrs:      tt.getErrs,
				createErr:    tt.createErr,
				templateName: "other-template",
			}
			executor := &SubstrateWorkspaceExecutor{
				control:        control,
				httpClient:     http.DefaultClient,
				routerURL:      "http://router.test",
				actorDNSSuffix: "actors.test",
				now:            time.Now,
			}

			_, err := executor.Claim(t.Context(), ClaimRequest{
				Namespace:       "ate-demo",
				ClaimName:       "actor-1",
				CreateIfMissing: true,
				Template:        TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"},
				Timeout:         time.Second,
			})
			if err == nil {
				t.Fatal("Claim() error = nil, want template mismatch")
			}
			if !IsKind(err, ErrorKindFailedPrecondition) {
				t.Fatalf("Claim() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
			}
			if !strings.Contains(err.Error(), "existing Substrate actor uses template ate-demo/other-template") {
				t.Fatalf("Claim() error = %q, want template mismatch context", err.Error())
			}
			if control.getCalls != tt.wantGetCalls {
				t.Fatalf("GetActor calls = %d, want %d", control.getCalls, tt.wantGetCalls)
			}
			if control.createCalls != tt.wantCreateCalls {
				t.Fatalf("CreateActor calls = %d, want %d", control.createCalls, tt.wantCreateCalls)
			}
		})
	}
}

func TestSubstrateClaimDerivesRetainedPhaseFromSuspendedActor(t *testing.T) {
	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusSuspended},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     http.DefaultClient,
		routerURL:      "http://router.test",
		actorDNSSuffix: "actors.test",
		now:            time.Now,
	}

	got, err := executor.Claim(t.Context(), ClaimRequest{
		Namespace:       "ate-demo",
		ClaimName:       "actor-1",
		CreateIfMissing: true,
		Template:        TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if !got.Reused || got.Phase != PhaseRetained {
		t.Fatalf("Claim() reused=%t phase=%s, want reused retained actor", got.Reused, got.Phase)
	}
}

func TestSubstrateConvergeActorsDeletesDeterministicActorsAboveTarget(t *testing.T) {
	const prefix = "orka-p-test"
	control := &recordingSubstrateControlClient{
		actors: []substrateActor{
			{ActorID: prefix + "-00000", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00001", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00002", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00003", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-manual", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: "other-pool-00099", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
		},
	}
	executor := &SubstrateActorPoolExecutor{control: control}

	created, deleted, err := executor.ConvergeSubstrateActors(t.Context(), prefix, 2, TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"})
	if err != nil {
		t.Fatalf("ConvergeSubstrateActors() error = %v", err)
	}
	if created != 0 || deleted != 2 {
		t.Fatalf("ConvergeSubstrateActors() created=%d deleted=%d, want 0/2", created, deleted)
	}
	wantDeleted := []string{prefix + "-00003", prefix + "-00002"}
	if !slices.Equal(control.deletedActorIDs, wantDeleted) {
		t.Fatalf("deleted actors = %#v, want %#v", control.deletedActorIDs, wantDeleted)
	}
	if control.createCalls != 0 {
		t.Fatalf("CreateActor calls = %d, want 0", control.createCalls)
	}
}

func TestSubstratePruneActorsDeletesDeterministicActorsAboveTarget(t *testing.T) {
	const prefix = "orka-p-test"
	control := &recordingSubstrateControlClient{
		actors: []substrateActor{
			{ActorID: prefix + "-00000", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00001", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00002", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-manual", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: "other-pool-00002", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
		},
	}
	executor := &SubstrateActorPoolExecutor{control: control}

	deleted, err := executor.PruneSubstrateActors(t.Context(), prefix, 1)
	if err != nil {
		t.Fatalf("PruneSubstrateActors() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("PruneSubstrateActors() deleted=%d, want 2", deleted)
	}
	wantDeleted := []string{prefix + "-00002", prefix + "-00001"}
	if !slices.Equal(control.deletedActorIDs, wantDeleted) {
		t.Fatalf("deleted actors = %#v, want %#v", control.deletedActorIDs, wantDeleted)
	}
	if control.createCalls != 0 {
		t.Fatalf("CreateActor calls = %d, want 0", control.createCalls)
	}
}

func TestSubstrateConvergeActorsReturnsPartialPruneCountOnError(t *testing.T) {
	const prefix = "orka-p-test"
	control := &recordingSubstrateControlClient{
		actors: []substrateActor{
			{ActorID: prefix + "-00000", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00001", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00002", TemplateNamespace: "ate-demo", TemplateName: "orka-codex-ci", Status: substrateStatusSuspended},
		},
		deleteErrs: []error{
			nil,
			NewError("delete actor", ErrorKindUnknown, "delete failed", false, nil),
		},
	}
	executor := &SubstrateActorPoolExecutor{control: control}

	created, deleted, err := executor.ConvergeSubstrateActors(t.Context(), prefix, 1, TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"})
	if err == nil {
		t.Fatal("ConvergeSubstrateActors() error = nil, want delete error")
	}
	if created != 0 || deleted != 1 {
		t.Fatalf("ConvergeSubstrateActors() created=%d deleted=%d, want 0/1", created, deleted)
	}
	wantDeleted := []string{prefix + "-00002", prefix + "-00001"}
	if !slices.Equal(control.deletedActorIDs, wantDeleted) {
		t.Fatalf("deleted actors = %#v, want %#v", control.deletedActorIDs, wantDeleted)
	}
}

func TestSubstrateConvergeActorsRejectsTemplateMismatchBelowTarget(t *testing.T) {
	const prefix = "orka-p-test"
	control := &recordingSubstrateControlClient{
		templateName: "old-template",
	}
	executor := &SubstrateActorPoolExecutor{control: control}

	created, deleted, err := executor.ConvergeSubstrateActors(t.Context(), prefix, 2, TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"})
	if err == nil {
		t.Fatal("ConvergeSubstrateActors() error = nil, want template mismatch")
	}
	if !IsKind(err, ErrorKindFailedPrecondition) {
		t.Fatalf("ConvergeSubstrateActors() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
	}
	if !strings.Contains(err.Error(), "existing Substrate actor uses template ate-demo/old-template") {
		t.Fatalf("ConvergeSubstrateActors() error = %q, want template mismatch context", err.Error())
	}
	if created != 0 || deleted != 0 {
		t.Fatalf("ConvergeSubstrateActors() created=%d deleted=%d, want 0/0 on mismatch", created, deleted)
	}
	if control.createCalls != 0 || control.listActorsCalls != 0 {
		t.Fatalf("create/list calls = %d/%d, want 0/0 after mismatch", control.createCalls, control.listActorsCalls)
	}
}

func TestSubstrateConvergeActorsRevalidatesConcurrentCreateAlreadyExists(t *testing.T) {
	const prefix = "orka-p-test"
	tests := []struct {
		name         string
		templateName string
		wantErr      bool
	}{
		{name: "matching template"},
		{name: "template mismatch", templateName: "old-template", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			control := &recordingSubstrateControlClient{
				getErrs: []error{
					NewError("get actor", ErrorKindNotFound, "actor not found", false, nil),
				},
				createErr:    NewError("create actor", ErrorKindAlreadyExists, "actor already exists", false, nil),
				templateName: tt.templateName,
			}
			executor := &SubstrateActorPoolExecutor{control: control}

			created, deleted, err := executor.ConvergeSubstrateActors(t.Context(), prefix, 1, TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"})
			if tt.wantErr {
				if err == nil {
					t.Fatal("ConvergeSubstrateActors() error = nil, want template mismatch")
				}
				if !IsKind(err, ErrorKindFailedPrecondition) {
					t.Fatalf("ConvergeSubstrateActors() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
				}
				if !strings.Contains(err.Error(), "existing Substrate actor uses template ate-demo/old-template") {
					t.Fatalf("ConvergeSubstrateActors() error = %q, want template mismatch context", err.Error())
				}
				if control.listActorsCalls != 0 {
					t.Fatalf("ListActors calls = %d, want 0 after mismatch", control.listActorsCalls)
				}
			} else if err != nil {
				t.Fatalf("ConvergeSubstrateActors() error = %v", err)
			}
			if created != 0 || deleted != 0 {
				t.Fatalf("ConvergeSubstrateActors() created=%d deleted=%d, want 0/0", created, deleted)
			}
			if control.getCalls != 2 {
				t.Fatalf("GetActor calls = %d, want 2", control.getCalls)
			}
			if control.createCalls != 1 {
				t.Fatalf("CreateActor calls = %d, want 1", control.createCalls)
			}
		})
	}
}

func TestSubstrateConvergeActorsDeletesStaleTemplateActorsWhenTargetZero(t *testing.T) {
	const prefix = "orka-p-test"
	control := &recordingSubstrateControlClient{
		actors: []substrateActor{
			{ActorID: prefix + "-00000", TemplateNamespace: "ate-demo", TemplateName: "old-template", Status: substrateStatusSuspended},
			{ActorID: prefix + "-00001", TemplateNamespace: "ate-demo", TemplateName: "old-template", Status: substrateStatusSuspended},
			{ActorID: prefix + "-manual", TemplateNamespace: "ate-demo", TemplateName: "old-template", Status: substrateStatusSuspended},
			{ActorID: "other-pool-00099", TemplateNamespace: "ate-demo", TemplateName: "old-template", Status: substrateStatusSuspended},
		},
	}
	executor := &SubstrateActorPoolExecutor{control: control}

	created, deleted, err := executor.ConvergeSubstrateActors(t.Context(), prefix, 0, TemplateRef{Namespace: "ate-demo", Name: "orka-codex-ci"})
	if err != nil {
		t.Fatalf("ConvergeSubstrateActors() error = %v", err)
	}
	if created != 0 || deleted != 2 {
		t.Fatalf("ConvergeSubstrateActors() created=%d deleted=%d, want 0/2", created, deleted)
	}
	wantDeleted := []string{prefix + "-00001", prefix + "-00000"}
	if !slices.Equal(control.deletedActorIDs, wantDeleted) {
		t.Fatalf("deleted actors = %#v, want %#v", control.deletedActorIDs, wantDeleted)
	}
	if control.createCalls != 0 {
		t.Fatalf("CreateActor calls = %d, want 0", control.createCalls)
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
		case r.Method == http.MethodPost && r.URL.Path == substrateTestExecPath:
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

func TestSubstrateExecRejectsResidentModeBeforeDaemonRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected daemon request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	executor := &SubstrateWorkspaceExecutor{
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
	}

	_, err := executor.Exec(t.Context(), ExecRequest{
		Ref:      WorkspaceRef{ID: "actor-1"},
		Command:  []string{"echo", "ok"},
		Timeout:  time.Second,
		Resident: true,
	})
	if err == nil {
		t.Fatal("Exec() error = nil, want unsupported resident execution error")
	}
	if !IsKind(err, ErrorKindFailedPrecondition) {
		t.Fatalf("Exec() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
	}
	if !strings.Contains(err.Error(), "resident execution is not supported yet") {
		t.Fatalf("Exec() error = %q, want resident unsupported context", err.Error())
	}
}

func TestSubstrateExecRetriesTransientPollingErrors(t *testing.T) {
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == substrateTestExecPath:
			_ = json.NewEncoder(w).Encode(substrateExecResponse{ExecID: "exec-1", Running: true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/exec/exec-1":
			polls++
			if polls == 1 {
				http.Error(w, "router warming", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(substrateExecResponse{
				ExecID:   "exec-1",
				Stdout:   "ok",
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
	if polls != 2 {
		t.Fatalf("poll count = %d, want retry after transient error", polls)
	}
	if got.Stdout != "ok" || got.ExitCode != 0 {
		t.Fatalf("Exec() result = stdout %q exit %d, want ok/0", got.Stdout, got.ExitCode)
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

func TestSubstrateBootstrapHandoffUploadUsesMintedSessionIdentity(t *testing.T) {
	var uploaded bool
	var execAuthorized bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == substrateTestFilesPath:
			if got := r.Header.Get("Authorization"); got != substrateTestBootstrapBearer {
				t.Errorf("upload Authorization = %q, want bootstrap bearer", got)
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
			if string(req.Files[0].Data) != "session-jwt" {
				t.Errorf("uploaded handoff token = %q, want minted session JWT", string(req.Files[0].Data))
			}
			uploaded = true
			_ = json.NewEncoder(w).Encode(substrateUploadResponse{})
		case r.Method == http.MethodPost && r.URL.Path == substrateTestExecPath:
			if got := r.Header.Get("Authorization"); got == "Bearer session-jwt" {
				execAuthorized = true
			} else {
				t.Errorf("exec Authorization = %q, want minted session JWT", got)
			}
			_ = json.NewEncoder(w).Encode(substrateExecResponse{ExitCode: 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	identity := &recordingSubstrateSessionIdentityClient{jwt: "session-jwt"}
	executor := &SubstrateWorkspaceExecutor{
		httpClient:              server.Client(),
		routerURL:               server.URL,
		actorDNSSuffix:          "actors.test",
		handoffToken:            substrateTestToken,
		bootstrapToken:          "bootstrap-token",
		sessionIdentity:         identity,
		sessionIdentityToken:    "worker-sa-token",
		sessionIdentityAudience: []string{substrateDefaultIdentityAudience},
		sessionIdentityAppID:    substrateDefaultIdentityAppID,
		sessionIdentityUserID:   substrateDefaultIdentityUserID,
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
	if identity.calls != 1 {
		t.Fatalf("MintJWT calls = %d, want 1", identity.calls)
	}
	if identity.bearerToken != "worker-sa-token" {
		t.Fatalf("MintJWT bearer token = %q, want worker-sa-token", identity.bearerToken)
	}
	if identity.req.SessionID != "actor-1" ||
		identity.req.AppID != substrateDefaultIdentityAppID ||
		identity.req.UserID != substrateDefaultIdentityUserID ||
		!slices.Equal(identity.req.Audience, []string{substrateDefaultIdentityAudience}) {
		t.Fatalf("MintJWT request = %#v, want default Orka identity for actor-1", identity.req)
	}

	_, err = executor.Exec(t.Context(), ExecRequest{
		Ref:     WorkspaceRef{ID: "actor-1"},
		Command: []string{"true"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if !execAuthorized {
		t.Fatal("Exec() did not use minted session JWT")
	}
}

func TestSubstrateBootstrapHandoffRequiredSessionIdentityFailsClosedWithoutCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected daemon request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	tests := []struct {
		name     string
		executor *SubstrateWorkspaceExecutor
	}{
		{
			name: "missing client",
			executor: &SubstrateWorkspaceExecutor{
				httpClient:              server.Client(),
				routerURL:               server.URL,
				actorDNSSuffix:          "actors.test",
				bootstrapToken:          "bootstrap-token",
				sessionIdentityToken:    "worker-sa-token",
				sessionIdentityRequired: true,
			},
		},
		{
			name: "missing bearer token",
			executor: &SubstrateWorkspaceExecutor{
				httpClient:              server.Client(),
				routerURL:               server.URL,
				actorDNSSuffix:          "actors.test",
				bootstrapToken:          "bootstrap-token",
				sessionIdentity:         &recordingSubstrateSessionIdentityClient{jwt: "session-jwt"},
				sessionIdentityRequired: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.executor.Upload(t.Context(), UploadRequest{
				Ref:              WorkspaceRef{ID: "actor-1"},
				BootstrapHandoff: true,
				Artifacts: []UploadArtifact{{
					Path: substrateHandoffTokenUploadPath,
					Data: []byte(substrateTestToken),
					Mode: 0o600,
				}},
				Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("Upload() error = nil, want required session identity error")
			}
			if !IsKind(err, ErrorKindFailedPrecondition) {
				t.Fatalf("Upload() error kind = %s, want %s", KindOf(err), ErrorKindFailedPrecondition)
			}
		})
	}
}

func TestSubstrateBootstrapHandoffConfiguredSessionIdentityFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected daemon request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	tests := []struct {
		name     string
		identity *recordingSubstrateSessionIdentityClient
		wantKind ErrorKind
	}{
		{
			name:     "mint error",
			identity: &recordingSubstrateSessionIdentityClient{err: NewError("mint session identity", ErrorKindFailedPrecondition, "not authorized", false, nil)},
			wantKind: ErrorKindFailedPrecondition,
		},
		{
			name:     "empty jwt",
			identity: &recordingSubstrateSessionIdentityClient{},
			wantKind: ErrorKindFailedPrecondition,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &SubstrateWorkspaceExecutor{
				httpClient:           server.Client(),
				routerURL:            server.URL,
				actorDNSSuffix:       "actors.test",
				bootstrapToken:       "bootstrap-token",
				sessionIdentity:      tt.identity,
				sessionIdentityToken: "session-identity-bearer",
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
			if err == nil {
				t.Fatal("Upload() error = nil, want session identity error")
			}
			if !IsKind(err, tt.wantKind) {
				t.Fatalf("Upload() error kind = %s, want %s", KindOf(err), tt.wantKind)
			}
			if tt.identity.calls != 1 {
				t.Fatalf("MintJWT calls = %d, want 1", tt.identity.calls)
			}
		})
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

func TestSubstrateWaitReadyCanSkipDaemonHealthCheck(t *testing.T) {
	daemonCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemonCalled = true
		http.Error(w, "workspace daemon should not be probed", http.StatusUnauthorized)
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

	got, err := executor.WaitReady(t.Context(), WaitReadyRequest{
		Ref:                   WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout:               time.Second,
		Boot:                  true,
		SkipDaemonHealthCheck: true,
	})
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if daemonCalled {
		t.Fatal("workspace daemon health endpoint was probed despite SkipDaemonHealthCheck")
	}
	if control.resumeCalls != 1 || len(control.resumeBoots) != 1 || !control.resumeBoots[0] {
		t.Fatalf("ResumeActor calls=%d boots=%#v, want one boot resume", control.resumeCalls, control.resumeBoots)
	}
	if got.Phase != PhaseReady || got.Message != "workspace actor running" {
		t.Fatalf("WaitReady() = phase %s message %q, want actor running readiness", got.Phase, got.Message)
	}
}

func TestSubstrateWaitReadyReportsPlacementAndResumeLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != substrateTestHealthPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	times := []time.Time{
		time.Unix(100, 0).UTC(),
		time.Unix(100, int64(750*time.Millisecond)).UTC(),
	}
	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusRunning},
		workers: []substrateWorker{{
			WorkerNamespace: "ate-demo",
			WorkerPool:      "codex-pool",
			WorkerPod:       "ateom-worker-1",
			ActorID:         "actor-1",
			IP:              "10.244.0.42",
		}},
		actors: []substrateActor{
			{ActorID: "actor-1", Status: substrateStatusRunning},
			{ActorID: "actor-2", Status: substrateStatusSuspended},
			{ActorID: "actor-3", Status: substrateStatusSuspended},
		},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now: func() time.Time {
			if len(times) == 0 {
				return time.Unix(100, int64(750*time.Millisecond)).UTC()
			}
			next := times[0]
			times = times[1:]
			return next
		},
	}

	got, err := executor.WaitReady(t.Context(), WaitReadyRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if control.listWorkersCalls != 1 {
		t.Fatalf("ListWorkers calls = %d, want 1", control.listWorkersCalls)
	}
	if control.listActorsCalls != 1 {
		t.Fatalf("ListActors calls = %d, want 1", control.listActorsCalls)
	}
	wantPlacement := Placement{
		WorkerNamespace: "ate-demo",
		WorkerPool:      "codex-pool",
		WorkerPodName:   "ateom-worker-1",
		PodIP:           "10.244.0.42",
	}
	if got.Placement != wantPlacement {
		t.Fatalf("placement = %#v, want %#v", got.Placement, wantPlacement)
	}
	if got.ResumeLatency != 750*time.Millisecond {
		t.Fatalf("resume latency = %s, want 750ms", got.ResumeLatency)
	}
	wantDensity := Density{
		WorkerCount:         1,
		ActorCount:          3,
		RunningActorCount:   1,
		SuspendedActorCount: 2,
		ActorsPerWorker:     "3.00",
	}
	if got.Density != wantDensity {
		t.Fatalf("density = %#v, want %#v", got.Density, wantDensity)
	}
}

func TestSubstrateWaitReadyTreatsPlacementLookupAsBestEffort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != substrateTestHealthPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	control := &recordingSubstrateControlClient{
		getStatuses:      []string{substrateStatusRunning},
		listWorkersDelay: time.Second,
		workers: []substrateWorker{{
			WorkerNamespace: "ate-demo",
			WorkerPool:      "codex-pool",
			WorkerPod:       "ateom-worker-1",
			ActorID:         "actor-1",
			IP:              "10.244.0.42",
		}},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
	}

	start := time.Now()
	got, err := executor.WaitReady(t.Context(), WaitReadyRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: 2 * time.Second,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("WaitReady() elapsed = %s, want placement lookup capped below 500ms", elapsed)
	}
	if control.listWorkersCalls != 1 {
		t.Fatalf("ListWorkers calls = %d, want 1", control.listWorkersCalls)
	}
	wantPlacement := Placement{
		WorkerNamespace: "ate-demo",
		WorkerPodName:   "ateom-pod-1",
		PodIP:           "10.244.0.10",
	}
	if got.Placement != wantPlacement {
		t.Fatalf("placement = %#v, want fallback %#v", got.Placement, wantPlacement)
	}
}

func TestSubstrateWaitReadyPassesBootToResumeActor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != substrateTestHealthPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
		Boot:    true,
	})
	if err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if len(control.resumeBoots) != 1 || !control.resumeBoots[0] {
		t.Fatalf("ResumeActor boot flags = %#v, want [true]", control.resumeBoots)
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

func TestSubstrateDeleteContinuesWhenRunningScrubFails(t *testing.T) {
	var scrubbed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == substrateTestScrubPath {
			scrubbed = true
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
	}

	got, err := executor.Delete(t.Context(), DeleteRequest{
		Ref:     WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !scrubbed {
		t.Fatal("Delete() did not attempt scrub before fallback delete")
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1 after scrub failure", control.suspendCalls)
	}
	if !control.deleted {
		t.Fatal("DeleteActor was not called after scrub failure")
	}
	if !got.Deleted || got.Phase != PhaseDeleted {
		t.Fatalf("Delete() = %#v, want deleted phase", got)
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

func TestSubstrateReleaseSkipScrubSuspendsRunningActor(t *testing.T) {
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
		suspendStatus: substrateStatusSuspended,
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     server.Client(),
		routerURL:      server.URL,
		actorDNSSuffix: "actors.test",
		handoffToken:   substrateTestToken,
		now:            time.Now,
	}

	got, err := executor.Release(t.Context(), ReleaseRequest{
		Ref:       WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
		Retain:    true,
		Timeout:   time.Second,
		SkipScrub: true,
	})
	if err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if scrubbed {
		t.Fatal("Release() scrubbed despite SkipScrub")
	}
	if control.suspendCalls != 1 {
		t.Fatalf("SuspendActor calls = %d, want 1", control.suspendCalls)
	}
	if !got.Retained || got.Phase != PhaseRetained {
		t.Fatalf("Release() = %#v, want retained phase", got)
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

func TestSubstrateDescribeDerivesRetainedFromSuspendedActor(t *testing.T) {
	control := &recordingSubstrateControlClient{
		getStatuses: []string{substrateStatusSuspended},
	}
	executor := &SubstrateWorkspaceExecutor{
		control:        control,
		httpClient:     http.DefaultClient,
		routerURL:      "http://router.test",
		actorDNSSuffix: "actors.test",
		now:            time.Now,
	}

	got, err := executor.Describe(t.Context(), DescribeRequest{
		Ref: WorkspaceRef{Namespace: "ate-demo", ID: "actor-1"},
	})
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if !got.Retained || got.Phase != PhaseRetained {
		t.Fatalf("Describe() = %#v, want retained phase derived from suspended actor", got)
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
	getStatuses       []string
	getErrs           []error
	getCalls          int
	createErr         error
	createCalls       int
	resumeErr         error
	resumeCalls       int
	resumeBoots       []bool
	suspendStatus     string
	suspendErr        error
	suspendCalls      int
	listWorkersErr    error
	listWorkersCalls  int
	listWorkersDelay  time.Duration
	workers           []substrateWorker
	listActorsErr     error
	listActorsCalls   int
	actors            []substrateActor
	templateName      string
	templateNamespace string
	deleted           bool
	deleteErrs        []error
	deletedActorIDs   []string
}

type recordingSubstrateSessionIdentityClient struct {
	calls       int
	req         substrateMintJWTRequest
	bearerToken string
	jwt         string
	err         error
}

func (c *recordingSubstrateSessionIdentityClient) MintJWT(
	ctx context.Context,
	req substrateMintJWTRequest,
	bearerToken string,
) (string, error) {
	c.calls++
	c.req = req
	c.bearerToken = bearerToken
	if c.err != nil {
		return "", c.err
	}
	return c.jwt, nil
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
	templateNamespace := c.templateNamespace
	if templateNamespace == "" {
		templateNamespace = "ate-demo"
	}
	templateName := c.templateName
	if templateName == "" {
		templateName = "orka-codex-ci"
	}
	return &substrateActor{
		ActorID:           actorID,
		TemplateNamespace: templateNamespace,
		TemplateName:      templateName,
		Status:            status,
		PodNamespace:      "ate-demo",
		PodName:           "ateom-pod-1",
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

func (c *recordingSubstrateControlClient) ResumeActor(ctx context.Context, actorID string, boot bool) (*substrateActor, error) {
	c.resumeCalls++
	c.resumeBoots = append(c.resumeBoots, boot)
	if c.resumeErr != nil {
		return nil, c.resumeErr
	}
	return &substrateActor{
		ActorID:           actorID,
		TemplateNamespace: "ate-demo",
		TemplateName:      "orka-codex-ci",
		Status:            substrateStatusResuming,
		PodNamespace:      "ate-demo",
		PodName:           "ateom-pod-1",
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
		PodNamespace:      "ate-demo",
		PodName:           "ateom-pod-1",
		PodIP:             "10.244.0.10",
	}, nil
}

func (c *recordingSubstrateControlClient) DeleteActor(ctx context.Context, actorID string) error {
	call := len(c.deletedActorIDs)
	c.deleted = true
	c.deletedActorIDs = append(c.deletedActorIDs, actorID)
	if call < len(c.deleteErrs) && c.deleteErrs[call] != nil {
		return c.deleteErrs[call]
	}
	return nil
}

func (c *recordingSubstrateControlClient) ListWorkers(ctx context.Context) ([]substrateWorker, error) {
	c.listWorkersCalls++
	if c.listWorkersDelay > 0 {
		timer := time.NewTimer(c.listWorkersDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if c.listWorkersErr != nil {
		return nil, c.listWorkersErr
	}
	return append([]substrateWorker(nil), c.workers...), nil
}

func (c *recordingSubstrateControlClient) ListActors(ctx context.Context) ([]substrateActor, error) {
	c.listActorsCalls++
	if c.listActorsErr != nil {
		return nil, c.listActorsErr
	}
	return append([]substrateActor(nil), c.actors...), nil
}

func TestWorkspaceDaemonErrorMapsRetryability(t *testing.T) {
	tests := map[string]struct {
		err       error
		kind      ErrorKind
		retryable bool
		contains  string
	}{
		"invalid url": {
			err:      &daemonprotocol.Error{Reason: daemonprotocol.ErrorReasonInvalidURL, Message: "invalid router URL"},
			kind:     ErrorKindInvalidArgument,
			contains: "invalid router URL",
		},
		"status 401": {
			err:      &daemonprotocol.Error{Reason: daemonprotocol.ErrorReasonStatus, Message: "daemon returned HTTP 401: unauthorized", StatusCode: http.StatusUnauthorized, Retryable: false},
			kind:     ErrorKindUnknown,
			contains: "HTTP 401",
		},
		"status 503": {
			err:       &daemonprotocol.Error{Reason: daemonprotocol.ErrorReasonStatus, Message: "daemon returned HTTP 503: warming", StatusCode: http.StatusServiceUnavailable, Retryable: true},
			kind:      ErrorKindUnknown,
			retryable: true,
			contains:  "HTTP 503",
		},
		"decode": {
			err:      &daemonprotocol.Error{Reason: daemonprotocol.ErrorReasonDecodeResponse, Message: "failed to decode response"},
			kind:     ErrorKindUnknown,
			contains: "decode",
		},
	}
	executor := &SubstrateWorkspaceExecutor{}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			mapped := executor.workspaceDaemonError(tt.err)
			if KindOf(mapped) != tt.kind {
				t.Fatalf("kind = %s, want %s (err=%v)", KindOf(mapped), tt.kind, mapped)
			}
			var workspaceErr *Error
			if !errors.As(mapped, &workspaceErr) {
				t.Fatalf("mapped error = %T %[1]v, want workspace Error", mapped)
			}
			if workspaceErr.Retryable != tt.retryable {
				t.Fatalf("Retryable = %t, want %t", workspaceErr.Retryable, tt.retryable)
			}
			if tt.contains != "" && !strings.Contains(mapped.Error(), tt.contains) {
				t.Fatalf("error = %q, want contains %q", mapped.Error(), tt.contains)
			}
		})
	}
}

func TestNewSubstrateActorPoolExecutorDoesNotRequireRouterConfig(t *testing.T) {
	executor, err := NewSubstrateActorPoolExecutor(SubstrateConfig{ControlClient: &recordingSubstrateControlClient{}})
	if err != nil {
		t.Fatalf("NewSubstrateActorPoolExecutor() error = %v", err)
	}
	if executor == nil || executor.control == nil {
		t.Fatalf("executor = %#v, want control-only adapter", executor)
	}
}
