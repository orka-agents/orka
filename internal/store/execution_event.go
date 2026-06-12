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
	"github.com/sozercan/orka/internal/metrics"
)

const (
	unknownMetricLabel = "unknown"

	ExecutionEventStreamTypeTask    = events.ExecutionEventStreamTypeTask
	ExecutionEventStreamTypeSession = events.ExecutionEventStreamTypeSession

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

// SanitizeExecutionEventPayloadFields applies the shared event redaction and
// truncation contract to store-facing event payload fields. Stores call this as
// a defense-in-depth boundary so direct appends cannot persist obvious secrets.
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
	metrics.RecordExecutionEventPayloadSanitization(
		event.StreamType,
		event.Type,
		executionEventPayloadContainsRedactionMarker(event),
		event.Truncation != nil && !event.Truncation.Empty(),
	)
	return nil
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

type executionEventStreamKey struct {
	namespace  string
	streamType string
	streamID   string
}

type sessionExecutionEventKey struct {
	namespace   string
	sessionName string
}

// FakeExecutionEventStore is an in-memory test implementation of ExecutionEventStore.
type FakeExecutionEventStore struct {
	mu            sync.Mutex
	now           func() time.Time
	events        []ExecutionEvent
	latest        map[executionEventStreamKey]int64
	latestSession map[sessionExecutionEventKey]int64
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
		now:           now,
		latest:        make(map[executionEventStreamKey]int64),
		latestSession: make(map[sessionExecutionEventKey]int64),
	}
}

// AppendExecutionEvent appends an event and assigns a per-stream sequence when missing.
func (s *FakeExecutionEventStore) AppendExecutionEvent(ctx context.Context, event *ExecutionEvent) (*ExecutionEvent, error) {
	started := time.Now()
	metricStreamType, metricEventType := metricLabelsForExecutionEvent(event)
	success := false
	defer func() {
		metrics.RecordExecutionEventAppend(metricStreamType, metricEventType, success, time.Since(started).Seconds())
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if event == nil {
		return nil, ValidationErrorf("execution event is required")
	}
	copy := cloneExecutionEvent(*event)
	copy.Namespace = strings.TrimSpace(copy.Namespace)
	copy.StreamType = strings.TrimSpace(copy.StreamType)
	if copy.StreamType == "" {
		copy.StreamType = ExecutionEventStreamTypeTask
	}
	copy.StreamID = strings.TrimSpace(copy.StreamID)
	copy.Type = strings.TrimSpace(copy.Type)
	copy.Severity = events.NormalizeExecutionEventSeverity(copy.Severity)
	copy.TaskName = strings.TrimSpace(copy.TaskName)
	copy.SessionName = strings.TrimSpace(copy.SessionName)
	copy.AgentName = strings.TrimSpace(copy.AgentName)
	copy.ToolName = strings.TrimSpace(copy.ToolName)
	copy.ToolCallID = strings.TrimSpace(copy.ToolCallID)

	if copy.Namespace == "" {
		return nil, ValidationErrorf("execution event namespace is required")
	}
	if !events.IsValidExecutionEventStreamType(copy.StreamType) {
		return nil, ValidationErrorf("unsupported execution event stream type %q", copy.StreamType)
	}
	if copy.StreamID == "" {
		return nil, ValidationErrorf("execution event stream id is required")
	}
	if !events.IsValidExecutionEventType(copy.Type) {
		return nil, ValidationErrorf("unsupported execution event type %q", copy.Type)
	}
	if err := SanitizeExecutionEventPayloadFields(&copy); err != nil {
		return nil, ValidationErrorf("invalid execution event payload: %v", err)
	}
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = s.now().UTC()
	}

	key := executionEventStreamKey{namespace: copy.Namespace, streamType: copy.StreamType, streamID: copy.StreamID}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existingType, approvalID, conflict := s.existingTerminalApprovalEvent(copy); conflict {
		return nil, TerminalApprovalConflict(existingType, approvalID)
	}

	latest := s.latest[key]
	if copy.Seq == 0 {
		copy.Seq = latest + 1
	} else if copy.Seq <= latest {
		return nil, ValidationErrorf("execution event seq must increase for stream %s/%s/%s", copy.Namespace, copy.StreamType, copy.StreamID)
	}
	copy.ID = strings.TrimSpace(copy.ID)
	if copy.ID == "" {
		copy.ID = fmt.Sprintf("%s/%s/%s/%d", copy.Namespace, copy.StreamType, copy.StreamID, copy.Seq)
	}
	s.latest[key] = copy.Seq
	if copy.SessionName != "" {
		sessionKey := sessionExecutionEventKey{namespace: copy.Namespace, sessionName: copy.SessionName}
		s.latestSession[sessionKey]++
		if copy.Internal == nil {
			copy.Internal = map[string]any{}
		}
		copy.Internal["sessionSeq"] = s.latestSession[sessionKey]
	}
	s.events = append(s.events, cloneExecutionEvent(copy))
	success = true
	return &copy, nil
}

// ListExecutionEvents returns events matching filter in ascending sequence order per stream.
func (s *FakeExecutionEventStore) ListExecutionEvents(ctx context.Context, filter ExecutionEventFilter) ([]ExecutionEvent, error) {
	started := time.Now()
	success := false
	defer func() {
		metrics.RecordExecutionEventList("task_store", success, time.Since(started).Seconds())
	}()
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
	success = true
	return matches, nil
}

// ListSessionExecutionEvents returns task-derived events for one session with an aggregated cursor.
func (s *FakeExecutionEventStore) ListSessionExecutionEvents(ctx context.Context, filter SessionExecutionEventFilter) ([]SessionExecutionEvent, int64, error) {
	started := time.Now()
	success := false
	defer func() {
		metrics.RecordExecutionEventList("session_store", success, time.Since(started).Seconds())
	}()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	filter = filter.Normalized()
	if err := filter.Validate(); err != nil {
		return nil, 0, err
	}
	types := make(map[string]struct{}, len(filter.EventTypes))
	for _, typ := range filter.EventTypes {
		types[typ] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sessionKey := sessionExecutionEventKey{namespace: filter.Namespace, sessionName: filter.SessionName}
	latestSeq := s.latestSession[sessionKey]
	matches := make([]SessionExecutionEvent, 0, min(len(s.events), filter.Limit))
	for _, event := range s.events {
		if event.Namespace != filter.Namespace || event.SessionName != filter.SessionName {
			continue
		}
		event = cloneExecutionEvent(event)
		sessionSeq := fakeExecutionEventSessionSeq(event)
		if sessionSeq <= filter.AfterSeq {
			continue
		}
		if len(types) > 0 {
			if _, ok := types[event.Type]; !ok {
				continue
			}
		}
		matches = append(matches, SessionExecutionEvent{
			ExecutionEvent: event,
			SessionSeq:     sessionSeq,
			TaskSeq:        event.Seq,
		})
		if len(matches) >= filter.Limit {
			break
		}
	}
	success = true
	return matches, latestSeq, nil
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

	kept := s.events[:0]
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

func (s *FakeExecutionEventStore) existingTerminalApprovalEvent(event ExecutionEvent) (existingType, approvalID string, conflict bool) {
	if !IsTerminalApprovalExecutionEventType(event.Type) {
		return "", "", false
	}
	approvalID = ApprovalIDFromExecutionEvent(event)
	if approvalID == "" {
		return "", "", false
	}
	for _, existing := range s.events {
		if existing.Namespace != event.Namespace ||
			existing.StreamType != event.StreamType ||
			existing.StreamID != event.StreamID ||
			!IsTerminalApprovalExecutionEventType(existing.Type) {
			continue
		}
		if ApprovalIDFromExecutionEvent(existing) == approvalID {
			return existing.Type, approvalID, true
		}
	}
	return "", approvalID, false
}

func metricLabelsForExecutionEvent(event *ExecutionEvent) (string, string) {
	if event == nil {
		return unknownMetricLabel, unknownMetricLabel
	}
	streamType := strings.TrimSpace(event.StreamType)
	if streamType == "" {
		streamType = ExecutionEventStreamTypeTask
	}
	if !events.IsValidExecutionEventStreamType(streamType) {
		streamType = unknownMetricLabel
	}
	eventType := strings.TrimSpace(event.Type)
	if !events.IsValidExecutionEventType(eventType) {
		eventType = unknownMetricLabel
	}
	return streamType, eventType
}

func executionEventPayloadContainsRedactionMarker(event *ExecutionEvent) bool {
	if event == nil {
		return false
	}
	return strings.Contains(event.Summary, events.ExecutionEventRedactedValue) ||
		strings.Contains(event.ContentText, events.ExecutionEventRedactedValue) ||
		strings.Contains(string(event.Content), events.ExecutionEventRedactedValue)
}

func fakeExecutionEventSessionSeq(event ExecutionEvent) int64 {
	if event.Internal == nil {
		return 0
	}
	switch value := event.Internal["sessionSeq"].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
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
