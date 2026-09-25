package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

// withRetainedMCPApproval owns the single SQLite boundary for a projection,
// including recovery's decision/outcome pair. Authority and effect reads must
// finish before entry. Cleanup either removes this projection after commit, or
// wins first and leaves no request on which a late writer can project evidence.
// Absence does not prove cleanup or execution outcome; receipts remain intact.
func withRetainedMCPApproval(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace, taskName string,
	expected approvals.Approval,
	project func(context.Context, approvals.Approval, []store.ExecutionEvent) error,
) (bool, error) {
	transactions, ok := eventStore.(store.TaskDataTransactionStore)
	if !ok {
		return false, errors.New("approval projection requires task data transactions")
	}
	retained := false
	err := transactions.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		listed, err := approvals.ListEvents(txCtx, eventStore, namespace, taskName)
		if err != nil {
			return err
		}
		listed = approvals.FilterEventsForTaskUID(listed, expected.TaskUID)
		for _, event := range listed {
			if event.Type != events.ExecutionEventTypeApprovalRequested || event.Namespace != namespace ||
				event.StreamType != events.ExecutionEventStreamTypeTask || event.StreamID != taskName || event.TaskName != taskName {
				continue
			}
			// Require an explicit UID on the original request, not the legacy
			// untagged-event fallback or a terminal-only derived approval.
			var request struct {
				ApprovalID string `json:"approvalID"`
				approvals.Approval
			}
			if json.Unmarshal(event.Content, &request) != nil {
				continue
			}
			request.ID = request.ApprovalID
			if mcpApprovalProjectionBindingMatches(request.Approval, expected) {
				retained = true
				break
			}
		}
		if !retained {
			return nil
		}
		// Derive decisions and the observed history position under the same
		// writer, rather than authorizing appends with a cached projection.
		for _, approval := range approvals.Derive(listed, time.Time{}) {
			if mcpApprovalProjectionBindingMatches(approval, expected) {
				return project(txCtx, approval, listed)
			}
		}
		retained = false
		return nil
	})
	return retained, err
}

func mcpApprovalProjectionBindingMatches(actual, expected approvals.Approval) bool {
	if actual.ID != expected.ID || actual.TaskUID == "" || actual.TaskUID != expected.TaskUID ||
		actual.Binding == nil || expected.Binding == nil || *actual.Binding != *expected.Binding {
		return false
	}
	if actual.ExpiresAt == nil || expected.ExpiresAt == nil {
		return actual.ExpiresAt == nil && expected.ExpiresAt == nil
	}
	return actual.ExpiresAt.Equal(*expected.ExpiresAt)
}
