package referenceadapter

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/conformance"
)

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
	if got := len(adapter.Deliveries()); got != 1 {
		t.Fatalf("successful provider sends = %d, want 1", got)
	}
	if got := adapter.Attempts("conformance-idempotency"); got != 2 {
		t.Fatalf("idempotency attempts = %d, want 2", got)
	}
}
