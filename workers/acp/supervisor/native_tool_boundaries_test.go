package supervisor

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Both policies use a writable workspace, so these tests exercise supervisor
// boundaries independently of the native runner's read-only tool selection.
func TestNativeToolPolicyWorkspaceBoundary(t *testing.T) {
	for _, mode := range []harnessv2.NativeToolPolicyMode{harnessv2.NativeToolPolicyFull, harnessv2.NativeToolPolicyRestricted} {
		for _, test := range []struct {
			name         string
			entries      []tarEntry
			relativeRoot string
			wantCreated  bool
		}{
			{name: "traversal", entries: []tarEntry{{name: "../escape", body: []byte("outside")}}},
			{name: "escaping symlink", entries: []tarEntry{{name: "link", typeFlag: tar.TypeSymlink, linkName: "../../outside"}}},
			{
				name: "relative root", relativeRoot: "app", wantCreated: true,
				entries: []tarEntry{
					{name: "app/visible.txt", body: []byte("in scope")},
					{name: "private/outside.txt", body: []byte("outside")},
				},
			},
		} {
			t.Run(string(mode)+"/"+test.name, func(t *testing.T) {
				server, cfg, create := newNativeToolBoundaryServer(t, mode, "immediate", "http://127.0.0.1:1")
				materializer, workspace := materializerForArchive(t, tarBytes(t, test.entries...))
				var destination string
				server.cfg.WorkspaceMaterializer = WorkspaceMaterializerFunc(func(ctx context.Context, request harnessv2.CreateRuntimeSessionRequest, path string) error {
					destination = path
					return materializer.Materialize(ctx, request, path)
				})
				create.Workspace.Baseline = workspace.Workspace.Baseline
				create.Workspace.RelativeRoot = test.relativeRoot
				create.WorkspaceArtifactAuthorization = workspace.WorkspaceArtifactAuthorization
				create.Metadata.RequestDigest = ""
				sealRequest(t, &create.Metadata.RequestDigest, create)
				response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
				if test.wantCreated {
					if response.Code != http.StatusCreated {
						t.Fatalf("valid workspace create status = %d", response.Code)
					}
					content, err := os.ReadFile(filepath.Join(destination, "visible.txt"))
					if err != nil || string(content) != "in scope" {
						t.Fatalf("relative-root file unavailable: %v", err)
					}
					for _, name := range []string{"app", "private", "outside.txt"} {
						if _, err := os.Lstat(filepath.Join(destination, name)); !os.IsNotExist(err) {
							t.Fatalf("out-of-scope path %s was exposed: %v", name, err)
						}
					}
					return
				}
				var failure harnessv2.ErrorResponse
				decodeResponse(t, response, &failure)
				if response.Code != http.StatusInternalServerError || !strings.Contains(failure.Message, "workspace materialization") {
					t.Fatalf("unsafe archive was not rejected during materialization: status=%d message=%s", response.Code, failure.Message)
				}
				if destination == "" {
					t.Fatal("workspace fixture was not exercised")
				}
				if _, err := os.Lstat(filepath.Dir(destination)); !os.IsNotExist(err) {
					t.Fatalf("rejected workspace session tree survived: %v", err)
				}
				server.mu.Lock()
				resident := len(server.sessions)
				server.mu.Unlock()
				if resident != 0 {
					t.Fatal("unsafe workspace admitted a runtime session")
				}
			})
		}
	}
}

func TestNativeToolPolicyChildCredentialIsolation(t *testing.T) {
	// The child canary rejects this synthetic value in its environment,
	// arguments, and session files, including inherited parent credentials.
	for _, name := range []string{"GITHUB_TOKEN", "OPENAI_API_KEY", "ORKA_CONTROLLER_TOKEN"} {
		t.Setenv(name, supervisorUpstreamTokenCanary)
	}
	for _, mode := range []harnessv2.NativeToolPolicyMode{harnessv2.NativeToolPolicyFull, harnessv2.NativeToolPolicyRestricted} {
		t.Run(string(mode), func(t *testing.T) {
			observed := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- r.Header.Clone()
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			t.Cleanup(upstream.Close)
			server, cfg, create := newNativeToolBoundaryServer(t, mode, providerProxyCanaryMode, upstream.URL)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("isolated ACP child create status = %d", created.Code)
			}
			server.mu.Lock()
			paths := server.sessions[create.RuntimeSessionID].paths
			server.mu.Unlock()
			for _, path := range []string{paths.Root, paths.Home, paths.Workspace, paths.Temp, paths.Config, paths.Cache, paths.Data, paths.State} {
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
					t.Fatalf("session directory is not private: %v", err)
				}
			}
			prompt := nativeToolBoundaryPrompt(t, cfg, create)
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"completed"`) {
				t.Fatalf("child isolation canary did not complete: status=%d", response.Code)
			}
			select {
			case headers := <-observed:
				if headers.Get(providerAuthorizationHeader) != "Bearer "+supervisorUpstreamTokenCanary {
					t.Fatal("provider proxy did not substitute its supervisor credential")
				}
				for _, name := range []string{providerAPIKeyHeader, providerCookieHeader, providerProxyAuthorizationHeader, providerForwardedForHeader, "X-Child-Secret"} {
					if headers.Get(name) != "" {
						t.Fatalf("child-local header %s reached upstream", name)
					}
				}
				if headers.Get("X-Canary-Safe") != "preserved" {
					t.Fatal("provider request did not preserve its safe canary header")
				}
			default:
				t.Fatal("ACP child did not exercise its provider proxy")
			}
		})
	}
}

func TestNativeToolPolicyCancellationRevokesAuthority(t *testing.T) {
	for _, mode := range []harnessv2.NativeToolPolicyMode{harnessv2.NativeToolPolicyFull, harnessv2.NativeToolPolicyRestricted} {
		t.Run(string(mode), func(t *testing.T) {
			var reached atomic.Int32
			disconnected := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := reached.Add(1)
				if call == 1 {
					defer close(disconnected)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					if _, err := io.WriteString(w, "data: waiting\n\n"); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
					}
				}
			}))
			t.Cleanup(upstream.Close)
			server, cfg, create := newNativeToolBoundaryServer(t, mode, "wait", upstream.URL)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d", created.Code)
			}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			binding := ProviderProxyBinding{BaseURL: state.providerProxy.baseURL, Credential: string(state.providerProxy.credential)}
			process := state.runtime.Process()
			paths := state.paths
			server.mu.Unlock()
			assertProviderProxyStatus(t, binding.BaseURL+"/responses", binding.Credential, http.StatusForbidden)
			prompt := nativeToolBoundaryPrompt(t, cfg, create)
			promptDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg))
			deadline := time.Now().Add(5 * time.Second)
			for {
				server.mu.Lock()
				running := state.descriptor.State == harnessv2.RuntimeSessionStatePromptRunning
				server.mu.Unlock()
				if running {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("prompt did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			active := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
			t.Cleanup(func() { _ = active.Body.Close() })
			if active.StatusCode != http.StatusOK {
				t.Fatalf("active provider request status = %d", active.StatusCode)
			}
			cancelRequest := testLateCancellation(t, create.Metadata.Fence)
			cancelRequest.Reason = harnessv2.CancelReasonUserRequested
			cancelRequest.SettlementDeadline = time.Now().UTC().Add(5 * time.Second)
			cancelRequest.Metadata.RequestDigest = ""
			sealRequest(t, &cancelRequest.Metadata.RequestDigest, cancelRequest)
			cancelled := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/cancel", cancelRequest, cfg)
			if cancelled.Code != http.StatusOK {
				t.Fatalf("cancel status = %d", cancelled.Code)
			}
			var result harnessv2.CancelPromptResponse
			decodeResponse(t, cancelled, &result)
			if !result.SettlementProven || result.Settlement.TerminalEvent != harnessv2.EventCancelled {
				t.Fatal("cancellation did not prove prompt settlement")
			}
			awaitRecorder(t, promptDone, "cancelled prompt did not finish")
			awaitSignal(t, disconnected, "cancelled provider request remained connected")
			late := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
			_ = late.Body.Close()
			if late.StatusCode != http.StatusForbidden && late.StatusCode != http.StatusNotFound {
				t.Fatalf("revoked provider request status = %d", late.StatusCode)
			}
			if reached.Load() != 1 {
				t.Fatalf("provider requests reached upstream %d times, want one", reached.Load())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			closeErr := server.Close(ctx)
			awaitSignal(t, process.Done(), "cancelled session retained its ACP process after cleanup")
			t.Run("descendant cleanup proof", func(t *testing.T) {
				if runtime.GOOS != linuxGOOS {
					t.Skip("complete UID descendant cleanup proof requires Linux")
				}
				if closeErr != nil {
					t.Fatalf("session cleanup failed: %v", closeErr)
				}
				if _, err := os.Lstat(paths.Root); !os.IsNotExist(err) {
					t.Fatalf("session tree survived proven cleanup: %v", err)
				}
			})
		})
	}
}

func newNativeToolBoundaryServer(t *testing.T, mode harnessv2.NativeToolPolicyMode, helperMode, upstreamURL string) (*Server, Config, harnessv2.CreateRuntimeSessionRequest) {
	t.Helper()
	cfg, profile := newTestConfigWithUpstream(t, helperMode, upstreamURL, supervisorUpstreamTokenCanary)
	var allowed []string
	if mode == harnessv2.NativeToolPolicyRestricted {
		allowed = []string{providerToolRead}
	}
	projection := testProviderProjectionRequest(t, providerKindClaude, "test-model", "", "", allowed, nil, mode == harnessv2.NativeToolPolicyFull, mode)
	production, err := providerProfile(providerKindClaude, "test-model", harnessv2.WorkspaceIntentWrite)
	if err != nil {
		t.Fatal(err)
	}
	// Use Claude's production native-policy projection with the fake ACP
	// executable. The existing canary's Codex-shaped provider transport is
	// only a fixture for the shared proxy and credential boundary.
	cfg.Provider.Kind = providerKindClaude
	cfg.Provider.ProjectSession = production.ProjectSession
	cfg.Capabilities.Provider.ProviderKinds = []string{providerKindClaude}
	profile.ProviderKind = providerKindClaude
	profile.AgentConfigurationDigest = projection.Profile.AgentConfigurationDigest
	profile.ToolPolicyDigest = projection.MCPConfiguration.ToolPolicyDigest
	profile.ApprovalPolicyDigest = projection.MCPConfiguration.ApprovalPolicyDigest
	profile.MCPConfigurationDigest = projection.MCPConfiguration.MCPConfigurationDigest
	profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
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
	create := testCreateSessionRequest(t, cfg, profile)
	create.AgentConfiguration = projection.AgentConfiguration
	create.MCPConfiguration = projection.MCPConfiguration
	create.Workspace.Intent = harnessv2.WorkspaceIntentWrite
	create.Metadata.RequestDigest = ""
	sealRequest(t, &create.Metadata.RequestDigest, create)
	return server, cfg, create
}

func nativeToolBoundaryPrompt(t *testing.T, cfg Config, create harnessv2.CreateRuntimeSessionRequest) harnessv2.StartPromptRequest {
	t.Helper()
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	policy := create.MCPConfiguration
	prompt.MCPAuthorization.ToolPolicy = policy.ToolPolicy
	prompt.MCPAuthorization.ToolPolicyDigest = policy.ToolPolicyDigest
	prompt.MCPAuthorization.ApprovalPolicy = policy.ApprovalPolicy
	prompt.MCPAuthorization.ApprovalPolicyDigest = policy.ApprovalPolicyDigest
	prompt.MCPAuthorization.MCPConfigurationDigest = policy.MCPConfigurationDigest
	prompt.Metadata.RequestDigest = ""
	sealRequest(t, &prompt.Metadata.RequestDigest, prompt)
	return prompt
}
