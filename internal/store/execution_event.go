package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
)

const (
	ExecutionEventStreamTypeTask    = events.ExecutionEventStreamTypeTask
	ExecutionEventStreamTypeSession = events.ExecutionEventStreamTypeSession

	DefaultExecutionEventLimit      = 100
	MaxExecutionEventLimit          = 1000
	MaxExecutionEventDedupeKeyBytes = 512
)

// ExecutionEvent is the store-facing representation of a task execution timeline event.
// Seq is monotonically increasing per (namespace, stream_type, stream_id).
type ExecutionEvent struct {
	ID          string                           `json:"id"`
	Namespace   string                           `json:"namespace"`
	StreamType  string                           `json:"streamType"`
	StreamID    string                           `json:"streamID"`
	Seq         int64                            `json:"seq"`
	Type        string                           `json:"type"`
	Severity    string                           `json:"severity"`
	TaskName    string                           `json:"taskName,omitempty"`
	SessionName string                           `json:"sessionName,omitempty"`
	AgentName   string                           `json:"agentName,omitempty"`
	ToolName    string                           `json:"toolName,omitempty"`
	ToolCallID  string                           `json:"toolCallID,omitempty"`
	Summary     string                           `json:"summary,omitempty"`
	Content     json.RawMessage                  `json:"content,omitempty"`
	ContentText string                           `json:"contentText,omitempty"`
	Truncation  *events.ExecutionEventTruncation `json:"truncation,omitempty"`
	CreatedAt   time.Time                        `json:"createdAt"`

	// Internal carries store/backend-only metadata and is intentionally omitted from API DTOs.
	Internal map[string]any `json:"-"`
}

// ExecutionEventFilter constrains execution event list queries.
type ExecutionEventFilter struct {
	Namespace   string
	StreamType  string
	StreamID    string
	TaskName    string
	SessionName string
	EventTypes  []string
	AfterSeq    int64
	Limit       int
}

// SessionExecutionEventFilter constrains the aggregated session event read model.
// AfterSeq is a session-level cursor assigned by deterministic ordering over task events.
type SessionExecutionEventFilter struct {
	Namespace   string
	SessionName string
	EventTypes  []string
	AfterSeq    int64
	Limit       int
}

// SessionExecutionEvent is a task-derived event with an aggregated session sequence.
type SessionExecutionEvent struct {
	ExecutionEvent
	SessionSeq int64
	TaskSeq    int64
}

// Normalized returns a copy of f with whitespace trimmed and limit defaults applied.
func (f ExecutionEventFilter) Normalized() ExecutionEventFilter {
	f.Namespace = strings.TrimSpace(f.Namespace)
	f.StreamType = strings.TrimSpace(f.StreamType)
	if f.StreamType == "" {
		f.StreamType = ExecutionEventStreamTypeTask
	}
	f.StreamID = strings.TrimSpace(f.StreamID)
	f.TaskName = strings.TrimSpace(f.TaskName)
	f.SessionName = strings.TrimSpace(f.SessionName)
	if f.Limit <= 0 {
		f.Limit = DefaultExecutionEventLimit
	} else if f.Limit > MaxExecutionEventLimit {
		f.Limit = MaxExecutionEventLimit
	}
	if f.AfterSeq < 0 {
		f.AfterSeq = 0
	}
	if len(f.EventTypes) > 0 {
		types := make([]string, 0, len(f.EventTypes))
		for _, typ := range f.EventTypes {
			if typ = strings.TrimSpace(typ); typ != "" {
				types = append(types, typ)
			}
		}
		f.EventTypes = types
	}
	return f
}

// Validate reports unsupported filter values.
func (f ExecutionEventFilter) Validate() error {
	f = f.Normalized()
	if !events.IsValidExecutionEventStreamType(f.StreamType) {
		return ValidationErrorf("unsupported execution event stream type %q", f.StreamType)
	}
	for _, typ := range f.EventTypes {
		if !events.IsValidExecutionEventType(typ) {
			return ValidationErrorf("unsupported execution event type %q", typ)
		}
	}
	return nil
}

// Normalized returns a copy of f with whitespace trimmed and limit defaults applied.
func (f SessionExecutionEventFilter) Normalized() SessionExecutionEventFilter {
	f.Namespace = strings.TrimSpace(f.Namespace)
	f.SessionName = strings.TrimSpace(f.SessionName)
	if f.Limit <= 0 {
		f.Limit = DefaultExecutionEventLimit
	} else if f.Limit > MaxExecutionEventLimit {
		f.Limit = MaxExecutionEventLimit
	}
	if f.AfterSeq < 0 {
		f.AfterSeq = 0
	}
	if len(f.EventTypes) > 0 {
		types := make([]string, 0, len(f.EventTypes))
		for _, typ := range f.EventTypes {
			if typ = strings.TrimSpace(typ); typ != "" {
				types = append(types, typ)
			}
		}
		f.EventTypes = types
	}
	return f
}

// Validate reports unsupported session event read-model filter values.
func (f SessionExecutionEventFilter) Validate() error {
	f = f.Normalized()
	if f.Namespace == "" {
		return ValidationErrorf("session event namespace is required")
	}
	if f.SessionName == "" {
		return ValidationErrorf("session name is required")
	}
	for _, typ := range f.EventTypes {
		if !events.IsValidExecutionEventType(typ) {
			return ValidationErrorf("unsupported execution event type %q", typ)
		}
	}
	return nil
}

func IsTerminalApprovalExecutionEventType(value string) bool {
	switch value {
	case events.ExecutionEventTypeApprovalApproved,
		events.ExecutionEventTypeApprovalDeclined,
		events.ExecutionEventTypeApprovalExpired,
		events.ExecutionEventTypeApprovalCancelled:
		return true
	default:
		return false
	}
}

func ApprovalIDFromExecutionEvent(event ExecutionEvent) string {
	toolCallID := strings.TrimSpace(event.ToolCallID)
	if len(event.Content) == 0 {
		return toolCallID
	}
	var content map[string]any
	if err := json.Unmarshal(event.Content, &content); err != nil {
		return toolCallID
	}
	for _, key := range []string{"approvalID", "approvalId", "approval_id", "id"} {
		if value, ok := content[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return toolCallID
}

func TerminalApprovalConflict(existingType, approvalID string) error {
	return fmt.Errorf("%w: approval %q already has terminal event %s", ErrConflict, approvalID, existingType)
}

// ExecutionEventStore defines the persistence/query contract for execution events.
type ExecutionEventStore interface {
	AppendExecutionEvent(ctx context.Context, event *ExecutionEvent) (*ExecutionEvent, error)
	ListExecutionEvents(ctx context.Context, filter ExecutionEventFilter) ([]ExecutionEvent, error)
	ListSessionExecutionEvents(ctx context.Context, filter SessionExecutionEventFilter) ([]SessionExecutionEvent, int64, error)
	GetLatestExecutionEventSeq(ctx context.Context, namespace, streamType, streamID string) (int64, error)
	DeleteExecutionEvents(ctx context.Context, namespace, streamType, streamID string) error
}

// DeduplicatingExecutionEventStore atomically appends one event for a caller-
// supplied key scoped to its execution-event stream. A repeated key returns the
// existing event with appended=false.
type DeduplicatingExecutionEventStore interface {
	ExecutionEventStore
	AppendExecutionEventIfAbsent(
		ctx context.Context,
		event *ExecutionEvent,
		dedupeKey string,
	) (persisted *ExecutionEvent, appended bool, err error)
}

// AtomicExecutionEventPlanStore persists a deduplicated execution event and
// its plan projection in one transaction. A repeated event key returns the
// existing event without changing the current plan projection.
type AtomicExecutionEventPlanStore interface {
	DeduplicatingExecutionEventStore
	AppendExecutionEventWithPlanIfAbsent(
		ctx context.Context,
		event *ExecutionEvent,
		dedupeKey string,
		plan *PlanState,
	) (persisted *ExecutionEvent, appended bool, err error)
}

// NormalizeExecutionEventDedupeKey validates and normalizes an opaque store-
// internal execution-event deduplication key.
func NormalizeExecutionEventDedupeKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ValidationErrorf("execution event dedupe key is required")
	}
	if len(value) > MaxExecutionEventDedupeKeyBytes {
		return "", ValidationErrorf("execution event dedupe key exceeds %d bytes", MaxExecutionEventDedupeKeyBytes)
	}
	return value, nil
}

// SanitizeExecutionEventPayloadFields applies the shared event redaction and
// truncation contract to store-facing event payload fields. Stores call this as
// a defense-in-depth boundary so direct appends cannot persist obvious secrets.
//
// It deliberately does NOT record redaction/truncation metrics: this function is
// invoked more than once per logical event (e.g. the harness mapper sanitizes,
// then the store sanitizes again on append), and a worker may have already
// redacted the payload in a different process before submission. Recording here
// therefore over- or under-counts. The redaction/truncation metric is recorded
// exactly once per persisted event at the store append boundary, from the final
// event state (see ExecutionEventPayloadSanitizationSignals).
func SanitizeExecutionEventPayloadFields(event *ExecutionEvent) error {
	if event == nil {
		return nil
	}
	payload, err := events.SanitizeExecutionEventPayload(event.Summary, event.Content, event.ContentText)
	if err != nil {
		return err
	}
	event.Summary = payload.Summary
	event.Content = payload.Content
	event.ContentText = payload.ContentText
	event.Truncation = MergeExecutionEventTruncation(event.Truncation, payload.Truncation)
	return nil
}

// ExecutionEventPayloadSanitizationSignals reports whether the final, persisted
// event payload shows evidence of redaction (the redaction marker is present in
// any text/JSON field) and/or truncation (truncation metadata is set). It is
// derived from the event's terminal state so it is correct regardless of which
// pass or process performed the sanitization, and is intended to be recorded
// exactly once per successful append.
func ExecutionEventPayloadSanitizationSignals(event *ExecutionEvent) (redacted, truncated bool) {
	if event == nil {
		return false, false
	}
	redacted = executionEventPayloadContainsRedactionMarker(event)
	truncated = event.Truncation != nil && !event.Truncation.Empty()
	return redacted, truncated
}

// MergeExecutionEventTruncation combines truncation metadata, preserving the
// highest original lengths without exposing raw values.
func MergeExecutionEventTruncation(values ...*events.ExecutionEventTruncation) *events.ExecutionEventTruncation {
	var merged events.ExecutionEventTruncation
	for _, value := range values {
		if value == nil {
			continue
		}
		merged.SummaryTruncated = merged.SummaryTruncated || value.SummaryTruncated
		merged.SummaryOriginalChars = max(merged.SummaryOriginalChars, value.SummaryOriginalChars)
		merged.ContentTextTruncated = merged.ContentTextTruncated || value.ContentTextTruncated
		merged.ContentTextOriginalChars = max(merged.ContentTextOriginalChars, value.ContentTextOriginalChars)
		merged.ContentJSONTruncated = merged.ContentJSONTruncated || value.ContentJSONTruncated
		merged.ContentJSONOriginalBytes = max(merged.ContentJSONOriginalBytes, value.ContentJSONOriginalBytes)
	}
	if merged.Empty() {
		return nil
	}
	return &merged
}

func executionEventPayloadContainsRedactionMarker(event *ExecutionEvent) bool {
	if event == nil {
		return false
	}
	return strings.Contains(event.Summary, events.ExecutionEventRedactedValue) ||
		strings.Contains(event.ContentText, events.ExecutionEventRedactedValue) ||
		strings.Contains(string(event.Content), events.ExecutionEventRedactedValue)
}
