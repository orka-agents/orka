/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
)

var metadataKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// DecodeEvent strictly decodes and validates one bounded event envelope.
func DecodeEvent(body []byte) (*EventEnvelope, error) {
	if len(body) == 0 {
		return nil, errors.New("request body is required")
	}
	if len(body) > MaxHTTPBodyBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes", MaxHTTPBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var event EventEnvelope
	if err := decoder.Decode(&event); err != nil {
		return nil, fmt.Errorf("invalid event envelope: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	NormalizeEvent(&event)
	if err := ValidateEvent(&event); err != nil {
		return nil, err
	}
	return &event, nil
}

// NormalizeEvent trims boundary whitespace while preserving provider-owned identity case.
func NormalizeEvent(event *EventEnvelope) {
	if event == nil {
		return
	}
	event.ProtocolVersion = strings.TrimSpace(event.ProtocolVersion)
	event.ExternalEventID = strings.TrimSpace(event.ExternalEventID)
	event.EventType = strings.TrimSpace(event.EventType)
	event.AccountID = strings.TrimSpace(event.AccountID)
	event.ContextID = strings.TrimSpace(event.ContextID)
	event.ThreadID = strings.TrimSpace(event.ThreadID)
	event.Sender.ID = strings.TrimSpace(event.Sender.ID)
	event.Sender.DisplayName = strings.TrimSpace(event.Sender.DisplayName)
	event.ReplyTarget = strings.TrimSpace(event.ReplyTarget)
	if event.Metadata == nil {
		return
	}
	normalized := make(map[string]string, len(event.Metadata))
	for key, value := range event.Metadata {
		normalized[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	event.Metadata = normalized
}

// ValidateEvent enforces V1 text, identity, timestamp, and metadata bounds.
func ValidateEvent(event *EventEnvelope) error {
	if event == nil {
		return errors.New("event is required")
	}
	if event.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", event.ProtocolVersion)
	}
	if event.EventType != EventTypeText {
		return fmt.Errorf("unsupported eventType %q", event.EventType)
	}
	for name, value := range map[string]string{
		"externalEventId": event.ExternalEventID,
		"accountId":       event.AccountID,
		"contextId":       event.ContextID,
		"sender.id":       event.Sender.ID,
	} {
		if err := validateRequiredIdentity(name, value); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"threadId":           event.ThreadID,
		"sender.displayName": event.Sender.DisplayName,
		"replyTarget":        event.ReplyTarget,
	} {
		if err := validateOptionalIdentity(name, value); err != nil {
			return err
		}
	}
	if event.Text == "" {
		return errors.New("text is required")
	}
	if !utf8.ValidString(event.Text) {
		return errors.New("text must be valid UTF-8")
	}
	if len(event.Text) > MaxTextBytes {
		return fmt.Errorf("text exceeds %d bytes", MaxTextBytes)
	}
	if containsUnsafeControl(event.Text, true) {
		return errors.New("text contains unsupported control characters")
	}
	if event.OccurredAt != nil && event.ReceivedAt != nil && event.ReceivedAt.Before(*event.OccurredAt) {
		return errors.New("receivedAt must not be before occurredAt")
	}
	if len(event.Metadata) > MaxMetadataEntries {
		return fmt.Errorf("metadata exceeds %d entries", MaxMetadataEntries)
	}
	for key, value := range event.Metadata {
		if key == "" || len(key) > MaxMetadataKeyBytes || !metadataKeyPattern.MatchString(key) {
			return fmt.Errorf("metadata key %q is invalid", truncateForError(key, 32))
		}
		if len(value) > MaxMetadataValueBytes || !utf8.ValidString(value) || containsUnsafeControl(value, false) {
			return fmt.Errorf("metadata value for %q is invalid", truncateForError(key, 32))
		}
	}
	return nil
}

// ValidateAllowedMetadata rejects metadata not explicitly allowed by a GatewayClass.
func ValidateAllowedMetadata(metadata map[string]string, allowed []string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[strings.TrimSpace(key)] = struct{}{}
	}
	for key := range metadata {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("metadata key %q is not allowed", truncateForError(key, 32))
		}
	}
	return nil
}

// DecodeCapabilities strictly decodes a bounded capability response.
func DecodeCapabilities(body []byte) (*CapabilitiesResponse, error) {
	if len(body) == 0 || len(body) > MaxAdapterResponseBytes {
		return nil, errors.New("invalid capabilities response size")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response CapabilitiesResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("invalid capabilities response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	response.ProtocolVersion = strings.TrimSpace(response.ProtocolVersion)
	response.AdapterName = strings.TrimSpace(response.AdapterName)
	response.AdapterVersion = strings.TrimSpace(response.AdapterVersion)
	if response.ProtocolVersion != Version {
		return nil, fmt.Errorf("adapter reported unsupported protocolVersion %q", response.ProtocolVersion)
	}
	if err := validateOptionalIdentity("adapterName", response.AdapterName); err != nil || response.AdapterName == "" {
		return nil, errors.New("adapterName is required and must be safe")
	}
	if err := validateOptionalIdentity("adapterVersion", response.AdapterVersion); err != nil {
		return nil, err
	}
	if !response.Capabilities.IdempotentDelivery {
		return nil, errors.New("adapter must advertise idempotentDelivery")
	}
	return &response, nil
}

// DecodeDeliveryResponse strictly decodes one bounded terminal delivery result.
func DecodeDeliveryResponse(body []byte) (*DeliveryResponse, error) {
	if len(body) == 0 || len(body) > MaxAdapterResponseBytes {
		return nil, errors.New("invalid delivery response size")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response DeliveryResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("invalid delivery response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	response.Status = strings.TrimSpace(response.Status)
	response.ProviderMessageID = strings.TrimSpace(response.ProviderMessageID)
	response.Message = SanitizeMessage(response.Message, 1024)
	switch response.Status {
	case DeliveryStatusDelivered, DeliveryStatusRetryableError, DeliveryStatusNonRetryableError:
	default:
		return nil, fmt.Errorf("unsupported delivery status %q", response.Status)
	}
	if err := validateOptionalIdentity("providerMessageId", response.ProviderMessageID); err != nil {
		return nil, err
	}
	return &response, nil
}

// ValidateDeliveryRequest enforces outbound V1 bounds before a network call.
func ValidateDeliveryRequest(request *DeliveryRequest) error {
	if request == nil {
		return errors.New("delivery request is required")
	}
	if request.ProtocolVersion != Version {
		return fmt.Errorf("unsupported protocolVersion %q", request.ProtocolVersion)
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
			return err
		}
	}
	if request.Kind != DeliveryKindFinal && request.Kind != DeliveryKindError {
		return fmt.Errorf("unsupported delivery kind %q", request.Kind)
	}
	if request.Text == "" || len(request.Text) > MaxTextBytes || !utf8.ValidString(request.Text) || containsUnsafeControl(request.Text, true) {
		return errors.New("delivery text is invalid")
	}
	return nil
}

// ConstantTimeBearerEqual compares bearer tokens without leaking prefix timing.
func ConstantTimeBearerEqual(got, want string) bool {
	got = strings.TrimSpace(got)
	want = strings.TrimSpace(want)
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// BearerToken extracts a case-sensitive bearer value from an Authorization header.
func BearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

// SanitizeMessage strips controls and bounds adapter-owned messages.
func SanitizeMessage(message string, limit int) string {
	message = redact.SensitiveText(strings.TrimSpace(message))
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, message)
	if limit > 0 && len(message) > limit {
		message = message[:limit]
		for !utf8.ValidString(message) && len(message) > 0 {
			message = message[:len(message)-1]
		}
	}
	return message
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return errors.New("request must contain exactly one JSON value")
}

func validateRequiredIdentity(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	return validateOptionalIdentity(name, value)
}

func validateOptionalIdentity(name, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxIdentityBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, MaxIdentityBytes)
	}
	if !utf8.ValidString(value) || containsUnsafeControl(value, false) {
		return fmt.Errorf("%s contains unsafe characters", name)
	}
	return nil
}

func containsUnsafeControl(value string, allowNewlines bool) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		if !unicode.IsControl(r) {
			return false
		}
		if allowNewlines && (r == '\n' || r == '\r' || r == '\t') {
			return false
		}
		return true
	})
}

func truncateForError(value string, limit int) string {
	value = SanitizeMessage(value, limit)
	if value == "" {
		return "<empty>"
	}
	return value
}
