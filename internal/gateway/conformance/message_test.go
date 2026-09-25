package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func TestCheckMessagesOnlyWhenCapable(t *testing.T) {
	for _, capable := range []bool{false, true} {
		for _, private := range []bool{false, true} {
			t.Run(map[bool]string{false: "legacy", true: "capable"}[capable]+map[bool]string{false: "/default", true: "/private"}[private], func(t *testing.T) {
				caps := defaultCapabilities()
				caps.Capabilities.InterimDelivery = capable
				fixture := testDeliveryFixture()
				var valid []protocol.DeliveryRequest
				var messageAttempts, sizeProbes int
				server := httptest.NewServer(testAdapterHandler("test-auth", caps, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					var delivery protocol.DeliveryRequest
					_ = json.Unmarshal(body, &delivery)
					if delivery.Kind == protocol.DeliveryKindMessage {
						messageAttempts++
					}
					if len(body) > protocol.MaxHTTPBodyBytes || protocol.ValidateDeliveryRequest(&delivery) != nil {
						if delivery.Kind == protocol.DeliveryKindMessage && len(delivery.Text) == protocol.MaxInterimTextBytes+1 {
							sizeProbes++
						}
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					valid = append(valid, delivery)
					writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "provider:" + delivery.DeliveryID})
				}))
				defer server.Close()
				target := Target{BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: server.Client()}
				if private {
					target.DeliveryFixture = &fixture
				}
				result := Check(context.Background(), target)
				if !result.Passed {
					t.Fatalf("check failed: %s", result.Message)
				}
				if !capable {
					if messageAttempts != 0 || len(valid) != 2 {
						t.Fatalf("noncapable sends: %d messages, %d valid", messageAttempts, len(valid))
					}
					return
				}
				var kinds []string
				for _, delivery := range valid {
					kinds = append(kinds, delivery.Kind)
					if private && (delivery.AccountID != fixture.AccountID || delivery.ContextID != fixture.ContextID || delivery.ThreadID != fixture.ThreadID || delivery.ReplyTarget != fixture.ReplyTarget || delivery.OriginatingEvent != fixture.OriginatingEventID || delivery.Text != "[Orka conformance check] No action required.") {
						t.Fatal("private fixture route/text changed")
					}
				}
				if !reflect.DeepEqual(kinds, []string{"message", "message", "message", "final", "final", "message"}) || sizeProbes != 1 {
					t.Fatalf("sequence=%v size probes=%d", kinds, sizeProbes)
				}
				if !reflect.DeepEqual(valid[0], valid[1]) || !reflect.DeepEqual(valid[0], valid[5]) || valid[0].DeliveryID == valid[2].DeliveryID || valid[2].OriginatingEvent != valid[3].OriginatingEvent {
					t.Fatal("interim replay/order identity changed")
				}
			})
		}
	}
}

func TestCheckInterimDeliveryDeduplication(t *testing.T) {
	for _, tt := range []struct {
		name             string
		dedupeByEvent    bool
		omitProviderID   bool
		wantPassed       bool
		wantInterimSends int
	}{
		{"delivery ID", false, false, true, 2},
		{"event and kind", true, false, false, 1},
		{"optional provider ID", false, true, true, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			caps := defaultCapabilities()
			caps.Capabilities.InterimDelivery = true
			receipts := make(map[string]protocol.DeliveryResponse)
			interimSends := 0
			server := httptest.NewServer(testAdapterHandler("test-auth", caps, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var delivery protocol.DeliveryRequest
				if len(body) > protocol.MaxHTTPBodyBytes || json.Unmarshal(body, &delivery) != nil || protocol.ValidateDeliveryRequest(&delivery) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				key := delivery.DeliveryID
				if tt.dedupeByEvent {
					key = delivery.OriginatingEvent + "/" + delivery.Kind
				}
				receipt, exists := receipts[key]
				if !exists {
					if delivery.Kind == protocol.DeliveryKindMessage {
						interimSends++
					}
					receipt = protocol.DeliveryResponse{Status: protocol.DeliveryStatusDelivered}
					if !tt.omitProviderID {
						receipt.ProviderMessageID = "provider:" + delivery.DeliveryID
					}
					receipts[key] = receipt
				}
				writeTestJSON(w, http.StatusOK, receipt)
			}))
			defer server.Close()
			result := Check(t.Context(), Target{BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: server.Client()})
			if result.Passed != tt.wantPassed {
				t.Fatalf("passed=%v, want %v: %s", result.Passed, tt.wantPassed, result.Message)
			}
			if interimSends != tt.wantInterimSends {
				t.Fatalf("interim sends=%d, want %d", interimSends, tt.wantInterimSends)
			}
			if !tt.wantPassed && !strings.Contains(result.Message, "distinct interim deliveries reused a provider message ID") {
				t.Fatalf("unexpected failure: %s", result.Message)
			}
		})
	}
}

func TestInterimReceiptsAllowChangingSafeDiagnostic(t *testing.T) {
	caps := defaultCapabilities()
	caps.Capabilities.InterimDelivery = true
	calls := 0
	server := httptest.NewServer(testAdapterHandler("test-auth", caps, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var delivery protocol.DeliveryRequest
		if json.Unmarshal(body, &delivery) != nil || len(body) > protocol.MaxHTTPBodyBytes || protocol.ValidateDeliveryRequest(&delivery) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls++
		writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{
			Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "provider:" + delivery.DeliveryID,
			Message: fmt.Sprintf("safe diagnostic %d", calls),
		})
	}))
	defer server.Close()
	result := Check(context.Background(), Target{BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: server.Client()})
	if !result.Passed {
		t.Fatalf("stable correlation rejected for changing diagnostic: %s", result.Message)
	}
}

func TestCheckRejectsInterimDuplicateCorrelationAndMasksPrivateErrors(t *testing.T) {
	for _, mode := range []string{"duplicate", "private error"} {
		t.Run(mode, func(t *testing.T) {
			caps := defaultCapabilities()
			caps.Capabilities.InterimDelivery = true
			fixture := testDeliveryFixture()
			messages := 0
			server := httptest.NewServer(testAdapterHandler("test-auth", caps, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var d protocol.DeliveryRequest
				_ = json.Unmarshal(body, &d)
				if len(body) > protocol.MaxHTTPBodyBytes || protocol.ValidateDeliveryRequest(&d) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if d.Kind == protocol.DeliveryKindMessage {
					messages++
					if mode == "private error" {
						writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusRetryableError, Message: fixture.ContextID})
						return
					}
				}
				provider := "provider:" + d.DeliveryID
				if messages == 2 {
					provider = "changed"
				}
				writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusDelivered, ProviderMessageID: provider})
			}))
			defer server.Close()
			result := Check(context.Background(), Target{BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: server.Client(), DeliveryFixture: &fixture})
			encoded, _ := json.Marshal(result)
			if result.Passed || messages == 0 || strings.Contains(string(encoded), fixture.ContextID) {
				t.Fatalf("interim failure/privacy not enforced: %s", encoded)
			}
		})
	}
}
