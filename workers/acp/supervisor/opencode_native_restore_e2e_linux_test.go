//go:build linux

package supervisor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/opencodestate"
	"github.com/orka-agents/orka/internal/workspacedelta"
	"golang.org/x/sys/unix"
)

// Run through scripts/opencode-native-restore-e2e.sh. This is deliberately
// opt-in: it requires the checksum-pinned native binary and Linux UID fencing
// in the real Orka OpenCode image, but no network or provider credentials.
func TestOpenCodeNativeRestoreE2E(t *testing.T) {
	if os.Getenv("ORKA_OPENCODE_NATIVE_RESTORE_E2E") != "1" {
		t.Skip("requires the pinned OpenCode image; run scripts/opencode-native-restore-e2e.sh")
	}
	verifyNativeRestoreBinary(t)
	if os.Geteuid() != 0 {
		t.Fatal("native restoration verification requires the supervisor's root UID boundary")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	for index, method := range []string{acp.MethodSessionResume, acp.MethodSessionLoad} {
		t.Run(strings.TrimPrefix(method, "session/"), func(t *testing.T) {
			runNativeRestoreE2E(t, method, 22001+index*2)
		})
	}
}

func verifyNativeRestoreBinary(t *testing.T) {
	t.Helper()
	file, err := os.Open("/opt/opencode/bin/opencode")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal(err)
	}
	want := acp.OpenCodeLinuxARM64BinarySHA256
	if runtime.GOARCH == "amd64" {
		want = acp.OpenCodeLinuxX64BinarySHA256
	} else if runtime.GOARCH != "arm64" {
		t.Fatal("unsupported OpenCode fixture architecture")
	}
	if hex.EncodeToString(digest.Sum(nil)) != want {
		t.Fatal("OpenCode native binary digest differs from the repository pin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, file.Name(), "--version")
	command.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/local/bin:/usr/bin:/bin", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_PURE=1"}
	version, err := command.Output()
	if err != nil || strings.TrimSpace(string(version)) != acp.OpenCodeVersion {
		t.Fatal("OpenCode native binary version differs from the repository pin")
	}
}

//nolint:gocyclo // Keep the native lifecycle's ordered cleanup and restoration assertions together.
func runNativeRestoreE2E(t *testing.T, method string, firstUID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	base, err := os.MkdirTemp("/tmp", "orka-native-restore-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o711); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("Native restore fixture repository.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := nativeRestoreWorkspaceDigest(t, ctx, workspace)
	fixture := newNativeRestoreFixture(t)
	t.Cleanup(func() {
		if t.Failed() {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			t.Logf("native fixture model requests=%d, tool calls=%d, rejection=%q", fixture.modelCalls, fixture.toolCalls, fixture.violation)
		}
	})
	upstream := httptest.NewServer(fixture)
	t.Cleanup(upstream.Close)
	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamBearerToken: fixture.upstreamBearer,
		ProviderKind: providerKindOpencode, Model: nativeRestoreModel, ModelOutputLimit: 4096,
	})
	mcpProxy, err := newMCPProxy(fixture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := mcpProxy.close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	first := prepareNativeRestoreRuntime(t, base, workspace, "original", firstUID, nativeRestoreOldTool, proxy, mcpProxy)
	first.start(t, ctx, nil, false)
	fixture.assertCounts(t, 0, 0)
	first.runPrompt(t, ctx, nativeRestoreSeedPrompt, nativeRestoreSeedAnswer, "")
	fixture.assertCounts(t, 2, 1)
	providerSessionID := first.runtime.ProviderSessionID()
	originalPID := first.runtime.Process().PID()
	first.stop(t)
	if err := acp.ReclaimSessionOwnership(first.config.Process.Paths.Root); err != nil {
		t.Fatal(err)
	}
	saved, err := opencodestate.Capture(ctx, nativeSessionDBPath(first.config.Process.Paths),
		providerSessionID, workspace, nativeRestoreModel)
	if err != nil {
		t.Fatalf("capture pinned native conversation: %v", err)
	}
	assertNativeRestoreSnapshotHasNoCredentials(t, saved, first, fixture.upstreamBearer)
	if err := acp.ReclaimSessionOwnership(workspace); err != nil {
		t.Fatal(err)
	}
	if got := nativeRestoreWorkspaceDigest(t, ctx, workspace); got != before {
		t.Fatal("initial read-only prompt changed the repository")
	}

	second := prepareNativeRestoreRuntime(t, base, workspace, "replacement", firstUID+1, nativeRestoreNewTool, proxy, mcpProxy)
	assertNativeRestoreFreshCredentials(t, first, second)
	// The real bootstrap command creates only the pinned schema. It is stopped
	// with descendant cleanup proof before any selected conversation is imported.
	bootstrap := &Server{}
	if err := bootstrap.initializeNativeSessionDatabase(ctx, second.config.Process); err != nil {
		t.Fatalf("initialize replacement native database: %v", err)
	}
	if err := acp.ReclaimSessionOwnership(second.config.Process.Paths.Root); err != nil {
		t.Fatal(err)
	}
	if err := opencodestate.Restore(ctx, nativeSessionDBPath(second.config.Process.Paths), saved,
		providerSessionID, workspace, nativeRestoreModel); err != nil {
		t.Fatalf("import selected native conversation: %v", err)
	}
	if err := acp.FinalizeSessionOwnership(second.config.Process.Paths.Root, firstUID+1, firstUID+1); err != nil {
		t.Fatal(err)
	}
	second.start(t, ctx, &acp.SessionRestore{
		SessionID: providerSessionID, ModelID: "orka/" + nativeRestoreModel, ModeID: "build",
	}, method == acp.MethodSessionLoad)
	if second.runtime.ProviderSessionID() != providerSessionID || second.runtime.ID() == first.runtime.ID() ||
		second.runtime.Process().PID() == originalPID || second.runtime.RestoreMethod() != method {
		t.Fatal("native restoration did not retain the provider session in a fresh runtime/process using the selected method")
	}
	// Keep both prompt gates closed through restoration and an observation
	// interval, then freeze the complete child UID before checking the workspace.
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fixture.assertCounts(t, 2, 1)
	trace := second.traceCounts(t)
	if trace["client "+method] != 1 || trace["client "+acp.MethodSessionNew] != 0 ||
		trace["client "+acp.MethodSessionPrompt] != 0 || trace["client "+acp.MethodSessionSetConfigOption] != 2 ||
		trace["agent "+acp.MethodRequestPermission] != 0 {
		t.Fatal("restore issued a prompt, requested permission, or failed to apply current model/mode exactly once")
	}
	if method == acp.MethodSessionLoad && trace["agent "+acp.MethodSessionUpdate] < 3 {
		t.Fatal("real session/load did not emit stored history to exercise history isolation")
	}
	if err := second.runtime.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if err := acp.ReclaimSessionOwnership(workspace); err != nil {
		t.Fatal(err)
	}
	if got := nativeRestoreWorkspaceDigest(t, ctx, workspace); got != before {
		t.Fatal("native restoration changed the repository")
	}
	if err := acp.FinalizeSessionOwnership(workspace, firstUID+1, firstUID+1); err != nil {
		t.Fatal(err)
	}
	if err := second.runtime.Thaw(); err != nil {
		t.Fatal(err)
	}
	second.runPrompt(t, ctx, nativeRestoreFollowupPrompt, nativeRestoreFinalAnswer, fixture.originalResult)
	fixture.assertCounts(t, 4, 2)
	if second.traceCounts(t)["client "+acp.MethodSessionPrompt] != 1 {
		t.Fatal("follow-up prompt was sent more than once")
	}
	second.stop(t)
	if err := acp.ReclaimSessionOwnership(workspace); err != nil {
		t.Fatal(err)
	}
	if got := nativeRestoreWorkspaceDigest(t, ctx, workspace); got != before {
		t.Fatal("restored read-only prompt changed the repository")
	}
	t.Logf("OpenCode %s %s: fresh UID/process and credentials; 0 model/tool calls during restore; prior user/assistant/tool history once; 1 follow-up prompt; current MCP policy; repository unchanged", acp.OpenCodeVersion, method)
}

type nativeRestoreRuntime struct {
	config                     acp.RuntimeSessionConfig
	runtime                    *acp.RuntimeSession
	provider                   *providerProxySession
	binding                    ProviderProxyBinding
	mcp                        *mcpProxySession
	mcpServer                  acp.MCPServer
	authorization              harnessv2.PromptMCPAuthorization
	lease                      harnessv2.PromptLease
	trace                      string
	stopped                    bool
	previousProviderCredential string
}

func prepareNativeRestoreRuntime(
	t *testing.T, base, workspace, id string, uid int, tool string,
	proxy *providerProxy, mcpProxy *mcpProxy,
) *nativeRestoreRuntime {
	t.Helper()
	paths, err := acp.PrepareSessionPaths(base, id)
	if err != nil {
		t.Fatal(err)
	}
	paths.Workspace = workspace
	profile, err := providerProfile(providerKindOpencode, nativeRestoreModel, harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.PrepareSession(paths); err != nil {
		t.Fatal(err)
	}
	provider, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.close)
	fence := harnessv2.Fence{
		RuntimeInstanceID: "native-fixture", SupervisorBootID: harnessv2.SupervisorBootID("boot-" + id), ControllerEpoch: 1,
		RuntimePoolUID: "native-fixture-pool", RuntimePoolGeneration: 1,
		RuntimeSessionUID: harnessv2.RuntimeSessionUID(id), RuntimeSessionGeneration: 1,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(testDigest("native-fixture-profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	authorization, lease := buildTestMCPAuthorization(t, fence, time.Now().UTC(), tool, harnessv2.MCPToolEffectReadOnly, false)
	mcp, mcpServer, err := mcpProxy.newSession(fence, authorization.Configuration())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mcp.close)
	request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: authorization.Configuration()}
	values, err := profile.EnvironmentForSession(request, paths, binding)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := acp.BuildChildEnvironment(paths, acp.EnvironmentConfig{Values: values})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{paths.Root, workspace} {
		if err := acp.FinalizeSessionOwnership(root, uid, uid); err != nil {
			t.Fatal(err)
		}
	}
	// Only method names go here. Keep this test trace readable by the root
	// supervisor without adding DAC_OVERRIDE to the production capability set.
	trace := filepath.Join(base, id+"-wire-methods")
	if err := os.WriteFile(trace, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(trace, uid, uid); err != nil {
		t.Fatal(err)
	}
	return &nativeRestoreRuntime{
		config: acp.RuntimeSessionConfig{
			ID: id, Generation: 1, ProfileDigest: string(fence.RuntimeProfileDigest),
			Process:    acp.ProcessConfig{Command: profile.Command, Args: profile.Args, Environment: environment, Paths: paths, UID: uid, GID: uid},
			MCPServers: []acp.MCPServer{mcpServer}, InitializeTimeout: 45 * time.Second, RestoreTimeout: 45 * time.Second,
			PromptLease: time.Minute, CancelGrace: time.Second,
		},
		provider: provider, binding: binding, mcp: mcp, mcpServer: mcpServer,
		authorization: authorization, lease: lease, trace: trace,
	}
}

func (r *nativeRestoreRuntime) start(t *testing.T, ctx context.Context, restore *acp.SessionRestore, forceLoad bool) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r.config.Process.Command = executable
	r.config.Process.Args = []string{"-test.run=^TestOpenCodeNativeRestoreWireHelper$"}
	r.config.Process.Environment = append(r.config.Process.Environment,
		"ORKA_NATIVE_RESTORE_WIRE_HELPER=1", "ORKA_NATIVE_RESTORE_TRACE="+r.trace)
	if forceLoad {
		r.config.Process.Environment = append(r.config.Process.Environment, "ORKA_NATIVE_RESTORE_FORCE_LOAD=1")
	}
	r.config.Restore = restore
	r.runtime, err = acp.NewRuntimeSession(ctx, r.config)
	if err != nil {
		if restoration, ok := errors.AsType[*acp.SessionRestoreError](err); ok {
			t.Fatalf("native ACP restore failed (method=%s, cleanup proven=%t, timeout=%t, cause type=%T)",
				restoration.Method, restoration.Cleanup.Proven, errors.Is(restoration, context.DeadlineExceeded), restoration.Cause)
		}
		t.Fatalf("native ACP create failed (timeout=%t, cause type=%T)", errors.Is(err, context.DeadlineExceeded), err)
	}
	t.Cleanup(func() { r.stop(t) })
}

func (r *nativeRestoreRuntime) stop(t *testing.T) {
	t.Helper()
	if r.stopped {
		return
	}
	r.provider.close()
	r.mcp.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	proof, err := r.runtime.Delete(ctx)
	if err != nil || !proof.Proven {
		t.Fatalf("native process and descendant cleanup was not proven: %v", err)
	}
	for _, root := range []string{r.config.Process.Paths.Root, r.config.Process.Paths.Workspace} {
		if err := acp.ReclaimSessionOwnership(root); err != nil {
			t.Fatal(err)
		}
	}
	r.stopped = true
}

func (r *nativeRestoreRuntime) runPrompt(t *testing.T, ctx context.Context, prompt, wantAnswer, forbiddenHistory string) {
	t.Helper()
	now := time.Now().UTC()
	r.lease.IssuedAt, r.lease.ExpiresAt = now, now.Add(time.Minute)
	r.authorization.ExpiresAt = r.lease.ExpiresAt
	if err := r.provider.activateWithMaxTurns(string(r.authorization.PromptID), 4, r.lease.ExpiresAt, now); err != nil {
		t.Fatal(err)
	}
	if r.previousProviderCredential != "" {
		// Test the retired capability while the replacement gate is active,
		// so an idle-gate denial cannot hide accidental credential reuse.
		response := doProviderProxyRequest(t, http.MethodPost, r.binding.BaseURL+"/chat/completions", r.previousProviderCredential,
			[]byte(fmt.Sprintf(`{"model":%q}`, nativeRestoreModel)), nil)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("old provider credential was not denied by the active replacement route: HTTP %d", response.StatusCode)
		}
	}
	if err := r.mcp.activate(r.authorization, r.lease, now); err != nil {
		t.Fatal(err)
	}
	if err := r.mcp.markRunning(r.authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	run, err := r.runtime.StartPrompt(ctx, string(r.authorization.PromptID), testDigest(prompt), []acp.ContentBlock{acp.Text(prompt)})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	accepted, usages := 0, 0
	for event := range run.Events {
		run.Release(event)
		switch event.Type {
		case acp.PromptEventAccepted:
			accepted++
		case acp.PromptEventPermissionRequested:
			t.Fatal("native read-only fixture unexpectedly requested permission")
		case acp.PromptEventUpdate:
			if forbiddenHistory != "" && (bytes.Contains(event.Update.Update, []byte(forbiddenHistory)) ||
				bytes.Contains(event.Update.Update, []byte(nativeRestoreSeedAnswer)) ||
				bytes.Contains(event.Update.Update, []byte("native-original-call"))) {
				t.Fatal("restored history entered the new prompt's event stream")
			}
			var update struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Content       json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(event.Update.Update, &update); err != nil {
				t.Fatal("invalid native prompt update")
			}
			if update.SessionUpdate == "agent_message_chunk" {
				var content acp.ContentBlock
				if err := json.Unmarshal(update.Content, &content); err != nil {
					t.Fatal("invalid native assistant content")
				}
				text.WriteString(content.Text)
			}
			if update.SessionUpdate == "usage_update" {
				usages++
			}
		}
	}
	result := <-run.Result
	if result.Outcome != acp.PromptOutcomeCompleted || result.Err != nil || accepted != 1 || usages != 1 || text.String() != wantAnswer {
		t.Fatalf("native prompt did not settle exactly once with expected output: outcome=%s, accepted=%d, usage updates=%d, expected answer=%t, error type=%T",
			result.Outcome, accepted, usages, text.String() == wantAnswer, result.Err)
	}
	r.provider.deactivate(string(r.authorization.PromptID))
	r.mcp.deactivate(r.authorization.PromptID, harnessv2.RuntimeSessionStateIdle)
	if err := r.provider.wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func (r *nativeRestoreRuntime) traceCounts(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(r.trace)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		counts[line]++
	}
	return counts
}

func nativeRestoreWorkspaceDigest(t *testing.T, ctx context.Context, workspace string) string {
	t.Helper()
	snapshot, err := workspacedelta.CaptureContext(ctx, workspace, workspacedelta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.ManifestDigest()
}

func assertNativeRestoreFreshCredentials(t *testing.T, first, second *nativeRestoreRuntime) {
	t.Helper()
	if first.binding.Credential == second.binding.Credential || first.binding.BaseURL == second.binding.BaseURL ||
		bytes.Equal(first.mcp.credential, second.mcp.credential) || first.mcpServer.URL == second.mcpServer.URL {
		t.Fatal("replacement provider/MCP bindings reused prior credentials or routes")
	}
	// Probe the old provider capability only after activating the next prompt;
	// MCP tools/list is safe while idle and still requires the new capability.
	second.previousProviderCredential = first.binding.Credential
	response := doMCPRequest(t, second.mcpServer.URL, string(first.mcp.credential), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old MCP credential accepted by replacement route: HTTP %d", response.StatusCode)
	}
}

func assertNativeRestoreSnapshotHasNoCredentials(t *testing.T, saved []byte, source *nativeRestoreRuntime, upstreamBearer string) {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(saved))
	if err != nil {
		t.Fatal("private native snapshot has invalid compression")
	}
	defer func() { _ = reader.Close() }()
	plain, err := io.ReadAll(io.LimitReader(reader, opencodestate.MaxDecompressedBytes+1))
	if err != nil || len(plain) > opencodestate.MaxDecompressedBytes {
		t.Fatal("private native snapshot exceeds decompression bounds")
	}
	for _, credential := range []string{
		upstreamBearer, source.binding.Credential, string(source.mcp.credential), openCodeServerPassword(source.binding.Credential),
	} {
		if bytes.Contains(plain, []byte(credential)) {
			t.Fatal("private native snapshot retained a credential")
		}
	}
}
