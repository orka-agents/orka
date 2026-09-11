package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const foundryRenewalTestPath = "/v2/runtime-sessions/session-1/prompts/prompt-1/lease"

func TestFoundryRejectedRenewalSettlesRemoteAuthority(t *testing.T) {
	renewEntered := make(chan struct{})
	releaseRenew := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseRenew) })
	defer release()
	var settlements atomic.Int32
	server, cfg, state, prompt := newFoundryRenewalTestPrompt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		switch r.URL.Path {
		case "/internal/v1/renew":
			close(renewEntered)
			select {
			case <-releaseRenew:
			case <-r.Context().Done():
				return
			}
		case "/internal/v1/settle":
			settlements.Add(1)
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	renew := newFoundryRenewalTestRequest(t, prompt)
	renew.Metadata.ExpiresAt = time.Now().UTC().Add(350 * time.Millisecond)
	renew.Metadata.RequestDigest = ""
	sealRequest(t, &renew.Metadata.RequestDigest, renew)
	done := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut,
		foundryRenewalTestPath, renew, cfg))
	awaitSignal(t, renewEntered, "renewal did not reach the broker")
	// Only the mutation authority expires. The original prompt/provider lease
	// remains live, so its watchdog cannot conceal missing compensation.
	timer := time.NewTimer(time.Until(renew.Metadata.ExpiresAt) + time.Millisecond)
	defer timer.Stop()
	<-timer.C
	release()
	select {
	case response := <-done:
		if response.Code != http.StatusGone {
			t.Fatalf("expired renewal status = %d, want 410", response.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expired renewal did not finish")
	}
	state.providerProxy.foundry.mu.Lock()
	remote := state.providerProxy.foundry.prompt
	closed, proven := remote.closed, remote.settlementProven
	state.providerProxy.foundry.mu.Unlock()
	if settlements.Load() != 1 || !closed || !proven {
		t.Fatal("rejected remotely acknowledged renewal left remote prompt authority open or unproven")
	}
}

func TestFoundryRenewalRejectionWaitsForProofAndRecordsUnknown(t *testing.T) {
	for _, proven := range []bool{true, false} {
		name := "proven"
		if !proven {
			name = "unproven"
		}
		t.Run(name, func(t *testing.T) {
			renewEntered, releaseRenew := make(chan struct{}), make(chan struct{})
			settleEntered, releaseProof := make(chan struct{}), make(chan struct{})
			renewReady := sync.OnceFunc(func() { close(releaseRenew) })
			proofReady := sync.OnceFunc(func() { close(releaseProof) })
			defer renewReady()
			defer proofReady()
			settleOnce := sync.OnceFunc(func() { close(settleEntered) })
			server, cfg, state, prompt := newFoundryRenewalTestPrompt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, proof := foundryTestProof(t, r)
				switch r.URL.Path {
				case "/internal/v1/renew":
					close(renewEntered)
					<-releaseRenew
				case "/internal/v1/settle":
					settleOnce()
					<-releaseProof
					proof.SettlementProven = proven
				}
				_ = json.NewEncoder(w).Encode(proof)
			}))
			renew := newFoundryRenewalTestRequest(t, prompt)
			done := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, foundryRenewalTestPath, renew, cfg))
			awaitSignal(t, renewEntered, "renewal did not reach the broker")
			// Model a current-generation change while the remote request is in
			// flight. This forces the post-ack lease validation rejection.
			server.mu.Lock()
			state.prompt.lease.Generation++
			server.mu.Unlock()
			renewReady()
			awaitSignal(t, settleEntered, "rejected renewal did not start exact remote settlement")
			select {
			case <-done:
				t.Fatal("renewal rejection returned before remote settlement was observed")
			case <-time.After(30 * time.Millisecond):
			}
			proofReady()
			response := readFoundryRenewalTestResponse(t, done)
			if response.Code != http.StatusConflict {
				t.Fatalf("stale lease renewal status = %d", response.Code)
			}
			server.mu.Lock()
			settlement := state.prompt.settlement
			server.mu.Unlock()
			want := harnessv2.EventCancelled
			if !proven {
				want = harnessv2.EventOutcomeUnknown
			}
			if settlement == nil || settlement.TerminalEvent != want {
				t.Fatalf("renewal compensation terminal = %v, want %s", settlement != nil, want)
			}
		})
	}
}

func TestFoundryRenewalLocalLeaseFailureClosesRemoteAuthority(t *testing.T) {
	renewEntered, releaseRenew := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseRenew) })
	defer release()
	server, cfg, state, prompt := newFoundryRenewalTestPrompt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		if r.URL.Path == "/internal/v1/renew" {
			close(renewEntered)
			<-releaseRenew
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	server.mu.Lock()
	originalRuntime := state.runtime
	// The ACP owner may already have removed its active prompt while the
	// supervisor still holds the pending HTTP projection. Capture that stale
	// owner for the renewal, then restore the real cleanup owner before ack.
	state.runtime = &acp.RuntimeSession{}
	server.mu.Unlock()
	defer func() {
		server.mu.Lock()
		state.runtime = originalRuntime
		server.mu.Unlock()
	}()
	renew := newFoundryRenewalTestRequest(t, prompt)
	done := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, foundryRenewalTestPath, renew, cfg))
	awaitSignal(t, renewEntered, "renewal did not reach the broker")
	server.mu.Lock()
	state.runtime = originalRuntime
	server.mu.Unlock()
	release()
	response := readFoundryRenewalTestResponse(t, done)
	if response.Code != http.StatusGone {
		t.Fatalf("stale ACP renewal status = %d", response.Code)
	}
	state.providerProxy.foundry.mu.Lock()
	closed, proven := state.providerProxy.foundry.prompt.closed, state.providerProxy.foundry.prompt.settlementProven
	state.providerProxy.foundry.mu.Unlock()
	if !closed || !proven {
		t.Fatal("ACP renewal failure retained remotely acknowledged authority")
	}
}

func TestFoundryDuplicateRenewalKeepsCommittedAuthority(t *testing.T) {
	entered := make(chan foundryBrokerContext, 2)
	releaseRenew := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseRenew) })
	defer release()
	var renewals, settlements atomic.Int32
	server, cfg, state, prompt := newFoundryRenewalTestPrompt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, proof := foundryTestProof(t, r)
		switch r.URL.Path {
		case "/internal/v1/renew":
			renewals.Add(1)
			entered <- value
			<-releaseRenew
		case "/internal/v1/settle":
			settlements.Add(1)
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	renew := newFoundryRenewalTestRequest(t, prompt)
	first := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, foundryRenewalTestPath, renew, cfg))
	second := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, foundryRenewalTestPath, renew, cfg))
	var remoteOperation string
	for range 2 {
		select {
		case value := <-entered:
			if remoteOperation != "" && remoteOperation != value.OperationID {
				t.Fatal("duplicate renewal selected another remote operation")
			}
			remoteOperation = value.OperationID
		case <-time.After(5 * time.Second):
			t.Fatal("both in-flight duplicate requests did not reach the broker")
		}
	}
	release()
	for _, done := range []<-chan *httptest.ResponseRecorder{first, second} {
		if response := readFoundryRenewalTestResponse(t, done); response.Code != http.StatusOK {
			t.Fatalf("duplicate lease renewal status = %d", response.Code)
		}
	}
	// A later exact replay must use the completed local operation record.
	response := performMutation(t, server.Handler(), http.MethodPut, foundryRenewalTestPath, renew, cfg)
	if response.Code != http.StatusOK || renewals.Load() != 2 || settlements.Load() != 0 {
		t.Fatal("successful duplicate renewal repeated or cancelled remote authority")
	}
	server.mu.Lock()
	lease, settled := state.prompt.lease, state.prompt.settlement != nil
	server.mu.Unlock()
	if lease != renew.Lease || settled {
		t.Fatal("duplicate renewal changed the committed prompt lease")
	}
}

func TestFoundryDisconnectedRenewalStillObtainsRemoteSettlement(t *testing.T) {
	renewEntered := make(chan struct{})
	var settlements atomic.Int32
	server, cfg, state, prompt := newFoundryRenewalTestPrompt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		switch r.URL.Path {
		case "/internal/v1/renew":
			close(renewEntered)
			<-r.Context().Done()
			return
		case "/internal/v1/settle":
			settlements.Add(1)
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	renew := newFoundryRenewalTestRequest(t, prompt)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut, foundryRenewalTestPath, renew, cfg).WithContext(ctx))
	awaitSignal(t, renewEntered, "renewal did not reach the broker")
	// Its acknowledgement is now unknowable. The caller's cancellation must
	// not also cancel the independent operation that closes remote authority.
	cancel()
	response := readFoundryRenewalTestResponse(t, done)
	if response.Code != http.StatusGone {
		t.Fatalf("disconnected renewal status = %d", response.Code)
	}
	state.providerProxy.foundry.mu.Lock()
	closed, proven := state.providerProxy.foundry.prompt.closed, state.providerProxy.foundry.prompt.settlementProven
	state.providerProxy.foundry.mu.Unlock()
	if settlements.Load() != 1 || !closed || !proven {
		t.Fatal("caller disconnection abandoned the remote renewal compensation")
	}
}

func TestFoundryLateRenewalContainmentPreservesContinuation(t *testing.T) {
	var settlements atomic.Int32
	session, _, first := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		if r.URL.Path == "/internal/v1/settle" {
			settlements.Add(1)
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	session.foundry.mu.Lock()
	oldRemote := session.foundry.prompt
	session.foundry.mu.Unlock()
	session.deactivate(string(first.Metadata.PromptID))
	if err := session.foundry.settle(t.Context(), string(first.Metadata.PromptID)); err != nil {
		t.Fatal(err)
	}
	settlement := settlementFromResult(acp.PromptResult{Outcome: acp.PromptOutcomeCompleted, Accepted: true}, time.Now().UTC())
	oldPrompt := &promptState{request: first, settlement: &settlement}
	next := first
	next.Metadata.PromptID = "continuation-prompt"
	next.Metadata.OperationID = "continuation-operation"
	next.Metadata.RequestDigest = ""
	next.MCPAuthorization.PromptID = next.Metadata.PromptID
	sealRequest(t, &next.Metadata.RequestDigest, next)
	if err := session.activatePrompt(next, 5, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current := &promptState{request: next, lease: next.Lease}
	state := &sessionState{providerProxy: session, prompt: current,
		descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStatePromptRunning}}
	server := &Server{}
	server.containRejectedRemoteRenewal(state, oldPrompt)
	if err := waitRejectedRenewalSettlement(t.Context(), session.foundry, oldRemote); err != nil {
		t.Fatal("captured old-turn proof was replaced by the active continuation")
	}
	if state.prompt != current || current.settlement != nil || state.descriptor.State != harnessv2.RuntimeSessionStatePromptRunning || settlements.Load() != 1 {
		t.Fatal("late renewal compensation changed a settled turn or its continuation")
	}
	if _, err := session.foundry.inferenceContext(string(next.Metadata.PromptID), 1, []byte(`{}`)); err != nil {
		t.Fatal("late renewal compensation revoked the continuation's remote authority")
	}
}

func readFoundryRenewalTestResponse(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-done:
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("renewal handler exceeded the fixture bound")
		return nil
	}
}

func newFoundryRenewalTestPrompt(t *testing.T, handler http.Handler) (*Server, Config, *sessionState, harnessv2.StartPromptRequest) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg, profile := newTestConfigWithUpstream(t, "wait", upstream.URL+"/v1", testUpstreamToken)
	profile.ProviderKind = providerKindFoundry
	profile.AgentConfigurationDigest = testDigest("foundry-config")
	profile.AdapterDigests = map[string]string{foundryAdapterName: testDigest("foundry-adapter")}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = profileDigest
	cfg.Capabilities.RuntimeProfileDigest = profileDigest
	cfg.Capabilities.AdapterDigests = profile.AdapterDigests
	cfg.Capabilities.SupportsAgentSessionConfiguration = false
	cfg.Capabilities.Provider = providerCapabilities(providerKindFoundry, profile.Model)
	cfg.Provider.Kind = providerKindFoundry
	cfg.Provider.AdapterName = foundryAdapterName
	cfg.Provider.AdapterDigest = profile.AdapterDigests[foundryAdapterName]
	cfg.Provider.ProjectSession = func(request harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, _ ProviderProxyBinding) (ProviderSessionProjection, error) {
		return foundrySessionProjection(request, profile.Model)
	}
	cfg.ProviderProxy.ProviderKind = providerKindFoundry
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
	create.AgentConfiguration = nil
	create.Metadata.RequestDigest = ""
	sealRequest(t, &create.Metadata.RequestDigest, create)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("Foundry fixture create status = %d", response.Code)
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	ctx, cancel := context.WithCancel(t.Context())
	done := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut,
		"/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg).WithContext(ctx))
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Foundry fixture prompt did not settle during cleanup")
		}
	})
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		server.mu.Lock()
		state := server.sessions[create.RuntimeSessionID]
		running := state != nil && state.prompt != nil && !state.prompt.acceptedAt.IsZero()
		server.mu.Unlock()
		if running {
			return server, cfg, state, prompt
		}
		select {
		case response := <-done:
			t.Fatalf("Foundry fixture prompt ended before renewal: status=%d", response.Code)
		case <-deadline.C:
			t.Fatal("Foundry fixture prompt did not become active")
		case <-tick.C:
		}
	}
}

func newFoundryRenewalTestRequest(t *testing.T, prompt harnessv2.StartPromptRequest) harnessv2.RenewPromptLeaseRequest {
	t.Helper()
	now := time.Now().UTC()
	metadata := testMetadata(prompt.Metadata.Fence, "foundry-renewal", true)
	metadata.ExpiresAt = now.Add(30 * time.Second)
	lease := harnessv2.PromptLease{Generation: prompt.Lease.Generation + 1, IssuedAt: now, ExpiresAt: prompt.Lease.ExpiresAt.Add(30 * time.Second)}
	authorization := prompt.MCPAuthorization
	authorization.LeaseGeneration = lease.Generation
	authorization.ExpiresAt = metadata.ExpiresAt
	request := harnessv2.RenewPromptLeaseRequest{Protocol: harnessv2.ProtocolVersion, Metadata: metadata,
		ExpectedLeaseGeneration: prompt.Lease.Generation, Lease: lease, MCPAuthorization: authorization}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return request
}
