/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
		TokenExchangeTotal,
		TokenExchangeDuration,
		ACPRuntimePoolDesiredReplicas,
		ACPRuntimePoolReadyReplicas,
		ACPRuntimePoolSessionsActive,
		ACPRuntimePoolPromptsInFlight,
		ACPRuntimePoolQueuedTasks,
		ACPRuntimePoolAdmissionState,
		ACPRuntimePoolScaleToZeroTotal,
	}

	for i, m := range metrics {
		if m == nil {
			t.Errorf("metric %d is nil", i)
		}
	}
}

func TestRecordACPRuntimePoolMetrics(t *testing.T) {
	ACPRuntimePoolDesiredReplicas.Reset()
	ACPRuntimePoolReadyReplicas.Reset()
	ACPRuntimePoolSessionsActive.Reset()
	ACPRuntimePoolPromptsInFlight.Reset()
	ACPRuntimePoolQueuedTasks.Reset()
	ACPRuntimePoolAdmissionState.Reset()
	ACPRuntimePoolScaleToZeroTotal.Reset()

	RecordACPRuntimePoolStatus("default", "codex-pool", 1, 1, 3, 2, 4, "Accepting")
	if got := getGaugeValue(ACPRuntimePoolDesiredReplicas, "default", "codex-pool"); got != 1 {
		t.Fatalf("desired replicas = %v, want 1", got)
	}
	if got := getGaugeValue(ACPRuntimePoolReadyReplicas, "default", "codex-pool"); got != 1 {
		t.Fatalf("ready replicas = %v, want 1", got)
	}
	if got := getGaugeValue(ACPRuntimePoolSessionsActive, "default", "codex-pool"); got != 3 {
		t.Fatalf("active sessions = %v, want 3", got)
	}
	if got := getGaugeValue(ACPRuntimePoolPromptsInFlight, "default", "codex-pool"); got != 2 {
		t.Fatalf("prompts in flight = %v, want 2", got)
	}
	if got := getGaugeValue(ACPRuntimePoolQueuedTasks, "default", "codex-pool"); got != 4 {
		t.Fatalf("queued tasks = %v, want 4", got)
	}
	if got := getGaugeValue(ACPRuntimePoolAdmissionState, "default", "codex-pool", "accepting"); got != 1 {
		t.Fatalf("accepting admission state = %v, want 1", got)
	}

	RecordACPRuntimePoolStatus("default", "codex-pool", 0, 1, 1, 0, 0, "Draining")
	if got := getGaugeValue(ACPRuntimePoolAdmissionState, "default", "codex-pool", "accepting"); got != 0 {
		t.Fatalf("stale accepting admission state = %v, want 0", got)
	}
	if got := getGaugeValue(ACPRuntimePoolAdmissionState, "default", "codex-pool", "draining"); got != 1 {
		t.Fatalf("draining admission state = %v, want 1", got)
	}

	RecordACPRuntimePoolScaleToZero("default", "codex-pool")
	if got := getCounterValue(ACPRuntimePoolScaleToZeroTotal, "default", "codex-pool"); got != 1 {
		t.Fatalf("scale-to-zero transitions = %v, want 1", got)
	}
}

func TestRecordContextTokenMetrics(t *testing.T) {
	ContextTokenAuthTotal.Reset()
	ContextTokenAuthorizationTotal.Reset()
	ContextTokenTTSExchangeTotal.Reset()
	ContextTokenTTSExchangeDuration.Reset()
	TokenExchangeTotal.Reset()
	TokenExchangeDuration.Reset()

	RecordContextTokenAuth("transaction-token", "success")
	if count := getCounterValue(ContextTokenAuthTotal, "transaction-token", "success"); count != 1 {
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

	RecordTokenExchange("direct", "rfc8693", "success", "ok", 0.5)
	if count := getCounterValue(TokenExchangeTotal, "direct", "rfc8693", "success", "ok"); count != 1 {
		t.Fatalf("TokenExchangeTotal = %v, want 1", count)
	}
	if count := getHistogramCount(TokenExchangeDuration, "direct", "rfc8693", "success", "ok"); count != 1 {
		t.Fatalf("TokenExchangeDuration count = %v, want 1", count)
	}
}

func getGaugeValue(gauge *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	if err := gauge.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func TestExecutionEventMetricsOwnRequestLabels(t *testing.T) {
	ExecutionEventsAppendedTotal.Reset()
	ExecutionEventAppendDuration.Reset()
	t.Cleanup(ExecutionEventsAppendedTotal.Reset)
	t.Cleanup(ExecutionEventAppendDuration.Reset)

	registry := prometheus.NewRegistry()
	registry.MustRegister(ExecutionEventsAppendedTotal, ExecutionEventAppendDuration)
	app := fiber.New()
	app.Get("/events/:streamType", func(c fiber.Ctx) error {
		streamType := c.Params("streamType")
		RecordExecutionEventAppend(streamType, "ModelMessage", true, 0.01)

		// Reuse Fiber's actual path buffer before another writer records the
		// same label. No pooling timing or unsafe string conversion is needed.
		c.Path("/events/xxxx")
		if streamType != "xxxx" {
			return fiber.NewError(http.StatusInternalServerError, "fixture did not reuse the borrowed path buffer")
		}
		RecordExecutionEventAppend("task", "ModelMessage", true, 0.02)
		c.Path("/events/task")
		return c.SendStatus(http.StatusNoContent)
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/events/task", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("request status = %d, want 204", response.StatusCode)
	}

	// A retained borrowed label leaves two series under different hashes but
	// identical label values, which turns the entire metrics scrape into a 500.
	scrape := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(
		scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if scrape.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", scrape.Code, scrape.Body.String())
	}
	if got := getCounterValue(ExecutionEventsAppendedTotal, "task", "ModelMessage"); got != 2 {
		t.Fatalf("appended events = %v, want 2", got)
	}
	if got := getHistogramCount(ExecutionEventAppendDuration, "task", "ModelMessage", "success"); got != 2 {
		t.Fatalf("append duration count = %v, want 2", got)
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

	RecordExecutionEventAppend("task", "TaskStarted", true, 0.01)
	RecordExecutionEventAppend("task", "TaskStarted", false, 0.02)
	RecordExecutionEventList("task_api", true, 0.03)
	done := RecordExecutionEventStreamOpen("task", true)
	RecordExecutionEventStreamError("task", "list")
	RecordExecutionEventPayloadSanitization("task", "ModelMessage", true, true)

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
}
