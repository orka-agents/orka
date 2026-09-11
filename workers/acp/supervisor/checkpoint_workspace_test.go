package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestCheckpointWorkspaceSurvivesNewRuntimeSessionIdentity(t *testing.T) {
	dataRoot := t.TempDir()
	newServer := func(pool harnessv2.RuntimePoolUID) (*Server, Config, harnessv2.RuntimeProfile) {
		cfg, profile := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", strings.Repeat("p", 32))
		cfg.Fence.RuntimePoolUID = pool
		cfg.DurableWorkspaceDir, cfg.DurableWorkspaceKey = dataRoot, "workspace"
		cfg.Capabilities.Limits.MaxResidentSessions = 1
		cfg.Capabilities.Limits.MaxConcurrentPrompts = 1
		server, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = server.Close(ctx)
		})
		return server, cfg, profile
	}

	source, sourceCfg, profile := newServer("source-pool")
	first := testCreateSessionRequest(t, sourceCfg, profile)
	created := performMutation(t, source.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", first, sourceCfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("source creation status=%d body=%s", created.Code, created.Body)
	}
	const file, contents = "saved-by-source.txt", "checkpoint data from the original session"
	source.mu.Lock()
	sourcePath := source.sessions[first.RuntimeSessionID].paths.Workspace
	source.mu.Unlock()
	if err := os.WriteFile(filepath.Join(sourcePath, file), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = source.Close(ctx)
	if got, err := os.ReadFile(filepath.Join(sourcePath, file)); err != nil || string(got) != contents {
		t.Fatalf("source checkpoint data disappeared before restore: %q, %v", got, err)
	}

	// The provider has restored this data volume into a fresh pool. Its new
	// conversation/runtime identity must not change the directory it restores.
	restored, restoredCfg, profile := newServer("restored-pool")
	var received []byte
	var readErr error
	inspected := false
	// Inspect the directory handed to the provider before it starts. Stopping
	// here also lets macOS verify data rebinding without requiring root to
	// launch a second child under the next non-reused UID.
	restored.cfg.Provider.ProjectSession = func(
		_ harnessv2.CreateRuntimeSessionRequest, paths acp.SessionPaths, _ ProviderProxyBinding,
	) (ProviderSessionProjection, error) {
		inspected = true
		received, readErr = os.ReadFile(filepath.Join(paths.Workspace, file))
		return ProviderSessionProjection{}, errors.New("stop after inspecting restored workspace")
	}
	request := testCreateSessionRequest(t, restoredCfg, profile)
	request.RuntimeSessionID = "restored-session"
	request.Metadata.Fence.RuntimeSessionUID = "new-runtime-session-uid"
	request.Metadata.OperationID = "create-restored-session"
	request.Workspace.ExpectDurableResume = true
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, restored.Handler(), http.MethodPut, "/v2/runtime-sessions/restored-session", request, restoredCfg)
	if response.Code != http.StatusInternalServerError || !inspected {
		t.Fatalf("restored creation status=%d body=%s", response.Code, response.Body)
	}
	if readErr != nil || string(received) != contents {
		t.Fatalf("restored provider did not receive checkpoint file: %q, %v", received, readErr)
	}

	// Missing restored data must stop before provider startup, including when
	// the committed marker survives but its directory does not.
	if err := os.RemoveAll(sourcePath); err != nil {
		t.Fatal(err)
	}
	inspected = false
	request.RuntimeSessionID = "missing-data-session"
	request.Metadata.Fence.RuntimeSessionUID = "missing-data-runtime-session-uid"
	request.Metadata.OperationID = "create-missing-data-session"
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	missing := performMutation(t, restored.Handler(), http.MethodPut, "/v2/runtime-sessions/missing-data-session", request, restoredCfg)
	var failure harnessv2.ErrorResponse
	if err := json.Unmarshal(missing.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if missing.Code != http.StatusConflict || failure.Code != harnessv2.ErrorCodeWorkspaceResumeLost || inspected {
		t.Fatalf("missing data was not rejected before provider startup: status=%d code=%s", missing.Code, failure.Code)
	}
}
