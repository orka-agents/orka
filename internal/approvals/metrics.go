package approvals

import (
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

// RecordDecision observes only a newly committed terminal event. Callers must
// exclude duplicate appends and wait for the enclosing transaction to commit.
// Reading or deriving approval state must never record an observation.
func RecordDecision(request Approval, decision *store.ExecutionEvent) {
	if decision == nil || request.Status != StatusPending || request.CreatedAt.IsZero() ||
		decision.CreatedAt.IsZero() || decision.CreatedAt.Before(request.CreatedAt) ||
		store.ApprovalIDFromExecutionEvent(*decision) != request.ID {
		return
	}
	var outcome string
	switch decision.Type {
	case events.ExecutionEventTypeApprovalApproved:
		outcome = "approved"
	case events.ExecutionEventTypeApprovalDeclined:
		outcome = "denied"
	case events.ExecutionEventTypeApprovalCancelled:
		outcome = "cancelled"
	case events.ExecutionEventTypeApprovalExpired:
		outcome = "expired"
	default:
		return
	}
	path := "worker"
	if request.Binding != nil {
		path = "acp"
	}
	metrics.ApprovalDecisionDuration.WithLabelValues(outcome, path).Observe(decision.CreatedAt.Sub(request.CreatedAt).Seconds())
}
