package storageclient

import (
	"context"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

const (
	unknownMetricLabel = "unknown"

	taskStoreListScope    = "task_store"
	sessionStoreListScope = "session_store"
)

func (c *Client) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	started := time.Now()
	listed, err := c.StateProvider.ListExecutionEvents(ctx, filter)
	metrics.RecordExecutionEventList(taskStoreListScope, err == nil, time.Since(started).Seconds())
	return listed, err
}

func (c *Client) ListSessionExecutionEvents(
	ctx context.Context,
	filter store.SessionExecutionEventFilter,
) ([]store.SessionExecutionEvent, int64, error) {
	started := time.Now()
	listed, latestSeq, err := c.StateProvider.ListSessionExecutionEvents(ctx, filter)
	metrics.RecordExecutionEventList(sessionStoreListScope, err == nil, time.Since(started).Seconds())
	return listed, latestSeq, err
}

func recordExecutionEventAppend(event *store.ExecutionEvent, success bool, started time.Time) {
	streamType, eventType := executionEventMetricLabels(event)
	metrics.RecordExecutionEventAppend(streamType, eventType, success, time.Since(started).Seconds())
}

func recordExecutionEventGroupAppend(appends []store.ExecutionEventAppend, success bool, started time.Time) {
	for _, entry := range appends {
		recordExecutionEventAppend(entry.Event, success, started)
	}
}

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
