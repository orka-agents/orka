package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func newPreparationTestServer(t *testing.T) (*Server, Config, harnessv2.RuntimeProfile) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("distinct session identities require a root supervisor")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("session preparation or initialization attempted inference")
		http.Error(w, "inference is not authorized", http.StatusForbidden)
	}))
	t.Cleanup(upstream.Close)
	cfg, profile := newTestConfigWithUpstream(t, adapterResumeMode, upstream.URL, strings.Repeat("p", 32))
	allocator, err := acp.NewUIDAllocator(10000, 10010, 10000)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UIDAllocator = allocator
	profile.EnforcementRequirementDigest = testDigest("complete-enforcement-requirement")
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = digest
	cfg.Capabilities.RuntimeProfileDigest = digest
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

func prepareTestSession(t *testing.T, server *Server, cfg Config, profile harnessv2.RuntimeProfile) harnessv2.CreateRuntimeSessionRequest {
	t.Helper()
	request := testCreateSessionRequest(t, cfg, profile)
	request.PrepareOnly = true
	sealRequest(t, &request.Metadata.RequestDigest, request)
	path, err := harnessv2.RuntimeSessionPath(request.RuntimeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	response := performMutation(t, server.Handler(), http.MethodPut, path, request, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", response.Code, response.Body.String())
	}
	var prepared harnessv2.CreateRuntimeSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if err := prepared.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	return request
}

func testLaunchSessionRequest(t *testing.T, prepared harnessv2.CreateRuntimeSessionRequest) harnessv2.LaunchRuntimeSessionRequest {
	t.Helper()
	request := harnessv2.LaunchRuntimeSessionRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: prepared.Metadata,
		PreparationOperationID: prepared.Metadata.OperationID,
	}
	request.Metadata.OperationID = "launch-" + prepared.Metadata.OperationID
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return request
}

func TestSessionPreparationDefersAdapterAndPromptUntilFencedLaunch(t *testing.T) {
	server, cfg, profile := newPreparationTestServer(t)
	prepared := prepareTestSession(t, server, cfg, profile)
	state := server.sessions[prepared.RuntimeSessionID]
	marker := filepath.Join(state.paths.Home, helperProviderSessionFile)
	if state.runtime != nil || state.prepared == nil || state.preparedDescriptor == nil {
		t.Fatal("preparation did not retain an unstarted session")
	}
	if _, err := os.Stat(state.paths.Workspace); err != nil {
		t.Fatalf("preparation did not materialize workspace: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("adapter initialized before launch: %v", err)
	}
	prompt := testStartPromptRequest(t, cfg, prepared.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	if response.Code != http.StatusConflict || state.prompt != nil {
		t.Fatalf("prepared session admitted prompt: %d %s", response.Code, response.Body.String())
	}
	launch := testLaunchSessionRequest(t, prepared)
	for _, mutate := range []func(*harnessv2.LaunchRuntimeSessionRequest){
		func(r *harnessv2.LaunchRuntimeSessionRequest) { r.PreparationOperationID = "other-create" },
		func(r *harnessv2.LaunchRuntimeSessionRequest) { r.Metadata.TaskUID = "other-task" },
		func(r *harnessv2.LaunchRuntimeSessionRequest) { r.Metadata.TaskAttempt++ },
		func(r *harnessv2.LaunchRuntimeSessionRequest) { r.Metadata.Fence.RuntimeSessionGeneration++ },
	} {
		bad := launch
		mutate(&bad)
		sealRequest(t, &bad.Metadata.RequestDigest, bad)
		response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", bad, cfg)
		if response.Code < 400 || state.runtime != nil {
			t.Fatalf("mismatched launch started adapter: %d %s", response.Code, response.Body.String())
		}
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := harnessv2.NewClient(httpServer.URL,
		harnessv2.WithControllerBearerToken(cfg.ControllerBearerToken),
		harnessv2.WithOperationCapabilitySecret(cfg.CapabilitySecret),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: cfg.Fence.RuntimeProfileDigest, RuntimeInstanceID: cfg.Fence.RuntimeInstanceID}),
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := client.Capabilities(t.Context())
	if err != nil || !capabilities.SupportsSessionPreparation {
		t.Fatalf("preparation negotiation failed: %#v (%v)", capabilities, err)
	}
	if _, err := client.Status(t.Context()); err != nil {
		t.Fatalf("prepared status failed protocol validation: %v", err)
	}
	launched, err := client.LaunchRuntimeSession(t.Context(), prepared.RuntimeSessionID, launch)
	if err != nil || launched.Session.ProviderSessionID == "" {
		t.Fatalf("launch failed: %#v (%v)", launched, err)
	}
	runtime := state.runtime
	if runtime == nil || runtime.Process() == nil || runtime.Process().PID() <= 0 {
		t.Fatal("launch did not create the real adapter subprocess")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("launched adapter did not initialize its provider session: %v", err)
	}
	server.mu.Lock()
	state.descriptor.State = harnessv2.RuntimeSessionStatePoisoned
	server.mu.Unlock()
	replayed, err := client.LaunchRuntimeSession(t.Context(), prepared.RuntimeSessionID, launch)
	if err != nil || replayed.Classification.Class != harnessv2.RequestClassificationDuplicate || state.runtime != runtime {
		t.Fatalf("launch replay repeated work or lost its receipt: %#v (%v)", replayed, err)
	}
	recreated, err := client.CreateRuntimeSession(t.Context(), prepared)
	if err != nil || recreated.Session.State != harnessv2.RuntimeSessionStatePrepared || state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned {
		t.Fatalf("prepare replay lost history or changed current state: %#v (%v)", recreated, err)
	}
}

func TestSessionPreparationCleanupWithoutAdapter(t *testing.T) {
	for _, mode := range []string{"delete", "drain", "close"} {
		t.Run(mode, func(t *testing.T) {
			server, cfg, profile := newPreparationTestServer(t)
			prepared := prepareTestSession(t, server, cfg, profile)
			state := server.sessions[prepared.RuntimeSessionID]
			root := state.paths.Root
			switch mode {
			case "delete":
				request := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: prepared.Metadata}
				request.Metadata.OperationID = "delete-prepared"
				sealRequest(t, &request.Metadata.RequestDigest, request)
				response := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", request, cfg)
				if response.Code != http.StatusOK {
					t.Fatalf("prepared deletion: %d %s", response.Code, response.Body.String())
				}
			case "drain":
				server.mu.Lock()
				server.drain.Requested = true
				state.drainCleanupScheduled = true
				server.mu.Unlock()
				server.cleanupDrainedSession(prepared.RuntimeSessionID, state)
			case "close":
				if err := server.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("prepared %s retained private files: %v", mode, err)
			}
			if state.runtime != nil {
				t.Fatal("cleanup started an adapter")
			}
			if mode != "close" {
				launch := testLaunchSessionRequest(t, prepared)
				response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", launch, cfg)
				if response.Code < 400 {
					t.Fatalf("deleted preparation launched: %d %s", response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestSessionPreparationKeepsResidentCapacityAndLaunchesAtIdentityReserve(t *testing.T) {
	server, cfg, profile := newPreparationTestServer(t)
	var last harnessv2.CreateRuntimeSessionRequest
	for i := 1; i <= 10; i++ {
		request := testCreateSessionRequest(t, cfg, profile)
		request.PrepareOnly = true
		request.RuntimeSessionID = harnessv2.RuntimeSessionID(fmt.Sprintf("session-%d", i))
		request.Metadata.Fence.RuntimeSessionUID = harnessv2.RuntimeSessionUID(fmt.Sprintf("session-uid-%d", i))
		request.Metadata.OperationID = harnessv2.OperationID(fmt.Sprintf("create-session-%d", i))
		sealRequest(t, &request.Metadata.RequestDigest, request)
		path, err := harnessv2.RuntimeSessionPath(request.RuntimeSessionID)
		if err != nil {
			t.Fatal(err)
		}
		response := performMutation(t, server.Handler(), http.MethodPut, path, request, cfg)
		if response.Code != http.StatusCreated {
			t.Fatalf("resident session %d refused: %d %s", i, response.Code, response.Body.String())
		}
		last = request
	}
	if server.cfg.Capabilities.Limits.MaxConcurrentPrompts != 4 || server.drain.AcceptingNewSessions || server.drain.Requested {
		t.Fatal("preparation changed prompt capacity or confused the identity reserve with drain")
	}
	launch := testLaunchSessionRequest(t, last)
	path, err := harnessv2.RuntimeSessionLaunchPath(last.RuntimeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	response := performMutation(t, server.Handler(), http.MethodPut, path, launch, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("identity reserve blocked an already allocated preparation: %d %s", response.Code, response.Body.String())
	}
}

func TestSessionPreparationFailedLaunchCannotRestart(t *testing.T) {
	server, cfg, profile := newPreparationTestServer(t)
	server.cfg.Provider.Command = filepath.Join(t.TempDir(), "missing-adapter")
	prepared := prepareTestSession(t, server, cfg, profile)
	launch := testLaunchSessionRequest(t, prepared)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", launch, cfg)
	if response.Code != http.StatusConflict {
		t.Fatalf("failed launch: %d %s", response.Code, response.Body.String())
	}
	state := server.sessions[prepared.RuntimeSessionID]
	if state.runtime != nil || state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned {
		t.Fatal("failed launch did not retain a poisoned no-child session")
	}
	server.cfg.Provider.Command = cfg.Provider.Command
	fresh := launch
	fresh.Metadata.OperationID = "another-launch"
	sealRequest(t, &fresh.Metadata.RequestDigest, fresh)
	response = performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", fresh, cfg)
	if response.Code < 400 || state.runtime != nil {
		t.Fatal("failed launch was automatically retried")
	}
	request := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: prepared.Metadata}
	request.Metadata.OperationID = "delete-failed-launch"
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response = performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", request, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("failed launch cleanup: %d %s", response.Code, response.Body.String())
	}
}

func TestSessionPreparationPreservesUntouchedDurableCheckpoint(t *testing.T) {
	for _, launch := range []bool{false, true} {
		t.Run(fmt.Sprintf("launch=%t", launch), func(t *testing.T) {
			server, cfg, profile := newPreparationTestServer(t)
			server.cfg.DurableWorkspaceDir = t.TempDir()
			request := testCreateSessionRequest(t, cfg, profile)
			request.PrepareOnly = true
			request.Workspace.ExpectDurableResume = true
			request.Workspace.ExpectDurableResumeMinGeneration = 1
			component := string(request.Metadata.Fence.RuntimeSessionUID)
			workspace, _, err := acp.PrepareDurableSessionWorkspace(server.cfg.DurableWorkspaceDir, component, 1)
			if err != nil {
				t.Fatal(err)
			}
			checkpointFile := filepath.Join(workspace, "checkpoint.txt")
			if err := os.WriteFile(checkpointFile, []byte("owner checkpoint"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := acp.CommitDurableSessionWorkspace(server.cfg.DurableWorkspaceDir, component, acp.DurableWorkspaceBinding{
				RepositoryIdentity: request.Workspace.Baseline.RepositoryIdentity, Revision: request.Workspace.Baseline.Revision,
				SessionIdentityHighWater: 1, SessionGeneration: 1,
			}); err != nil {
				t.Fatal(err)
			}
			sealRequest(t, &request.Metadata.RequestDigest, request)
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
			if response.Code != http.StatusCreated {
				t.Fatalf("prepare durable session: %d %s", response.Code, response.Body.String())
			}
			if _, committed, err := acp.PrepareDurableSessionWorkspace(server.cfg.DurableWorkspaceDir, component, 1); err != nil || committed == nil {
				t.Fatalf("preparation invalidated untouched checkpoint: %#v (%v)", committed, err)
			}
			if launch {
				launchRequest := testLaunchSessionRequest(t, request)
				response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", launchRequest, cfg)
				if response.Code != http.StatusOK {
					t.Fatalf("launch durable session: %d %s", response.Code, response.Body.String())
				}
			}
			if err := server.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, committed, err := acp.PrepareDurableSessionWorkspace(server.cfg.DurableWorkspaceDir, component, 1); err != nil || committed == nil {
				t.Fatalf("cleanup lost committed checkpoint: %#v (%v)", committed, err)
			}
			if content, err := os.ReadFile(checkpointFile); err != nil || string(content) != "owner checkpoint" {
				t.Fatalf("checkpoint content changed: %q (%v)", content, err)
			}
		})
	}
}

func TestSessionPreparationCapabilityIsNegotiatedForStrictLegacyClients(t *testing.T) {
	server, _, _ := newPreparationTestServer(t)
	for _, supported := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, harnessv2.CapabilitiesPath, nil)
		if supported {
			request.Header.Set(harnessv2.SessionPreparationHeader, "1")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		_, present := body["supportsSessionPreparation"]
		if response.Code != http.StatusOK || present != supported {
			t.Fatalf("negotiated=%t returned field=%t status=%d", supported, present, response.Code)
		}
	}
}

func TestSessionPreparationAdapterGate(t *testing.T) {
	gate := os.Getenv("ORKA_TEST_PREPARATION_GATE")
	if gate == "" {
		return
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Get(gate)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		os.Exit(2)
	}
	TestSupervisorACPHelper(t)
}

func TestSessionPreparationLaunchSerializesDeletionAndDrain(t *testing.T) {
	for _, mode := range []string{"drain", "failed-launch-drain", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			server, cfg, profile := newPreparationTestServer(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
					if mode == "failed-launch-drain" {
						w.WriteHeader(http.StatusForbidden)
					}
				case <-r.Context().Done():
				}
			}))
			defer gate.Close()
			defer releaseOnce.Do(func() { close(release) })
			server.cfg.Provider.Args = []string{"-test.run=^TestSessionPreparationAdapterGate$"}
			server.cfg.Provider.Environment["ORKA_TEST_PREPARATION_GATE"] = gate.URL
			prepared := prepareTestSession(t, server, cfg, profile)
			state := server.sessions[prepared.RuntimeSessionID]
			root := state.paths.Root
			launch := testLaunchSessionRequest(t, prepared)
			result := make(chan *httptest.ResponseRecorder, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				result <- performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", launch, cfg)
			}()
			defer func() {
				releaseOnce.Do(func() { close(release) })
				timer := time.NewTimer(10 * time.Second)
				defer timer.Stop()
				select {
				case <-finished:
				case <-timer.C:
					t.Error("test adapter launch did not settle during cleanup")
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("adapter launch did not reach its initialization gate")
			}
			duplicate := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", launch, cfg)
			if duplicate.Code != http.StatusConflict {
				t.Fatalf("in-flight launch replay did not remain uncertain: %d %s", duplicate.Code, duplicate.Body.String())
			}
			deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: prepared.Metadata}
			deletion.Metadata.OperationID = "delete-during-launch"
			sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
			response := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, cfg)
			if response.Code != http.StatusConflict {
				t.Fatalf("in-flight launch was treated as a no-child preparation: %d %s", response.Code, response.Body.String())
			}
			if mode == "shutdown" {
				if err := server.Close(ctx); err == nil {
					t.Fatal("shutdown claimed cleanup while adapter initialization was in flight")
				}
			} else {
				drain := harnessv2.DrainRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(cfg.Fence, "drain-during-launch", false)}
				drain.Metadata.TaskUID = ""
				drain.Metadata.TaskAttempt = 0
				sealRequest(t, &drain.Metadata.RequestDigest, drain)
				response := performMutation(t, server.Handler(), http.MethodPut, harnessv2.DrainPath, drain, cfg)
				if response.Code != http.StatusOK {
					t.Fatalf("drain during launch: %d %s", response.Code, response.Body.String())
				}
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("in-flight launch lost its private tree before stop proof: %v", err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case response = <-result:
			case <-ctx.Done():
				t.Fatal("adapter launch did not settle")
			}
			expected := http.StatusOK
			if mode == "failed-launch-drain" {
				expected = http.StatusConflict
			}
			if response.Code != expected {
				t.Fatalf("settled launch status=%d expected=%d body=%s", response.Code, expected, response.Body.String())
			}
			waitPreparedSessionRetired(t, ctx, server, prepared)
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("settled drain retained private files: %v", err)
			}
		})
	}
}

func waitPreparedSessionRetired(t *testing.T, ctx context.Context, server *Server, request harnessv2.CreateRuntimeSessionRequest) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		server.mu.Lock()
		_, resident := server.sessions[request.RuntimeSessionID]
		_, retired := server.tombstones[request.Metadata.Fence.RuntimeSessionUID]
		server.mu.Unlock()
		if !resident && retired {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("settled session did not complete the requested cleanup")
		}
	}
}

func TestSessionPreparationCompletingAfterShutdownRetires(t *testing.T) {
	server, cfg, profile := newPreparationTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	server.cfg.Provider.ProjectSession = func(_ harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, _ ProviderProxyBinding) (ProviderSessionProjection, error) {
		close(entered)
		<-release
		return ProviderSessionProjection{}, nil
	}
	request := testCreateSessionRequest(t, cfg, profile)
	request.PrepareOnly = true
	sealRequest(t, &request.Metadata.RequestDigest, request)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
	}()
	defer releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("preparation did not reach the provider projection")
	}
	if err := server.Close(ctx); err == nil {
		t.Fatal("shutdown claimed in-flight preparation cleanup")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case response := <-result:
		if response.Code != http.StatusCreated {
			t.Fatalf("settled preparation: %d %s", response.Code, response.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("preparation did not settle")
	}
	waitPreparedSessionRetired(t, ctx, server, request)
}

func TestSessionPreparationReplaySurvivesUnprovenLaunchCleanup(t *testing.T) {
	server, cfg, profile := newPreparationTestServer(t)
	prepared := prepareTestSession(t, server, cfg, profile)
	server.mu.Lock()
	state := server.sessions[prepared.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStatePoisoned
	state.createCleanupUnproven = &failedCreateReplay{
		operationID: "launch-create-session-1", requestDigest: harnessv2.RequestDigest(testDigest("launch")),
		statusCode: http.StatusConflict, code: harnessv2.ErrorCodeCleanupUnproven, message: createCleanupUnprovenMessage,
	}
	server.mu.Unlock()
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", prepared, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("launch failure replaced the preparation receipt: %d %s", response.Code, response.Body.String())
	}
	var replay harnessv2.CreateRuntimeSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if err := replay.ValidateFor(prepared); err != nil {
		t.Fatal(err)
	}
	if state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned || state.createCleanupUnproven == nil || state.runtime != nil {
		t.Fatal("historical preparation receipt changed current cleanup authority")
	}
}
func TestSessionPreparationNativeRestoreIsNotLoadedUntilLaunch(t *testing.T) {
	for _, launch := range []bool{false, true} {
		t.Run(fmt.Sprintf("launch=%t", launch), func(t *testing.T) {
			fixture := newNativeCreateFixture(t, "native-resume-held")
			request := fixture.request
			request.PrepareOnly = true
			request.Profile.EnforcementRequirementDigest = testDigest("native-enforcement")
			digest, err := harnessv2.CanonicalProfileDigest(request.Profile)
			if err != nil {
				t.Fatal(err)
			}
			request.Metadata.Fence.RuntimeProfileDigest = digest
			request.NativeRestore.Snapshot.RuntimeProfileDigest = digest
			fixture.cfg.Fence.RuntimeProfileDigest = digest
			fixture.server.cfg.Fence.RuntimeProfileDigest = digest
			fixture.server.cfg.Capabilities.RuntimeProfileDigest = digest
			sealRequest(t, &request.Metadata.RequestDigest, request)
			response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, fixture.cfg)
			if response.Code != http.StatusCreated {
				t.Fatalf("native preparation: %d %s", response.Code, response.Body.String())
			}
			var prepared harnessv2.CreateRuntimeSessionResponse
			if err := json.Unmarshal(response.Body.Bytes(), &prepared); err != nil {
				t.Fatal(err)
			}
			if err := prepared.ValidateFor(request); err != nil {
				t.Fatal(err)
			}
			if prepared.Session.NativeRestoration != nil || prepared.Session.ProviderSessionID != "" {
				t.Fatal("preparation claimed native provider restoration")
			}
			if _, err := os.Stat(fixture.journal); err != nil {
				t.Fatalf("native preparation did not retain its install journal: %v", err)
			}
			if launch {
				launchRequest := testLaunchSessionRequest(t, request)
				response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/launch", launchRequest, fixture.cfg)
				if response.Code != http.StatusOK {
					t.Fatalf("native launch: %d %s", response.Code, response.Body.String())
				}
				var launched harnessv2.LaunchRuntimeSessionResponse
				if err := json.Unmarshal(response.Body.Bytes(), &launched); err != nil {
					t.Fatal(err)
				}
				if err := launched.ValidateFor(request.RuntimeSessionID, launchRequest); err != nil {
					t.Fatal(err)
				}
				if launched.Session.NativeRestoration == nil || !launched.Session.NativeRestoration.Loaded ||
					launched.Session.ProviderSessionID != request.NativeRestore.Snapshot.ProviderSessionID {
					t.Fatal("launch did not confirm the actual native resume")
				}
			}
			deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: request.Metadata}
			deletion.Metadata.OperationID = "delete-native-preparation"
			sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
			response = performMutation(t, fixture.server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, fixture.cfg)
			if response.Code != http.StatusOK {
				t.Fatalf("native preparation cleanup: %d %s", response.Code, response.Body.String())
			}
			assertNativeCreatePathsAbsent(t, fixture)
		})
	}
}
