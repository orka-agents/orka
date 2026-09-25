package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDecodeEventNormalizesAndValidates(t *testing.T) {
	event, err := DecodeEvent([]byte(`{
		"protocolVersion":"orka.gateway.v1",
		"externalEventId":" event-1 ",
		"eventType":"text",
		"accountId":"acct",
		"contextId":"room",
		"sender":{"id":"user-1"},
		"text":"hello",
		"metadata":{"trace":" safe "}
	}`))
	if err != nil {
		t.Fatalf("DecodeEvent() error = %v", err)
	}
	if event.ExternalEventID != "event-1" || event.Metadata["trace"] != "safe" {
		t.Fatalf("unexpected normalized event: %#v", event)
	}
}

func TestDecodeEventRejectsBoundsAndUnknownFields(t *testing.T) {
	base := `{"protocolVersion":"orka.gateway.v1","externalEventId":"e","eventType":"text","accountId":"a","contextId":"c","sender":{"id":"s"},"text":"x"}`
	if _, err := DecodeEvent([]byte(strings.TrimSuffix(base, "}") + `,"rawPayload":{}}`)); err == nil {
		t.Fatal("DecodeEvent() accepted unknown rawPayload")
	}
	if _, err := DecodeEvent([]byte(strings.Repeat("x", MaxHTTPBodyBytes+1))); err == nil {
		t.Fatal("DecodeEvent() accepted oversized body")
	}
}

func TestDecodeEventRejectsRawMetadataEntryOverflow(t *testing.T) {
	metadata := make(map[string]string, MaxMetadataEntries+1)
	for i := 0; i <= MaxMetadataEntries; i++ {
		metadata[strings.Repeat(" ", i)+"trace"] = "safe"
	}
	body, err := json.Marshal(EventEnvelope{
		ProtocolVersion: Version,
		ExternalEventID: "event",
		EventType:       EventTypeText,
		AccountID:       "account",
		ContextID:       "context",
		Sender:          Sender{ID: "sender"},
		Text:            "hello",
		Metadata:        metadata,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	wantError := fmt.Sprintf("metadata exceeds %d entries", MaxMetadataEntries)
	if _, err := DecodeEvent(body); err == nil || !strings.Contains(err.Error(), wantError) {
		t.Fatalf("DecodeEvent() = %v, want raw metadata entry bound error", err)
	}
}

func TestDecodeEventRejectsMetadataKeyNormalizationCollision(t *testing.T) {
	body := []byte(`{
		"protocolVersion":"orka.gateway.v1",
		"externalEventId":"event",
		"eventType":"text",
		"accountId":"account",
		"contextId":"context",
		"sender":{"id":"sender"},
		"text":"hello",
		"metadata":{"trace":"first"," trace ":"second"}
	}`)
	if _, err := DecodeEvent(body); err == nil || !strings.Contains(err.Error(), `duplicate normalized key "trace"`) {
		t.Fatalf("DecodeEvent() = %v, want normalized metadata key collision error", err)
	}
}

func TestValidateAllowedMetadataFailsClosed(t *testing.T) {
	if err := ValidateAllowedMetadata(map[string]string{"trace": "x"}, nil); err == nil {
		t.Fatal("ValidateAllowedMetadata() accepted unlisted key")
	}
	if err := ValidateAllowedMetadata(map[string]string{"trace": "x"}, []string{"trace"}); err != nil {
		t.Fatalf("ValidateAllowedMetadata() error = %v", err)
	}
}

func TestConstantTimeBearerEqual(t *testing.T) {
	if !ConstantTimeBearerEqual("token", "token") {
		t.Fatal("equal token rejected")
	}
	if ConstantTimeBearerEqual("token", "tokex") || ConstantTimeBearerEqual("", "") {
		t.Fatal("unequal or empty token accepted")
	}
}

func TestInterimDeliveryCapabilitiesAreOptionalAndStrict(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		wantError   bool
	}{
		{"absent", "", false},
		{"enabled", `,"interimDelivery":true`, false},
		{"disabled", `,"interimDelivery":false`, false},
		{"unknown", `,"futureDelivery":true`, true},
		{"wrong type", `,"interimDelivery":"true"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"protocolVersion":"orka.gateway.v1","adapterName":"test","capabilities":{"idempotentDelivery":true` + tc.field + `}}`)
			response, err := DecodeCapabilities(body)
			if (err != nil) != tc.wantError {
				t.Fatalf("DecodeCapabilities() error = %v", err)
			}
			if err == nil {
				encoded, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), `"interimDelivery":true`) != (tc.name == "enabled") {
					t.Fatalf("capability round trip = %s", encoded)
				}
			}
		})
	}
}

func TestDeliveryKindSpecificTextBounds(t *testing.T) {
	for _, tc := range []struct {
		name, kind, text string
		wantError        bool
	}{
		{"message", "message", "working", false},
		{"message boundary", "message", strings.Repeat("é", (16<<10)/2), false},
		{"message overflow", "message", strings.Repeat("x", (16<<10)+1), true},
		{"final boundary", "final", strings.Repeat("x", 64<<10), false},
		{"error boundary", "error", strings.Repeat("x", 64<<10), false},
		{"final overflow", "final", strings.Repeat("x", (64<<10)+1), true},
		{"error overflow", "error", strings.Repeat("x", (64<<10)+1), true},
		{"unknown kind", "progress", "working", true},
		{"invalid UTF8", "message", "\xff", true},
		{"control", "message", "work\x00ing", true},
		{"empty", "message", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := &DeliveryRequest{ProtocolVersion: Version, DeliveryID: "delivery", IdempotencyID: "operation", OriginatingEvent: "event", AccountID: "account", ContextID: "context", ReplyTarget: "reply", Kind: tc.kind, Text: tc.text}
			if err := ValidateDeliveryRequest(request); (err != nil) != tc.wantError {
				t.Fatalf("ValidateDeliveryRequest() error = %v", err)
			}
		})
	}
}

func TestSanitizeMessageRedactsCredentials(t *testing.T) {
	message := SanitizeMessage("Authorization: Bearer secret-token and token is abc123", 1024)
	if strings.Contains(message, "secret-token") || strings.Contains(message, "abc123") {
		t.Fatalf("SanitizeMessage() leaked credential: %q", message)
	}
}
