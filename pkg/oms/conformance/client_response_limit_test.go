package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const testResponseAuthorizationValue = "test-value"

func TestContractClientEnforcesAdvertisedMaxResponseBytes(t *testing.T) {
	responseBody := []byte(`{"status":"ok"}`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", jsonMediaType)
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(server.Close)
	client, err := newContractClient(Target{
		BaseURL:              server.URL,
		AuthorizationValue:   testResponseAuthorizationValue,
		HTTPClient:           server.Client(),
		InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	limits := testCapabilityLimits()
	limits.MaxResponseBytes = len(responseBody)
	client.configureLimits(limits)
	if _, err := client.doResponse(
		context.Background(), http.MethodGet, "/v1/test", client.authorizationValue, nil, nil,
	); err != nil {
		t.Fatalf("response at advertised bound failed: %v", err)
	}

	limits.MaxResponseBytes--
	client.configureLimits(limits)
	if _, err := client.doResponse(
		context.Background(), http.MethodGet, "/v1/test", client.authorizationValue, nil, nil,
	); err == nil {
		t.Fatal("response above advertised maxResponseBytes was accepted")
	} else if !strings.Contains(err.Error(), "maxResponseBytes") {
		t.Fatalf("response bound error = %q, want maxResponseBytes context", err)
	}
}

func TestContractClientEnforcesHardMaxResponseBytes(t *testing.T) {
	responseBody := bytes.Repeat([]byte("x"), protocol.MaxAdapterResponseBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", jsonMediaType)
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(responseBody)
	}))
	t.Cleanup(server.Close)
	client, err := newContractClient(Target{
		BaseURL:              server.URL,
		AuthorizationValue:   testResponseAuthorizationValue,
		HTTPClient:           server.Client(),
		InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.doResponse(
		context.Background(), http.MethodGet, "/v1/test", client.authorizationValue, nil, nil,
	); err == nil {
		t.Fatal("response above the hard maxResponseBytes was accepted")
	} else if !strings.Contains(err.Error(), "hard maxResponseBytes") {
		t.Fatalf("response bound error = %q, want hard maxResponseBytes context", err)
	}
}

func TestProbeRetroactivelyEnforcesAdvertisedMaxResponseBytes(t *testing.T) {
	binding := DefaultBinding()
	healthBody := encodeTestJSON(t, protocol.HealthResponse{ProtocolVersion: protocol.Version, Status: "ok"})

	for _, tc := range []struct {
		name              string
		maxResponseBytes  int
		prepareHealthBody func([]byte, int) []byte
		wantPath          string
	}{
		{
			name:             "capability response",
			maxResponseBytes: len(healthBody),
			prepareHealthBody: func(body []byte, _ int) []byte {
				return body
			},
			wantPath: protocol.PathCapabilities,
		},
		{
			name:             "health response",
			maxResponseBytes: 2048,
			prepareHealthBody: func(body []byte, limit int) []byte {
				return append(body, bytes.Repeat([]byte(" "), limit-len(body)+1)...)
			},
			wantPath: protocol.PathHealth,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capabilities := testCapabilitiesResponse(binding)
			capabilities.Limits.MaxResponseBytes = tc.maxResponseBytes
			capabilityBody := encodeTestJSON(t, capabilities)
			servedHealthBody := tc.prepareHealthBody(append([]byte(nil), healthBody...), tc.maxResponseBytes)
			if tc.wantPath == protocol.PathCapabilities && len(capabilityBody) <= tc.maxResponseBytes {
				t.Fatalf("capability fixture length = %d, want > %d", len(capabilityBody), tc.maxResponseBytes)
			}
			if tc.wantPath == protocol.PathHealth && len(capabilityBody) > tc.maxResponseBytes {
				t.Fatalf("capability fixture length = %d, want <= %d", len(capabilityBody), tc.maxResponseBytes)
			}

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", jsonMediaType)
				writer.Header().Set("Cache-Control", "no-store")
				switch request.URL.Path {
				case protocol.PathHealth:
					_, _ = writer.Write(servedHealthBody)
				case protocol.PathCapabilities:
					_, _ = writer.Write(capabilityBody)
				default:
					http.NotFound(writer, request)
				}
			}))
			t.Cleanup(server.Close)
			client, err := newContractClient(Target{
				BaseURL:              server.URL,
				AuthorizationValue:   testResponseAuthorizationValue,
				HTTPClient:           server.Client(),
				InsecureLoopbackOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = probe(context.Background(), client, binding)
			if err == nil {
				t.Fatalf("probe() accepted oversized %s", tc.name)
			}
			for _, want := range []string{tc.wantPath, "maxResponseBytes"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("probe() error = %q, want %q", err, want)
				}
			}
		})
	}
}

func testCapabilitiesResponse(binding protocol.Binding) protocol.CapabilitiesResponse {
	return protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version,
		Binding:         binding,
		AdapterName:     "adapter",
		AdapterVersion:  "v1",
		Revision:        "revision-1",
		ExpiresAt:       time.Now().Add(time.Minute),
		Capabilities: protocol.Capabilities{
			DurableIdempotency:         true,
			IdempotencyDigestConflicts: true,
			CreateIfAbsent:             true,
			ConditionalMutation:        true,
			MonotonicGenerations:       true,
			DeleteHighWatermark:        true,
			DurableRoutingFence:        true,
			OperationLookup:            true,
			ExactGet:                   true,
			StablePagination:           true,
			ExclusiveOwnership:         true,
			KeywordSearch:              true,
			AuditVersionVisibility:     true,
		},
		Limits: testCapabilityLimits(),
	}
}

func encodeTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}
