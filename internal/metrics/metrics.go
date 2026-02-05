/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// Task metrics
	TasksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_tasks_total",
			Help: "Total number of tasks by type, phase, and namespace",
		},
		[]string{"type", "phase", "namespace"},
	)

	TasksActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_tasks_active",
			Help: "Number of currently running tasks",
		},
		[]string{"type", "namespace"},
	)

	TaskDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mercan_task_duration_seconds",
			Help:    "Task execution duration in seconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s to ~1h
		},
		[]string{"type", "phase", "namespace"},
	)

	TaskQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_task_queue_depth",
			Help: "Number of tasks waiting in queue by priority level",
		},
		[]string{"priority"},
	)

	TaskRetries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_task_retries_total",
			Help: "Total number of task retry attempts",
		},
		[]string{"namespace"},
	)

	// Webhook metrics
	WebhookDeliveries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_webhook_deliveries_total",
			Help: "Total webhook delivery attempts by status",
		},
		[]string{"status"},
	)

	// API metrics
	APIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_api_requests_total",
			Help: "Total API requests by endpoint, method, and status",
		},
		[]string{"endpoint", "method", "status"},
	)

	APIRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mercan_api_request_duration_seconds",
			Help:    "API request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "method"},
	)

	// Session metrics
	SessionsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_sessions_total",
			Help: "Total active sessions by namespace",
		},
		[]string{"namespace"},
	)

	SessionMessages = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_session_messages_total",
			Help: "Total messages appended to sessions",
		},
		[]string{"namespace"},
	)

	SessionQueueWaiting = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_session_queue_waiting",
			Help: "Tasks waiting for session lock by session",
		},
		[]string{"session", "namespace"},
	)

	// Tool metrics
	ToolsDiscovered = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_tools_discovered",
			Help: "Number of Tool CRDs discovered per namespace",
		},
		[]string{"namespace"},
	)

	ToolCalls = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_tool_calls_total",
			Help: "Total tool invocations by tool name and status",
		},
		[]string{"tool", "status"},
	)

	ToolCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mercan_tool_call_duration_seconds",
			Help:    "Tool HTTP call latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"tool"},
	)

	ToolHealthStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_tool_health_status",
			Help: "Tool availability (1=available, 0=unavailable)",
		},
		[]string{"tool", "namespace"},
	)

	// Agent metrics
	AgentsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_agents_total",
			Help: "Total agents by namespace",
		},
		[]string{"namespace"},
	)

	AgentTasksActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mercan_agent_tasks_active",
			Help: "Active tasks per agent",
		},
		[]string{"agent", "namespace"},
	)

	AgentTasksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_agent_tasks_total",
			Help: "Total tasks per agent",
		},
		[]string{"agent", "namespace"},
	)

	// Skill metrics
	SkillsLoaded = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mercan_skills_loaded_total",
			Help: "Skills loaded by namespace and name",
		},
		[]string{"skill", "namespace"},
	)
)

func init() {
	// Register all metrics with controller-runtime's registry
	metrics.Registry.MustRegister(
		// Task metrics
		TasksTotal,
		TasksActive,
		TaskDuration,
		TaskQueueDepth,
		TaskRetries,

		// Webhook metrics
		WebhookDeliveries,

		// API metrics
		APIRequestsTotal,
		APIRequestDuration,

		// Session metrics
		SessionsTotal,
		SessionMessages,
		SessionQueueWaiting,

		// Tool metrics
		ToolsDiscovered,
		ToolCalls,
		ToolCallDuration,
		ToolHealthStatus,

		// Agent metrics
		AgentsTotal,
		AgentTasksActive,
		AgentTasksTotal,

		// Skill metrics
		SkillsLoaded,
	)
}

// RecordTaskCreated records when a task is created
func RecordTaskCreated(taskType, namespace string) {
	TasksTotal.WithLabelValues(taskType, "Pending", namespace).Inc()
	TasksActive.WithLabelValues(taskType, namespace).Inc()
}

// RecordTaskCompleted records when a task completes
func RecordTaskCompleted(taskType, phase, namespace string, durationSeconds float64) {
	TasksTotal.WithLabelValues(taskType, phase, namespace).Inc()
	TasksActive.WithLabelValues(taskType, namespace).Dec()
	TaskDuration.WithLabelValues(taskType, phase, namespace).Observe(durationSeconds)
}

// RecordTaskRetry records a task retry
func RecordTaskRetry(namespace string) {
	TaskRetries.WithLabelValues(namespace).Inc()
}

// RecordWebhookDelivery records a webhook delivery attempt
func RecordWebhookDelivery(success bool) {
	status := "success"
	if !success {
		status = "failure"
	}
	WebhookDeliveries.WithLabelValues(status).Inc()
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

// RecordToolCall records a tool invocation
func RecordToolCall(tool string, success bool, durationSeconds float64) {
	status := "success"
	if !success {
		status = "failure"
	}
	ToolCalls.WithLabelValues(tool, status).Inc()
	ToolCallDuration.WithLabelValues(tool).Observe(durationSeconds)
}
