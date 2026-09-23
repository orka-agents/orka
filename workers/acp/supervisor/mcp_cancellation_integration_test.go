package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type delayedMCPPromptCancellation struct {
	promptMutationExecutor
	entered chan struct{}
	release chan struct{}
}

func (m *delayedMCPPromptCancellation) CancelPrompt(ctx context.Context, promptID string) (acp.PromptResult, error) {
	close(m.entered)
	select {
	case <-m.release:
		return m.promptMutationExecutor.CancelPrompt(ctx, promptID)
	case <-ctx.Done():
		return acp.PromptResult{}, ctx.Err()
	}
}

func TestSupervisorCancellationRevokesMCPBeforeCourtesyCancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	cfg, profile := newTestConfigWithUpstream(t, "wait", upstream.URL, testUpstreamToken)
	authorization, _ := testMCPAuthorization(t, cfg.Fence, time.Now().UTC(), false)
	profile.ToolPolicyDigest = authorization.ToolPolicyDigest
	profile.ApprovalPolicyDigest = authorization.ApprovalPolicyDigest
	profile.MCPConfigurationDigest = authorization.MCPConfigurationDigest
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = digest
	cfg.Capabilities.RuntimeProfileDigest = digest
	started := make(chan harnessv2.MCPBrokerCallRequest, 1)
	revoked := make(chan struct{})
	var brokerCalls atomic.Int32
	cfg.MCPBroker = MCPBrokerFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		first := brokerCalls.Add(1) == 1
		if first {
			started <- request
		}
		<-ctx.Done()
		if first {
			close(revoked)
		}
		return harnessv2.MCPBrokerCallResponse{}, ctx.Err()
	})
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil && (runtime.GOOS == linuxGOOS ||
			!strings.Contains(err.Error(), "ACP adapter descendant cleanup could not be proven")) {
			t.Errorf("supervisor cleanup: %v", err)
		}
	})
	create := testCreateSessionRequest(t, cfg, profile)
	create.MCPConfiguration.ToolPolicy = authorization.ToolPolicy
	create.MCPConfiguration.ApprovalPolicy = authorization.ApprovalPolicy
	create.Metadata.RequestDigest = ""
	sealRequest(t, &create.Metadata.RequestDigest, create)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d", created.Code)
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	mutations := &delayedMCPPromptCancellation{
		promptMutationExecutor: state.runtime, entered: make(chan struct{}), release: make(chan struct{}),
	}
	state.promptMutations = mutations
	mcp := state.mcpProxy
	server.mu.Unlock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(mutations.release) }) }
	defer release()

	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	authorization.RuntimeSessionUID = create.Metadata.Fence.RuntimeSessionUID
	authorization.SessionGeneration = create.Metadata.Fence.RuntimeSessionGeneration
	authorization.ExpiresAt = prompt.MCPAuthorization.ExpiresAt
	prompt.MCPAuthorization = authorization
	prompt.Metadata.RequestDigest = ""
	sealRequest(t, &prompt.Metadata.RequestDigest, prompt)
	promptCtx, cancelPrompt := context.WithCancel(t.Context())
	defer cancelPrompt()
	promptDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut,
		"/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg).WithContext(promptCtx))
	awaitMCPCancellationPromptRunning(t, server, state)
	callDone := startMCPCancellationRequest(t, mcp, "active")
	select {
	case request := <-started:
		if request.Metadata.Fence != create.Metadata.Fence ||
			!reflect.DeepEqual(request.Authorization, prompt.MCPAuthorization) || request.Call.ToolName != "lookup" ||
			request.Metadata.TaskUID != prompt.Metadata.TaskUID || request.Metadata.TaskAttempt != prompt.Metadata.TaskAttempt ||
			request.Metadata.PromptID != prompt.Metadata.PromptID || request.Lease != prompt.Lease {
			t.Fatal("broker call lost the admitted prompt identity or canonical tool policy")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authorized MCP request did not reach the broker")
	}
	cancellation := testLateCancellation(t, create.Metadata.Fence)
	cancelDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut,
		"/v2/runtime-sessions/session-1/prompts/prompt-1/cancel", cancellation, cfg))
	awaitSignal(t, mutations.entered, "cancellation did not reach the real runtime boundary")
	awaitSignal(t, revoked, "courtesy cancellation delayed broker authority revocation")
	lateDone := startMCPCancellationRequest(t, mcp, "late")
	assertMCPCancellationPending(t, callDone, "broker revocation error escaped before courtesy cancellation")
	assertMCPCancellationPending(t, lateDone, "post-revocation rejection escaped before courtesy cancellation")
	assertStillWaiting(t, promptDone, "real ACP child settled before receiving cancellation")
	if brokerCalls.Load() != 1 {
		t.Fatal("post-revocation request reached the broker")
	}

	// The production waiter must unblock from the real child's response. It
	// must not hold a proxy/runtime lock needed by courtesy cancel or teardown.
	release()
	var settled harnessv2.CancelPromptResponse
	decodeResponse(t, awaitRecorder(t, cancelDone, "cancellation did not settle"), &settled)
	if err := settled.Validate(); err != nil {
		t.Fatal(err)
	}
	if !settled.SettlementProven || settled.Settlement.TerminalEvent != harnessv2.EventCancelled || settled.ForcedTermination {
		t.Fatalf("cancellation lost the child's settlement: %#v", settled)
	}
	assertMCPCancellationError(t, callDone, "active", -32002)
	assertMCPCancellationError(t, lateDone, "late", -32001)
	response := awaitRecorder(t, promptDone, "original prompt did not settle")
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(response.Body.Bytes()), eventLimits(cfg.Capabilities.Limits),
		harnessv2.EventExpectationFromMetadata(prompt.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil || len(events) == 0 || events[len(events)-1].Type != harnessv2.EventCancelled {
		t.Fatalf("original stream lost cancellation: events=%#v error=%v", events, err)
	}
	if brokerCalls.Load() != 1 || len(mcp.calls) != 0 {
		t.Fatal("cancellation retained an MCP request or repeated the broker call")
	}
	assertMCPCancellationCleanup(t, server, cfg, create, state)
}

func assertMCPCancellationCleanup(t *testing.T, server *Server, cfg Config, create harnessv2.CreateRuntimeSessionRequest, state *sessionState) {
	t.Helper()
	// A non-success prompt schedules automatic cleanup. Wait for that attempt
	// rather than racing it with a second delete. Non-Linux cannot prove UID
	// descendant retirement and must retain the session without a tombstone.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		remaining := server.sessions[create.RuntimeSessionID]
		tombstone, deleted := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
		retained := remaining == state && state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned && !state.drainCleanupScheduled
		server.mu.Unlock()
		if runtime.GOOS == linuxGOOS && remaining == nil {
			if !deleted || tombstone.RuntimeSessionUID != create.Metadata.Fence.RuntimeSessionUID ||
				tombstone.RuntimeSessionGeneration != create.Metadata.Fence.RuntimeSessionGeneration ||
				tombstone.RuntimeProfileDigest != create.Metadata.Fence.RuntimeProfileDigest {
				t.Fatal("automatic cleanup lost the exact session tombstone")
			}
			if err := tombstone.Validate(); err != nil {
				t.Fatalf("automatic cleanup produced an invalid tombstone: %v", err)
			}
			return
		}
		if runtime.GOOS != linuxGOOS && retained {
			if deleted {
				t.Fatal("unproven descendant cleanup produced a tombstone")
			}
			deletion := harnessv2.DeleteRuntimeSessionRequest{
				Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "mcp-cancel-cleanup", false),
				Reason: "cancellation integration cleanup",
			}
			sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
			deleteDone := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodDelete,
				"/v2/runtime-sessions/session-1", deletion, cfg))
			select {
			case response := <-deleteDone:
				var failure harnessv2.ErrorResponse
				decodeResponse(t, response, &failure)
				if response.Code != http.StatusInternalServerError || failure.Code != harnessv2.ErrorCodeSessionPoisoned {
					t.Fatal("non-Linux deletion did not fail closed on unproven descendant cleanup")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session deletion deadlocked after MCP cancellation")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("automatic session cleanup did not finish after MCP cancellation")
}

func awaitMCPCancellationPromptRunning(t *testing.T, server *Server, state *sessionState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		running := state.descriptor.State == harnessv2.RuntimeSessionStatePromptRunning
		server.mu.Unlock()
		if running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("prompt did not become active")
}

type mcpCancellationHTTPResult struct {
	status   int
	response mcpJSONRPCResponse
	err      error
}

func startMCPCancellationRequest(t *testing.T, session *mcpProxySession, id string) <-chan mcpCancellationHTTPResult {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": "lookup", "arguments": map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, session.url, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+string(session.credential))
	request.Header.Set("Content-Type", "application/json")
	done := make(chan mcpCancellationHTTPResult, 1)
	go func() {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			done <- mcpCancellationHTTPResult{err: err}
			return
		}
		defer response.Body.Close() //nolint:errcheck
		result := mcpCancellationHTTPResult{status: response.StatusCode}
		result.err = json.NewDecoder(response.Body).Decode(&result.response)
		done <- result
	}()
	return done
}

func assertMCPCancellationPending(t *testing.T, done <-chan mcpCancellationHTTPResult, message string) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("%s: status=%d error=%v", message, result.status, result.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertMCPCancellationError(t *testing.T, done <-chan mcpCancellationHTTPResult, id string, code int) {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil || result.status != http.StatusOK || string(result.response.ID) != `"`+id+`"` ||
			result.response.Error == nil || result.response.Error.Code != code || result.response.Result != nil {
			t.Fatalf("revoked MCP response = %#v, transport error = %v", result.response, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP request did not finish after cancellation settled")
	}
}
