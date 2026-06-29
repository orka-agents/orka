package store

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	eventtypes "github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/metrics"
)

func TestEventDerivedLatencyRecordsToolCallPairOnce(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	deriver := NewExecutionEventSLODeriver()
	events := []ExecutionEvent{
		execEvent(1, eventtypes.ExecutionEventTypeToolCallStarted, now, "call-1"),
		execEvent(2, eventtypes.ExecutionEventTypeToolCallCompleted, now.Add(500*time.Millisecond), "call-1"),
	}

	deriver.Derive(events)
	deriver.Derive(events)

	if got := derivedHistogramCount(EventSLOToolCall, "success"); got != 1 {
		t.Fatalf("tool call latency count = %v, want 1", got)
	}
}

func TestEventDerivedLatencyRecordsFailureCounterAndLatency(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	deriver := NewExecutionEventSLODeriver()

	deriver.Derive([]ExecutionEvent{
		execEvent(1, eventtypes.ExecutionEventTypeToolCallStarted, now, "call-2"),
		execEvent(2, eventtypes.ExecutionEventTypeToolCallFailed, now.Add(750*time.Millisecond), "call-2"),
	})

	if got := derivedHistogramCount(EventSLOToolCall, "failure"); got != 1 {
		t.Fatalf("tool failure latency count = %v, want 1", got)
	}
	if got := derivedFailureCounter(EventSLOToolCall, eventtypes.ExecutionEventTypeToolCallFailed); got != 1 {
		t.Fatalf("tool failure counter = %v, want 1", got)
	}
}

func TestEventDerivedLatencyMissingCompletionDoesNotRecordLatency(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	deriver := NewExecutionEventSLODeriver()

	deriver.Derive([]ExecutionEvent{execEvent(1, eventtypes.ExecutionEventTypeModelRequestStarted, now, "")})

	if got := derivedHistogramCount(EventSLOModelRequest, "success"); got != 0 {
		t.Fatalf("model latency count = %v, want 0 without completion", got)
	}
}

func TestEventDerivedLatencyFailureWithoutStartCountsFailureOnly(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	deriver := NewExecutionEventSLODeriver()

	deriver.Derive([]ExecutionEvent{execEvent(7, eventtypes.ExecutionEventTypeAgentRuntimeFailed, now, "")})

	if got := derivedHistogramCount(EventSLOAgentRuntime, "failure"); got != 0 {
		t.Fatalf("agent runtime failure latency count = %v, want 0 without start", got)
	}
	if got := derivedFailureCounter(EventSLOAgentRuntime, eventtypes.ExecutionEventTypeAgentRuntimeFailed); got != 1 {
		t.Fatalf("agent runtime failure counter = %v, want 1", got)
	}
}

func TestEventDerivedLatencyCoversConfiguredPairs(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	deriver := NewExecutionEventSLODeriver()

	deriver.Derive([]ExecutionEvent{
		execEvent(1, eventtypes.ExecutionEventTypeTaskStarted, now, ""),
		execEvent(2, eventtypes.ExecutionEventTypeWorkerStarted, now.Add(time.Second), ""),
		execEvent(3, eventtypes.ExecutionEventTypeWorkspacePreparationStarted, now.Add(2*time.Second), ""),
		execEvent(4, eventtypes.ExecutionEventTypeWorkspacePreparationCompleted, now.Add(3*time.Second), ""),
		execEvent(5, eventtypes.ExecutionEventTypeAgentRuntimeStarted, now.Add(4*time.Second), ""),
		execEvent(6, eventtypes.ExecutionEventTypeAgentRuntimeCompleted, now.Add(5*time.Second), ""),
		execEvent(7, eventtypes.ExecutionEventTypeModelRequestStarted, now.Add(6*time.Second), ""),
		execEvent(8, eventtypes.ExecutionEventTypeModelRequestCompleted, now.Add(7*time.Second), ""),
	})

	for _, measurement := range []string{EventSLOTaskToWorkerStart, EventSLOWorkspacePreparation, EventSLOAgentRuntime, EventSLOModelRequest} {
		if got := derivedHistogramCount(measurement, "success"); got != 1 {
			t.Fatalf("%s latency count = %v, want 1", measurement, got)
		}
	}
}

func TestEventDerivedLatencyRecordedFromExecutionEventStoreAppend(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	eventStore := NewFakeExecutionEventStoreWithClock(func() time.Time { return now })

	if _, err := eventStore.AppendExecutionEvent(context.Background(), &ExecutionEvent{
		Namespace:  "default",
		StreamType: eventtypes.ExecutionEventStreamTypeTask,
		StreamID:   "task-store",
		Type:       eventtypes.ExecutionEventTypeToolCallStarted,
		ToolCallID: "call-store",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(start): %v", err)
	}
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &ExecutionEvent{
		Namespace:  "default",
		StreamType: eventtypes.ExecutionEventStreamTypeTask,
		StreamID:   "task-store",
		Type:       eventtypes.ExecutionEventTypeToolCallCompleted,
		ToolCallID: "call-store",
		CreatedAt:  now.Add(250 * time.Millisecond),
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(end): %v", err)
	}

	if got := derivedHistogramCount(EventSLOToolCall, "success"); got != 1 {
		t.Fatalf("store append derived latency count = %v, want 1", got)
	}
}

func TestEventDerivedLatencyDoesNotReplayDurableHistoryOnAppendHotPath(t *testing.T) {
	resetDerivedEventMetrics()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	eventStore := NewFakeExecutionEventStoreWithClock(func() time.Time { return now })

	if _, err := eventStore.AppendExecutionEvent(context.Background(), &ExecutionEvent{
		Namespace:  "default",
		StreamType: eventtypes.ExecutionEventStreamTypeTask,
		StreamID:   "task-restart",
		Type:       eventtypes.ExecutionEventTypeToolCallStarted,
		ToolCallID: "call-restart",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(start): %v", err)
	}
	terminal, err := eventStore.AppendExecutionEvent(context.Background(), &ExecutionEvent{
		Namespace:  "default",
		StreamType: eventtypes.ExecutionEventStreamTypeTask,
		StreamID:   "task-restart",
		Type:       eventtypes.ExecutionEventTypeToolCallCompleted,
		ToolCallID: "call-restart",
		CreatedAt:  now.Add(250 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent(end): %v", err)
	}

	resetDerivedEventMetrics()
	DeriveExecutionEventSLOFromAppend(context.Background(), NewExecutionEventSLODeriver(), eventStore, *terminal)

	if got := derivedHistogramCount(EventSLOToolCall, "success"); got != 0 {
		t.Fatalf("derived latency count = %v, want 0 without in-memory start", got)
	}
}

func TestEventDerivedLatencyEvictsCompletedStartsAndBoundsDedupe(t *testing.T) {
	resetDerivedEventMetrics()
	deriver := NewExecutionEventSLODeriver()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	for i := range int64(maxExecutionEventSLODeriverEntries + 2) {
		callID := "call-" + time.Duration(i).String()
		deriver.Derive([]ExecutionEvent{
			execEvent(i*2+1, eventtypes.ExecutionEventTypeToolCallStarted, now.Add(time.Duration(i)*time.Millisecond), callID),
			execEvent(i*2+2, eventtypes.ExecutionEventTypeToolCallCompleted, now.Add(time.Duration(i)*time.Millisecond+time.Millisecond), callID),
		})
	}
	if len(deriver.starts) != 0 {
		t.Fatalf("completed starts retained = %d, want 0", len(deriver.starts))
	}
	if len(deriver.startOrder) != 0 {
		t.Fatalf("completed start order retained = %d, want 0", len(deriver.startOrder))
	}
	if len(deriver.emitted) > maxExecutionEventSLODeriverEntries {
		t.Fatalf("emitted dedupe entries = %d, want <= %d", len(deriver.emitted), maxExecutionEventSLODeriverEntries)
	}
}

func resetDerivedEventMetrics() {
	metrics.ExecutionEventDerivedLatency.Reset()
	metrics.ExecutionEventDerivedFailuresTotal.Reset()
}

func derivedHistogramCount(measurement, result string) uint64 {
	var metric dto.Metric
	observer := metrics.ExecutionEventDerivedLatency.WithLabelValues(measurement, result)
	promMetric, ok := observer.(prometheus.Metric)
	if !ok {
		return 0
	}
	if err := promMetric.Write(&metric); err != nil {
		return 0
	}
	return metric.GetHistogram().GetSampleCount()
}

func derivedFailureCounter(category, eventType string) float64 {
	var metric dto.Metric
	if err := metrics.ExecutionEventDerivedFailuresTotal.WithLabelValues(category, eventType).Write(&metric); err != nil {
		return 0
	}
	return metric.GetCounter().GetValue()
}

func execEvent(seq int64, typ string, createdAt time.Time, toolCallID string) ExecutionEvent {
	return ExecutionEvent{
		ID:         "default/task/task-a/" + typ + "/" + time.Duration(seq).String(),
		Namespace:  "default",
		StreamType: eventtypes.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		Seq:        seq,
		Type:       typ,
		ToolName:   "tool",
		ToolCallID: toolCallID,
		CreatedAt:  createdAt,
	}
}
