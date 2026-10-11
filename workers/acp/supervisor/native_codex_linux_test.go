//go:build linux

package supervisor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// This opt-in test runs inside the pinned Codex runtime image. It uses only
// local scripted inference and newly supplied test credentials.
func TestPinnedNativeCodexACPRoundTrip(t *testing.T) {
	if os.Getenv("ORKA_NATIVE_CODEX_TEST") != "1" {
		t.Skip("requires the pinned Codex runtime image and ORKA_NATIVE_CODEX_TEST=1")
	}
	version, err := exec.Command("/opt/codex/bin/codex", "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli "+acp.CodexCLIVersion {
		t.Fatal("native test requires the exact pinned Codex CLI")
	}
	const threadID = "01a10020-1222-76e3-977d-d5165792ae72"
	const fixtureName = "rollout-2026-10-02T21-57-44-" + threadID + ".jsonl"
	original, err := os.ReadFile(filepath.Join("../../../internal/codexstate/testdata", fixtureName))
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	rollout := filepath.Join(source, "sessions", "2026", "10", "02", fixtureName)
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, original, 0o600); err != nil {
		t.Fatal(err)
	}
	const revokedSourceCredential = "revoked-source-test-credential"
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"OPENAI_API_KEY":"`+revokedSourceCredential+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := codexstate.Capture(t.Context(), source, threadID)
	if err != nil {
		t.Fatal(err)
	}
	assertNativeBundleExcludesCredentials(t, data, revokedSourceCredential)
	first := runPinnedNativeTurn(t, data, threadID, "first-current-test-credential", "NATIVE_ORka_REPLY_ONE", "", harnessv2.WorkspaceIntentRead)
	second := runPinnedNativeTurn(t, first.Data, threadID, "second-current-test-credential", "NATIVE_ORka_REPLY_TWO", "NATIVE_ORka_REPLY_ONE", harnessv2.WorkspaceIntentRead)
	if second.ProviderSessionID != threadID || first.DataDigest == second.DataDigest {
		t.Fatal("return transfer did not preserve native UUID and append the second turn")
	}
	written := runPinnedNativeTurn(t, second.Data, threadID, "write-current-test-credential", "NATIVE_ORka_WRITE", "NATIVE_ORka_REPLY_TWO", harnessv2.WorkspaceIntentWrite)
	if written.ProviderSessionID != threadID || written.DataDigest == second.DataDigest {
		t.Fatal("write continuation did not preserve native UUID and append its turn")
	}
	unchanged, err := os.ReadFile(rollout)
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatal("native round trip changed source home")
	}
}

func runPinnedNativeTurn(t *testing.T, data []byte, threadID, freshCredential, reply, previousReply string, intent harnessv2.WorkspaceIntent) harnessv2.NativeSessionSnapshot {
	t.Helper()
	upstream, capturedRequests := newPinnedNativeUpstream(t, freshCredential, reply, intent)
	defer upstream.Close()
	const model = "gpt-5.4"
	cfg, _ := newTestConfigWithUpstream(t, "unused", upstream.URL+"/v1", freshCredential)
	allowed := []string{providerToolRead, providerToolGrep, providerToolGlob}
	if intent == harnessv2.WorkspaceIntentWrite {
		allowed = nil
	}
	projection := testProviderProjectionRequest(t, "codex", model, "Use the current Orka runtime policy.", "high", allowed, nil, intent == harnessv2.WorkspaceIntentWrite)
	projection.Profile.WorkspaceIntent = intent
	provider, err := providerProfile("codex", model, intent)
	if err != nil {
		t.Fatal(err)
	}
	profile := projection.Profile
	profile.AdapterDigests = map[string]string{provider.AdapterName: provider.AdapterDigest}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider = provider
	cfg.ProviderProxy.Model = model
	cfg.Fence.RuntimeProfileDigest = profileDigest
	cfg.Capabilities.RuntimeProfileDigest = profileDigest
	cfg.Capabilities.AdapterDigests = profile.AdapterDigests
	cfg.Capabilities.Provider.Models = []string{model}
	cfg.Capabilities.SupportsNativeSessions = true
	cfg.InitializeTimeout = time.Minute
	var assertUploaded func(harnessv2.ArtifactReference)
	if intent == harnessv2.WorkspaceIntentWrite {
		cfg.ArtifactUploader, assertUploaded = pinnedNativeArtifactUploader(t, reply)
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	create := testCreateSessionRequest(t, cfg, profile)
	create.Workspace.Intent = intent
	create.Metadata.ExpiresAt = time.Now().UTC().Add(3 * time.Minute)
	create.AgentConfiguration = projection.AgentConfiguration
	create.MCPConfiguration = projection.MCPConfiguration
	create.NativeRestore = &harnessv2.NativeSessionRestore{Snapshot: harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: codexstate.DataDigest(data), ProviderSessionID: threadID, ProviderKind: "codex", ProviderVersion: acp.CodexCLIVersion,
		RuntimeSessionUID: create.Metadata.Fence.RuntimeSessionUID, RuntimeProfileDigest: profileDigest, WorkingDirectory: "/workspace",
	}}
	sealRequest(t, &create.Metadata.RequestDigest, create)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("pinned native load failed: %d %s", created.Code, created.Body.String())
	}
	var descriptor harnessv2.CreateRuntimeSessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	if err := descriptor.ValidateFor(create); err != nil {
		t.Fatal(err)
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	prompt.MCPAuthorization.ToolPolicyDigest = create.MCPConfiguration.ToolPolicyDigest
	prompt.MCPAuthorization.ApprovalPolicyDigest = create.MCPConfiguration.ApprovalPolicyDigest
	prompt.MCPAuthorization.MCPConfigurationDigest = create.MCPConfiguration.MCPConfigurationDigest
	prompt.MCPAuthorization.ToolPolicy = create.MCPConfiguration.ToolPolicy
	prompt.MCPAuthorization.ApprovalPolicy = create.MCPConfiguration.ApprovalPolicy
	prompt.MCPAuthorization.ExpiresAt = prompt.Metadata.ExpiresAt
	prompt.Input.Content[0].Text = "Continue this native session under the current Orka policy."
	sealRequest(t, &prompt.Metadata.RequestDigest, prompt)
	result := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	if result.Code != http.StatusOK || !bytes.Contains(result.Body.Bytes(), []byte(reply)) {
		t.Fatalf("pinned native prompt failed: %d %s", result.Code, result.Body.String())
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	settlementDigest := state.prompt.settlementDigest
	server.mu.Unlock()
	drain := harnessv2.DrainRequest{Protocol: harnessv2.ProtocolVersion, Metadata: harnessv2.MutationMetadata{
		Fence: cfg.Fence, OperationID: "drain-before-native-capture", RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}, Reason: "rollout"}
	sealRequest(t, &drain.Metadata.RequestDigest, drain)
	if draining := performMutation(t, server.Handler(), http.MethodPut, harnessv2.DrainPath, drain, cfg); draining.Code != http.StatusOK {
		t.Fatalf("native drain failed: %d %s", draining.Code, draining.Body.String())
	}
	delta := harnessv2.CreateWorkspaceDeltaRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "native-delta", true), DeltaID: "native-delta", Intent: create.Workspace.Intent, VerifiedBaseline: create.Workspace.Baseline, PromptSettlementDigest: settlementDigest, Limits: harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, MaxEntries: 100}}
	sealRequest(t, &delta.Metadata.RequestDigest, delta)
	validated := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/workspace-deltas/native-delta", delta, cfg)
	if validated.Code != http.StatusOK {
		t.Fatalf("pinned native workspace validation failed: %d %s", validated.Code, validated.Body.String())
	}
	var deltaResponse harnessv2.CreateWorkspaceDeltaResponse
	if err := json.Unmarshal(validated.Body.Bytes(), &deltaResponse); err != nil {
		t.Fatal(err)
	}
	if err := deltaResponse.ValidateFor(delta); err != nil {
		t.Fatal(err)
	}
	if intent == harnessv2.WorkspaceIntentWrite {
		content, err := os.ReadFile(filepath.Join(state.paths.Workspace, "native-write.txt"))
		if err != nil || string(content) != reply+"\n" {
			t.Fatal("native write-intent continuation did not write the exact workspace file")
		}
		if deltaResponse.Delta.State != harnessv2.WorkspaceDeltaPrepared || deltaResponse.Delta.ChangedFileCount != 1 || deltaResponse.Delta.EntryCount != 1 {
			t.Fatalf("native write-intent continuation did not prepare its publishable delta: %+v", deltaResponse.Delta)
		}
		assertUploaded(*deltaResponse.Delta.Artifact)
		finalizePinnedNativePublication(t, server, cfg, prompt.Metadata, delta.DeltaID)
	}
	server.mu.Lock()
	scheduled := state.drainCleanupScheduled
	server.mu.Unlock()
	if scheduled {
		t.Fatal("drain scheduled native deletion before controller capture")
	}
	request := nativeCaptureRequest(t, create.Metadata.Fence)
	captured := performMutation(t, server.Handler(), http.MethodPost, "/v2/runtime-sessions/session-1/native-session", request, cfg)
	if captured.Code != http.StatusOK {
		t.Fatalf("pinned native capture failed: %d %s", captured.Code, captured.Body.String())
	}
	var response harnessv2.CaptureNativeSessionResponse
	if err := json.Unmarshal(captured.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if err := response.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	requests := capturedRequests()
	if !bytes.Contains(requests[0], []byte("Run the shell command to print the SessionKit nonce")) || previousReply != "" && !bytes.Contains(requests[0], []byte(previousReply)) {
		t.Fatal("resumed provider did not receive the native source and prior continuation history")
	}
	assertNativeBundleExcludesCredentials(t, response.Snapshot.Data, freshCredential, "revoked-source-test-credential")
	var bundle struct {
		Rollout []byte `json:"rollout"`
	}
	if err := json.Unmarshal(response.Snapshot.Data, &bundle); err != nil {
		t.Fatal(err)
	}
	assertCurrentNativeSettings(t, bundle.Rollout, state.paths.Workspace)
	assertSavedNativeFilesDeleted(t, server, cfg, create, state)
	return response.Snapshot
}

func newPinnedNativeUpstream(t *testing.T, freshCredential, reply string, intent harnessv2.WorkspaceIntent) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var providerRequests [][]byte
	var wrongCredential bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		wrongCredential = wrongCredential || r.Header.Get("Authorization") != "Bearer "+freshCredential
		providerRequests = append(providerRequests, append([]byte(nil), body...))
		requestNumber := len(providerRequests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(event map[string]any) {
			encoded, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
		}
		if intent == harnessv2.WorkspaceIntentWrite && requestNumber == 1 {
			item, err := pinnedNativeWriteCall(body, reply)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
			write(map[string]any{"type": "response.created", "response": map[string]any{"id": "native-write-response"}})
			added := map[string]any{"type": "function_call", "id": item["id"], "call_id": item["call_id"], "name": item["name"], "arguments": "", "status": "in_progress"}
			if namespace, ok := item["namespace"]; ok {
				added["namespace"] = namespace
			}
			write(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added})
			write(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": item["id"], "delta": item["arguments"]})
			write(map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": item["id"], "arguments": item["arguments"]})
			write(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			write(map[string]any{"type": "response.completed", "response": map[string]any{"id": "native-write-response", "status": "completed", "output": []any{item}}})
			return
		}
		if intent == harnessv2.WorkspaceIntentWrite && !bytes.Contains(body, []byte(reply+"_WRITTEN")) {
			http.Error(w, "native write did not return the current shell marker", http.StatusUnprocessableEntity)
			return
		}
		write(map[string]any{"type": "response.created", "response": map[string]any{"id": "native-test-response"}})
		item := map[string]any{"type": "message", "role": "assistant", "id": "native-test-message", "content": []any{map[string]any{"type": "output_text", "text": reply}}}
		write(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		write(map[string]any{"type": "response.output_text.delta", "item_id": "native-test-message", "output_index": 0, "content_index": 0, "delta": reply})
		write(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		write(map[string]any{"type": "response.completed", "response": map[string]any{"id": "native-test-response", "usage": map[string]any{"input_tokens": 10, "output_tokens": 10, "total_tokens": 20}}})
	}))
	return upstream, func() [][]byte {
		t.Helper()
		mu.Lock()
		requests := append([][]byte(nil), providerRequests...)
		credentialFailed := wrongCredential
		mu.Unlock()
		if credentialFailed || len(requests) == 0 {
			t.Fatal("resumed provider did not use the newly supplied credential")
		}
		return requests
	}
}

// This fixture stores the actual delta bytes and returns their content-addressed
// receipt. It exercises the production uploader without a forge or live model.
func pinnedNativeArtifactUploader(t *testing.T, marker string) (*RemoteArtifactUploader, func(harnessv2.ArtifactReference)) {
	t.Helper()
	var mu sync.Mutex
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if r.Method != http.MethodPut || err != nil || len(data) > 1<<20 {
			http.Error(w, "invalid workspace delta upload", http.StatusBadRequest)
			return
		}
		digest := artifactcap.DigestBytes(data)
		artifactID, err := artifactcap.ArtifactIDForDigest(digest)
		if err != nil {
			http.Error(w, "invalid artifact digest", http.StatusBadRequest)
			return
		}
		mu.Lock()
		uploaded = append([]byte(nil), data...)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"artifactID": artifactID, "digest": digest, "sizeBytes": len(data),
			"mediaType": artifactcap.MediaTypeWorkspaceDelta, "createdAt": time.Now().UTC(),
		})
	}))
	t.Cleanup(server.Close)
	authorizer := ArtifactAuthorizationProviderFunc(func(context.Context, ArtifactAuthorizationRequest) (artifactcap.Authorization, error) {
		return artifactcap.Authorization{Capability: "native-test-artifact-capability", RequestDigest: testDigest("native-test-artifact-binding")}, nil
	})
	client, err := newDefaultArtifactClient(server.URL, server.Client(), authorizer)
	if err != nil {
		t.Fatal(err)
	}
	uploader, err := NewRemoteArtifactUploader(client)
	if err != nil {
		t.Fatal(err)
	}
	return uploader, func(reference harnessv2.ArtifactReference) {
		mu.Lock()
		data := append([]byte(nil), uploaded...)
		mu.Unlock()
		if reference.Digest != artifactcap.DigestBytes(data) || reference.SizeBytes != int64(len(data)) {
			t.Fatal("native write delta receipt does not match the persisted bytes")
		}
		archive := tar.NewReader(bytes.NewReader(data))
		for {
			header, err := archive.Next()
			if err != nil {
				t.Fatal("native write delta archive is invalid or lacks the written file")
			}
			if header.Name != "files/native-write.txt" {
				continue
			}
			content, err := io.ReadAll(io.LimitReader(archive, 1024))
			if err != nil || string(content) != marker+"\n" {
				t.Fatal("persisted native write delta does not contain the exact written file")
			}
			return
		}
	}
}

func finalizePinnedNativePublication(t *testing.T, server *Server, cfg Config, metadata harnessv2.MutationMetadata, deltaID harnessv2.WorkspaceDeltaID) {
	t.Helper()
	metadata.OperationID = "native-write-publication-finalize"
	metadata.ExpiresAt = time.Now().UTC().Add(time.Minute)
	request := harnessv2.FinalizeRuntimeSessionPublicationRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: metadata, WorkspaceDeltaID: deltaID,
		PublicationID: "native-write-publication", PublicationGeneration: 1, PublicationVersion: 1,
		TerminalState: harnessv2.PublicationTerminalVerifiedExact, TerminalReceiptDigest: testDigest("native-write-publication-receipt"),
	}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/publication-finalization", request, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("native write publication finalization failed: %d %s", response.Code, response.Body.String())
	}
	var finalized harnessv2.FinalizeRuntimeSessionPublicationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &finalized); err != nil {
		t.Fatal(err)
	}
	if err := finalized.ValidateFor(request); err != nil || finalized.Session.State != harnessv2.RuntimeSessionStateFinalizing {
		t.Fatal("native write publication did not finalize before capture")
	}
}

func pinnedNativeWriteCall(body []byte, marker string) (map[string]any, error) {
	type tool struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Tools []tool `json:"tools"`
	}
	var request struct {
		Tools []tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	for _, advertised := range request.Tools {
		candidates := []tool{advertised}
		namespace := ""
		if advertised.Type == "namespace" {
			candidates, namespace = advertised.Tools, advertised.Name
		}
		for _, candidate := range candidates {
			if candidate.Type != "function" || candidate.Name != "exec_command" {
				continue
			}
			arguments, err := json.Marshal(map[string]any{
				"cmd":           "printf '%s\\n' '" + marker + "' > native-write.txt && printf '%s\\n' '" + marker + "_WRITTEN'",
				"yield_time_ms": 10000, "max_output_tokens": 1024,
			})
			if err != nil {
				return nil, err
			}
			item := map[string]any{
				"type": "function_call", "status": "completed", "id": "native-write-call", "call_id": "native-write-call-" + marker,
				"name": candidate.Name, "arguments": string(arguments),
			}
			if namespace != "" {
				item["namespace"] = namespace
			}
			return item, nil
		}
	}
	return nil, fmt.Errorf("pinned Codex did not advertise exec_command for the write-intent continuation")
}

func assertNativeBundleExcludesCredentials(t *testing.T, data []byte, credentials ...string) {
	t.Helper()
	var bundle struct {
		Manifest []byte `json:"manifest"`
		Rollout  []byte `json:"rollout"`
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	for _, credential := range credentials {
		if bytes.Contains(bundle.Manifest, []byte(credential)) || bytes.Contains(bundle.Rollout, []byte(credential)) {
			t.Fatal("credential entered decoded native bundle")
		}
	}
}

func assertCurrentNativeSettings(t *testing.T, rollout []byte, cwd string) {
	t.Helper()
	var sawCurrentContext, sawCurrentProvider bool
	for line := range bytes.SplitSeq(rollout, []byte("\n")) {
		var record struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		if record.Type == "turn_context" && record.Payload["cwd"] == cwd && record.Payload["approval_policy"] == "on-request" {
			sandbox, _ := record.Payload["sandbox_policy"].(map[string]any)
			sawCurrentContext = sandbox["type"] == "external-sandbox"
		}
		if record.Payload["type"] == "thread_settings_applied" {
			settings, _ := record.Payload["thread_settings"].(map[string]any)
			if settings["cwd"] == cwd && settings["model_provider_id"] == codexProviderID && settings["approval_policy"] == "on-request" {
				sawCurrentProvider = true
			}
		}
	}
	if !sawCurrentContext || !sawCurrentProvider {
		t.Fatalf("native effective current settings missing: context=%v provider=%v", sawCurrentContext, sawCurrentProvider)
	}
}

func assertSavedNativeFilesDeleted(t *testing.T, server *Server, cfg Config, create harnessv2.CreateRuntimeSessionRequest, state *sessionState) {
	t.Helper()
	journal := filepath.Join(cfg.SessionBaseDir, ".native-install", sessionPathID(state.descriptor.RuntimeSessionUID, state.descriptor.Generation))
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("native installation journal disappeared before saved capture acknowledgement")
	}
	deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "delete-saved-native", false), Reason: "snapshot saved"}
	sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
	if deleted := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, cfg); deleted.Code != http.StatusOK {
		t.Fatalf("native delete failed: %d %s", deleted.Code, deleted.Body.String())
	}
	for _, path := range []string{state.paths.Root, journal} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("explicit deletion retained native private session files")
		}
	}
}
