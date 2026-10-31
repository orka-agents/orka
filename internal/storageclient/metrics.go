package storageclient

import (
	"context"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

// The client records Orka's execution-event store metrics: appends, store
// reads and payload redaction and truncation. Providers record none of them,
// so every provider, including one running in another process, reports the
// same series in Orka's registry and nothing is counted twice.

const (
	unknownMetricLabel = "unknown"

	// Store reads use these list scopes; the API records its own reads as
	// task_api and session_api.
	taskStoreListScope    = "task_store"
	sessionStoreListScope = "session_store"
)

// ListExecutionEvents reads execution events and records the store read.
func (c *Client) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	started := time.Now()
	listed, err := c.StateProvider.ListExecutionEvents(ctx, filter)
	metrics.RecordExecutionEventList(taskStoreListScope, err == nil, time.Since(started).Seconds())
	return listed, err
}

// ListSessionExecutionEvents reads one Session's execution events and records
// the store read.
func (c *Client) ListSessionExecutionEvents(
	ctx context.Context,
	filter store.SessionExecutionEventFilter,
) ([]store.SessionExecutionEvent, int64, error) {
	started := time.Now()
	listed, latestSeq, err := c.StateProvider.ListSessionExecutionEvents(ctx, filter)
	metrics.RecordExecutionEventList(sessionStoreListScope, err == nil, time.Since(started).Seconds())
	return listed, latestSeq, err
}

// recordExecutionEventAppend records one append attempt, timed from the start
// of the client call and labelled by the event the caller submitted. An event
// the client refuses counts as a failure, as does a provider error, and a
// deduplicated replay counts as an appended event. An invalid deduplication
// key or a missing plan is not an append attempt and is not counted; the
// client checks those before calling the provider, as SQLite did before it
// started timing.
func recordExecutionEventAppend(event *store.ExecutionEvent, success bool, started time.Time) {
	streamType, eventType := executionEventMetricLabels(event)
	metrics.RecordExecutionEventAppend(streamType, eventType, success, time.Since(started).Seconds())
}

// recordExecutionEventSanitization counts redaction and truncation once per
// newly persisted event, from its final stored state.
func recordExecutionEventSanitization(submitted, appended *store.ExecutionEvent) {
	streamType, eventType := executionEventMetricLabels(submitted)
	redacted, truncated := store.ExecutionEventPayloadSanitizationSignals(appended)
	metrics.RecordExecutionEventPayloadSanitization(streamType, eventType, redacted, truncated)
}

func executionEventMetricLabels(event *store.ExecutionEvent) (string, string) {
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
