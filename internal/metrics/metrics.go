/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package metrics

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelUnknown  = "unknown"
	labelClosed   = "closed"
	labelDraining = "draining"
)

const (
	labelStreamType = "stream_type"
	labelAmbiguous  = "ambiguous"
)

const (
	labelStatus      = "status"
	labelNamespace   = "namespace"
	labelResult      = "result"
	labelReason      = "reason"
	labelRuntimePool = "runtime_pool"
	labelEventType   = "event_type"
	labelScope       = "scope"
	labelState       = "state"
	labelAccepting   = "accepting"
)

var (
	ApprovalDecisionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_approval_decision_duration_seconds",
			Help:    "Time from approval request to committed decision in seconds",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 14400, 86400},
		},
		[]string{"outcome", "path"},
	)

	APIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_api_requests_total",
			Help: "Total API requests by endpoint, method, and status",
		},
		[]string{"endpoint", "method", labelStatus},
	)

	APIRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_api_request_duration_seconds",
			Help:    "API request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "method"},
	)

	SkillsLoaded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_skills_loaded_total",
			Help: "Skills loaded by namespace and name",
		},
		[]string{"skill", labelNamespace},
	)

	ContextTokenAuthTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_context_token_auth_total",
			Help: "Total context-token authentication attempts by profile and result",
		},
		[]string{"profile", labelResult},
	)

	ContextTokenAuthorizationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_context_token_authorization_total",
			Help: "Total context-token authorization decisions by action, result, and low-cardinality reason",
		},
		[]string{"action", labelResult, labelReason},
	)

	ContextTokenTTSExchangeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_context_token_tts_exchange_total",
			Help: "Total context-token TTS exchange attempts by result and low-cardinality reason",
		},
		[]string{labelResult, labelReason},
	)

	ContextTokenTTSExchangeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_context_token_tts_exchange_duration_seconds",
			Help:    "Context-token TTS exchange latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelResult, labelReason},
	)

	TokenExchangeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_token_exchange_total",
			Help: "Total OAuth token exchanges by adapter, grant class, result, and low-cardinality reason",
		},
		[]string{"adapter", "grant_class", labelResult, labelReason},
	)

	TokenExchangeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_token_exchange_duration_seconds",
			Help:    "OAuth token exchange latency by adapter, grant class, result, and low-cardinality reason",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"adapter", "grant_class", labelResult, labelReason},
	)

	ACPWorkspaceRetentionActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_acp_workspace_retention_actions_total",
			Help: "Retention enforcement actions applied to class-backed ACP execution workspaces",
		},
		[]string{"action", labelReason},
	)

	ACPWorkspaceRuntimePhaseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_acp_workspace_runtime_phase_duration_seconds",
			Help:    "Duration of each phase of starting or stopping an execution-workspace runtime, by path and phase",
			Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600, 900},
		},
		[]string{"path", "phase"},
	)

	StorageRecoveryHold = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_storage_recovery_hold",
			Help: "1 while the installation is held for storage recovery, by the reason the hold records",
		},
		[]string{labelReason},
	)

	ACPRuntimePoolDesiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_acp_runtime_pool_desired_replicas",
			Help: "Desired replicas for a controller-owned ACP RuntimePool",
		},
		[]string{labelNamespace, labelRuntimePool},
	)

	ACPRuntimePoolReadyReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_acp_runtime_pool_ready_replicas",
			Help: "Authoritatively selected Ready replicas for a controller-owned ACP RuntimePool",
		},
		[]string{labelNamespace, labelRuntimePool},
	)

	ACPRuntimePoolSessionsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_acp_runtime_pool_sessions_active",
			Help: "Authenticated supervisor count of resident RuntimeSessions",
		},
		[]string{labelNamespace, labelRuntimePool},
	)

	ACPRuntimePoolPromptsInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_acp_runtime_pool_prompts_in_flight",
			Help: "Authenticated supervisor count of active prompts",
		},
		[]string{labelNamespace, labelRuntimePool},
	)

	ACPRuntimePoolQueuedTasks = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_acp_runtime_pool_queued_tasks",
			Help: "Durable unsatisfied Task demand assigned to an ACP RuntimePool",
		},
		[]string{labelNamespace, labelRuntimePool},
	)

	ACPRuntimePoolAdmissionState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_acp_runtime_pool_admission_state",
			Help: "Authoritative RuntimePool admission state as a one-hot gauge",
		},
		[]string{labelNamespace, labelRuntimePool, labelState},
	)

	ACPRuntimePoolScaleToZeroTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_acp_runtime_pool_scale_to_zero_total",
			Help: "Completed RuntimePool scale-to-zero transitions",
		},
		[]string{labelNamespace, labelRuntimePool},
	)

	RepositoryMonitorCommandsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_commands_total",
			Help: "Repository monitor command events by intent and status",
		},
		[]string{"intent", labelStatus},
	)

	RepositoryMonitorWorkActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_work_actions_total",
			Help: "Repository monitor workflow actions by desired action and status",
		},
		[]string{"desired_action", labelStatus},
	)

	RepositoryMonitorGitHubMutationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_github_mutations_total",
			Help: "Repository monitor controller-owned GitHub mutations by operation and status",
		},
		[]string{"operation", labelStatus},
	)

	RepositoryMonitorBlocksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_blocks_total",
			Help: "Repository monitor policy, stale snapshot, and rate-limit blocks by reason",
		},
		[]string{labelReason},
	)

	RepositoryMonitorReviewsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_reviews_total",
			Help: "Repository monitor pull request review records by review mode and recorded verdict",
		},
		[]string{"mode", "verdict"},
	)

	RepositoryMonitorReviewFindingsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_repository_monitor_review_findings_total",
			Help: "Repository monitor review findings by priority and outcome: reported when first recorded, fixed when a later review resolves one",
		},
		[]string{"priority", "outcome"},
	)

	RepositoryMonitorReviewRoundsToPass = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "orka_repository_monitor_review_rounds_to_pass",
			Help:    "Distinct pull request heads reviewed up to and including a pull request's first passed review",
			Buckets: []float64{1, 2, 3, 4, 5, 7, 10, 15},
		},
	)

	ExecutionEventsAppendedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_events_appended_total",
			Help: "Total execution events appended by stream type and event type",
		},
		[]string{labelStreamType, labelEventType},
	)

	ExecutionEventAppendFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_append_failures_total",
			Help: "Total execution event append failures by stream type and event type",
		},
		[]string{labelStreamType, labelEventType},
	)

	ExecutionEventAppendDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_execution_event_append_duration_seconds",
			Help:    "Execution event append latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelStreamType, labelEventType, labelResult},
	)

	ExecutionEventListRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_list_requests_total",
			Help: "Total execution event list/read-model requests by scope and result",
		},
		[]string{labelScope, labelResult},
	)

	ExecutionEventListDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_execution_event_list_duration_seconds",
			Help:    "Execution event list/read-model latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelScope, labelResult},
	)

	ExecutionEventStreamConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "orka_execution_event_stream_connections_current",
			Help: "Current execution event SSE stream connections by scope",
		},
		[]string{labelScope},
	)

	ExecutionEventStreamReconnectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_stream_reconnects_total",
			Help: "Total execution event SSE reconnects detected by after cursor by scope",
		},
		[]string{labelScope},
	)

	ExecutionEventStreamErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_stream_errors_total",
			Help: "Total execution event SSE stream errors by scope and low-cardinality reason",
		},
		[]string{labelScope, labelReason},
	)

	ExecutionEventRedactionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_redactions_total",
			Help: "Total execution events whose payloads contained redacted sensitive values by stream type and event type",
		},
		[]string{labelStreamType, labelEventType},
	)

	ExecutionEventTruncationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_execution_event_truncations_total",
			Help: "Total execution events whose payloads were truncated by stream type and event type",
		},
		[]string{labelStreamType, labelEventType},
	)

	TaskWaitTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_task_wait_transitions_total",
			Help: "Parked agent turn transitions by wait kind and new state",
		},
		[]string{"kind", labelState},
	)

	TaskWaitDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "orka_task_wait_duration_seconds",
			Help:    "Time a parked agent turn held no runtime capacity, from arming to its final state",
			Buckets: []float64{5, 30, 60, 300, 900, 1800, 3600, 7200, 21600, 86400},
		},
		[]string{"kind", labelState},
	)

	TaskWaitRuntimeReleasesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_task_wait_runtime_releases_total",
			Help: "Resident RuntimeSessions retired because their turn parked, by result",
		},
		[]string{labelResult},
	)

	WaitingForCapacityTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "orka_waiting_for_capacity_total",
			Help: "Transitions into a retryable wait for runtime or provider capacity, by resource and reason",
		},
		[]string{"resource", labelReason},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ApprovalDecisionDuration,
		ACPWorkspaceRetentionActionsTotal,
		ACPWorkspaceRuntimePhaseDuration,
		StorageRecoveryHold,
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
		RepositoryMonitorCommandsTotal,
		RepositoryMonitorWorkActionsTotal,
		RepositoryMonitorGitHubMutationsTotal,
		RepositoryMonitorBlocksTotal,
		RepositoryMonitorReviewsTotal,
		RepositoryMonitorReviewFindingsTotal,
		RepositoryMonitorReviewRoundsToPass,
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
		TaskWaitTransitionsTotal,
		TaskWaitDuration,
		TaskWaitRuntimeReleasesTotal,
		WaitingForCapacityTotal,
	)
}

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

func RecordContextTokenAuth(profile, result string) {
	ContextTokenAuthTotal.WithLabelValues(normalizeMetricLabel(profile), normalizeMetricLabel(result)).Inc()
}

func RecordContextTokenAuthorization(action, result, reason string) {
	ContextTokenAuthorizationTotal.WithLabelValues(
		normalizeMetricLabel(action),
		normalizeMetricLabel(result),
		normalizeMetricLabel(reason),
	).Inc()
}

func RecordContextTokenTTSExchange(result, reason string, durationSeconds float64) {
	result = normalizeMetricLabel(result)
	reason = normalizeMetricLabel(reason)
	ContextTokenTTSExchangeTotal.WithLabelValues(result, reason).Inc()
	ContextTokenTTSExchangeDuration.WithLabelValues(result, reason).Observe(durationSeconds)
}

func RecordTokenExchange(adapter, grantClass, result, reason string, durationSeconds float64) {
	adapter = normalizeMetricLabel(adapter)
	grantClass = normalizeMetricLabel(grantClass)
	result = normalizeMetricLabel(result)
	reason = normalizeMetricLabel(reason)
	TokenExchangeTotal.WithLabelValues(adapter, grantClass, result, reason).Inc()
	TokenExchangeDuration.WithLabelValues(adapter, grantClass, result, reason).Observe(durationSeconds)
}

var acpRuntimePoolAdmissionStates = [...]string{labelUnknown, labelClosed, labelAccepting, labelDraining, labelAmbiguous}

func RecordACPRuntimePoolStatus(
	namespace, runtimePool string,
	desiredReplicas, readyReplicas, sessionsActive, promptsInFlight, queuedTasks int32,
	admissionState string,
) {
	namespace = normalizeMetricLabel(namespace)
	runtimePool = normalizeMetricLabel(runtimePool)
	ACPRuntimePoolDesiredReplicas.WithLabelValues(namespace, runtimePool).Set(float64(desiredReplicas))
	ACPRuntimePoolReadyReplicas.WithLabelValues(namespace, runtimePool).Set(float64(readyReplicas))
	ACPRuntimePoolSessionsActive.WithLabelValues(namespace, runtimePool).Set(float64(sessionsActive))
	ACPRuntimePoolPromptsInFlight.WithLabelValues(namespace, runtimePool).Set(float64(promptsInFlight))
	ACPRuntimePoolQueuedTasks.WithLabelValues(namespace, runtimePool).Set(float64(queuedTasks))
	admissionState = normalizeACPRuntimePoolAdmissionState(admissionState)
	for _, state := range acpRuntimePoolAdmissionStates {
		value := 0.0
		if state == admissionState {
			value = 1
		}
		ACPRuntimePoolAdmissionState.WithLabelValues(namespace, runtimePool, state).Set(value)
	}
}

func RecordACPRuntimePoolScaleToZero(namespace, runtimePool string) {
	ACPRuntimePoolScaleToZeroTotal.WithLabelValues(
		normalizeMetricLabel(namespace), normalizeMetricLabel(runtimePool),
	).Inc()
}

func DeleteACPRuntimePool(namespace, runtimePool string) {
	namespace = normalizeMetricLabel(namespace)
	runtimePool = normalizeMetricLabel(runtimePool)
	ACPRuntimePoolDesiredReplicas.DeleteLabelValues(namespace, runtimePool)
	ACPRuntimePoolReadyReplicas.DeleteLabelValues(namespace, runtimePool)
	ACPRuntimePoolSessionsActive.DeleteLabelValues(namespace, runtimePool)
	ACPRuntimePoolPromptsInFlight.DeleteLabelValues(namespace, runtimePool)
	ACPRuntimePoolQueuedTasks.DeleteLabelValues(namespace, runtimePool)
	for _, state := range acpRuntimePoolAdmissionStates {
		ACPRuntimePoolAdmissionState.DeleteLabelValues(namespace, runtimePool, state)
	}
	ACPRuntimePoolScaleToZeroTotal.DeleteLabelValues(namespace, runtimePool)
}

func normalizeACPRuntimePoolAdmissionState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case labelClosed:
		return labelClosed
	case labelAccepting:
		return labelAccepting
	case labelDraining:
		return labelDraining
	case labelAmbiguous:
		return labelAmbiguous
	default:
		return labelUnknown
	}
}

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

func RecordExecutionEventList(scope string, success bool, durationSeconds float64) {
	scope = normalizeMetricLabel(scope)
	result := "success"
	if !success {
		result = "error"
	}
	ExecutionEventListRequestsTotal.WithLabelValues(scope, result).Inc()
	ExecutionEventListDuration.WithLabelValues(scope, result).Observe(durationSeconds)
}

func RecordExecutionEventStreamOpen(scope string, reconnect bool) func() {
	scope = normalizeMetricLabel(scope)
	ExecutionEventStreamConnections.WithLabelValues(scope).Inc()
	if reconnect {
		ExecutionEventStreamReconnectsTotal.WithLabelValues(scope).Inc()
	}
	return func() { ExecutionEventStreamConnections.WithLabelValues(scope).Dec() }
}

func RecordExecutionEventStreamError(scope, reason string) {
	ExecutionEventStreamErrorsTotal.WithLabelValues(normalizeMetricLabel(scope), normalizeMetricLabel(reason)).Inc()
}

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

func CounterVecValue(counter *prometheus.CounterVec, labels ...string) float64 {
	var m dto.Metric
	if err := counter.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func RecordRepositoryMonitorCommand(intent, status string) {
	RepositoryMonitorCommandsTotal.WithLabelValues(normalizeMetricLabel(intent), normalizeMetricLabel(status)).Inc()
}

func RecordRepositoryMonitorWorkAction(desiredAction, status string) {
	RepositoryMonitorWorkActionsTotal.WithLabelValues(normalizeMetricLabel(desiredAction), normalizeMetricLabel(status)).Inc()
}

func RecordRepositoryMonitorGitHubMutation(operation, status string) {
	RepositoryMonitorGitHubMutationsTotal.WithLabelValues(normalizeMetricLabel(operation), normalizeMetricLabel(status)).Inc()
}

func RecordRepositoryMonitorBlock(reason string) {
	RepositoryMonitorBlocksTotal.WithLabelValues(normalizeMetricLabel(reason)).Inc()
}

func RecordRepositoryMonitorReview(mode, verdict string) {
	RepositoryMonitorReviewsTotal.WithLabelValues(normalizeMetricLabel(mode), normalizeMetricLabel(verdict)).Inc()
}

func RecordRepositoryMonitorReviewFindings(priority, outcome string, count int) {
	if count <= 0 {
		return
	}
	RepositoryMonitorReviewFindingsTotal.WithLabelValues(normalizeMetricLabel(priority), normalizeMetricLabel(outcome)).Add(float64(count))
}

func RecordRepositoryMonitorReviewRoundsToPass(rounds int) {
	RepositoryMonitorReviewRoundsToPass.Observe(float64(rounds))
}

func RecordACPWorkspaceRetentionAction(action, reason string) {
	ACPWorkspaceRetentionActionsTotal.WithLabelValues(normalizeMetricLabel(action), normalizeMetricLabel(reason)).Inc()
}

func RecordACPWorkspaceRuntimePhase(path, phase string, duration time.Duration) {
	if duration < 0 {
		return
	}
	ACPWorkspaceRuntimePhaseDuration.WithLabelValues(normalizeMetricLabel(path), normalizeMetricLabel(phase)).Observe(duration.Seconds())
}

func RecordTaskWaitTransition(kind, state string, waited time.Duration) {
	TaskWaitTransitionsTotal.WithLabelValues(normalizeMetricLabel(kind), normalizeMetricLabel(state)).Inc()
	if waited > 0 {
		TaskWaitDuration.WithLabelValues(normalizeMetricLabel(kind), normalizeMetricLabel(state)).Observe(waited.Seconds())
	}
}

func RecordTaskWaitRuntimeRelease(result string) {
	TaskWaitRuntimeReleasesTotal.WithLabelValues(normalizeMetricLabel(result)).Inc()
}

func RecordWaitingForCapacity(resource, reason string) {
	WaitingForCapacityTotal.WithLabelValues(normalizeMetricLabel(resource), normalizeMetricLabel(reason)).Inc()
}

func normalizeMetricLabel(value string) string {
	if value == "" {
		return labelUnknown
	}
	return strings.Clone(value)
}
