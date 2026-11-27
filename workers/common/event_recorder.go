package common

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/events"
)

const defaultDetachedEventTimeout = 250 * time.Millisecond

type EventRecorder interface {
	Record(ctx context.Context, typ string, opts ...EventOption)
}

type StrictEventRecorder interface {
	RecordStrict(ctx context.Context, typ string, opts ...EventOption) error
}

type EventOption func(*RecordedEvent)

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

type NoopEventRecorder struct{}

var _ EventRecorder = NoopEventRecorder{}

func (NoopEventRecorder) Record(context.Context, string, ...EventOption) {}

func (NoopEventRecorder) RecordStrict(context.Context, string, ...EventOption) error {
	return fmt.Errorf("strict execution event recorder is not configured")
}

func RecordEvent(ctx context.Context, recorder EventRecorder, typ string, opts ...EventOption) {
	if recorder == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	recorder.Record(ctx, typ, opts...)
}

func RecordEventWithTimeout(recorder EventRecorder, typ string, timeout time.Duration, opts ...EventOption) {
	if timeout <= 0 {
		timeout = defaultDetachedEventTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	RecordEvent(ctx, recorder, typ, opts...)
}

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

type FakeEventRecorder struct {
	mu     sync.Mutex
	now    func() time.Time
	events []RecordedEvent
}

var _ EventRecorder = (*FakeEventRecorder)(nil)

func NewFakeEventRecorder() *FakeEventRecorder {
	return NewFakeEventRecorderWithClock(time.Now)
}

func NewFakeEventRecorderWithClock(now func() time.Time) *FakeEventRecorder {
	if now == nil {
		now = time.Now
	}
	return &FakeEventRecorder{now: now}
}

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

func (r *FakeEventRecorder) RecordStrict(ctx context.Context, typ string, opts ...EventOption) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	r.Record(ctx, typ, opts...)
	return nil
}

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

func (r *FakeEventRecorder) EventTypes() []string {
	recorded := r.Events()
	types := make([]string, 0, len(recorded))
	for _, event := range recorded {
		types = append(types, event.Type)
	}
	return types
}

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
