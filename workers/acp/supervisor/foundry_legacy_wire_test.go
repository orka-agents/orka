package supervisor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// These top-level schemas match the approval-aware controller at 51d3360dd,
// before Foundry recovery fields were added. Its decoder rejects unknown fields.
type legacyFoundryCapabilities struct {
	Protocol                          string                                    `json:"protocol"`
	Transport                         string                                    `json:"transport"`
	ACPVersion                        string                                    `json:"acpVersion"`
	RuntimeProfileDigest              harnessv2.ProfileDigest                   `json:"runtimeProfileDigest"`
	ProfileDigestSchemaVersion        uint32                                    `json:"profileDigestSchemaVersion"`
	AdapterDigests                    map[string]string                         `json:"adapterDigests"`
	Limits                            harnessv2.ProtocolLimits                  `json:"limits"`
	Provider                          harnessv2.ProviderCapabilities            `json:"provider"`
	WorkspaceGovernance               harnessv2.WorkspaceGovernanceCapabilities `json:"workspaceGovernance"`
	SupportsDrain                     bool                                      `json:"supportsDrain"`
	SupportsPublicationFinalization   bool                                      `json:"supportsPublicationFinalization"`
	SupportsAgentSessionConfiguration bool                                      `json:"supportsAgentSessionConfiguration,omitempty"`
}

type legacyFoundryStatus struct {
	Protocol                string                              `json:"protocol"`
	Fence                   harnessv2.Fence                     `json:"fence"`
	Lifecycle               harnessv2.SupervisorLifecycle       `json:"lifecycle"`
	Drain                   harnessv2.DrainStatus               `json:"drain"`
	Sessions                []harnessv2.RuntimeSessionStatus    `json:"sessions"`
	ActivePrompts           []harnessv2.ActivePromptStatus      `json:"activePrompts"`
	PendingPermissions      []harnessv2.PendingPermissionStatus `json:"pendingPermissions"`
	Pressure                harnessv2.PressureMetadata          `json:"pressure"`
	SessionIdentityCapacity *harnessv2.SessionIdentityCapacity  `json:"sessionIdentityCapacity,omitempty"`
	Timestamp               time.Time                           `json:"timestamp"`
}

func foundryStatusRequest(t *testing.T, cfg Config) *http.Request {
	t.Helper()
	nonce, err := harnessv2.NewCapabilityNonce()
	if err != nil {
		t.Fatal(err)
	}
	binding := harnessv2.StatusCapabilityBinding{
		RuntimeProfileDigest: cfg.Fence.RuntimeProfileDigest, RuntimeInstanceID: cfg.Fence.RuntimeInstanceID,
	}
	capability, err := harnessv2.SignStatusCapability(cfg.CapabilitySecret,
		harnessv2.NewStatusCapabilityClaims(binding, nonce, time.Now().UTC().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, harnessv2.StatusPath, nil)
	request.Header.Set("Authorization", "Bearer "+cfg.ControllerBearerToken)
	request.Header.Set(OperationCapabilityHeader, capability)
	return request
}

func TestFoundryWithoutRecoveryPreservesLegacyWireContract(t *testing.T) {
	for _, approvals := range []bool{false, true} {
		for _, upgradedBroker := range []bool{false, true} {
			var calls atomic.Int32
			server, cfg, _ := newFoundryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if upgradedBroker {
					writeJSON(w, http.StatusOK, foundryRecoveryIdentity())
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			server.cfg.Capabilities.Provider.SupportsBrokeredToolApprovals = approvals
			if server.cfg.Capabilities.SupportsFoundryRecovery {
				t.Fatal("Foundry provider identity implicitly enabled recovery")
			}
			capabilities := httptest.NewRecorder()
			server.Handler().ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, harnessv2.CapabilitiesPath, nil))
			var legacyCapabilities legacyFoundryCapabilities
			decoder := json.NewDecoder(capabilities.Body)
			decoder.DisallowUnknownFields()
			if capabilities.Code != http.StatusOK || decoder.Decode(&legacyCapabilities) != nil ||
				legacyCapabilities.Provider.SupportsBrokeredToolApprovals != approvals {
				t.Fatal("legacy controller cannot decode unchanged Foundry capabilities")
			}
			status := httptest.NewRecorder()
			server.Handler().ServeHTTP(status, foundryStatusRequest(t, cfg))
			var legacyStatus legacyFoundryStatus
			decoder = json.NewDecoder(status.Body)
			decoder.DisallowUnknownFields()
			if status.Code != http.StatusOK || decoder.Decode(&legacyStatus) != nil || legacyStatus.Fence != cfg.Fence {
				t.Fatal("legacy controller cannot decode unchanged Foundry status")
			}
			retirement := performMutation(t, server.Handler(), http.MethodPut, harnessv2.FoundryBootRetirementPath,
				foundryRecoveryRequest(t, cfg), cfg)
			if retirement.Code != http.StatusBadRequest || calls.Load() != 0 || len(server.foundryRecoveryOps) != 0 {
				t.Fatal("disabled recovery probed or mutated the broker")
			}
		}
	}
}

func TestFoundryQualifiedStatusFailsClosedWithoutIdentity(t *testing.T) {
	server, cfg, _ := newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	status := httptest.NewRecorder()
	server.Handler().ServeHTTP(status, foundryStatusRequest(t, cfg))
	var response harnessv2.ErrorResponse
	if status.Code != http.StatusConflict || json.Unmarshal(status.Body.Bytes(), &response) != nil ||
		response.Code != harnessv2.ErrorCodeCleanupUnproven || !response.Retryable {
		t.Fatal("qualified recovery did not reject missing broker identity with a retryable cleanup error")
	}
}

func TestFoundryRecoveryConfigRequiresProviderAndCapabilities(t *testing.T) {
	cfg, _ := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1/v1", testUpstreamToken)
	cfg.Capabilities.SupportsFoundryRecovery = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-Foundry provider advertised recovery")
	}
	_, cfg, _ = newFoundryRecoveryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	cfg.RequireCapabilities = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("Foundry recovery accepted disabled operation authorization")
	}
}
