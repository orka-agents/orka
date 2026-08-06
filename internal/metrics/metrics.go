/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricLabelUnknown               = "unknown"
	securityInventoryLabelEligible   = "eligible"
	securityInventoryLabelTruncated  = "truncated"
	securityInventoryLabelUnreadable = "unreadable"
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

	TokenExchangeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_token_exchange_total",
			Help: "Total OAuth token exchanges by adapter, grant class, result, and low-cardinality reason",
		},
		[]string{"adapter", "grant_class", "result", "reason"},
	)

	TokenExchangeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_token_exchange_duration_seconds",
			Help:    "OAuth token exchange latency by adapter, grant class, result, and low-cardinality reason",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"adapter", "grant_class", "result", "reason"},
	)

	// Repository monitor workflow metrics. Labels are low-cardinality intent/action/status values.
	RepositoryMonitorCommandsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_commands_total",
			Help: "Repository monitor command events by intent and status",
		},
		[]string{"intent", "status"},
	)

	RepositoryMonitorWorkActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_work_actions_total",
			Help: "Repository monitor workflow actions by desired action and status",
		},
		[]string{"desired_action", "status"},
	)

	RepositoryMonitorGitHubMutationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_github_mutations_total",
			Help: "Repository monitor controller-owned GitHub mutations by operation and status",
		},
		[]string{"operation", "status"},
	)

	RepositoryMonitorBlocksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_blocks_total",
			Help: "Repository monitor policy, stale snapshot, and rate-limit blocks by reason",
		},
		[]string{"reason"},
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

	SecurityOutputWritesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_output_writes_total",
			Help: "Repository security result and artifact writes by binding mode, outcome, and stable reason",
		},
		[]string{"kind", "mode", "outcome", "reason"},
	)

	SecurityValidationRejectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_validation_rejections_total",
			Help: "Repository security validation artifact rejections by stable class",
		},
		[]string{"reason"},
	)

	SecurityIsolationOutcomesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_isolation_outcomes_total",
			Help: "Repository security analysis-isolation capability observations by requested policy and effective outcome",
		},
		[]string{"policy", "outcome"},
	)

	SecurityInventoryEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_inventory_entries_total",
			Help: "Repository security mapper inventory records by bounded disposition",
		},
		[]string{"disposition"},
	)

	SecurityInventoryReasonClassesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_inventory_reason_classes_total",
			Help: "Repository security mapper inventory records by bounded disposition and reason class",
		},
		[]string{"disposition", "reason_class"},
	)

	SecurityTargetVerificationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_target_verification_total",
			Help: "Repository security target verification observations by outcome",
		},
		[]string{"outcome"},
	)

	SecurityBundleSealingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_security_bundle_sealing_total",
			Help: "Repository security bundle sealing attempts by mode and outcome",
		},
		[]string{"mode", "outcome"},
	)

	SecurityBundleSealingDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_security_bundle_sealing_duration_seconds",
			Help:    "Repository security bundle sealing latency by mode and outcome",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"mode", "outcome"},
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
		TokenExchangeTotal,
		TokenExchangeDuration,
		RepositoryMonitorCommandsTotal,
		RepositoryMonitorWorkActionsTotal,
		RepositoryMonitorGitHubMutationsTotal,
		RepositoryMonitorBlocksTotal,
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
		SecurityOutputWritesTotal,
		SecurityValidationRejectionsTotal,
		SecurityIsolationOutcomesTotal,
		SecurityInventoryEntriesTotal,
		SecurityInventoryReasonClassesTotal,
		SecurityTargetVerificationTotal,
		SecurityBundleSealingTotal,
		SecurityBundleSealingDuration,
	)
}

// RecordSecurityOutputWrite records a low-cardinality worker-output binding decision.
func RecordSecurityOutputWrite(kind, mode, outcome, reason string) {
	SecurityOutputWritesTotal.WithLabelValues(
		normalizeMetricLabel(kind),
		normalizeMetricLabel(mode),
		normalizeMetricLabel(outcome),
		normalizeMetricLabel(reason),
	).Inc()
}

// RecordSecurityValidationRejection records a stable validation ingestion failure class.
func RecordSecurityValidationRejection(reason string) {
	SecurityValidationRejectionsTotal.WithLabelValues(normalizeMetricLabel(reason)).Inc()
}

// RecordSecurityIsolationOutcome records a bounded analysis-isolation capability outcome.
func RecordSecurityIsolationOutcome(policy, outcome string) {
	SecurityIsolationOutcomesTotal.WithLabelValues(
		normalizeSecurityIsolationPolicy(policy),
		normalizeSecurityIsolationOutcome(outcome),
	).Inc()
}

// RecordSecurityInventoryEntries records mapper inventory records using only
// bounded disposition and reason-class labels.
func RecordSecurityInventoryEntries(disposition, reason string, count int) {
	if count <= 0 {
		return
	}
	disposition = normalizeSecurityInventoryDisposition(disposition)
	reason = normalizeSecurityInventoryReasonClass(reason)
	SecurityInventoryEntriesTotal.WithLabelValues(disposition).Add(float64(count))
	SecurityInventoryReasonClassesTotal.WithLabelValues(disposition, reason).Add(float64(count))
}

// RecordSecurityTargetVerification records one target receipt verification outcome.
func RecordSecurityTargetVerification(outcome string) {
	SecurityTargetVerificationTotal.WithLabelValues(normalizeMetricLabel(outcome)).Inc()
}

// RecordSecurityBundleSealing records a bundle sealing attempt and latency.
func RecordSecurityBundleSealing(mode, outcome string, durationSeconds float64) {
	mode = normalizeMetricLabel(mode)
	outcome = normalizeMetricLabel(outcome)
	SecurityBundleSealingTotal.WithLabelValues(mode, outcome).Inc()
	SecurityBundleSealingDuration.WithLabelValues(mode, outcome).Observe(durationSeconds)
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

// RecordContextTokenTTSExchange records a transaction-token TTS exchange attempt.
func RecordContextTokenTTSExchange(result, reason string, durationSeconds float64) {
	result = normalizeMetricLabel(result)
	reason = normalizeMetricLabel(reason)
	ContextTokenTTSExchangeTotal.WithLabelValues(result, reason).Inc()
	ContextTokenTTSExchangeDuration.WithLabelValues(result, reason).Observe(durationSeconds)
}

// RecordTokenExchange records one low-cardinality OAuth exchange observation.
func RecordTokenExchange(adapter, grantClass, result, reason string, durationSeconds float64) {
	adapter = normalizeMetricLabel(adapter)
	grantClass = normalizeMetricLabel(grantClass)
	result = normalizeMetricLabel(result)
	reason = normalizeMetricLabel(reason)
	TokenExchangeTotal.WithLabelValues(adapter, grantClass, result, reason).Inc()
	TokenExchangeDuration.WithLabelValues(adapter, grantClass, result, reason).Observe(durationSeconds)
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

// RecordRepositoryMonitorCommand records a durable command event decision.
func RecordRepositoryMonitorCommand(intent, status string) {
	RepositoryMonitorCommandsTotal.WithLabelValues(normalizeMetricLabel(intent), normalizeMetricLabel(status)).Inc()
}

// RecordRepositoryMonitorWorkAction records a workflow action transition.
func RecordRepositoryMonitorWorkAction(desiredAction, status string) {
	RepositoryMonitorWorkActionsTotal.WithLabelValues(normalizeMetricLabel(desiredAction), normalizeMetricLabel(status)).Inc()
}

// RecordRepositoryMonitorGitHubMutation records one GitHub write audit result.
func RecordRepositoryMonitorGitHubMutation(operation, status string) {
	RepositoryMonitorGitHubMutationsTotal.WithLabelValues(normalizeMetricLabel(operation), normalizeMetricLabel(status)).Inc()
}

// RecordRepositoryMonitorBlock records a low-cardinality monitor block reason.
func RecordRepositoryMonitorBlock(reason string) {
	RepositoryMonitorBlocksTotal.WithLabelValues(normalizeMetricLabel(reason)).Inc()
}

func normalizeSecurityIsolationPolicy(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "legacy", "prefer-hardened", "require-hardened":
		return value
	default:
		return metricLabelUnknown
	}
}

func normalizeSecurityIsolationOutcome(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "legacy", "hardened", "fallback", "unverified", "failed":
		return value
	default:
		return metricLabelUnknown
	}
}

func normalizeSecurityInventoryDisposition(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "reviewable", securityInventoryLabelEligible:
		return securityInventoryLabelEligible
	case "excluded", "assigned", "omitted", securityInventoryLabelTruncated, securityInventoryLabelUnreadable:
		return value
	default:
		return metricLabelUnknown
	}
}

func normalizeSecurityInventoryReasonClass(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "supported-reviewable-file":
		return "eligible"
	case "assigned-to-review-slice":
		return "assigned"
	case "no-deterministic-review-slice":
		return "unassigned"
	case "symlink":
		return "symlink"
	case "secret-like-path":
		return "secret_like"
	case "lockfile":
		return "lockfile"
	case "unsupported-type":
		return "unsupported_type"
	case "vcs-directory":
		return "vcs"
	case "dependency-directory":
		return "dependency"
	case "generated-directory":
		return "generated"
	case "cache-directory":
		return "cache"
	case "virtualenv-directory":
		return "virtualenv"
	case "entrypoint-reference-cap", "context-reference-cap", "test-reference-cap", "mapper_inventory_entry_limit", "maxfiles", "maxbytes", securityInventoryLabelTruncated:
		return securityInventoryLabelTruncated
	case securityInventoryLabelUnreadable:
		return securityInventoryLabelUnreadable
	case "":
		return metricLabelUnknown
	default:
		return "other"
	}
}

func normalizeMetricLabel(value string) string {
	if value == "" {
		return metricLabelUnknown
	}
	return value
}
