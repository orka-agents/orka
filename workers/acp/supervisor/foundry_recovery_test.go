package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func newFoundryRecoveryTestServer(t *testing.T, handler http.Handler) (*Server, Config, harnessv2.CreateRuntimeSessionRequest) {
	t.Helper()
	return newFoundryTestServerWithRecovery(t, handler, true)
}

func foundryRecoveryIdentity() harnessv2.FoundryBrokerIdentity {
	return harnessv2.FoundryBrokerIdentity{Protocol: harnessv2.FoundryBrokerProtocol,
		LedgerIdentityDigest: testDigest("ledger"), AgentConfigurationDigest: testDigest("foundry-config")}
}

func foundryRecoveryRequest(t *testing.T, cfg Config) harnessv2.FoundryBootRetirementRequest {
	t.Helper()
	old := cfg.Fence
	old.SupervisorBootID = "lost-supervisor-boot"
	request := harnessv2.FoundryBootRetirementRequest{Protocol: harnessv2.ProtocolVersion,
		Metadata: harnessv2.MutationMetadata{Fence: cfg.Fence, OperationID: "retire-old-boot",
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion, ExpiresAt: time.Now().UTC().Add(time.Minute)},
		RetiredFence: old, Broker: foundryRecoveryIdentity()}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return request
}

func foundryRecoveryProof(t *testing.T, r *http.Request) harnessv2.FoundryBootRetirementProof {
	t.Helper()
	if r.Header.Get(providerAuthorizationHeader) != "Bearer "+testUpstreamToken || r.Header.Get(foundryContextHeader) != "" ||
		r.Method != http.MethodPost || r.URL.Path != "/internal/v1/retire-boot" {
		t.Error("recovery did not use the isolated authenticated boot retirement route")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Protocol                 string          `json:"protocol"`
		LedgerIdentityDigest     string          `json:"ledgerIdentityDigest"`
		AgentConfigurationDigest string          `json:"agentConfigurationDigest"`
		RetiredFence             harnessv2.Fence `json:"retiredFence"`
		OperationID              string          `json:"operationID"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	fence, err := json.Marshal(request.RetiredFence)
	if err != nil {
		t.Fatal(err)
	}
	proof := harnessv2.FoundryBootRetirementProof{Protocol: request.Protocol,
		LedgerIdentityDigest: request.LedgerIdentityDigest, AgentConfigurationDigest: request.AgentConfigurationDigest,
		OperationID: request.OperationID, ContextSHA256: foundrySHA256(body), RetiredFenceDigest: foundrySHA256(fence),
		State: "retired", Sealed: true, SettlementProven: true, RetirementProven: true, OwnerSetDigest: foundrySHA256([]byte("[]"))}
	proof.ProofDigest = proof.CanonicalProofDigest()
	return proof
}

func TestFoundryRecoveryRelaysOnlyExactAuthorizedRetirement(t *testing.T) {
	var retired atomic.Int32
	server, cfg, _ := newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/identity" {
			if r.Header.Get(providerAuthorizationHeader) != "Bearer "+testUpstreamToken {
				t.Error("identity probe lacked broker authentication")
			}
			writeJSON(w, http.StatusOK, foundryRecoveryIdentity())
			return
		}
		retired.Add(1)
		writeJSON(w, http.StatusOK, foundryRecoveryProof(t, r))
	}))
	request := foundryRecoveryRequest(t, cfg)
	for _, expectedClass := range []harnessv2.RequestClassification{harnessv2.RequestClassificationFresh, harnessv2.RequestClassificationDuplicate} {
		response := performMutation(t, server.Handler(), http.MethodPut, harnessv2.FoundryBootRetirementPath, request, cfg)
		var result harnessv2.FoundryBootRetirementResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.ValidateFor(request) != nil || result.Classification.Class != expectedClass {
			t.Fatalf("retirement relay response=%d class=%s", response.Code, result.Classification.Class)
		}
	}
	if retired.Load() != 2 || len(server.sessions) != 0 || len(server.poolOps) != 0 {
		t.Fatal("recovery unexpectedly created session or normal pool authority")
	}
	request.RetiredFence.SupervisorBootID = "a-different-lost-boot"
	sealRequest(t, &request.Metadata.RequestDigest, request)
	conflict := performMutation(t, server.Handler(), http.MethodPut, harnessv2.FoundryBootRetirementPath, request, cfg)
	if conflict.Code != http.StatusConflict || retired.Load() != 2 {
		t.Fatal("same operation identity reached the broker with a different subject")
	}
}

func TestFoundryRecoveryRejectsUnauthorizedOrMismatchedRelay(t *testing.T) {
	for name, change := range map[string]func(*http.Request, *harnessv2.FoundryBootRetirementRequest, *Config){
		"no bearer": func(r *http.Request, _ *harnessv2.FoundryBootRetirementRequest, _ *Config) {
			r.Header.Del("Authorization")
		},
		"no capability": func(r *http.Request, _ *harnessv2.FoundryBootRetirementRequest, _ *Config) {
			r.Header.Del(OperationCapabilityHeader)
		},
		"recovery disabled": func(_ *http.Request, _ *harnessv2.FoundryBootRetirementRequest, cfg *Config) {
			cfg.Capabilities.SupportsFoundryRecovery = false
		},
		"capability disabled": func(_ *http.Request, _ *harnessv2.FoundryBootRetirementRequest, cfg *Config) {
			cfg.RequireCapabilities = false
		},
		"stale relay": func(_ *http.Request, r *harnessv2.FoundryBootRetirementRequest, _ *Config) {
			r.Metadata.Fence.SupervisorBootID = "wrong-relay"
		},
		"wrong ledger": func(_ *http.Request, r *harnessv2.FoundryBootRetirementRequest, _ *Config) {
			r.Broker.LedgerIdentityDigest = testDigest("replacement-ledger")
		},
		"wrong config": func(_ *http.Request, r *harnessv2.FoundryBootRetirementRequest, _ *Config) {
			r.Broker.AgentConfigurationDigest = testDigest("replacement-config")
		},
		"wrong provider": func(_ *http.Request, _ *harnessv2.FoundryBootRetirementRequest, cfg *Config) {
			cfg.Provider.Kind = providerKindCodex
		},
	} {
		t.Run(name, func(t *testing.T) {
			var retired atomic.Int32
			server, cfg, _ := newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/identity" {
					writeJSON(w, http.StatusOK, foundryRecoveryIdentity())
					return
				}
				retired.Add(1)
				writeJSON(w, http.StatusOK, foundryRecoveryProof(t, r))
			}))
			request := foundryRecoveryRequest(t, cfg)
			// Apply header and subject changes before signing the final request.
			headerRequest := httptest.NewRequest(http.MethodPut, harnessv2.FoundryBootRetirementPath, nil)
			headerRequest.Header.Set("Authorization", "present")
			headerRequest.Header.Set(OperationCapabilityHeader, "present")
			change(headerRequest, &request, &server.cfg)
			sealRequest(t, &request.Metadata.RequestDigest, request)
			httpRequest := mutationHTTPRequest(t, http.MethodPut, harnessv2.FoundryBootRetirementPath, request, cfg)
			for _, header := range []string{"Authorization", OperationCapabilityHeader} {
				if headerRequest.Header.Get(header) == "" {
					httpRequest.Header.Del(header)
				}
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httpRequest)
			if response.Code < 400 || retired.Load() != 0 {
				t.Fatal("invalid relay authority reached boot retirement")
			}
		})
	}
}

func TestFoundryRecoveryRejectsUnprovenBrokerResponseWithoutRetry(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing zero counter": func(p map[string]any) { delete(p, "activeInvocations") },
		"null counter":         func(p map[string]any) { p["pendingCreates"] = nil },
		"ambiguous invocation": func(p map[string]any) { p["ambiguousInvocations"] = 1 },
		"pending create":       func(p map[string]any) { p["pendingCreates"] = 1 },
		"wrong context":        func(p map[string]any) { p["contextSHA256"] = testDigest("wrong-context") },
		"tampered proof":       func(p map[string]any) { p["ownerCount"] = 5 },
		"unknown field":        func(p map[string]any) { p["ignored"] = true },
		"response too large":   func(p map[string]any) { p["state"] = strings.Repeat("x", foundryControlResponseLimit+1) },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server, cfg, _ := newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/identity" {
					writeJSON(w, http.StatusOK, foundryRecoveryIdentity())
					return
				}
				calls.Add(1)
				body, err := json.Marshal(foundryRecoveryProof(t, r))
				if err != nil {
					t.Fatal(err)
				}
				var proof map[string]any
				if err := json.Unmarshal(body, &proof); err != nil {
					t.Fatal(err)
				}
				mutate(proof)
				writeJSON(w, http.StatusOK, proof)
			}))
			request := foundryRecoveryRequest(t, cfg)
			response := performMutation(t, server.Handler(), http.MethodPut, harnessv2.FoundryBootRetirementPath, request, cfg)
			if response.Code != http.StatusConflict || calls.Load() != 1 || len(server.sessions) != 0 {
				t.Fatal("incomplete cleanup was accepted, replayed, or admitted work")
			}
		})
	}
}

func TestFoundryStatusRequiresAndPinsBrokerIdentity(t *testing.T) {
	var mode atomic.Int32
	server, _, _ := newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/identity" {
			t.Error("status attempted a broker mutation")
		}
		if mode.Load() == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		identity := foundryRecoveryIdentity()
		if mode.Load() == 2 {
			identity.LedgerIdentityDigest = testDigest("replacement-ledger")
		}
		writeJSON(w, http.StatusOK, identity)
	}))
	if identity, err := server.foundryStatusIdentity(t.Context()); err == nil || identity != nil {
		t.Fatal("qualified recovery accepted a broker without identity")
	}
	mode.Store(1)
	if identity, err := server.foundryStatusIdentity(t.Context()); err != nil || identity == nil || *identity != foundryRecoveryIdentity() {
		t.Fatal("authenticated broker identity was not observed")
	}
	for _, next := range []int32{2, 0} {
		mode.Store(next)
		if _, err := server.foundryStatusIdentity(t.Context()); err == nil {
			t.Fatal("pinned broker identity changed or disappeared without failure")
		}
	}
}

func TestFoundryRecoveryReplayCapacityPreservesLiveOperations(t *testing.T) {
	server, cfg, _ := newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	request := foundryRecoveryRequest(t, cfg)
	now := time.Now().UTC()
	for i := range maxFoundryRecoveryOperations {
		request.Metadata.OperationID = harnessv2.OperationID(fmt.Sprintf("retire-%d", i))
		sealRequest(t, &request.Metadata.RequestDigest, request)
		if _, err := server.classifyFoundryRecovery(request, now); err != nil {
			t.Fatal(err)
		}
	}
	last := request
	request.Metadata.OperationID = "one-too-many"
	sealRequest(t, &request.Metadata.RequestDigest, request)
	if _, err := server.classifyFoundryRecovery(request, now); err == nil {
		t.Fatal("unbounded retirement journal accepted")
	}
	if result, err := server.classifyFoundryRecovery(last, now); err != nil || result.Class != harnessv2.RequestClassificationDuplicate {
		t.Fatal("full retirement journal evicted a live operation")
	}
	later := request.Metadata.ExpiresAt.Add(operationReplayRetentionSlack + time.Second)
	request.Metadata.ExpiresAt = later.Add(time.Minute)
	sealRequest(t, &request.Metadata.RequestDigest, request)
	if _, err := server.classifyFoundryRecovery(request, later); err != nil || len(server.foundryRecoveryOps) != 1 {
		t.Fatal("expired retirement operations were not reclaimed")
	}
}
