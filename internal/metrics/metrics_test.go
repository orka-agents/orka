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

// Helper function to get gauge value
func getGaugeValue(gauge *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	if err := gauge.WithLabelValues(labels...).Write(&m); err != nil {
		return 0
	}
	return m.GetGauge().GetValue()
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

func TestRecordTaskCreated(t *testing.T) {
	// Reset metrics before test
	TasksTotal.Reset()
	TasksActive.Reset()

	RecordTaskCreated("ai", "default")

	// Check that counters were incremented
	total := getCounterValue(TasksTotal, "ai", "Pending", "default")
	if total != 1 {
		t.Errorf("TasksTotal = %v, want 1", total)
	}

	active := getGaugeValue(TasksActive, "ai", "default")
	if active != 1 {
		t.Errorf("TasksActive = %v, want 1", active)
	}
}

func TestRecordTaskCompleted(t *testing.T) {
	// Reset metrics before test
	TasksTotal.Reset()
	TasksActive.Reset()
	TaskDuration.Reset()

	// First create a task
	TasksActive.WithLabelValues("container", "default").Inc()

	RecordTaskCompleted("container", "Succeeded", "default", 10.5)

	// Check total was incremented
	total := getCounterValue(TasksTotal, "container", "Succeeded", "default")
	if total != 1 {
		t.Errorf("TasksTotal = %v, want 1", total)
	}

	// Check active was decremented
	active := getGaugeValue(TasksActive, "container", "default")
	if active != 0 {
		t.Errorf("TasksActive = %v, want 0", active)
	}

	// Check duration was recorded
	durationCount := getHistogramCount(TaskDuration, "container", "Succeeded", "default")
	if durationCount != 1 {
		t.Errorf("TaskDuration count = %v, want 1", durationCount)
	}
}

func TestRecordTaskRetry(t *testing.T) {
	TaskRetries.Reset()

	RecordTaskRetry("default")

	retries := getCounterValue(TaskRetries, "default")
	if retries != 1 {
		t.Errorf("TaskRetries = %v, want 1", retries)
	}

	// Call again
	RecordTaskRetry("default")
	retries = getCounterValue(TaskRetries, "default")
	if retries != 2 {
		t.Errorf("TaskRetries = %v, want 2", retries)
	}
}

func TestRecordWebhookDelivery(t *testing.T) {
	WebhookDeliveries.Reset()

	tests := []struct {
		name    string
		success bool
		status  string
	}{
		{
			name:    "success",
			success: true,
			status:  "success",
		},
		{
			name:    "failure",
			success: false,
			status:  "failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RecordWebhookDelivery(tt.success)

			count := getCounterValue(WebhookDeliveries, tt.status)
			if count != 1 {
				t.Errorf("WebhookDeliveries[%s] = %v, want 1", tt.status, count)
			}
		})
		// Reset for next test
		WebhookDeliveries.Reset()
	}
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

func TestRecordToolCall(t *testing.T) {
	ToolCalls.Reset()
	ToolCallDuration.Reset()

	tests := []struct {
		name    string
		tool    string
		success bool
		status  string
	}{
		{
			name:    "success",
			tool:    "web_search",
			success: true,
			status:  "success",
		},
		{
			name:    "failure",
			tool:    "code_exec",
			success: false,
			status:  "failure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ToolCalls.Reset()
			ToolCallDuration.Reset()

			RecordToolCall(tt.tool, tt.success, 0.5)

			count := getCounterValue(ToolCalls, tt.tool, tt.status)
			if count != 1 {
				t.Errorf("ToolCalls = %v, want 1", count)
			}

			durationCount := getHistogramCount(ToolCallDuration, tt.tool)
			if durationCount != 1 {
				t.Errorf("ToolCallDuration count = %v, want 1", durationCount)
			}
		})
	}
}

func TestMetricsRegistered(t *testing.T) {
	// Verify that all metrics are not nil (registered during init)
	metrics := []any{
		TasksTotal,
		TasksActive,
		TaskDuration,
		TaskQueueDepth,
		TaskRetries,
		WebhookDeliveries,
		APIRequestsTotal,
		APIRequestDuration,
		SessionsTotal,
		SessionMessages,
		SessionQueueWaiting,
		ToolsDiscovered,
		ToolCalls,
		ToolCallDuration,
		ToolHealthStatus,
		AgentsTotal,
		AgentTasksActive,
		AgentTasksTotal,
		SkillsLoaded,
	}

	for i, m := range metrics {
		if m == nil {
			t.Errorf("metric %d is nil", i)
		}
	}
}
