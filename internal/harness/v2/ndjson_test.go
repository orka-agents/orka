package v2

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testUpdateEvent(t *testing.T, sequence uint64, timestamp time.Time, text string) Event {
	t.Helper()
	return Event{
		Protocol: ProtocolVersion,
		Type:     EventUpdate,
		Identity: testEventIdentity(t, sequence, timestamp),
		Update: &UpdateEvent{
			Kind:             UpdateAssistantMessageChunk,
			AssistantMessage: &AssistantMessageChunk{Text: text},
		},
	}
}

func testPermissionEvent(t *testing.T, sequence uint64, timestamp time.Time) Event {
	t.Helper()
	return Event{
		Protocol: ProtocolVersion,
		Type:     EventPermissionRequested,
		Identity: testEventIdentity(t, sequence, timestamp),
		PermissionRequested: &PermissionRequestedEvent{
			RequestID:  "permission-1",
			ToolCallID: "tool-1",
			Title:      "Allow write?",
			Options: []PermissionOption{
				{OptionID: "allow-once", Name: "Allow once", Kind: PermissionOptionAllowOnce},
				{OptionID: "reject-once", Name: "Reject once", Kind: PermissionOptionRejectOnce},
			},
			ExpiresAt: timestamp.Add(time.Minute),
		},
	}
}

func TestBoundedNDJSONRoundTrip(t *testing.T) {
	limits := DefaultEventStreamLimits()
	expectation := testExpectation(t)
	var stream bytes.Buffer
	encoder, err := NewEventEncoder(&stream, limits, expectation)
	if err != nil {
		t.Fatalf("NewEventEncoder() error = %v", err)
	}
	events := []Event{
		testAcceptedEvent(t),
		testUpdateEvent(t, 2, testNow.Add(time.Millisecond), "working"),
		testPermissionEvent(t, 3, testNow.Add(2*time.Millisecond)),
		testCompletedEvent(t, 4, testNow.Add(3*time.Millisecond)),
	}
	for i := range events {
		if err := encoder.Encode(events[i]); err != nil {
			t.Fatalf("Encode(%d) error = %v", i, err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("EventEncoder.Close() error = %v", err)
	}
	if got := bytes.Count(stream.Bytes(), []byte{'\n'}); got != len(events) {
		t.Fatalf("NDJSON newline count = %d, want %d", got, len(events))
	}

	decoder, err := NewEventDecoder(bytes.NewReader(stream.Bytes()), limits, expectation)
	if err != nil {
		t.Fatalf("NewEventDecoder() error = %v", err)
	}
	decoded, err := decoder.DecodeAll()
	if err != nil {
		t.Fatalf("DecodeAll() error = %v", err)
	}
	if len(decoded) != len(events) || decoded[0].Type != EventAccepted || decoded[len(decoded)-1].Type != EventCompleted {
		t.Fatalf("decoded events = %#v", decoded)
	}
}

func TestNDJSONDecoderRejectsUnknownFieldsDuplicateKeysAndBlankLines(t *testing.T) {
	limits := DefaultEventStreamLimits()
	expectation := testExpectation(t)
	acceptedJSON, err := json.Marshal(testAcceptedEvent(t))
	if err != nil {
		t.Fatalf("marshal accepted event: %v", err)
	}

	inputs := map[string][]byte{
		"unknown field": append(append([]byte{}, acceptedJSON[:len(acceptedJSON)-1]...), append([]byte(`,"unexpected":true}`), '\n')...),
		"duplicate key": append([]byte(`{"protocol":"orka.harness.v2",`), append(acceptedJSON[1:], '\n')...),
		"blank line":    []byte("\n"),
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			decoder, err := NewEventDecoder(bytes.NewReader(input), limits, expectation)
			if err != nil {
				t.Fatalf("NewEventDecoder() error = %v", err)
			}
			if _, err := decoder.Decode(); err == nil || !errors.Is(err, ErrMalformedEvent) {
				t.Fatalf("Decode() error = %v, want ErrMalformedEvent", err)
			} else if !IsPoisoningStreamError(err) {
				t.Fatalf("IsPoisoningStreamError(%v) = false", err)
			}
		})
	}
}

func TestNDJSONLineAndTerminalPayloadBounds(t *testing.T) {
	limits := EventStreamLimits{
		MaxLineBytes:             1024,
		MaxTerminalResultBytes:   512,
		MaxBufferedEvents:        16,
		MaxUpdateEventsPerSecond: 10,
	}
	decoder, err := NewEventDecoder(strings.NewReader(strings.Repeat("x", 1025)+"\n"), limits, testExpectation(t))
	if err != nil {
		t.Fatalf("NewEventDecoder() error = %v", err)
	}
	if _, err := decoder.Decode(); err == nil || !errors.Is(err, ErrEventLineTooLarge) {
		t.Fatalf("Decode(oversized) error = %v", err)
	}

	limits.MaxLineBytes = 4096
	limits.MaxTerminalResultBytes = 100
	var stream bytes.Buffer
	encoder, err := NewEventEncoder(&stream, limits, testExpectation(t))
	if err != nil {
		t.Fatalf("NewEventEncoder() error = %v", err)
	}
	if err := encoder.Encode(testAcceptedEvent(t)); err != nil {
		t.Fatalf("Encode(accepted) error = %v", err)
	}
	terminal := testCompletedEvent(t, 2, testNow.Add(time.Second))
	terminal.Completed.Result.Content[0].Text = strings.Repeat("x", 512)
	if err := encoder.Encode(terminal); err == nil || !strings.Contains(err.Error(), "terminal payload") {
		t.Fatalf("Encode(oversized terminal) error = %v", err)
	}
}

func TestEventStreamRejectsLateTrafficSequenceRegressionAndIdentitySwap(t *testing.T) {
	limits := DefaultEventStreamLimits()
	validator, err := NewEventStreamValidator(limits, testExpectation(t))
	if err != nil {
		t.Fatalf("NewEventStreamValidator() error = %v", err)
	}
	if err := validator.ValidateNext(testAcceptedEvent(t), testNow); err != nil {
		t.Fatalf("accepted validation error = %v", err)
	}
	if err := validator.ValidateNext(testCompletedEvent(t, 2, testNow.Add(time.Second)), testNow); err != nil {
		t.Fatalf("terminal validation error = %v", err)
	}
	if err := validator.ValidateNext(testUpdateEvent(t, 3, testNow.Add(2*time.Second), "late"), testNow); !errors.Is(err, ErrEventAfterTerminal) {
		t.Fatalf("late event error = %v, want ErrEventAfterTerminal", err)
	}

	validator, _ = NewEventStreamValidator(limits, testExpectation(t))
	_ = validator.ValidateNext(testAcceptedEvent(t), testNow)
	if err := validator.ValidateNext(testUpdateEvent(t, 1, testNow.Add(time.Second), "regressed"), testNow); !errors.Is(err, ErrEventSequence) {
		t.Fatalf("sequence regression error = %v", err)
	}

	validator, _ = NewEventStreamValidator(limits, testExpectation(t))
	badIdentity := testAcceptedEvent(t)
	badIdentity.Identity.RuntimeInstanceID = "other-instance"
	if err := validator.ValidateNext(badIdentity, testNow); !errors.Is(err, ErrEventIdentityMismatch) {
		t.Fatalf("identity mismatch error = %v", err)
	}
}

func TestEventStreamRequiresAcceptedAndExactlyOneTerminal(t *testing.T) {
	limits := DefaultEventStreamLimits()
	expectation := testExpectation(t)

	validator, _ := NewEventStreamValidator(limits, expectation)
	if err := validator.ValidateNext(testUpdateEvent(t, 1, testNow, "no acceptance"), testNow); !errors.Is(err, ErrMissingAcceptedEvent) {
		t.Fatalf("first update error = %v", err)
	}

	var stream bytes.Buffer
	encoder, _ := NewEventEncoder(&stream, limits, expectation)
	if err := encoder.Encode(testAcceptedEvent(t)); err != nil {
		t.Fatalf("Encode(accepted) error = %v", err)
	}
	decoder, _ := NewEventDecoder(bytes.NewReader(stream.Bytes()), limits, expectation)
	if _, err := decoder.DecodeAll(); !errors.Is(err, ErrMissingTerminalEvent) {
		t.Fatalf("DecodeAll(missing terminal) error = %v", err)
	}
}

func TestEventUpdateRateAndBufferedEventLimits(t *testing.T) {
	limits := DefaultEventStreamLimits()
	limits.MaxUpdateEventsPerSecond = 2
	validator, _ := NewEventStreamValidator(limits, testExpectation(t))
	if err := validator.ValidateNext(testAcceptedEvent(t), testNow); err != nil {
		t.Fatalf("accepted validation error = %v", err)
	}
	for sequence := uint64(2); sequence <= 3; sequence++ {
		if err := validator.ValidateNext(testUpdateEvent(t, sequence, testNow.Add(time.Duration(sequence)*time.Millisecond), "update"), testNow); err != nil {
			t.Fatalf("update %d validation error = %v", sequence, err)
		}
	}
	if err := validator.ValidateNext(testUpdateEvent(t, 4, testNow.Add(4*time.Millisecond), "overflow"), testNow); !errors.Is(err, ErrEventRateExceeded) {
		t.Fatalf("rate overflow error = %v", err)
	}

	limits = DefaultEventStreamLimits()
	validator, _ = NewEventStreamValidator(limits, testExpectation(t))
	validator.maxUpdateBytesPerSec = 1
	if err := validator.ValidateNext(testAcceptedEvent(t), testNow); err != nil {
		t.Fatalf("accepted validation error = %v", err)
	}
	if err := validator.ValidateNext(testUpdateEvent(t, 2, testNow.Add(time.Millisecond), "bounded"), testNow); !errors.Is(err, ErrEventByteRateExceeded) {
		t.Fatalf("byte-rate overflow error = %v, want ErrEventByteRateExceeded", err)
	} else if !IsPoisoningStreamError(err) {
		t.Fatalf("byte-rate overflow was not poisoning: %v", err)
	}

	limits = DefaultEventStreamLimits()
	limits.MaxBufferedEvents = 2
	var stream bytes.Buffer
	encoder, _ := NewEventEncoder(&stream, limits, testExpectation(t))
	for _, event := range []Event{
		testAcceptedEvent(t),
		testUpdateEvent(t, 2, testNow.Add(time.Millisecond), "update"),
		testCompletedEvent(t, 3, testNow.Add(2*time.Millisecond)),
	} {
		if err := encoder.Encode(event); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}
	decoder, _ := NewEventDecoder(bytes.NewReader(stream.Bytes()), limits, testExpectation(t))
	if _, err := decoder.DecodeAll(); !errors.Is(err, ErrBufferedEventOverflow) {
		t.Fatalf("DecodeAll(buffer overflow) error = %v", err)
	}
}

func TestNDJSONDecoderChargesActualUpdateLineBytes(t *testing.T) {
	limits := DefaultEventStreamLimits()
	expectation := testExpectation(t)
	acceptedJSON, err := json.Marshal(testAcceptedEvent(t))
	if err != nil {
		t.Fatalf("marshal accepted event: %v", err)
	}
	updateJSON, err := json.Marshal(testUpdateEvent(t, 2, testNow.Add(time.Millisecond), "bounded"))
	if err != nil {
		t.Fatalf("marshal update event: %v", err)
	}
	paddedUpdate := append(bytes.Repeat([]byte{' '}, 64), updateJSON...)
	if len(paddedUpdate) <= len(updateJSON) {
		t.Fatalf("padded update size = %d, want greater than compact size %d", len(paddedUpdate), len(updateJSON))
	}
	stream := append(append(append([]byte{}, acceptedJSON...), '\n'), paddedUpdate...)
	stream = append(stream, '\n')
	decoder, err := NewEventDecoder(bytes.NewReader(stream), limits, expectation)
	if err != nil {
		t.Fatalf("NewEventDecoder() error = %v", err)
	}
	decoder.validator.maxUpdateBytesPerSec = len(updateJSON)
	if _, err := decoder.Decode(); err != nil {
		t.Fatalf("Decode(accepted) error = %v", err)
	}
	if _, err := decoder.Decode(); !errors.Is(err, ErrEventByteRateExceeded) {
		t.Fatalf("Decode(padded update) error = %v, want ErrEventByteRateExceeded", err)
	} else if !IsPoisoningStreamError(err) {
		t.Fatalf("padded update byte-rate overflow was not poisoning: %v", err)
	}
}

func TestOutcomeUnknownEventIsTerminalAndNonRetryable(t *testing.T) {
	event := Event{
		Protocol: ProtocolVersion,
		Type:     EventOutcomeUnknown,
		Identity: testEventIdentity(t, 2, testNow.Add(time.Second)),
		OutcomeUnknown: &OutcomeUnknownEvent{
			Code:              "settlement_unproven",
			Message:           "provider may have accepted the prompt",
			ForcedTermination: true,
		},
	}
	limits := DefaultEventStreamLimits()
	if err := event.Validate(limits); err != nil {
		t.Fatalf("outcome unknown event validation error = %v", err)
	}
	event.OutcomeUnknown.Retryable = true
	if err := event.Validate(limits); err == nil || !strings.Contains(err.Error(), "never") {
		t.Fatalf("retryable outcome unknown validation = %v", err)
	}
}
