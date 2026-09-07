package supervisor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func foundryTestProof(t *testing.T, r *http.Request) (foundryBrokerContext, foundryBrokerProof) {
	t.Helper()
	if r.Header.Get(providerAuthorizationHeader) != "Bearer "+testUpstreamToken {
		t.Error("broker did not receive the supervisor credential")
	}
	values := r.Header.Values(foundryContextHeader)
	if len(values) != 1 {
		t.Errorf("broker received %d trusted context headers", len(values))
	}
	raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get(foundryContextHeader))
	if err != nil {
		t.Error("broker context was not unpadded base64url")
	}
	var value foundryBrokerContext
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Error("broker context was invalid JSON")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || foundrySHA256(body) != value.BodySHA256 {
		t.Error("broker context did not bind the exact request body")
	}
	owner, _ := json.Marshal(value.Owner)
	proof := foundryBrokerProof{
		Protocol: foundryBrokerProtocol, OwnerDigest: foundrySHA256(owner),
		OperationID: value.OperationID, ContextSHA256: foundrySHA256(raw),
		State: "open", LeaseGeneration: value.LeaseGeneration,
	}
	if value.LeaseExpiresAt != nil {
		proof.LeaseExpiresAt = value.LeaseExpiresAt.Format(time.RFC3339Nano)
	}
	switch r.URL.Path {
	case "/internal/v1/settle":
		proof.State, proof.SettlementProven, proof.ProofDigest = "settled", true, testDigest("settle-proof")
	case "/internal/v1/retire":
		proof.State, proof.SettlementProven, proof.RetirementProven, proof.ProofDigest = "retired", true, true, testDigest("retire-proof")
	}
	return value, proof
}

func newFoundryTestSession(t *testing.T, handler http.Handler) (*providerProxySession, ProviderProxyBinding, harnessv2.StartPromptRequest) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamBearerToken: testUpstreamToken,
		ProviderKind: providerKindFoundry, Model: "test-model",
	})
	cfg, profile := newTestConfigWithUpstream(t, "immediate", upstream.URL+"/v1", testUpstreamToken)
	profile.ProviderKind = providerKindFoundry
	profile.AgentConfigurationDigest = testDigest("foundry-config")
	profile.AdapterDigests = map[string]string{foundryAdapterName: testDigest("foundry-adapter")}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Fence.RuntimeProfileDigest = profileDigest
	create := testCreateSessionRequest(t, cfg, profile)
	create.AgentConfiguration = nil
	create.Metadata.RequestDigest = ""
	sealRequest(t, &create.Metadata.RequestDigest, create)
	session, binding, err := proxy.newSessionForRequest(create)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		session.close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = session.wait(ctx)
	})
	request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	if err := session.activatePrompt(request, 5, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return session, binding, request
}

func TestFoundryProxyBindsControllerContextAndRestrictsRoutes(t *testing.T) {
	observed := make(chan foundryBrokerContext, 4)
	session, binding, prompt := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, proof := foundryTestProof(t, r)
		if r.URL.Path == "/v1/responses" {
			observed <- value
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"opaque-response","output":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	body := []byte(`{"model":"test-model","stream":true,"store":true,"input":"hello"}`)
	response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, body, http.Header{
		foundryContextHeader: []string{"child-selected-owner", "duplicate-owner"},
	})
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Foundry inference status = %d", response.StatusCode)
	}
	var actual foundryBrokerContext
	select {
	case actual = <-observed:
	case <-time.After(time.Second):
		t.Fatal("no broker inference observed")
	}
	if actual.Owner != prompt.Metadata.Fence || actual.TaskUID != prompt.Metadata.TaskUID || actual.TaskAttempt != prompt.Metadata.TaskAttempt ||
		actual.PromptID != prompt.Metadata.PromptID || actual.PromptRequestDigest != prompt.Metadata.RequestDigest || actual.LeaseGeneration != prompt.Lease.Generation ||
		actual.LeaseExpiresAt == nil || !actual.LeaseExpiresAt.Equal(prompt.Lease.ExpiresAt) || actual.InvocationSequence != 1 ||
		actual.AgentConfigurationDigest != testDigest("foundry-config") || actual.BodySHA256 != foundrySHA256(body) {
		t.Fatal("inference did not carry the exact controller-owned identity, lease, configuration, and body digest")
	}
	for _, route := range []string{"/chat/completions", "/responses?api-version=other", "/internal/v1/retire", "/models"} {
		response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+route, binding.Credential, body, nil)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("forbidden route %q status = %d", route, response.StatusCode)
		}
	}
	select {
	case <-observed:
		t.Fatal("forbidden route reached the broker")
	default:
	}
	session.deactivate(string(prompt.Metadata.PromptID))
	if err := session.foundry.settle(t.Context(), string(prompt.Metadata.PromptID)); err != nil {
		t.Fatal(err)
	}
}

func TestFoundrySettlementWaitsForRemoteProofAndBlocksNewPrompt(t *testing.T) {
	settlementStarted := make(chan struct{}, 1)
	allowProof := make(chan struct{})
	session, _, request := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		if r.URL.Path == "/internal/v1/settle" {
			select {
			case settlementStarted <- struct{}{}:
			default:
			}
			select {
			case <-allowProof:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	session.deactivate(string(request.Metadata.PromptID))
	<-settlementStarted
	next := request
	next.Metadata.PromptID = "next-prompt"
	if err := session.activatePrompt(next, 5, time.Now()); err == nil {
		t.Fatal("new prompt admitted before remote settlement proof")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := session.foundry.settle(ctx, string(request.Metadata.PromptID)); err == nil {
		t.Fatal("settlement succeeded before remote proof")
	}
	close(allowProof)
	if err := session.foundry.settle(t.Context(), string(request.Metadata.PromptID)); err != nil {
		t.Fatal(err)
	}
	if err := session.activatePrompt(next, 5, time.Now()); err != nil {
		t.Fatalf("next prompt rejected after remote settlement: %v", err)
	}
}

func TestFoundryRejectsMismatchedOrIncompleteRemoteProof(t *testing.T) {
	cases := map[string]func(*foundryBrokerProof){
		"owner":          func(p *foundryBrokerProof) { p.OwnerDigest = testDigest("wrong-owner") },
		"context":        func(p *foundryBrokerProof) { p.ContextSHA256 = testDigest("wrong-context") },
		"operation":      func(p *foundryBrokerProof) { p.OperationID = "wrong-operation" },
		"protocol":       func(p *foundryBrokerProof) { p.Protocol = "wrong-protocol" },
		"active":         func(p *foundryBrokerProof) { p.ActiveInvocations = 1 },
		"ambiguous":      func(p *foundryBrokerProof) { p.AmbiguousInvocations = 1 },
		"create-pending": func(p *foundryBrokerProof) { p.CreatePending = true },
		"missing-proof":  func(p *foundryBrokerProof) { p.ProofDigest = "" },
		"not-settled":    func(p *foundryBrokerProof) { p.SettlementProven = false },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			session, _, request := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, proof := foundryTestProof(t, r)
				if r.URL.Path == "/internal/v1/settle" {
					corrupt(&proof)
				}
				_ = json.NewEncoder(w).Encode(proof)
			}))
			prompt := &promptState{request: request}
			server := &Server{}
			for _, outcome := range []acp.PromptOutcome{acp.PromptOutcomeCompleted, acp.PromptOutcomeCancelled, acp.PromptOutcomeFailed} {
				result := server.settleRemoteProvider(&sessionState{providerProxy: session}, prompt, acp.PromptResult{Outcome: outcome, Accepted: true})
				if result.Outcome != acp.PromptOutcomeOutcomeUnknown {
					t.Fatalf("unproven remote cleanup reported %s", result.Outcome)
				}
			}
		})
	}
}

func TestFoundryPendingCleanupRetriesOnlyTheSameControl(t *testing.T) {
	var calls atomic.Int32
	var original string
	session, _, request := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		if r.URL.Path == "/internal/v1/settle" {
			if calls.Add(1) == 1 {
				original = r.Header.Get(foundryContextHeader)
				proof.State, proof.SettlementProven, proof.ProofDigest = "settling", false, ""
				w.WriteHeader(http.StatusConflict)
			} else if original != r.Header.Get(foundryContextHeader) {
				t.Error("cleanup retry changed the operation context")
			}
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	if err := session.foundry.settle(t.Context(), string(request.Metadata.PromptID)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("settle calls = %d, want one pending response then one proof", calls.Load())
	}
}

func TestFoundryLeaseRenewalAndExpiryRevokeRemotePrompt(t *testing.T) {
	renewed := make(chan foundryBrokerContext, 1)
	settled := make(chan struct{}, 1)
	session, _, request := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, proof := foundryTestProof(t, r)
		switch r.URL.Path {
		case "/internal/v1/renew":
			renewed <- value
		case "/internal/v1/settle":
			select {
			case settled <- struct{}{}:
			default:
			}
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	lease := request.Lease
	lease.Generation++
	lease.ExpiresAt = time.Now().Add(100 * time.Millisecond).UTC()
	if err := session.foundry.renew(t.Context(), string(request.Metadata.PromptID), lease); err != nil {
		t.Fatal(err)
	}
	if err := session.renew(string(request.Metadata.PromptID), lease.ExpiresAt, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := <-renewed; got.LeaseGeneration != lease.Generation || !got.LeaseExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatal("remote renewal lost the controller lease")
	}
	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("lease expiry did not request remote settlement")
	}
	if _, err := session.foundry.inferenceContext(string(request.Metadata.PromptID), 1, []byte(`{}`)); err == nil {
		t.Fatal("expired remote prompt still admitted inference")
	}
}

func TestFoundryDeletionRequiresRetirementEvenWithoutInference(t *testing.T) {
	var retirementCalls atomic.Int32
	var allowProof atomic.Bool
	session, _, _ := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, proof := foundryTestProof(t, r)
		if r.URL.Path == "/internal/v1/retire" {
			retirementCalls.Add(1)
			if value.PromptID != "" || value.TaskUID != "" || value.LeaseExpiresAt != nil || value.InvocationSequence != 0 {
				t.Error("owner retirement included prompt-controlled identity")
			}
			if !allowProof.Load() {
				proof.RetirementProven = false
			}
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	session.close()
	if err := session.wait(t.Context()); err == nil {
		t.Fatal("local transport closure was accepted as remote retirement")
	}
	allowProof.Store(true)
	if err := session.wait(t.Context()); err != nil {
		t.Fatalf("idempotent retirement retry did not obtain proof: %v", err)
	}
	if retirementCalls.Load() != 2 {
		t.Fatalf("remote retirement requests = %d", retirementCalls.Load())
	}
}

func TestFoundryRetirementAcceptsHistoricalCreationEvidence(t *testing.T) {
	for _, created := range []bool{false, true} {
		name := "never-created"
		if created {
			name = "created-then-deleted"
		}
		t.Run(name, func(t *testing.T) {
			session, _, _ := newFoundryTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, proof := foundryTestProof(t, r)
				proof.RemoteSessionCreated = created
				_ = json.NewEncoder(w).Encode(proof)
			}))
			session.close()
			if err := session.wait(t.Context()); err != nil {
				t.Fatalf("confirmed retirement rejected historical creation evidence: %v", err)
			}
		})
	}
}

func TestFoundryProfileHasOnlyFrozenBrokeredCapabilities(t *testing.T) {
	t.Setenv(EnvFoundryAdapterDigest, testDigest("foundry-adapter"))
	t.Setenv(EnvAgentConfigurationDigest, testDigest("foundry-config"))
	profile, err := providerProfile(providerKindFoundry, "gpt-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Command != "/agent-runtime-foundry" || strings.Join(profile.Args, " ") != "--protocol acp --config /agent/foundry.json" {
		t.Fatal("Foundry child command is not fixed")
	}
	capabilities := providerCapabilities(providerKindFoundry, "gpt-test")
	if capabilities.SupportsPermissions || capabilities.SupportsImages || capabilities.SupportsAudio || capabilities.SupportsEmbeddedResources || !capabilities.SupportsCancel || !capabilities.SupportsTools {
		t.Fatal("Foundry advertised capabilities its ACP child does not provide")
	}
	request := agentKitBrokeredProjectionRequest(t)
	request.Profile.ProviderKind = providerKindFoundry
	if _, err := profile.ProjectSession(request, acp.SessionPaths{}, ProviderProxyBinding{}); err != nil {
		t.Fatal(err)
	}
	request.AgentConfiguration = &harnessv2.AgentSessionConfiguration{}
	if _, err := profile.ProjectSession(request, acp.SessionPaths{}, ProviderProxyBinding{}); err == nil {
		t.Fatal("Foundry accepted a per-Task configuration override")
	}
	env, err := profile.EnvironmentForSession(request, acp.SessionPaths{}, ProviderProxyBinding{BaseURL: "http://127.0.0.1:1234/v1", Credential: "fixture-local-credential"})
	if err != nil || len(env) != 4 || env[foundryConfigDigestEnv] != testDigest("foundry-config") || env[foundryModelEnv] != "gpt-test" {
		t.Fatal("Foundry child environment is not limited to the local proxy and frozen profile")
	}
}

func TestSupervisorFoundryAcceptsFrozenConfiguration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, proof := foundryTestProof(t, r)
		_ = json.NewEncoder(w).Encode(proof)
	}))
	t.Cleanup(upstream.Close)
	cfg, profile := newTestConfigWithUpstream(t, "immediate", upstream.URL+"/v1", testUpstreamToken)
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
	request := testCreateSessionRequest(t, cfg, profile)
	request.AgentConfiguration = nil
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("Foundry frozen-profile create status = %d", response.Code)
	}
}
