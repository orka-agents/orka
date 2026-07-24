package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzDecodeEvent(f *testing.F) {
	f.Add([]byte(`{"protocolVersion":"orka.gateway.v1","externalEventId":"e","eventType":"text","accountId":"a","contextId":"c","sender":{"id":"s"},"text":"hello"}`))
	f.Add([]byte(`{"protocolVersion":"orka.gateway.v1","externalEventId":"e","eventType":"text","accountId":"a","contextId":"c","sender":{"id":"s"},"text":"hello","metadata":{"trace":"safe"}}`))
	f.Add([]byte(`{"protocolVersion":"orka.gateway.v1","externalEventId":"e","eventType":"text","accountId":"a","contextId":"c","sender":{"id":"s"},"text":"hello","metadata":{"trace":"first"," trace ":"second"}}`))
	f.Add([]byte(`{"raw":"provider payload"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		event, err := DecodeEvent(body)
		if err == nil {
			if event == nil {
				t.Fatal("DecodeEvent returned nil event without error")
			}
			if err := ValidateEvent(event); err != nil {
				t.Fatalf("decoded event failed validation: %v", err)
			}
		}
	})
}

func FuzzDecodeCapabilities(f *testing.F) {
	f.Add([]byte(`{"protocolVersion":"orka.gateway.v1","adapterName":"adapter","adapterVersion":"v1","capabilities":{"inboundText":true,"outboundText":true,"idempotentDelivery":true}}`))
	f.Add([]byte(`{"protocolVersion":"orka.gateway.v2","adapterName":"adapter","capabilities":{"idempotentDelivery":true}}`))
	f.Add([]byte(`{"protocolVersion":"orka.gateway.v1","adapterName":"adapter","capabilities":{"idempotentDelivery":false,"unknown":true}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		response, err := DecodeCapabilities(body)
		if err != nil {
			return
		}
		if response == nil {
			t.Fatal("DecodeCapabilities returned nil response without error")
		}
		if response.ProtocolVersion != Version {
			t.Fatalf("protocolVersion = %q, want %q", response.ProtocolVersion, Version)
		}
		if response.AdapterName == "" {
			t.Fatal("successful capabilities response has empty adapterName")
		}
		if err := validateOptionalIdentity("adapterName", response.AdapterName); err != nil {
			t.Fatalf("successful capabilities response has invalid adapterName: %v", err)
		}
		if err := validateOptionalIdentity("adapterVersion", response.AdapterVersion); err != nil {
			t.Fatalf("successful capabilities response has invalid adapterVersion: %v", err)
		}
		if !response.Capabilities.IdempotentDelivery {
			t.Fatal("successful capabilities response lacks idempotentDelivery")
		}

		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if _, err := DecodeCapabilities(encoded); err != nil {
			t.Fatalf("normalized capabilities response did not round trip: %v", err)
		}
	})
}

func FuzzDecodeDeliveryResponse(f *testing.F) {
	f.Add([]byte(`{"status":"delivered","providerMessageId":"message-1"}`))
	f.Add([]byte(`{"status":"retryableError","message":"retry"}`))
	f.Add([]byte(`{"status":"nonRetryableError","message":"Authorization: Bearer secret-token"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		response, err := DecodeDeliveryResponse(body)
		if err != nil {
			return
		}
		if response == nil {
			t.Fatal("DecodeDeliveryResponse returned nil response without error")
		}
		switch response.Status {
		case DeliveryStatusDelivered, DeliveryStatusRetryableError, DeliveryStatusNonRetryableError:
		default:
			t.Fatalf("successful delivery response has unsupported status %q", response.Status)
		}
		if err := validateOptionalIdentity("providerMessageId", response.ProviderMessageID); err != nil {
			t.Fatalf("successful delivery response has invalid providerMessageId: %v", err)
		}
		if len(response.Message) > 1024 || !utf8.ValidString(response.Message) || containsUnsafeControl(response.Message, true) {
			t.Fatalf("successful delivery response has unsafe sanitized message %q", response.Message)
		}

		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		if _, err := DecodeDeliveryResponse(encoded); err != nil {
			t.Fatalf("normalized delivery response did not round trip: %v", err)
		}
	})
}

func FuzzValidateDeliveryRequest(f *testing.F) {
	f.Add(Version, "delivery", "idempotency", "event", "account", "context", "reply", DeliveryKindFinal, "hello")
	f.Add(Version, "", "idempotency", "event", "account", "context", "reply", DeliveryKindError, "failure")
	f.Add("orka.gateway.v2", "delivery", "idempotency", "event", "account", "context", "reply", "unknown", "\x00")
	f.Fuzz(func(t *testing.T, protocolVersion, deliveryID, idempotencyID, eventID, accountID, contextID, replyTarget, kind, text string) {
		request := &DeliveryRequest{
			ProtocolVersion:  protocolVersion,
			DeliveryID:       deliveryID,
			IdempotencyID:    idempotencyID,
			OriginatingEvent: eventID,
			Kind:             kind,
			AccountID:        accountID,
			ContextID:        contextID,
			ReplyTarget:      replyTarget,
			Text:             text,
		}
		if err := ValidateDeliveryRequest(request); err != nil {
			return
		}
		if request.ProtocolVersion != Version {
			t.Fatalf("successful request has protocolVersion %q", request.ProtocolVersion)
		}
		for name, value := range map[string]string{
			"deliveryId":         request.DeliveryID,
			"idempotencyId":      request.IdempotencyID,
			"originatingEventId": request.OriginatingEvent,
			"accountId":          request.AccountID,
			"contextId":          request.ContextID,
			"replyTarget":        request.ReplyTarget,
		} {
			if err := validateRequiredIdentity(name, strings.TrimSpace(value)); err != nil {
				t.Fatalf("successful request has invalid %s: %v", name, err)
			}
		}
		if request.Kind != DeliveryKindFinal && request.Kind != DeliveryKindError {
			t.Fatalf("successful request has unsupported kind %q", request.Kind)
		}
		if request.Text == "" || len(request.Text) > MaxTextBytes || !utf8.ValidString(request.Text) || containsUnsafeControl(request.Text, true) {
			t.Fatalf("successful request has invalid text %q", request.Text)
		}
	})
}

func FuzzValidateAllowedMetadata(f *testing.F) {
	f.Add("trace", "safe", "trace")
	f.Add("trace", "safe", "other")
	f.Add(" bad key ", " value ", "bad key")
	f.Fuzz(func(t *testing.T, key, value, allowedKey string) {
		event := &EventEnvelope{
			ProtocolVersion: Version,
			ExternalEventID: "event",
			EventType:       EventTypeText,
			AccountID:       "account",
			ContextID:       "context",
			Sender:          Sender{ID: "sender"},
			Text:            "hello",
			Metadata:        map[string]string{key: value},
		}
		if err := NormalizeEvent(event); err != nil {
			t.Fatalf("NormalizeEvent() error = %v", err)
		}
		if err := ValidateEvent(event); err == nil {
			for normalizedKey, normalizedValue := range event.Metadata {
				if normalizedKey == "" || len(normalizedKey) > MaxMetadataKeyBytes || !metadataKeyPattern.MatchString(normalizedKey) {
					t.Fatalf("successful event has invalid metadata key %q", normalizedKey)
				}
				if len(normalizedValue) > MaxMetadataValueBytes || !utf8.ValidString(normalizedValue) || containsUnsafeControl(normalizedValue, false) {
					t.Fatalf("successful event has invalid metadata value %q", normalizedValue)
				}
			}
		}

		err := ValidateAllowedMetadata(event.Metadata, []string{allowedKey})
		wantAllowed := strings.TrimSpace(key) == strings.TrimSpace(allowedKey)
		if wantAllowed && err != nil {
			t.Fatalf("exactly allowed metadata was rejected: %v", err)
		}
		if !wantAllowed && err == nil {
			t.Fatal("unlisted metadata was accepted")
		}
	})
}
