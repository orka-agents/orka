package store

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/orka/internal/events"
)

const (
	ExecutionEventStreamTypeTask = events.ExecutionEventStreamTypeTask

	DefaultExecutionEventLimit = 100
	MaxExecutionEventLimit     = 1000
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

// Validate reports unsupported Wave 0 filter values.
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

// ExecutionEventStore defines the persistence/query contract for execution events.
type ExecutionEventStore interface {
	AppendExecutionEvent(ctx context.Context, event *ExecutionEvent) (*ExecutionEvent, error)
	ListExecutionEvents(ctx context.Context, filter ExecutionEventFilter) ([]ExecutionEvent, error)
	GetLatestExecutionEventSeq(ctx context.Context, namespace, streamType, streamID string) (int64, error)
	DeleteExecutionEvents(ctx context.Context, namespace, streamType, streamID string) error
}

type executionEventStreamKey struct {
	namespace  string
	streamType string
	streamID   string
}

// FakeExecutionEventStore is an in-memory test implementation of ExecutionEventStore.
type FakeExecutionEventStore struct {
	mu     sync.Mutex
	now    func() time.Time
	events []ExecutionEvent
	latest map[executionEventStreamKey]int64
}

var _ ExecutionEventStore = (*FakeExecutionEventStore)(nil)

// NewFakeExecutionEventStore creates an empty in-memory execution event store for tests.
func NewFakeExecutionEventStore() *FakeExecutionEventStore {
	return NewFakeExecutionEventStoreWithClock(time.Now)
}

// NewFakeExecutionEventStoreWithClock creates a fake store with a deterministic clock.
func NewFakeExecutionEventStoreWithClock(now func() time.Time) *FakeExecutionEventStore {
	if now == nil {
		now = time.Now
	}
	return &FakeExecutionEventStore{
		now:    now,
		latest: make(map[executionEventStreamKey]int64),
	}
}

// AppendExecutionEvent appends an event and assigns a per-stream sequence when missing.
func (s *FakeExecutionEventStore) AppendExecutionEvent(ctx context.Context, event *ExecutionEvent) (*ExecutionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if event == nil {
		return nil, ValidationErrorf("execution event is required")
	}
	eventCopy := cloneExecutionEvent(*event)
	eventCopy.Namespace = strings.TrimSpace(eventCopy.Namespace)
	eventCopy.StreamType = strings.TrimSpace(eventCopy.StreamType)
	if eventCopy.StreamType == "" {
		eventCopy.StreamType = ExecutionEventStreamTypeTask
	}
	eventCopy.StreamID = strings.TrimSpace(eventCopy.StreamID)
	eventCopy.Type = strings.TrimSpace(eventCopy.Type)
	eventCopy.Severity = events.NormalizeExecutionEventSeverity(eventCopy.Severity)
	eventCopy.TaskName = strings.TrimSpace(eventCopy.TaskName)
	eventCopy.SessionName = strings.TrimSpace(eventCopy.SessionName)
	eventCopy.AgentName = strings.TrimSpace(eventCopy.AgentName)
	eventCopy.ToolName = strings.TrimSpace(eventCopy.ToolName)
	eventCopy.ToolCallID = strings.TrimSpace(eventCopy.ToolCallID)

	if eventCopy.Namespace == "" {
		return nil, ValidationErrorf("execution event namespace is required")
	}
	if !events.IsValidExecutionEventStreamType(eventCopy.StreamType) {
		return nil, ValidationErrorf("unsupported execution event stream type %q", eventCopy.StreamType)
	}
	if eventCopy.StreamID == "" {
		return nil, ValidationErrorf("execution event stream id is required")
	}
	if !events.IsValidExecutionEventType(eventCopy.Type) {
		return nil, ValidationErrorf("unsupported execution event type %q", eventCopy.Type)
	}
	payload, err := events.SanitizeExecutionEventPayload(eventCopy.Summary, eventCopy.Content, eventCopy.ContentText)
	if err != nil {
		return nil, ValidationErrorf("invalid execution event payload: %v", err)
	}
	eventCopy.Summary = payload.Summary
	eventCopy.Content = payload.Content
	eventCopy.ContentText = payload.ContentText
	eventCopy.Truncation = mergeExecutionEventTruncation(eventCopy.Truncation, payload.Truncation)
	if eventCopy.CreatedAt.IsZero() {
		eventCopy.CreatedAt = s.now().UTC()
	}

	key := executionEventStreamKey{namespace: eventCopy.Namespace, streamType: eventCopy.StreamType, streamID: eventCopy.StreamID}

	s.mu.Lock()
	defer s.mu.Unlock()

	latest := s.latest[key]
	if eventCopy.Seq == 0 {
		eventCopy.Seq = latest + 1
	} else if eventCopy.Seq <= latest {
		return nil, ValidationErrorf("execution event seq must increase for stream %s/%s/%s", eventCopy.Namespace, eventCopy.StreamType, eventCopy.StreamID)
	}
	eventCopy.ID = strings.TrimSpace(eventCopy.ID)
	if eventCopy.ID == "" {
		eventCopy.ID = fmt.Sprintf("%s/%s/%s/%d", eventCopy.Namespace, eventCopy.StreamType, eventCopy.StreamID, eventCopy.Seq)
	}
	s.latest[key] = eventCopy.Seq
	s.events = append(s.events, cloneExecutionEvent(eventCopy))
	return &eventCopy, nil
}

// ListExecutionEvents returns events matching filter in ascending sequence order per stream.
func (s *FakeExecutionEventStore) ListExecutionEvents(ctx context.Context, filter ExecutionEventFilter) ([]ExecutionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filter = filter.Normalized()
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	types := make(map[string]struct{}, len(filter.EventTypes))
	for _, typ := range filter.EventTypes {
		types[typ] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	matches := make([]ExecutionEvent, 0, len(s.events))
	for _, event := range s.events {
		if filter.Namespace != "" && event.Namespace != filter.Namespace {
			continue
		}
		if filter.StreamType != "" && event.StreamType != filter.StreamType {
			continue
		}
		if filter.StreamID != "" && event.StreamID != filter.StreamID {
			continue
		}
		if filter.TaskName != "" && event.TaskName != filter.TaskName {
			continue
		}
		if filter.SessionName != "" && event.SessionName != filter.SessionName {
			continue
		}
		if len(types) > 0 {
			if _, ok := types[event.Type]; !ok {
				continue
			}
		}
		if event.Seq <= filter.AfterSeq {
			continue
		}
		matches = append(matches, cloneExecutionEvent(event))
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Namespace != matches[j].Namespace {
			return matches[i].Namespace < matches[j].Namespace
		}
		if matches[i].StreamType != matches[j].StreamType {
			return matches[i].StreamType < matches[j].StreamType
		}
		if matches[i].StreamID != matches[j].StreamID {
			return matches[i].StreamID < matches[j].StreamID
		}
		return matches[i].Seq < matches[j].Seq
	})
	if len(matches) > filter.Limit {
		matches = matches[:filter.Limit]
	}
	return matches, nil
}

// GetLatestExecutionEventSeq returns the latest sequence for a stream or zero when empty.
func (s *FakeExecutionEventStore) GetLatestExecutionEventSeq(ctx context.Context, namespace, streamType, streamID string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	filter := ExecutionEventFilter{Namespace: namespace, StreamType: streamType, StreamID: streamID}.Normalized()
	if err := filter.Validate(); err != nil {
		return 0, err
	}
	key := executionEventStreamKey{namespace: filter.Namespace, streamType: filter.StreamType, streamID: filter.StreamID}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[key], nil
}

// DeleteExecutionEvents removes all events for a stream.
func (s *FakeExecutionEventStore) DeleteExecutionEvents(ctx context.Context, namespace, streamType, streamID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	filter := ExecutionEventFilter{Namespace: namespace, StreamType: streamType, StreamID: streamID}.Normalized()
	if err := filter.Validate(); err != nil {
		return err
	}
	key := executionEventStreamKey{namespace: filter.Namespace, streamType: filter.StreamType, streamID: filter.StreamID}

	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]ExecutionEvent, 0, len(s.events))
	for _, event := range s.events {
		if event.Namespace == filter.Namespace && event.StreamType == filter.StreamType && event.StreamID == filter.StreamID {
			continue
		}
		kept = append(kept, event)
	}
	s.events = kept
	delete(s.latest, key)
	return nil
}

func mergeExecutionEventTruncation(existing, sanitized *events.ExecutionEventTruncation) *events.ExecutionEventTruncation {
	if existing == nil {
		return cloneExecutionEventTruncation(sanitized)
	}
	if sanitized == nil {
		return cloneExecutionEventTruncation(existing)
	}
	merged := *existing
	merged.SummaryTruncated = merged.SummaryTruncated || sanitized.SummaryTruncated
	merged.SummaryOriginalChars = max(merged.SummaryOriginalChars, sanitized.SummaryOriginalChars)
	merged.ContentTextTruncated = merged.ContentTextTruncated || sanitized.ContentTextTruncated
	merged.ContentTextOriginalChars = max(merged.ContentTextOriginalChars, sanitized.ContentTextOriginalChars)
	merged.ContentJSONTruncated = merged.ContentJSONTruncated || sanitized.ContentJSONTruncated
	merged.ContentJSONOriginalBytes = max(merged.ContentJSONOriginalBytes, sanitized.ContentJSONOriginalBytes)
	return &merged
}

func cloneExecutionEventTruncation(value *events.ExecutionEventTruncation) *events.ExecutionEventTruncation {
	if value == nil {
		return nil
	}
	truncationCopy := *value
	return &truncationCopy
}

func cloneExecutionEvent(event ExecutionEvent) ExecutionEvent {
	if event.Content != nil {
		event.Content = append(json.RawMessage(nil), event.Content...)
	}
	if event.Internal != nil {
		internal := make(map[string]any, len(event.Internal))
		maps.Copy(internal, event.Internal)
		event.Internal = internal
	}
	if event.Truncation != nil {
		truncation := *event.Truncation
		event.Truncation = &truncation
	}
	return event
}
