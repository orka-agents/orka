package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/pkg/oms/protocol"
)

func TestProbeAcceptsReferenceCompatibleShortBearerToken(t *testing.T) {
	shortValue := strings.Repeat("a", 1)
	binding := DefaultBinding()
	healthBody := encodeTestJSON(t, protocol.HealthResponse{ProtocolVersion: protocol.Version, Status: "ok"})
	capabilityBody := encodeTestJSON(t, testCapabilitiesResponse(binding))

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", jsonMediaType)
		writer.Header().Set("Cache-Control", "no-store")
		if request.Header.Get("Authorization") != "Bearer "+shortValue {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case protocol.PathHealth:
			_, _ = writer.Write(healthBody)
		case protocol.PathCapabilities:
			_, _ = writer.Write(capabilityBody)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	client, err := newContractClient(Target{
		BaseURL:              server.URL,
		AuthorizationValue:   shortValue,
		HTTPClient:           server.Client(),
		InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.rawBearerToken != shortValue || client.authorizationValue != "Bearer "+shortValue {
		t.Fatalf("credentials = raw %q authorization %q", client.rawBearerToken, client.authorizationValue)
	}
	if _, err := probe(context.Background(), client, binding); err != nil {
		t.Fatalf("probe() rejected reference-compatible short bearer token: %v", err)
	}
}

func TestContainsCredentialDetectsCompleteShortBearerValue(t *testing.T) {
	shortValue := strings.Repeat("a", 1)
	complete := "Bearer " + shortValue
	if !containsCredential([]byte(`{"message":"`+complete+`"}`), complete) {
		t.Fatal("complete Authorization value leak was not detected for a short bearer token")
	}
}
