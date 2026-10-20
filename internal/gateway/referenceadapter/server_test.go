package referenceadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/conformance"
	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func TestLegacyReferenceAdapterPassesWithoutAnyMessageProbe(t *testing.T) {
	adapter := New("test-token", WithInterimDelivery(false))
	handler := adapter.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/deliveries" {
			body, _ := io.ReadAll(r.Body)
			var delivery protocol.DeliveryRequest
			if err := json.Unmarshal(body, &delivery); err != nil {
				t.Error(err)
			}
			if delivery.Kind == protocol.DeliveryKindMessage {
				t.Error("non-capable adapter received a message probe")
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL: server.URL, AuthorizationValue: "test-token", HTTPClient: server.Client(), ReferenceFixtures: true,
	})
	if !result.Passed || len(adapter.Deliveries()) != 1 || adapter.Deliveries()[0].Kind != protocol.DeliveryKindFinal {
		t.Fatalf("legacy adapter conformance: %+v", result)
	}
}

func TestReferenceAdapterPassesConformance(t *testing.T) {
	adapter := New("secret-token")
	server := httptest.NewServer(adapter.Handler())
	defer server.Close()

	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL: server.URL, AuthorizationValue: "secret-token", HTTPClient: server.Client(), ReferenceFixtures: true,
	})
	if !result.Passed {
		t.Fatalf("conformance failed: %s", result.Message)
	}
	sends := adapter.Deliveries()
	if len(sends) != 3 || sends[0].Kind != "message" || sends[1].Kind != "message" || sends[2].Kind != "final" {
		t.Fatalf("successful provider sends out of order: %+v", sends)
	}
	if got := adapter.Attempts("conformance-message-1"); got != 3 {
		t.Fatalf("interim replay attempts = %d, want 3", got)
	}
	if got := adapter.Attempts("conformance-idempotency"); got != 2 {
		t.Fatalf("idempotency attempts = %d, want 2", got)
	}
}
