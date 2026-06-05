/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
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

func normalizeMetricLabel(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
