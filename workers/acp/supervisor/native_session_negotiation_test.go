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

func TestNativeSessionCapabilitiesNegotiatedForStrictOlderClients(t *testing.T) {
	server, _, _ := newTestServer(t, "immediate")
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
			var old struct {
				harnessv2.CapabilitiesResponse
				Limits legacyNativeProtocolLimits `json:"limits"`
			}
			decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
			decoder.DisallowUnknownFields()
			err := decoder.Decode(&old)
			if header == "1" {
				require.Error(t, err, "strict old shape must reject negotiated-only new field")
			} else {
				require.NoError(t, err, "older controller must retain strict capability decoding")
				require.NotContains(t, response.Body.String(), "maxNativeSessionBytes")
			}
			var current harnessv2.CapabilitiesResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &current))
			if header == "1" {
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
