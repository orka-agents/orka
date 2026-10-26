package approvals

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

func TestRecordDecisionUsesEventTimestamps(t *testing.T) {
	metrics.ApprovalDecisionDuration.Reset()
	t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics.ApprovalDecisionDuration)
	created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	request := Approval{ID: "synthetic-approval", Status: StatusPending, CreatedAt: created}
	decision := &store.ExecutionEvent{Type: events.ExecutionEventTypeApprovalApproved,
		ToolCallID: request.ID, CreatedAt: created.Add(75 * time.Second)}
	RecordDecision(request, decision)
	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	require.Len(t, families[0].Metric, 1)
	histogram := families[0].Metric[0].Histogram
	require.EqualValues(t, 1, histogram.GetSampleCount())
	require.Equal(t, 75.0, histogram.GetSampleSum())
	for _, bucket := range histogram.Bucket {
		want := uint64(0)
		if bucket.GetUpperBound() >= 75 {
			want = 1
		}
		require.Equal(t, want, bucket.GetCumulativeCount())
	}
}

func TestRecordDecisionOmitsInvalidObservations(t *testing.T) {
	for _, scenario := range []string{"missing decision", "missing request timestamp", "missing decision timestamp", "backwards timestamp", "already decided", "different approval", "execution outcome"} {
		t.Run(scenario, func(t *testing.T) {
			metrics.ApprovalDecisionDuration.Reset()
			t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(metrics.ApprovalDecisionDuration)
			now := time.Now().UTC()
			request := Approval{ID: "synthetic-approval", Status: StatusPending, CreatedAt: now}
			decision := &store.ExecutionEvent{Type: events.ExecutionEventTypeApprovalApproved, ToolCallID: request.ID, CreatedAt: now.Add(time.Second)}
			switch scenario {
			case "missing decision":
				decision = nil
			case "missing request timestamp":
				request.CreatedAt = time.Time{}
			case "missing decision timestamp":
				decision.CreatedAt = time.Time{}
			case "backwards timestamp":
				decision.CreatedAt = now.Add(-time.Second)
			case "already decided":
				request.Status = StatusApproved
			case "different approval":
				decision.ToolCallID = "different-approval"
			case "execution outcome":
				decision.Type = events.ExecutionEventTypeApprovalExecutionUpdated
			}
			RecordDecision(request, decision)
			families, err := registry.Gather()
			require.NoError(t, err)
			require.Empty(t, families)
		})
	}
}
