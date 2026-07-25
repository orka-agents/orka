package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func TestProbeRejectsPublicHealthAndCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(protocol.HealthResponse{Status: "ok"})
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(protocol.CapabilitiesResponse{
				ProtocolVersion: protocol.Version, AdapterName: "public", AdapterVersion: "v1",
				Capabilities: protocol.Capabilities{IdempotentDelivery: true},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result := Probe(context.Background(), Target{
		BaseURL: server.URL, AuthorizationValue: "auth-value", HTTPClient: server.Client(),
	})
	if result.Passed {
		t.Fatal("Probe() accepted public health/capability endpoints")
	}
}

func TestCheckValidatesDeliverySizeBounds(t *testing.T) {
	const fixtureValue = "size-probe-sentinel"
	var sawOversizedText, sawOversizedBody bool
	responses := map[string]protocol.DeliveryResponse{}
	server := httptest.NewServer(testAdapterHandler(fixtureValue, defaultCapabilities(), func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		if len(body) > protocol.MaxHTTPBodyBytes {
			sawOversizedBody = true
			writeTestJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
			return
		}
		var delivery protocol.DeliveryRequest
		if err := json.Unmarshal(body, &delivery); err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if len(delivery.Text) > protocol.MaxTextBytes {
			sawOversizedText = true
			writeTestJSON(w, http.StatusUnprocessableEntity, protocol.DeliveryResponse{
				Status: protocol.DeliveryStatusNonRetryableError,
			})
			return
		}
		response, ok := responses[delivery.DeliveryID]
		if !ok {
			response = protocol.DeliveryResponse{
				Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "provider:" + delivery.DeliveryID,
			}
			responses[delivery.DeliveryID] = response
		}
		writeTestJSON(w, http.StatusOK, response)
	}))
	defer server.Close()

	result := Check(context.Background(), Target{
		BaseURL: server.URL, AuthorizationValue: fixtureValue, HTTPClient: server.Client(),
	})
	if !result.Passed {
		t.Fatalf("Check() failed: %s", result.Message)
	}
	if !sawOversizedText || !sawOversizedBody {
		t.Fatalf("size probes seen = text:%t body:%t, want both", sawOversizedText, sawOversizedBody)
	}
}

func TestCheckRejectsAdapterThatAcceptsOversizedText(t *testing.T) {
	const fixtureValue = "accepting-adapter-sentinel"
	server := httptest.NewServer(testAdapterHandler(fixtureValue, defaultCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{
			Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "provider-message",
		})
	}))
	defer server.Close()

	result := Check(context.Background(), Target{
		BaseURL: server.URL, AuthorizationValue: fixtureValue, HTTPClient: server.Client(),
	})
	if result.Passed {
		t.Fatal("Check() accepted an adapter that delivered oversized text")
	}
	if !strings.Contains(result.Message, "accepted oversized text") {
		t.Fatalf("Check() message = %q, want oversized-text failure", result.Message)
	}
}

func TestProbeRejectsOversizedAdapterResponse(t *testing.T) {
	const fixtureValue = "response-size-sentinel"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fixtureValue {
			writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), protocol.MaxAdapterResponseBytes+1))
	}))
	defer server.Close()

	result := Probe(context.Background(), Target{
		BaseURL: server.URL, AuthorizationValue: fixtureValue, HTTPClient: server.Client(),
	})
	if result.Passed {
		t.Fatal("Probe() accepted an oversized adapter response")
	}
	if !strings.Contains(result.Message, "response exceeded size limit") {
		t.Fatalf("Probe() message = %q, want response-size failure", result.Message)
	}
	if len(result.Message) > conformanceMessageLimit {
		t.Fatalf("Probe() message length = %d, want <= %d", len(result.Message), conformanceMessageLimit)
	}
}

func TestProbeRejectsAndRedactsCredentialInCapabilities(t *testing.T) {
	const fixtureValue = "capability-redaction-sentinel"
	capabilities := defaultCapabilities()
	capabilities.AdapterName = "adapter-" + fixtureValue
	server := httptest.NewServer(testAdapterHandler(fixtureValue, capabilities, nil))
	defer server.Close()

	result := Probe(context.Background(), Target{
		BaseURL: server.URL, AuthorizationValue: fixtureValue, HTTPClient: server.Client(),
	})
	if result.Passed {
		t.Fatal("Probe() accepted a capability response containing its credential")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte(fixtureValue)) {
		t.Fatalf("Probe() output leaked credential: %s", encoded)
	}
}

func TestCheckRejectsAndRedactsCredentialInDeliveryResponse(t *testing.T) {
	const fixtureValue = "delivery-redaction-sentinel"
	server := httptest.NewServer(testAdapterHandler(fixtureValue, defaultCapabilities(), func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) > protocol.MaxHTTPBodyBytes {
			writeTestJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
			return
		}
		var delivery protocol.DeliveryRequest
		if json.Unmarshal(body, &delivery) != nil || len(delivery.Text) > protocol.MaxTextBytes {
			writeTestJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{
				Status: protocol.DeliveryStatusNonRetryableError,
			})
			return
		}
		writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{
			Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "provider-" + fixtureValue,
		})
	}))
	defer server.Close()

	result := Check(context.Background(), Target{
		BaseURL: server.URL, AuthorizationValue: fixtureValue, HTTPClient: server.Client(),
	})
	if result.Passed {
		t.Fatal("Check() accepted a delivery response containing its credential")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte(fixtureValue)) {
		t.Fatalf("Check() output leaked credential: %s", encoded)
	}
}

func TestSanitizeCheckResultCopiesAndRedactsAllOutputFields(t *testing.T) {
	const fixtureValue = "output-redaction-sentinel"
	capabilities := &protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version,
		AdapterName:     "adapter-" + fixtureValue,
		AdapterVersion:  fixtureValue,
	}
	original := CheckResult{
		Message:      "Authorization: Bearer " + fixtureValue,
		Capabilities: capabilities,
	}
	got := SanitizeCheckResult(original, fixtureValue)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte(fixtureValue)) {
		t.Fatalf("SanitizeCheckResult() leaked credential: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(redactedValue)) {
		t.Fatalf("SanitizeCheckResult() output = %s, want redaction marker", encoded)
	}
	if capabilities.AdapterName != "adapter-"+fixtureValue || capabilities.AdapterVersion != fixtureValue {
		t.Fatal("SanitizeCheckResult() mutated caller-owned capabilities")
	}
}

func defaultCapabilities() protocol.CapabilitiesResponse {
	return protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version,
		AdapterName:     "test-adapter",
		AdapterVersion:  "v1",
		Capabilities: protocol.Capabilities{
			InboundText: true, OutboundText: true, IdempotentDelivery: true,
		},
	}
}

func testAdapterHandler(
	token string,
	capabilities protocol.CapabilitiesResponse,
	deliver http.HandlerFunc,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.URL.Path {
		case "/v1/health":
			writeTestJSON(w, http.StatusOK, protocol.HealthResponse{Status: "ok"})
		case "/v1/capabilities":
			writeTestJSON(w, http.StatusOK, capabilities)
		case "/v1/deliveries":
			if deliver == nil {
				http.NotFound(w, r)
				return
			}
			deliver(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
