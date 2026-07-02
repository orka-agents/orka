package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	eventtypes "github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/metrics"
)

const (
	EventSLOTaskToWorkerStart    = "task_to_worker_start"
	EventSLOModelRequest         = "model_request"
	EventSLOToolCall             = "tool_call"
	EventSLOWorkspacePreparation = "workspace_preparation"
	EventSLOAgentRuntime         = "agent_runtime"

	maxExecutionEventSLODeriverEntries = 10000
)

// ExecutionEventSLODeriver converts durable execution event start/end pairs into
// low-cardinality latency and failure metrics. A deriver instance is stateful so
// callers may feed overlapping list/replay batches without double-counting pairs.
type ExecutionEventSLODeriver struct {
	mu       sync.Mutex
	starts   map[string]ExecutionEvent
	emitted  map[string]struct{}
	failures map[string]struct{}

	startOrder   []string
	emittedOrder []string
	failureOrder []string
}

// DeriveExecutionEventSLOFromAppend records SLO metrics for an appended event.
// Derivation is best-effort and intentionally does not affect append success.
// Keep append hot paths O(1): do not replay durable stream history here.
func DeriveExecutionEventSLOFromAppend(
	_ context.Context,
	deriver *ExecutionEventSLODeriver,
	_ ExecutionEventStore,
	event ExecutionEvent,
) {
	if deriver == nil {
		return
	}
	deriver.Derive([]ExecutionEvent{event})
}

// NewExecutionEventSLODeriver creates an empty idempotent event-derived SLO engine.
func NewExecutionEventSLODeriver() *ExecutionEventSLODeriver {
	return &ExecutionEventSLODeriver{
		starts:   map[string]ExecutionEvent{},
		emitted:  map[string]struct{}{},
		failures: map[string]struct{}{},
	}
}

// Derive records metrics for any complete event pairs in input. Input may be a
// full replay or an incremental batch; already-derived pairs are ignored.
func (d *ExecutionEventSLODeriver) Derive(input []ExecutionEvent) {
	if d == nil || len(input) == 0 {
		return
	}
	events := append([]ExecutionEvent(nil), input...)
	sort.SliceStable(events, func(i, j int) bool {
		return executionEventDerivationSortKey(events[i]) < executionEventDerivationSortKey(events[j])
	})

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, event := range events {
		if spec, ok := startSpecForEvent(event); ok {
			key := startKey(spec.measurement, event)
			if _, ok := d.starts[key]; !ok {
				d.startOrder = append(d.startOrder, key)
			}
			d.starts[key] = event
			d.pruneStarts()
			continue
		}
		if spec, ok := terminalSpecForEvent(event); ok {
			d.recordTerminal(spec, event)
		}
	}
}

func (d *ExecutionEventSLODeriver) recordTerminal(spec eventPairSpec, end ExecutionEvent) {
	if spec.failure {
		failureKey := eventDerivationIdentity(end) + "|failure|" + spec.measurement
		if d.rememberFailure(failureKey) {
			metrics.RecordExecutionEventDerivedFailure(spec.measurement, end.Type)
		}
	}

	startKey := startKey(spec.measurement, end)
	start, ok := d.starts[startKey]
	if !ok || start.CreatedAt.IsZero() || end.CreatedAt.IsZero() || end.CreatedAt.Before(start.CreatedAt) {
		return
	}
	pairKey := eventDerivationIdentity(start) + "|" + eventDerivationIdentity(end) + "|" + spec.measurement
	if !d.rememberEmitted(pairKey) {
		return
	}
	delete(d.starts, startKey)
	d.removeStartOrder(startKey)
	result := "success"
	if spec.failure {
		result = "failure"
	}
	metrics.RecordExecutionEventDerivedLatency(spec.measurement, result, end.CreatedAt.Sub(start.CreatedAt).Seconds())
}

func (d *ExecutionEventSLODeriver) rememberEmitted(key string) bool {
	if _, ok := d.emitted[key]; ok {
		return false
	}
	d.emitted[key] = struct{}{}
	d.emittedOrder = append(d.emittedOrder, key)
	d.pruneSet(d.emitted, &d.emittedOrder)
	return true
}

func (d *ExecutionEventSLODeriver) rememberFailure(key string) bool {
	if _, ok := d.failures[key]; ok {
		return false
	}
	d.failures[key] = struct{}{}
	d.failureOrder = append(d.failureOrder, key)
	d.pruneSet(d.failures, &d.failureOrder)
	return true
}

func (d *ExecutionEventSLODeriver) pruneStarts() {
	d.compactStartOrder()
	for len(d.starts) > maxExecutionEventSLODeriverEntries && len(d.startOrder) > 0 {
		oldest := d.startOrder[0]
		d.startOrder = d.startOrder[1:]
		delete(d.starts, oldest)
	}
}

func (d *ExecutionEventSLODeriver) removeStartOrder(key string) {
	for i, value := range d.startOrder {
		if value == key {
			copy(d.startOrder[i:], d.startOrder[i+1:])
			d.startOrder = d.startOrder[:len(d.startOrder)-1]
			return
		}
	}
}

func (d *ExecutionEventSLODeriver) compactStartOrder() {
	if len(d.startOrder) <= len(d.starts)*2+maxExecutionEventSLODeriverEntries {
		return
	}
	compacted := d.startOrder[:0]
	for _, key := range d.startOrder {
		if _, ok := d.starts[key]; ok {
			compacted = append(compacted, key)
		}
	}
	d.startOrder = compacted
}

func (d *ExecutionEventSLODeriver) pruneSet(values map[string]struct{}, order *[]string) {
	for len(values) > maxExecutionEventSLODeriverEntries && len(*order) > 0 {
		oldest := (*order)[0]
		*order = (*order)[1:]
		delete(values, oldest)
	}
}

type eventPairSpec struct {
	measurement string
	failure     bool
}

func startSpecForEvent(event ExecutionEvent) (eventPairSpec, bool) {
	switch event.Type {
	case eventtypes.ExecutionEventTypeTaskStarted:
		return eventPairSpec{measurement: EventSLOTaskToWorkerStart}, true
	case eventtypes.ExecutionEventTypeModelRequestStarted:
		return eventPairSpec{measurement: EventSLOModelRequest}, true
	case eventtypes.ExecutionEventTypeToolCallStarted:
		return eventPairSpec{measurement: EventSLOToolCall}, true
	case eventtypes.ExecutionEventTypeWorkspacePreparationStarted:
		return eventPairSpec{measurement: EventSLOWorkspacePreparation}, true
	case eventtypes.ExecutionEventTypeAgentRuntimeStarted:
		return eventPairSpec{measurement: EventSLOAgentRuntime}, true
	default:
		return eventPairSpec{}, false
	}
}

func terminalSpecForEvent(event ExecutionEvent) (eventPairSpec, bool) {
	switch event.Type {
	case eventtypes.ExecutionEventTypeWorkerStarted:
		return eventPairSpec{measurement: EventSLOTaskToWorkerStart}, true
	case eventtypes.ExecutionEventTypeModelRequestCompleted:
		return eventPairSpec{measurement: EventSLOModelRequest}, true
	case eventtypes.ExecutionEventTypeModelRequestFailed:
		return eventPairSpec{measurement: EventSLOModelRequest, failure: true}, true
	case eventtypes.ExecutionEventTypeToolCallCompleted:
		return eventPairSpec{measurement: EventSLOToolCall}, true
	case eventtypes.ExecutionEventTypeToolCallFailed:
		return eventPairSpec{measurement: EventSLOToolCall, failure: true}, true
	case eventtypes.ExecutionEventTypeWorkspacePreparationCompleted:
		return eventPairSpec{measurement: EventSLOWorkspacePreparation}, true
	case eventtypes.ExecutionEventTypeWorkspacePreparationFailed:
		return eventPairSpec{measurement: EventSLOWorkspacePreparation, failure: true}, true
	case eventtypes.ExecutionEventTypeAgentRuntimeCompleted:
		return eventPairSpec{measurement: EventSLOAgentRuntime}, true
	case eventtypes.ExecutionEventTypeAgentRuntimeFailed:
		return eventPairSpec{measurement: EventSLOAgentRuntime, failure: true}, true
	default:
		return eventPairSpec{}, false
	}
}

func startKey(measurement string, event ExecutionEvent) string {
	return strings.Join([]string{
		streamIdentity(event),
		measurement,
		correlationIdentity(measurement, event),
	}, "|")
}

func correlationIdentity(measurement string, event ExecutionEvent) string {
	switch measurement {
	case EventSLOToolCall:
		if strings.TrimSpace(event.ToolCallID) != "" {
			return "toolCallID:" + strings.TrimSpace(event.ToolCallID)
		}
		if strings.TrimSpace(event.ToolName) != "" {
			return "toolName:" + strings.TrimSpace(event.ToolName)
		}
	}
	return "stream"
}

func streamIdentity(event ExecutionEvent) string {
	streamType := strings.TrimSpace(event.StreamType)
	if streamType == "" {
		streamType = eventtypes.ExecutionEventStreamTypeTask
	}
	return strings.Join([]string{strings.TrimSpace(event.Namespace), streamType, strings.TrimSpace(event.StreamID)}, "/")
}

func eventDerivationIdentity(event ExecutionEvent) string {
	if strings.TrimSpace(event.ID) != "" {
		return strings.TrimSpace(event.ID)
	}
	return fmt.Sprintf("%s/%d/%s", streamIdentity(event), event.Seq, event.Type)
}

func executionEventDerivationSortKey(event ExecutionEvent) string {
	created := event.CreatedAt.UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf("%s/%020d/%s/%s", streamIdentity(event), event.Seq, created, event.Type)
}
