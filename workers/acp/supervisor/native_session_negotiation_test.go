package supervisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

// This is the pre-native-limit nested wire shape. Strict decoding must continue
// to work for an old controller, without relaxing any unknown-field checks.
type legacyNativeProtocolLimits struct {
	MaxResidentSessions      uint32 `json:"maxResidentSessions"`
	MaxConcurrentPrompts     uint32 `json:"maxConcurrentPrompts"`
	MaxRequestBytes          int    `json:"maxRequestBytes"`
	MaxEventLineBytes        int    `json:"maxEventLineBytes"`
	MaxTerminalResultBytes   int    `json:"maxTerminalResultBytes"`
	MaxBufferedEvents        int    `json:"maxBufferedEvents"`
	MaxUpdateEventsPerSecond int    `json:"maxUpdateEventsPerSecond"`
	MinPromptLeaseMillis     int64  `json:"minPromptLeaseMillis"`
	MaxPromptLeaseMillis     int64  `json:"maxPromptLeaseMillis"`
	MaxPendingPermissions    uint32 `json:"maxPendingPermissions"`
	MaxWorkspaceDeltaBytes   int64  `json:"maxWorkspaceDeltaBytes"`
}

// Freeze the old top-level wire shape as well as its nested limits. Embedding
// the current response would accidentally accept newly added capability fields.
type legacyNativeCapabilities struct {
	Protocol                          string                                    `json:"protocol"`
	Transport                         string                                    `json:"transport"`
	ACPVersion                        string                                    `json:"acpVersion"`
	RuntimeProfileDigest              harnessv2.ProfileDigest                   `json:"runtimeProfileDigest"`
	ProfileDigestSchemaVersion        uint32                                    `json:"profileDigestSchemaVersion"`
	AdapterDigests                    map[string]string                         `json:"adapterDigests"`
	Limits                            legacyNativeProtocolLimits                `json:"limits"`
	Provider                          harnessv2.ProviderCapabilities            `json:"provider"`
	WorkspaceGovernance               harnessv2.WorkspaceGovernanceCapabilities `json:"workspaceGovernance"`
	SupportsDrain                     bool                                      `json:"supportsDrain"`
	SupportsPublicationFinalization   bool                                      `json:"supportsPublicationFinalization"`
	SupportsAgentSessionConfiguration bool                                      `json:"supportsAgentSessionConfiguration,omitempty"`
	SupportsFoundryRecovery           bool                                      `json:"supportsFoundryRecovery,omitempty"`
}

func TestNativeSessionCapabilitiesNegotiatedForStrictOlderClients(t *testing.T) {
	server, _, _ := newTestServer(t, "immediate")
	server.cfg.Capabilities.SupportsNativeSessions = true
	for _, header := range []string{"", "unsupported", "1"} {
		t.Run("header="+header, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, harnessv2.CapabilitiesPath, nil)
			if header != "" {
				request.Header.Set(harnessv2.NativeSessionLimitsHeader, header)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, harnessv2.NativeSessionLimitsHeader, response.Header().Get("Vary"))
			var old legacyNativeCapabilities
			decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
			decoder.DisallowUnknownFields()
			err := decoder.Decode(&old)
			if header == "1" {
				require.Error(t, err, "strict old shape must reject negotiated-only new field")
			} else {
				require.NoError(t, err, "older controller must retain strict capability decoding")
				require.NotContains(t, response.Body.String(), "maxNativeSessionBytes")
				require.NotContains(t, response.Body.String(), "supportsNativeSessions")
			}
			var current harnessv2.CapabilitiesResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &current))
			if header == "1" {
				require.True(t, current.SupportsNativeSessions)
				require.Equal(t, harnessv2.DefaultMaxNativeSessionBytes, current.Limits.EffectiveMaxNativeSessionBytes())
			} else {
				require.Equal(t, harnessv2.LegacyMaxNativeSessionBytes, current.Limits.EffectiveMaxNativeSessionBytes())
			}
		})
	}
	endpoint := httptest.NewServer(server.Handler())
	defer endpoint.Close()
	client, err := harnessv2.NewClient(endpoint.URL)
	require.NoError(t, err)
	capabilities, err := client.Capabilities(t.Context())
	require.NoError(t, err)
	require.Equal(t, harnessv2.DefaultMaxNativeSessionBytes, capabilities.Limits.EffectiveMaxNativeSessionBytes(), "new client must request negotiated capability")
	require.True(t, capabilities.SupportsNativeSessions)
	require.True(t, server.cfg.Capabilities.SupportsNativeSessions, "legacy reads must not mutate the configured capability")
}

func TestNativeCaptureReplayPreservesReceiptAcrossCallerLimits(t *testing.T) {
	server := &Server{cfg: Config{Capabilities: harnessv2.CapabilitiesResponse{Limits: harnessv2.ProtocolLimits{MaxNativeSessionBytes: 8 << 20}}}}
	capture := &nativeSessionCapture{finished: true, snapshot: harnessv2.NativeSessionSnapshot{Data: bytes.Repeat([]byte("x"), 1<<20)}}
	classification := harnessv2.Classification{Class: harnessv2.RequestClassificationDuplicate, Phase: harnessv2.OperationPhaseApplied}
	for _, limit := range []int{harnessv2.LegacyMaxNativeSessionBytes, 8 << 20} {
		response := httptest.NewRecorder()
		server.writeNativeCapture(response, capture, classification, limit)
		if limit == harnessv2.LegacyMaxNativeSessionBytes {
			require.Equal(t, http.StatusUnprocessableEntity, response.Code)
			var failure harnessv2.ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
			require.Equal(t, harnessv2.ErrorCodeNativeCaptureUnsupported, failure.Code)
		} else {
			require.Equal(t, http.StatusOK, response.Code)
			var actual harnessv2.CaptureNativeSessionResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &actual))
			require.Equal(t, capture.snapshot.Data, actual.Snapshot.Data)
		}
		require.Len(t, capture.snapshot.Data, 1<<20, "reader limit must not rewrite original receipt")
		require.Empty(t, capture.failure)
	}
	legacy := httptest.NewRequest(http.MethodPost, "/native-session", nil)
	require.Equal(t, harnessv2.LegacyMaxNativeSessionBytes, server.nativeCaptureLimit(legacy))
	legacy.Header.Set(harnessv2.NativeSessionLimitsHeader, "1")
	require.Equal(t, 8<<20, server.nativeCaptureLimit(legacy))
}
