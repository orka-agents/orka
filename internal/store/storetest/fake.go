package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

const unknownMetricLabel = "unknown"

type executionEventStreamKey struct {
	namespace  string
	streamType string
	streamID   string
}

type executionEventDedupeIndexKey struct {
	executionEventStreamKey
	dedupeKey string
}

type sessionExecutionEventKey struct {
	namespace   string
	sessionName string
}

// FakeExecutionEventStore is an in-memory test implementation of store.ExecutionEventStore.
type FakeExecutionEventStore struct {
	mu            sync.Mutex
	now           func() time.Time
	events        []store.ExecutionEvent
	latest        map[executionEventStreamKey]int64
	latestSession map[sessionExecutionEventKey]int64
	dedupe        map[executionEventDedupeIndexKey]store.ExecutionEvent
}

var _ store.ExecutionEventStore = (*FakeExecutionEventStore)(nil)
var _ store.DeduplicatingExecutionEventStore = (*FakeExecutionEventStore)(nil)

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
		dedupe:        make(map[executionEventDedupeIndexKey]store.ExecutionEvent),
	}
}

// AppendExecutionEvent appends an event and assigns a per-stream sequence when missing.
func (s *FakeExecutionEventStore) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	appended, _, err := s.appendExecutionEvent(ctx, event, "")
	return appended, err
}

// AppendExecutionEventIfAbsent atomically appends an event only when its
// stream-scoped deduplication key has not already been used.
func (s *FakeExecutionEventStore) AppendExecutionEventIfAbsent(
	ctx context.Context,
	event *store.ExecutionEvent,
	dedupeKey string,
) (*store.ExecutionEvent, bool, error) {
	dedupeKey, err := store.NormalizeExecutionEventDedupeKey(dedupeKey)
	if err != nil {
		return nil, false, err
	}
	return s.appendExecutionEvent(ctx, event, dedupeKey)
}

func (s *FakeExecutionEventStore) appendExecutionEvent(
	ctx context.Context,
	event *store.ExecutionEvent,
	dedupeKey string,
) (*store.ExecutionEvent, bool, error) {
	started := time.Now()
	metricStreamType, metricEventType := metricLabelsForExecutionEvent(event)
	success := false
	defer func() {
		metrics.RecordExecutionEventAppend(metricStreamType, metricEventType, success, time.Since(started).Seconds())
	}()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if event == nil {
		return nil, false, store.ValidationErrorf("execution event is required")
	}
	copy := cloneExecutionEvent(*event)
	copy.Namespace = strings.TrimSpace(copy.Namespace)
	copy.StreamType = strings.TrimSpace(copy.StreamType)
	if copy.StreamType == "" {
		copy.StreamType = store.ExecutionEventStreamTypeTask
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
		return nil, false, store.ValidationErrorf("execution event namespace is required")
	}
	if !events.IsValidExecutionEventStreamType(copy.StreamType) {
		return nil, false, store.ValidationErrorf("unsupported execution event stream type %q", copy.StreamType)
	}
	if copy.StreamID == "" {
		return nil, false, store.ValidationErrorf("execution event stream id is required")
	}
	if !events.IsValidExecutionEventType(copy.Type) {
		return nil, false, store.ValidationErrorf("unsupported execution event type %q", copy.Type)
	}
	if err := store.SanitizeExecutionEventPayloadFields(&copy); err != nil {
		return nil, false, store.ValidationErrorf("invalid execution event payload: %v", err)
	}
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = s.now().UTC()
	}

	key := executionEventStreamKey{namespace: copy.Namespace, streamType: copy.StreamType, streamID: copy.StreamID}

	s.mu.Lock()
	defer s.mu.Unlock()
	indexKey := executionEventDedupeIndexKey{executionEventStreamKey: key, dedupeKey: dedupeKey}
	if dedupeKey != "" {
		if existing, ok := s.dedupe[indexKey]; ok {
			success = true
			clone := cloneExecutionEvent(existing)
			return &clone, false, nil
		}
	}

	if existingType, approvalID, conflict := s.existingTerminalApprovalEvent(copy); conflict {
		return nil, false, store.TerminalApprovalConflict(existingType, approvalID)
	}

	latest := s.latest[key]
	if copy.Seq == 0 {
		copy.Seq = latest + 1
	} else if copy.Seq <= latest {
		return nil, false, store.ValidationErrorf("execution event seq must increase for stream %s/%s/%s", copy.Namespace, copy.StreamType, copy.StreamID)
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
	if dedupeKey != "" {
		s.dedupe[indexKey] = cloneExecutionEvent(copy)
	}
	redacted, truncated := store.ExecutionEventPayloadSanitizationSignals(&copy)
	metrics.RecordExecutionEventPayloadSanitization(metricStreamType, metricEventType, redacted, truncated)
	success = true
	return &copy, true, nil
}

// ListExecutionEvents returns events matching filter in ascending sequence order per stream.
func (s *FakeExecutionEventStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
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

	matches := make([]store.ExecutionEvent, 0, len(s.events))
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
func (s *FakeExecutionEventStore) ListSessionExecutionEvents(ctx context.Context, filter store.SessionExecutionEventFilter) ([]store.SessionExecutionEvent, int64, error) {
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
	matches := make([]store.SessionExecutionEvent, 0, min(len(s.events), filter.Limit))
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
		matches = append(matches, store.SessionExecutionEvent{
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
	filter := store.ExecutionEventFilter{Namespace: namespace, StreamType: streamType, StreamID: streamID}.Normalized()
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
	filter := store.ExecutionEventFilter{Namespace: namespace, StreamType: streamType, StreamID: streamID}.Normalized()
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
	for dedupeKey := range s.dedupe {
		if dedupeKey.executionEventStreamKey == key {
			delete(s.dedupe, dedupeKey)
		}
	}
	return nil
}

func (s *FakeExecutionEventStore) existingTerminalApprovalEvent(event store.ExecutionEvent) (existingType, approvalID string, conflict bool) {
	if !store.IsTerminalApprovalExecutionEventType(event.Type) {
		return "", "", false
	}
	approvalID = store.ApprovalIDFromExecutionEvent(event)
	if approvalID == "" {
		return "", "", false
	}
	for _, existing := range s.events {
		if existing.Namespace != event.Namespace ||
			existing.StreamType != event.StreamType ||
			existing.StreamID != event.StreamID ||
			!store.IsTerminalApprovalExecutionEventType(existing.Type) {
			continue
		}
		if store.ApprovalIDFromExecutionEvent(existing) == approvalID {
			return existing.Type, approvalID, true
		}
	}
	return "", approvalID, false
}

func metricLabelsForExecutionEvent(event *store.ExecutionEvent) (string, string) {
	if event == nil {
		return unknownMetricLabel, unknownMetricLabel
	}
	streamType := strings.TrimSpace(event.StreamType)
	if streamType == "" {
		streamType = store.ExecutionEventStreamTypeTask
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

func fakeExecutionEventSessionSeq(event store.ExecutionEvent) int64 {
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

func cloneExecutionEvent(event store.ExecutionEvent) store.ExecutionEvent {
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
