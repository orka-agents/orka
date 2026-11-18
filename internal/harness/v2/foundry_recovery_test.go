package v2

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testFoundryRetirementRequest(t *testing.T, now time.Time) FoundryBootRetirementRequest {
	t.Helper()
	fence := testFence(t)
	fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "", 0
	old := fence
	old.SupervisorBootID = "lost-boot"
	old.ControllerEpoch--
	request := FoundryBootRetirementRequest{
		Protocol:     ProtocolVersion,
		Metadata:     MutationMetadata{Fence: fence, OperationID: "retire-lost-boot", RequestDigestSchemaVersion: RequestDigestSchemaVersion, ExpiresAt: now.Add(time.Minute)},
		RetiredFence: old,
		Broker:       FoundryBrokerIdentity{Protocol: FoundryBrokerProtocol, LedgerIdentityDigest: testSHA256("ledger"), AgentConfigurationDigest: testSHA256("configuration")},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	return request
}

func testFoundryRetirementProof(t *testing.T, request FoundryBootRetirementRequest) FoundryBootRetirementProof {
	t.Helper()
	body, err := request.BrokerRequestBody()
	if err != nil {
		t.Fatal(err)
	}
	fence, err := json.Marshal(request.RetiredFence)
	if err != nil {
		t.Fatal(err)
	}
	proof := FoundryBootRetirementProof{
		Protocol: request.Broker.Protocol, LedgerIdentityDigest: request.Broker.LedgerIdentityDigest,
		AgentConfigurationDigest: request.Broker.AgentConfigurationDigest, OperationID: string(request.Metadata.OperationID),
		ContextSHA256: testSHA256(string(body)), RetiredFenceDigest: testSHA256(string(fence)),
		State: "retired", Sealed: true, SettlementProven: true, RetirementProven: true, OwnerSetDigest: testSHA256("[]"),
	}
	proof.ProofDigest = proof.CanonicalProofDigest()
	return proof
}

func TestFoundryRetirementRequiresExactPoolWideAuthority(t *testing.T) {
	request := testFoundryRetirementRequest(t, testNow)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*FoundryBootRetirementRequest){
		"current boot": func(r *FoundryBootRetirementRequest) {
			r.RetiredFence.SupervisorBootID = r.Metadata.Fence.SupervisorBootID
		},
		"future epoch": func(r *FoundryBootRetirementRequest) {
			r.RetiredFence.ControllerEpoch = r.Metadata.Fence.ControllerEpoch + 1
		},
		"other instance":   func(r *FoundryBootRetirementRequest) { r.RetiredFence.RuntimeInstanceID = "other-instance" },
		"other pool":       func(r *FoundryBootRetirementRequest) { r.RetiredFence.RuntimePoolUID = "other-pool" },
		"other generation": func(r *FoundryBootRetirementRequest) { r.RetiredFence.RuntimePoolGeneration++ },
		"other profile": func(r *FoundryBootRetirementRequest) {
			r.RetiredFence.RuntimeProfileDigest = ProfileDigest(testSHA256("other-profile"))
		},
		"retired session": func(r *FoundryBootRetirementRequest) {
			r.RetiredFence.RuntimeSessionUID, r.RetiredFence.RuntimeSessionGeneration = "one-session", 1
		},
		"relay session": func(r *FoundryBootRetirementRequest) {
			r.Metadata.Fence.RuntimeSessionUID, r.Metadata.Fence.RuntimeSessionGeneration = "one-session", 1
		},
		"task authority":        func(r *FoundryBootRetirementRequest) { r.Metadata.TaskUID, r.Metadata.TaskAttempt = "task", 1 },
		"expired":               func(r *FoundryBootRetirementRequest) { r.Metadata.ExpiresAt = testNow },
		"missing ledger":        func(r *FoundryBootRetirementRequest) { r.Broker.LedgerIdentityDigest = "" },
		"missing configuration": func(r *FoundryBootRetirementRequest) { r.Broker.AgentConfigurationDigest = "" },
		"wrong protocol":        func(r *FoundryBootRetirementRequest) { r.Broker.Protocol = ProtocolVersion },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			mutate(&changed)
			sealRequest(t, changed, &changed.Metadata.RequestDigest)
			if err := changed.ValidateAt(testNow); err == nil {
				t.Fatal("invalid recovery authority was accepted")
			}
		})
	}
	request.Broker.LedgerIdentityDigest = testSHA256("replacement-ledger")
	if err := request.ValidateAt(testNow); err == nil {
		t.Fatal("request body tampering did not invalidate the signed digest")
	}
}

func TestFoundryRetirementProofRejectsIncompleteOrDifferentEvidence(t *testing.T) {
	request := testFoundryRetirementRequest(t, testNow)
	proof := testFoundryRetirementProof(t, request)
	if err := proof.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*FoundryBootRetirementProof){
		"protocol":             func(p *FoundryBootRetirementProof) { p.Protocol = "other" },
		"ledger":               func(p *FoundryBootRetirementProof) { p.LedgerIdentityDigest = testSHA256("different-ledger") },
		"configuration":        func(p *FoundryBootRetirementProof) { p.AgentConfigurationDigest = testSHA256("different-config") },
		"operation":            func(p *FoundryBootRetirementProof) { p.OperationID = "different-operation" },
		"context":              func(p *FoundryBootRetirementProof) { p.ContextSHA256 = testSHA256("different-request") },
		"retired fence":        func(p *FoundryBootRetirementProof) { p.RetiredFenceDigest = testSHA256("different-fence") },
		"state":                func(p *FoundryBootRetirementProof) { p.State = "open" },
		"seal":                 func(p *FoundryBootRetirementProof) { p.Sealed = false },
		"settlement":           func(p *FoundryBootRetirementProof) { p.SettlementProven = false },
		"retirement":           func(p *FoundryBootRetirementProof) { p.RetirementProven = false },
		"active invocation":    func(p *FoundryBootRetirementProof) { p.ActiveInvocations = 1 },
		"ambiguous invocation": func(p *FoundryBootRetirementProof) { p.AmbiguousInvocations = 1 },
		"pending create":       func(p *FoundryBootRetirementProof) { p.PendingCreates = 1 },
		"empty owner set":      func(p *FoundryBootRetirementProof) { p.OwnerSetDigest = testSHA256("null") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := proof
			mutate(&changed)
			changed.ProofDigest = changed.CanonicalProofDigest()
			if err := changed.ValidateFor(request); err == nil {
				t.Fatal("invalid retirement evidence was accepted")
			}
		})
	}
	proof.OwnerCount = 1
	proof.OwnerSetDigest = testSHA256("one owner")
	if err := proof.ValidateFor(request); err == nil {
		t.Fatal("owner set tampering did not invalidate the evidence digest")
	}
	proof.ProofDigest = proof.CanonicalProofDigest()
	if err := proof.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
}

func TestFoundryRetirementProofRequiresEveryWireField(t *testing.T) {
	proof := testFoundryRetirementProof(t, testFoundryRetirementRequest(t, testNow))
	body, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for key, original := range fields {
		for _, replacement := range []json.RawMessage{nil, json.RawMessage("null")} {
			t.Run(key+string(replacement), func(t *testing.T) {
				if replacement == nil {
					delete(fields, key)
				} else {
					fields[key] = replacement
				}
				bad, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				var decoded FoundryBootRetirementProof
				if err := json.Unmarshal(bad, &decoded); err == nil {
					t.Fatal("missing or null proof field accepted")
				}
				fields[key] = original
			})
		}
	}
	for _, extra := range []string{`,"activeInvocations":0}`, `,"extra":true}`} {
		var decoded FoundryBootRetirementProof
		if err := json.Unmarshal(append(append([]byte(nil), body[:len(body)-1]...), []byte(extra)...), &decoded); err == nil {
			t.Fatal("duplicate or unknown proof field accepted")
		}
	}
}

func TestFoundryRetirementClientNeverRetriesMutations(t *testing.T) {
	for _, succeeds := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "successful"}[succeeds], func(t *testing.T) {
			request := testFoundryRetirementRequest(t, time.Now().UTC())
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPut || r.URL.Path != FoundryBootRetirementPath || r.Header.Get("Authorization") != "Bearer "+clientTestBearer {
					t.Error("unexpected recovery transport or authentication")
				}
				if err := VerifyOperationCapability(clientTestCapabilitySecret, r.Header.Get(OperationCapabilityHeader), request.Metadata, false, time.Now().UTC()); err != nil {
					t.Error("missing exact operation capability")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(body), `"retiredFence"`) {
					t.Error("retirement subject missing from request")
				}
				if !succeeds {
					writeClientTestJSON(w, http.StatusConflict, ErrorResponse{Protocol: ProtocolVersion, Code: ErrorCodeCleanupUnproven, Message: "retirement pending", Retryable: true})
					return
				}
				writeClientTestJSON(w, http.StatusOK, FoundryBootRetirementResponse{Protocol: ProtocolVersion,
					Classification: Classification{Class: RequestClassificationFresh}, RelayFence: request.Metadata.Fence,
					RequestDigest: request.Metadata.RequestDigest, Proof: testFoundryRetirementProof(t, request)})
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithControllerBearerToken(clientTestBearer), WithOperationCapabilitySecret(clientTestCapabilitySecret))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.RetireFoundryBoot(t.Context(), request)
			if (err == nil) != succeeds || calls.Load() != 1 {
				t.Fatalf("retirement success=%v error=%v calls=%d", succeeds, err, calls.Load())
			}
			if !succeeds {
				var clientErr *ClientError
				if !errors.As(err, &clientErr) || clientErr.Kind != ClientErrorHTTP ||
					clientErr.StatusCode != http.StatusConflict || clientErr.Code != ErrorCodeCleanupUnproven || !clientErr.Retryable {
					t.Fatalf("pending retirement lost its retryable cleanup classification: %#v", err)
				}
			}
		})
	}
}

func TestFoundryStatusPreservesCleanupUnproven(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/status" {
			t.Error("unexpected status request")
		}
		writeClientTestJSON(w, http.StatusConflict, ErrorResponse{
			Protocol: ProtocolVersion, Code: ErrorCodeCleanupUnproven, Message: "broker identity unavailable", Retryable: true,
		})
	}))
	defer server.Close()
	_, err := clientTestClient(t, server.URL).Status(t.Context())
	var clientErr *ClientError
	if !errors.As(err, &clientErr) || clientErr.Kind != ClientErrorHTTP ||
		clientErr.StatusCode != http.StatusConflict || clientErr.Code != ErrorCodeCleanupUnproven || !clientErr.Retryable {
		t.Fatalf("status lost its retryable cleanup classification: %#v", err)
	}
}

func TestFoundryRetirementProofSharedBrokerVector(t *testing.T) {
	proof := FoundryBootRetirementProof{Protocol: FoundryBrokerProtocol,
		LedgerIdentityDigest: "sha256:" + strings.Repeat("1", 64), AgentConfigurationDigest: "sha256:" + strings.Repeat("2", 64),
		RetiredFenceDigest: "sha256:" + strings.Repeat("3", 64), OwnerSetDigest: "sha256:" + strings.Repeat("4", 64),
		State: "retired", Sealed: true, SettlementProven: true, RetirementProven: true, OwnerCount: 2}
	if got := proof.CanonicalProofDigest(); got != "sha256:f2dc20e1892c36df3942420dbbdce832b9aedc520139af8feefb13486b9f1dbb" {
		t.Fatalf("public proof digest disagrees with broker vector: %s", got)
	}
}
