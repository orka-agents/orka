package v2

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	DefaultMaxEventLineBytes        = 512 << 10
	DefaultMaxTerminalResultBytes   = 256 << 10
	DefaultMaxBufferedEvents        = 256
	DefaultMaxUpdateEventsPerSecond = 100
	// DefaultMaxUpdateBytesPerSecond is a fixed orka.harness.v2 protocol
	// invariant applied to each event stream; it is not a negotiated
	// ProtocolLimits value. The exported name is retained for compatibility.
	DefaultMaxUpdateBytesPerSecond = 32 << 20

	absoluteMaxEventLineBytes   = MaxCanonicalJSONBytes
	absoluteMaxBufferedEvents   = 16_384
	absoluteMaxUpdatesPerSecond = 10_000
)

var (
	ErrEventLineTooLarge     = errors.New("event line too large")
	ErrMalformedEvent        = errors.New("malformed event")
	ErrEventIdentityMismatch = errors.New("event identity mismatch")
	ErrEventSequence         = errors.New("invalid event sequence")
	ErrEventRateExceeded     = errors.New("event update rate exceeded")
	ErrEventByteRateExceeded = errors.New("event update byte rate exceeded")
	ErrEventAfterTerminal    = errors.New("event after terminal")
	ErrMissingAcceptedEvent  = errors.New("missing accepted event")
	ErrMissingTerminalEvent  = errors.New("missing terminal event")
	ErrBufferedEventOverflow = errors.New("buffered event limit exceeded")
)

type EventStreamLimits struct {
	MaxLineBytes             int `json:"maxLineBytes"`
	MaxTerminalResultBytes   int `json:"maxTerminalResultBytes"`
	MaxBufferedEvents        int `json:"maxBufferedEvents"`
	MaxUpdateEventsPerSecond int `json:"maxUpdateEventsPerSecond"`
}

func DefaultEventStreamLimits() EventStreamLimits {
	return EventStreamLimits{
		MaxLineBytes:             DefaultMaxEventLineBytes,
		MaxTerminalResultBytes:   DefaultMaxTerminalResultBytes,
		MaxBufferedEvents:        DefaultMaxBufferedEvents,
		MaxUpdateEventsPerSecond: DefaultMaxUpdateEventsPerSecond,
	}
}

func (l EventStreamLimits) Validate() error {
	if l.MaxLineBytes <= 0 || l.MaxLineBytes > absoluteMaxEventLineBytes {
		return fmt.Errorf("max event line bytes must be in range 1..%d", absoluteMaxEventLineBytes)
	}
	if l.MaxTerminalResultBytes <= 0 || l.MaxTerminalResultBytes > l.MaxLineBytes {
		return fmt.Errorf("max terminal result bytes must be positive and no greater than max line bytes")
	}
	if l.MaxBufferedEvents <= 0 || l.MaxBufferedEvents > absoluteMaxBufferedEvents {
		return fmt.Errorf("max buffered events must be in range 1..%d", absoluteMaxBufferedEvents)
	}
	if l.MaxUpdateEventsPerSecond <= 0 || l.MaxUpdateEventsPerSecond > absoluteMaxUpdatesPerSecond {
		return fmt.Errorf("max update events per second must be in range 1..%d", absoluteMaxUpdatesPerSecond)
	}
	return nil
}

type EventStreamValidator struct {
	limits               EventStreamLimits
	expectation          EventExpectation
	accepted             bool
	terminal             bool
	lastSequence         uint64
	lastTimestamp        time.Time
	updateWindow         time.Time
	updatesInWindow      int
	updateBytesInWindow  int
	maxUpdateBytesPerSec int
}

func NewEventStreamValidator(limits EventStreamLimits, expectation EventExpectation) (*EventStreamValidator, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := expectation.Validate(); err != nil {
		return nil, fmt.Errorf("event expectation: %w", err)
	}
	return &EventStreamValidator{limits: limits, expectation: expectation, maxUpdateBytesPerSec: DefaultMaxUpdateBytesPerSecond}, nil
}

func (v *EventStreamValidator) ValidateNext(event Event, arrival time.Time) error {
	return v.validateNext(event, arrival, -1)
}

// validateNext validates an event and, when encodedUpdateBytes is non-negative,
// charges that exact encoded line size for update-byte accounting. Direct
// ValidateNext callers retain the previous behavior of charging the event's
// compact JSON encoding.
func (v *EventStreamValidator) validateNext(event Event, arrival time.Time, encodedUpdateBytes int) error {
	if v == nil {
		return fmt.Errorf("event stream validator is required")
	}
	if v.terminal {
		return ErrEventAfterTerminal
	}
	if err := event.Validate(v.limits); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedEvent, err)
	}
	if err := v.expectation.Matches(event.Identity); err != nil {
		return fmt.Errorf("%w: %v", ErrEventIdentityMismatch, err)
	}
	if !v.accepted {
		if event.Type != EventAccepted {
			return fmt.Errorf("%w: first event is %q", ErrMissingAcceptedEvent, event.Type)
		}
		if event.Identity.Sequence != 1 {
			return fmt.Errorf("%w: accepted event sequence is %d, want 1", ErrEventSequence, event.Identity.Sequence)
		}
		v.accepted = true
	} else {
		if event.Type == EventAccepted {
			return fmt.Errorf("%w: duplicate accepted event", ErrMalformedEvent)
		}
		if event.Identity.Sequence <= v.lastSequence {
			return fmt.Errorf("%w: sequence %d is not greater than %d", ErrEventSequence, event.Identity.Sequence, v.lastSequence)
		}
		if event.Identity.Timestamp.Before(v.lastTimestamp) {
			return fmt.Errorf("%w: timestamp moved backwards", ErrEventSequence)
		}
	}
	if event.Type == EventUpdate {
		if arrival.IsZero() {
			arrival = time.Now().UTC()
		}
		if v.updateWindow.IsZero() || arrival.Before(v.updateWindow) || arrival.Sub(v.updateWindow) >= time.Second {
			v.updateWindow = arrival
			v.updatesInWindow = 0
			v.updateBytesInWindow = 0
		}
		v.updatesInWindow++
		if v.updatesInWindow > v.limits.MaxUpdateEventsPerSecond {
			return fmt.Errorf("%w: received %d updates in one second, limit %d", ErrEventRateExceeded, v.updatesInWindow, v.limits.MaxUpdateEventsPerSecond)
		}
		if encodedUpdateBytes < 0 {
			encoded, err := json.Marshal(event)
			if err != nil {
				return fmt.Errorf("%w: marshal update event: %v", ErrMalformedEvent, err)
			}
			encodedUpdateBytes = len(encoded)
		}
		v.updateBytesInWindow += encodedUpdateBytes
		if v.updateBytesInWindow > v.maxUpdateBytesPerSec {
			return fmt.Errorf("%w: received %d update bytes in one second, limit %d", ErrEventByteRateExceeded, v.updateBytesInWindow, v.maxUpdateBytesPerSec)
		}
	}
	v.lastSequence = event.Identity.Sequence
	v.lastTimestamp = event.Identity.Timestamp
	if event.Type.IsTerminal() {
		v.terminal = true
	}
	return nil
}

func (v *EventStreamValidator) Finish() error {
	if v == nil {
		return fmt.Errorf("event stream validator is required")
	}
	if !v.accepted {
		return ErrMissingAcceptedEvent
	}
	if !v.terminal {
		return ErrMissingTerminalEvent
	}
	return nil
}

func (v *EventStreamValidator) TerminalSeen() bool {
	return v != nil && v.terminal
}

type EventDecoder struct {
	reader    *bufio.Reader
	limits    EventStreamLimits
	validator *EventStreamValidator
}

func NewEventDecoder(reader io.Reader, limits EventStreamLimits, expectation EventExpectation) (*EventDecoder, error) {
	if reader == nil {
		return nil, fmt.Errorf("event reader is required")
	}
	validator, err := NewEventStreamValidator(limits, expectation)
	if err != nil {
		return nil, err
	}
	return &EventDecoder{
		reader:    bufio.NewReaderSize(reader, limits.MaxLineBytes+2),
		limits:    limits,
		validator: validator,
	}, nil
}

func (d *EventDecoder) Decode() (Event, error) {
	if d == nil || d.reader == nil || d.validator == nil {
		return Event{}, fmt.Errorf("event decoder is not initialized")
	}
	line, err := readBoundedNDJSONLine(d.reader, d.limits.MaxLineBytes)
	if err != nil {
		return Event{}, err
	}
	if _, err := parseCanonicalJSON(line); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformedEvent, err)
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var event Event
	if err := dec.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("%w: decode event: %v", ErrMalformedEvent, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrMalformedEvent, err)
	}
	if err := d.validator.validateNext(event, time.Now().UTC(), len(line)); err != nil {
		return Event{}, err
	}
	return event, nil
}

// DecodeAll reads a complete finite event stream. Because this helper buffers,
// it enforces MaxBufferedEvents. Streaming callers should use Decode and persist
// or process each event before reading the next one.
func (d *EventDecoder) DecodeAll() ([]Event, error) {
	if d == nil {
		return nil, fmt.Errorf("event decoder is required")
	}
	events := make([]Event, 0, min(d.limits.MaxBufferedEvents, 16))
	for {
		event, err := d.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(events) == d.limits.MaxBufferedEvents {
			return nil, fmt.Errorf("%w: limit %d", ErrBufferedEventOverflow, d.limits.MaxBufferedEvents)
		}
		events = append(events, event)
	}
	if err := d.validator.Finish(); err != nil {
		return nil, err
	}
	return events, nil
}

type EventEncoder struct {
	writer    io.Writer
	limits    EventStreamLimits
	validator *EventStreamValidator
}

func NewEventEncoder(writer io.Writer, limits EventStreamLimits, expectation EventExpectation) (*EventEncoder, error) {
	if writer == nil {
		return nil, fmt.Errorf("event writer is required")
	}
	validator, err := NewEventStreamValidator(limits, expectation)
	if err != nil {
		return nil, err
	}
	return &EventEncoder{writer: writer, limits: limits, validator: validator}, nil
}

func (e *EventEncoder) Encode(event Event) error {
	if e == nil || e.writer == nil || e.validator == nil {
		return fmt.Errorf("event encoder is not initialized")
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if len(line) > e.limits.MaxLineBytes {
		return fmt.Errorf("%w: encoded line is %d bytes, limit %d", ErrEventLineTooLarge, len(line), e.limits.MaxLineBytes)
	}
	if err := e.validator.validateNext(event, time.Now().UTC(), len(line)); err != nil {
		return err
	}
	line = append(line, '\n')
	if err := writeAll(e.writer, line); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

func (e *EventEncoder) Close() error {
	if e == nil || e.validator == nil {
		return fmt.Errorf("event encoder is not initialized")
	}
	return e.validator.Finish()
}

func readBoundedNDJSONLine(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("%w: limit %d", ErrEventLineTooLarge, maxBytes)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read event line: %w", err)
	}
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return nil, io.EOF
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("%w: blank NDJSON line", ErrMalformedEvent)
	}
	if len(line) > maxBytes {
		return nil, fmt.Errorf("%w: line is %d bytes, limit %d", ErrEventLineTooLarge, len(line), maxBytes)
	}
	return line, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// IsPoisoningStreamError reports protocol/limit violations that require the
// RuntimeSession to be poisoned rather than reused. Ordinary EOF is not a
// poisoning error; a missing terminal discovered by Finish is.
func IsPoisoningStreamError(err error) bool {
	if err == nil || errors.Is(err, io.EOF) {
		return false
	}
	for _, target := range []error{
		ErrEventLineTooLarge,
		ErrMalformedEvent,
		ErrEventIdentityMismatch,
		ErrEventSequence,
		ErrEventRateExceeded,
		ErrEventByteRateExceeded,
		ErrEventAfterTerminal,
		ErrMissingAcceptedEvent,
		ErrMissingTerminalEvent,
		ErrBufferedEventOverflow,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
