package referenceadapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func postDelivery(t *testing.T, adapter *Server, delivery protocol.DeliveryRequest) protocol.DeliveryResponse {
	t.Helper()
	body, err := json.Marshal(delivery)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	adapter.Handler().ServeHTTP(recorder, req)
	var response protocol.DeliveryResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func messageDelivery(id, kind string) protocol.DeliveryRequest {
	return protocol.DeliveryRequest{ProtocolVersion: protocol.Version, DeliveryID: id, IdempotencyID: id,
		OriginatingEvent: "event", AccountID: "account", ContextID: "room", ReplyTarget: "reply", Kind: kind, Text: "safe fixture text"}
}

func TestMessagesOrderedAndReplayAfterTerminal(t *testing.T) {
	for _, terminal := range []string{protocol.DeliveryKindFinal, protocol.DeliveryKindError} {
		t.Run(terminal, func(t *testing.T) {
			adapter := New("test-token")
			first := messageDelivery("message-1", protocol.DeliveryKindMessage)
			receipt := postDelivery(t, adapter, first)
			if receipt.Status != protocol.DeliveryStatusDelivered {
				t.Fatalf("message rejected: %+v", receipt)
			}
			for _, delivery := range []protocol.DeliveryRequest{first, messageDelivery("message-2", protocol.DeliveryKindMessage), messageDelivery("terminal", terminal)} {
				if got := postDelivery(t, adapter, delivery); got.Status != protocol.DeliveryStatusDelivered {
					t.Fatalf("delivery rejected: %+v", got)
				}
			}
			if got := postDelivery(t, adapter, first); got != receipt {
				t.Fatalf("replay changed receipt: %+v", got)
			}
			if got := postDelivery(t, adapter, messageDelivery("late", protocol.DeliveryKindMessage)); got.Status != protocol.DeliveryStatusNonRetryableError {
				t.Fatalf("late new message accepted: %+v", got)
			}
			sends := adapter.Deliveries()
			if len(sends) != 3 || sends[0].DeliveryID != "message-1" || sends[1].DeliveryID != "message-2" || sends[2].Kind != terminal {
				t.Fatalf("provider order/dedupe: %+v", sends)
			}
		})
	}
}

func TestInterimAdvertisementAndOptOut(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		adapter := New("test-token", WithInterimDelivery(enabled))
		req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		recorder := httptest.NewRecorder()
		adapter.Handler().ServeHTTP(recorder, req)
		caps, err := protocol.DecodeCapabilities(recorder.Body.Bytes())
		if err != nil || caps.Capabilities.InterimDelivery != enabled {
			t.Fatalf("capabilities: %v %s", err, recorder.Body)
		}
		if !enabled && bytes.Contains(recorder.Body.Bytes(), []byte("interimDelivery")) {
			t.Fatal("opt-out must omit capability for strict older controllers")
		}
		response := postDelivery(t, adapter, messageDelivery("message", protocol.DeliveryKindMessage))
		if (response.Status == protocol.DeliveryStatusDelivered) != enabled {
			t.Fatalf("capability gate: %+v", response)
		}
	}
}

func TestReferenceAdapterBoundsRetainedIdentitiesWithoutEvictingReceipts(t *testing.T) {
	adapter := New("test-token")
	first := messageDelivery("first", protocol.DeliveryKindFinal)
	receipt := postDelivery(t, adapter, first)
	for i := 1; i < maxRetainedDeliveries; i++ {
		delivery := messageDelivery(fmt.Sprint(i), protocol.DeliveryKindFinal)
		delivery.Metadata = map[string]string{"fixture": "permanent"}
		postDelivery(t, adapter, delivery)
	}
	if got := postDelivery(t, adapter, messageDelivery("overflow", protocol.DeliveryKindFinal)); got.Status != protocol.DeliveryStatusNonRetryableError {
		t.Fatal("capacity overflow accepted")
	}
	if got := postDelivery(t, adapter, first); got != receipt {
		t.Fatal("capacity evicted receipt")
	}
	if len(adapter.attempts) != maxRetainedDeliveries || len(adapter.Deliveries()) != 1 {
		t.Fatal("retained storage exceeded bound")
	}
}
