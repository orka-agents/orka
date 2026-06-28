package common

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sozercan/orka/internal/events"
)

const defaultDetachedEventTimeout = 250 * time.Millisecond

// EventRecorder records worker-side execution events.
type EventRecorder interface {
	Record(ctx context.Context, typ string, opts ...EventOption)
}

// StrictEventRecorder records events synchronously and returns transport or
// validation errors to callers that cannot safely continue without persistence.
type StrictEventRecorder interface {
	RecordStrict(ctx context.Context, typ string, opts ...EventOption) error
}

// EventOption customizes a worker execution event before it is recorded.
type EventOption func(*RecordedEvent)

// RecordedEvent is the worker-facing shape captured by fakes and future clients.
type RecordedEvent struct {
	Type        string
	Severity    string
	TaskName    string
	SessionName string
	AgentName   string
	ToolName    string
	ToolCallID  string
	Summary     string
	Content     json.RawMessage
	ContentText string
	Truncation  *events.ExecutionEventTruncation
	CreatedAt   time.Time
}

// NoopEventRecorder discards worker events.
type NoopEventRecorder struct{}

var _ EventRecorder = NoopEventRecorder{}

// Record implements EventRecorder.
func (NoopEventRecorder) Record(context.Context, string, ...EventOption) {}

// RecordStrict implements StrictEventRecorder and fails closed because a no-op
// recorder cannot prove that a required event was persisted.
func (NoopEventRecorder) RecordStrict(context.Context, string, ...EventOption) error {
	return fmt.Errorf("strict execution event recorder is not configured")
}

// RecordEvent emits a best-effort worker execution event.
// Event recording must never change worker task behavior, so nil recorders and
// recorder implementation panics are ignored.
func RecordEvent(ctx context.Context, recorder EventRecorder, typ string, opts ...EventOption) {
	if recorder == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	recorder.Record(ctx, typ, opts...)
}

// RecordEventWithTimeout emits a best-effort event using a fresh bounded
// context. Use this for terminal events that may run after the worker's signal
// context is already canceled.
func RecordEventWithTimeout(recorder EventRecorder, typ string, timeout time.Duration, opts ...EventOption) {
	if timeout <= 0 {
		timeout = defaultDetachedEventTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	RecordEvent(ctx, recorder, typ, opts...)
}

// RecordEventStrict emits an execution event through a strict recorder. Unlike
// RecordEvent, errors are returned so approval requests can fail closed.
func RecordEventStrict(ctx context.Context, recorder EventRecorder, typ string, opts ...EventOption) (err error) {
	if recorder == nil {
		return fmt.Errorf("strict execution event recorder is not configured")
	}
	strict, ok := recorder.(StrictEventRecorder)
	if !ok {
		return fmt.Errorf("execution event recorder %T does not support strict recording", recorder)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("strict execution event recorder panicked: %v", recovered)
		}
	}()
	return strict.RecordStrict(ctx, typ, opts...)
}

// FakeEventRecorder captures worker events in memory for tests.
type FakeEventRecorder struct {
	mu     sync.Mutex
	now    func() time.Time
	events []RecordedEvent
}

var _ EventRecorder = (*FakeEventRecorder)(nil)

// NewFakeEventRecorder creates an empty fake event recorder.
func NewFakeEventRecorder() *FakeEventRecorder {
	return NewFakeEventRecorderWithClock(time.Now)
}

// NewFakeEventRecorderWithClock creates a fake event recorder with a deterministic clock.
func NewFakeEventRecorderWithClock(now func() time.Time) *FakeEventRecorder {
	if now == nil {
		now = time.Now
	}
	return &FakeEventRecorder{now: now}
}

// Record captures the event after applying options and the shared redaction/truncation contract.
func (r *FakeEventRecorder) Record(ctx context.Context, typ string, opts ...EventOption) {
	if r == nil {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		return
	}
	event := RecordedEvent{
		Type:      events.NormalizeExecutionEventType(typ),
		Severity:  events.ExecutionEventSeverityInfo,
		CreatedAt: r.now().UTC(),
	}
	if event.Type == "" {
		event.Type = typ
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&event)
		}
	}
	payload, err := events.SanitizeExecutionEventPayload(event.Summary, event.Content, event.ContentText)
	if err == nil {
		event.Summary = payload.Summary
		event.Content = payload.Content
		event.ContentText = payload.ContentText
		event.Truncation = payload.Truncation
	} else {
		event.Summary, event.ContentText, event.Truncation = sanitizeRecordedEventTextFields(event.Summary, event.ContentText)
		event.Content = nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, cloneRecordedEvent(event))
}

// RecordStrict captures the event synchronously and returns context errors.
func (r *FakeEventRecorder) RecordStrict(ctx context.Context, typ string, opts ...EventOption) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	r.Record(ctx, typ, opts...)
	return nil
}

// Events returns a snapshot of recorded events.
func (r *FakeEventRecorder) Events() []RecordedEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedEvent, 0, len(r.events))
	for _, event := range r.events {
		out = append(out, cloneRecordedEvent(event))
	}
	return out
}

// EventTypes returns recorded event types in order.
func (r *FakeEventRecorder) EventTypes() []string {
	recorded := r.Events()
	types := make([]string, 0, len(recorded))
	for _, event := range recorded {
		types = append(types, event.Type)
	}
	return types
}

// Reset clears captured events.
func (r *FakeEventRecorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func WithEventSeverity(severity string) EventOption {
	return func(event *RecordedEvent) {
		event.Severity = events.NormalizeExecutionEventSeverity(severity)
	}
}

func WithEventTaskName(taskName string) EventOption {
	return func(event *RecordedEvent) {
		event.TaskName = taskName
	}
}

func WithEventSessionName(sessionName string) EventOption {
	return func(event *RecordedEvent) {
		event.SessionName = sessionName
	}
}

func WithEventAgentName(agentName string) EventOption {
	return func(event *RecordedEvent) {
		event.AgentName = agentName
	}
}

func WithEventToolName(toolName string) EventOption {
	return func(event *RecordedEvent) {
		event.ToolName = toolName
	}
}

func WithEventToolCallID(toolCallID string) EventOption {
	return func(event *RecordedEvent) {
		event.ToolCallID = toolCallID
	}
}

func WithEventSummary(summary string) EventOption {
	return func(event *RecordedEvent) {
		event.Summary = summary
	}
}

func WithEventContent(content json.RawMessage) EventOption {
	return func(event *RecordedEvent) {
		if content == nil {
			event.Content = nil
			return
		}
		event.Content = append(json.RawMessage(nil), content...)
	}
}

func WithEventContentText(contentText string) EventOption {
	return func(event *RecordedEvent) {
		event.ContentText = contentText
	}
}

func cloneRecordedEvent(event RecordedEvent) RecordedEvent {
	if event.Content != nil {
		event.Content = append(json.RawMessage(nil), event.Content...)
	}
	if event.Truncation != nil {
		truncation := *event.Truncation
		event.Truncation = &truncation
	}
	return event
}

func sanitizeRecordedEventTextFields(summary, contentText string) (string, string, *events.ExecutionEventTruncation) {
	var metadata events.ExecutionEventTruncation
	var truncated bool
	var originalChars int

	summary, truncated, originalChars = events.RedactAndTruncateExecutionEventText(
		summary,
		events.MaxExecutionEventSummaryChars,
	)
	if truncated {
		metadata.SummaryTruncated = true
		metadata.SummaryOriginalChars = originalChars
	}
	contentText, truncated, originalChars = events.RedactAndTruncateExecutionEventText(
		contentText,
		events.MaxExecutionEventContentTextChars,
	)
	if truncated {
		metadata.ContentTextTruncated = true
		metadata.ContentTextOriginalChars = originalChars
	}
	if metadata.Empty() {
		return summary, contentText, nil
	}
	return summary, contentText, &metadata
}
