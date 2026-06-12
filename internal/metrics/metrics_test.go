/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Helper function to get counter value
func getCounterValue(counter *prometheus.CounterVec, labels ...string) float64 {
	var m dto.Metric
	if err := counter.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// Helper function to get histogram count
func getHistogramCount(histogram *prometheus.HistogramVec, labels ...string) uint64 {
	var m dto.Metric
	observer := histogram.WithLabelValues(labels...)
	// Type assert Observer to Metric to access Write method
	metric, ok := observer.(prometheus.Metric)
	if !ok {
		return 0
	}
	if err := metric.Write(&m); err != nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func TestRecordAPIRequest(t *testing.T) {
	APIRequestsTotal.Reset()
	APIRequestDuration.Reset()

	tests := []struct {
		name       string
		status     int
		wantStatus string
	}{
		{
			name:       "2xx success",
			status:     200,
			wantStatus: "2xx",
		},
		{
			name:       "201 created",
			status:     201,
			wantStatus: "2xx",
		},
		{
			name:       "4xx client error",
			status:     400,
			wantStatus: "4xx",
		},
		{
			name:       "404 not found",
			status:     404,
			wantStatus: "4xx",
		},
		{
			name:       "5xx server error",
			status:     500,
			wantStatus: "5xx",
		},
		{
			name:       "503 unavailable",
			status:     503,
			wantStatus: "5xx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			APIRequestsTotal.Reset()
			APIRequestDuration.Reset()

			RecordAPIRequest("/api/v1/tasks", "GET", tt.status, 0.1)

			count := getCounterValue(APIRequestsTotal, "/api/v1/tasks", "GET", tt.wantStatus)
			if count != 1 {
				t.Errorf("APIRequestsTotal = %v, want 1", count)
			}

			durationCount := getHistogramCount(APIRequestDuration, "/api/v1/tasks", "GET")
			if durationCount != 1 {
				t.Errorf("APIRequestDuration count = %v, want 1", durationCount)
			}
		})
	}
}

func TestMetricsRegistered(t *testing.T) {
	// Verify that all metrics are not nil (registered during init)
	metrics := []any{
		APIRequestsTotal,
		APIRequestDuration,
		SkillsLoaded,
		ContextTokenAuthTotal,
		ContextTokenAuthorizationTotal,
		ContextTokenTTSExchangeTotal,
		ContextTokenTTSExchangeDuration,
	}

	for i, m := range metrics {
		if m == nil {
			t.Errorf("metric %d is nil", i)
		}
	}
}

func TestRecordContextTokenMetrics(t *testing.T) {
	ContextTokenAuthTotal.Reset()
	ContextTokenAuthorizationTotal.Reset()
	ContextTokenTTSExchangeTotal.Reset()
	ContextTokenTTSExchangeDuration.Reset()

	RecordContextTokenAuth("kontxt", "success")
	if count := getCounterValue(ContextTokenAuthTotal, "kontxt", "success"); count != 1 {
		t.Fatalf("ContextTokenAuthTotal = %v, want 1", count)
	}

	RecordContextTokenAuth("", "")
	if count := getCounterValue(ContextTokenAuthTotal, "unknown", "unknown"); count != 1 {
		t.Fatalf("ContextTokenAuthTotal unknown = %v, want 1", count)
	}

	RecordContextTokenAuthorization("createTask", "denied", "missing_scope")
	if count := getCounterValue(ContextTokenAuthorizationTotal, "createTask", "denied", "missing_scope"); count != 1 {
		t.Fatalf("ContextTokenAuthorizationTotal = %v, want 1", count)
	}

	RecordContextTokenTTSExchange("success", "ok", 0.25)
	if count := getCounterValue(ContextTokenTTSExchangeTotal, "success", "ok"); count != 1 {
		t.Fatalf("ContextTokenTTSExchangeTotal = %v, want 1", count)
	}
	if count := getHistogramCount(ContextTokenTTSExchangeDuration, "success", "ok"); count != 1 {
		t.Fatalf("ContextTokenTTSExchangeDuration count = %v, want 1", count)
	}
}

func getGaugeValue(gauge *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	if err := gauge.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func TestExecutionEventMetricsNormalizeInvalidLabels(t *testing.T) {
	ExecutionEventsAppendedTotal.Reset()
	ExecutionEventAppendFailuresTotal.Reset()
	ExecutionEventAppendDuration.Reset()
	ExecutionEventRedactionsTotal.Reset()
	ExecutionEventTruncationsTotal.Reset()
	ExecutionEventDerivedFailuresTotal.Reset()

	RecordExecutionEventAppend("m/e2", "WorkspacePreparationStarted", true, 0.01)
	RecordExecutionEventAppend("task", "unexpected/event", false, 0.02)
	RecordExecutionEventPayloadSanitization("custom/task", "not-real", true, true)
	RecordExecutionEventDerivedFailure("tool_call", "not-real")

	if got := getCounterValue(ExecutionEventsAppendedTotal, "invalid", "WorkspacePreparationStarted"); got != 1 {
		t.Fatalf("appended invalid stream=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventAppendFailuresTotal, "task", "invalid"); got != 1 {
		t.Fatalf("append failures invalid event=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventRedactionsTotal, "invalid", "invalid"); got != 1 {
		t.Fatalf("redactions invalid labels=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventTruncationsTotal, "invalid", "invalid"); got != 1 {
		t.Fatalf("truncations invalid labels=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventDerivedFailuresTotal, "tool_call", "invalid"); got != 1 {
		t.Fatalf("derived failures invalid event=%v, want 1", got)
	}
}

func TestRecordExecutionEventMetrics(t *testing.T) {
	ExecutionEventsAppendedTotal.Reset()
	ExecutionEventAppendFailuresTotal.Reset()
	ExecutionEventAppendDuration.Reset()
	ExecutionEventListRequestsTotal.Reset()
	ExecutionEventListDuration.Reset()
	ExecutionEventStreamConnections.Reset()
	ExecutionEventStreamReconnectsTotal.Reset()
	ExecutionEventStreamErrorsTotal.Reset()
	ExecutionEventRedactionsTotal.Reset()
	ExecutionEventTruncationsTotal.Reset()
	ExecutionEventDerivedLatency.Reset()
	ExecutionEventDerivedFailuresTotal.Reset()

	RecordExecutionEventAppend("task", "TaskStarted", true, 0.01)
	RecordExecutionEventAppend("task", "TaskStarted", false, 0.02)
	RecordExecutionEventList("task_api", true, 0.03)
	done := RecordExecutionEventStreamOpen("task", true)
	RecordExecutionEventStreamError("task", "list")
	RecordExecutionEventPayloadSanitization("task", "ModelMessage", true, true)
	RecordExecutionEventDerivedLatency("tool_call", "success", 0.5)
	RecordExecutionEventDerivedFailure("tool_call", "ToolCallFailed")

	if got := getCounterValue(ExecutionEventsAppendedTotal, "task", "TaskStarted"); got != 1 {
		t.Fatalf("appended=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventAppendFailuresTotal, "task", "TaskStarted"); got != 1 {
		t.Fatalf("append failures=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventListRequestsTotal, "task_api", "success"); got != 1 {
		t.Fatalf("list requests=%v, want 1", got)
	}
	if got := getGaugeValue(ExecutionEventStreamConnections, "task"); got != 1 {
		t.Fatalf("stream gauge=%v, want 1", got)
	}
	done()
	if got := getGaugeValue(ExecutionEventStreamConnections, "task"); got != 0 {
		t.Fatalf("stream gauge after close=%v, want 0", got)
	}
	if got := getCounterValue(ExecutionEventStreamReconnectsTotal, "task"); got != 1 {
		t.Fatalf("reconnects=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventStreamErrorsTotal, "task", "list"); got != 1 {
		t.Fatalf("stream errors=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventRedactionsTotal, "task", "ModelMessage"); got != 1 {
		t.Fatalf("redactions=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventTruncationsTotal, "task", "ModelMessage"); got != 1 {
		t.Fatalf("truncations=%v, want 1", got)
	}
	if got := getHistogramCount(ExecutionEventDerivedLatency, "tool_call", "success"); got != 1 {
		t.Fatalf("derived latency count=%v, want 1", got)
	}
	if got := getCounterValue(ExecutionEventDerivedFailuresTotal, "tool_call", "ToolCallFailed"); got != 1 {
		t.Fatalf("derived failures=%v, want 1", got)
	}
}
