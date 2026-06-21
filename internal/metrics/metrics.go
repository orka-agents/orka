/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// API metrics
	APIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_api_requests_total",
			Help: "Total API requests by endpoint, method, and status",
		},
		[]string{"endpoint", "method", "status"},
	)

	APIRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_api_request_duration_seconds",
			Help:    "API request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "method"},
	)

	// Skill metrics
	SkillsLoaded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_skills_loaded_total",
			Help: "Skills loaded by namespace and name",
		},
		[]string{"skill", "namespace"},
	)

	// Context-token metrics
	ContextTokenAuthTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_context_token_auth_total",
			Help: "Total context-token authentication attempts by profile and result",
		},
		[]string{"profile", "result"},
	)

	ContextTokenAuthorizationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_context_token_authorization_total",
			Help: "Total context-token authorization decisions by action, result, and low-cardinality reason",
		},
		[]string{"action", "result", "reason"},
	)

	ContextTokenTTSExchangeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_context_token_tts_exchange_total",
			Help: "Total context-token TTS exchange attempts by result and low-cardinality reason",
		},
		[]string{"result", "reason"},
	)

	ContextTokenTTSExchangeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_context_token_tts_exchange_duration_seconds",
			Help:    "Context-token TTS exchange latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result", "reason"},
	)

	// Execution event metrics. Labels intentionally exclude task/session IDs.
	ExecutionEventsAppendedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_events_appended_total",
			Help: "Total execution events appended by stream type and event type",
		},
		[]string{"stream_type", "event_type"},
	)

	ExecutionEventAppendFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_append_failures_total",
			Help: "Total execution event append failures by stream type and event type",
		},
		[]string{"stream_type", "event_type"},
	)

	ExecutionEventAppendDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_execution_event_append_duration_seconds",
			Help:    "Execution event append latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"stream_type", "event_type", "result"},
	)

	ExecutionEventListRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_list_requests_total",
			Help: "Total execution event list/read-model requests by scope and result",
		},
		[]string{"scope", "result"},
	)

	ExecutionEventListDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_execution_event_list_duration_seconds",
			Help:    "Execution event list/read-model latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"scope", "result"},
	)

	ExecutionEventStreamConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_execution_event_stream_connections_current",
			Help: "Current execution event SSE stream connections by scope",
		},
		[]string{"scope"},
	)

	ExecutionEventStreamReconnectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_stream_reconnects_total",
			Help: "Total execution event SSE reconnects detected by after cursor by scope",
		},
		[]string{"scope"},
	)

	ExecutionEventStreamErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_stream_errors_total",
			Help: "Total execution event SSE stream errors by scope and low-cardinality reason",
		},
		[]string{"scope", "reason"},
	)

	ExecutionEventRedactionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_redactions_total",
			Help: "Total execution events whose payloads contained redacted sensitive values by stream type and event type",
		},
		[]string{"stream_type", "event_type"},
	)

	ExecutionEventTruncationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_truncations_total",
			Help: "Total execution events whose payloads were truncated by stream type and event type",
		},
		[]string{"stream_type", "event_type"},
	)

	ExecutionEventDerivedLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_execution_event_derived_latency_seconds",
			Help:    "Latency derived from execution event start/end pairs",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"measurement", "result"},
	)

	ExecutionEventDerivedFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_derived_failures_total",
			Help: "Failure counts derived from execution event terminal/failure event types",
		},
		[]string{"category", "event_type"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		APIRequestsTotal,
		APIRequestDuration,
		SkillsLoaded,
		ContextTokenAuthTotal,
		ContextTokenAuthorizationTotal,
		ContextTokenTTSExchangeTotal,
		ContextTokenTTSExchangeDuration,
		ExecutionEventsAppendedTotal,
		ExecutionEventAppendFailuresTotal,
		ExecutionEventAppendDuration,
		ExecutionEventListRequestsTotal,
		ExecutionEventListDuration,
		ExecutionEventStreamConnections,
		ExecutionEventStreamReconnectsTotal,
		ExecutionEventStreamErrorsTotal,
		ExecutionEventRedactionsTotal,
		ExecutionEventTruncationsTotal,
		ExecutionEventDerivedLatency,
		ExecutionEventDerivedFailuresTotal,
	)
}

// RecordAPIRequest records an API request
func RecordAPIRequest(endpoint, method string, status int, durationSeconds float64) {
	statusStr := "2xx"
	if status >= 400 && status < 500 {
		statusStr = "4xx"
	} else if status >= 500 {
		statusStr = "5xx"
	}
	APIRequestsTotal.WithLabelValues(endpoint, method, statusStr).Inc()
	APIRequestDuration.WithLabelValues(endpoint, method).Observe(durationSeconds)
}

// RecordContextTokenAuth records a context-token authentication attempt.
func RecordContextTokenAuth(profile, result string) {
	ContextTokenAuthTotal.WithLabelValues(normalizeMetricLabel(profile), normalizeMetricLabel(result)).Inc()
}

// RecordContextTokenAuthorization records a context-token authorization decision.
func RecordContextTokenAuthorization(action, result, reason string) {
	ContextTokenAuthorizationTotal.WithLabelValues(
		normalizeMetricLabel(action),
		normalizeMetricLabel(result),
		normalizeMetricLabel(reason),
	).Inc()
}

// RecordContextTokenTTSExchange records a kontxt TTS token exchange attempt.
func RecordContextTokenTTSExchange(result, reason string, durationSeconds float64) {
	result = normalizeMetricLabel(result)
	reason = normalizeMetricLabel(reason)
	ContextTokenTTSExchangeTotal.WithLabelValues(result, reason).Inc()
	ContextTokenTTSExchangeDuration.WithLabelValues(result, reason).Observe(durationSeconds)
}

// RecordExecutionEventAppend records append success/failure and latency using low-cardinality labels.
func RecordExecutionEventAppend(streamType, eventType string, success bool, durationSeconds float64) {
	streamType = normalizeMetricLabel(streamType)
	eventType = normalizeMetricLabel(eventType)
	result := "success"
	if success {
		ExecutionEventsAppendedTotal.WithLabelValues(streamType, eventType).Inc()
	} else {
		result = "error"
		ExecutionEventAppendFailuresTotal.WithLabelValues(streamType, eventType).Inc()
	}
	ExecutionEventAppendDuration.WithLabelValues(streamType, eventType, result).Observe(durationSeconds)
}

// RecordExecutionEventList records list/read-model request count and latency.
func RecordExecutionEventList(scope string, success bool, durationSeconds float64) {
	scope = normalizeMetricLabel(scope)
	result := "success"
	if !success {
		result = "error"
	}
	ExecutionEventListRequestsTotal.WithLabelValues(scope, result).Inc()
	ExecutionEventListDuration.WithLabelValues(scope, result).Observe(durationSeconds)
}

// RecordExecutionEventStreamOpen records stream lifecycle and reconnect detection.
func RecordExecutionEventStreamOpen(scope string, reconnect bool) func() {
	scope = normalizeMetricLabel(scope)
	ExecutionEventStreamConnections.WithLabelValues(scope).Inc()
	if reconnect {
		ExecutionEventStreamReconnectsTotal.WithLabelValues(scope).Inc()
	}
	return func() { ExecutionEventStreamConnections.WithLabelValues(scope).Dec() }
}

// RecordExecutionEventStreamError records a low-cardinality stream failure reason.
func RecordExecutionEventStreamError(scope, reason string) {
	ExecutionEventStreamErrorsTotal.WithLabelValues(normalizeMetricLabel(scope), normalizeMetricLabel(reason)).Inc()
}

// RecordExecutionEventPayloadSanitization records event-level redaction/truncation signals.
func RecordExecutionEventPayloadSanitization(streamType, eventType string, redacted, truncated bool) {
	streamType = normalizeMetricLabel(streamType)
	eventType = normalizeMetricLabel(eventType)
	if redacted {
		ExecutionEventRedactionsTotal.WithLabelValues(streamType, eventType).Inc()
	}
	if truncated {
		ExecutionEventTruncationsTotal.WithLabelValues(streamType, eventType).Inc()
	}
}

// CounterVecValue returns the current value of a CounterVec for the given label
// values. It is intended for tests asserting metric accuracy across packages.
func CounterVecValue(counter *prometheus.CounterVec, labels ...string) float64 {
	var m dto.Metric
	if err := counter.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// RecordExecutionEventDerivedLatency records one idempotent event-derived latency observation.
func RecordExecutionEventDerivedLatency(measurement, result string, durationSeconds float64) {
	ExecutionEventDerivedLatency.WithLabelValues(normalizeMetricLabel(measurement), normalizeMetricLabel(result)).Observe(durationSeconds)
}

// RecordExecutionEventDerivedFailure records one event-derived failure category.
func RecordExecutionEventDerivedFailure(category, eventType string) {
	ExecutionEventDerivedFailuresTotal.WithLabelValues(normalizeMetricLabel(category), normalizeMetricLabel(eventType)).Inc()
}

func normalizeMetricLabel(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
