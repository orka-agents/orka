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

// maxMCPApprovalProjectionAttempts bounds repeating a projection whose cleanup
// fence or approval history moved between reading the request and writing the
// projection.
const maxMCPApprovalProjectionAttempts = 3

// errMCPApprovalHistoryChanged reports that a terminal decision was recorded
// after the projection read the approval, so its derived state is stale.
var errMCPApprovalHistoryChanged = errors.New("approval history changed during projection")

// withRetainedMCPApproval runs one projection, including recovery's
// decision/outcome pair, under the Task's cleanup fence. Authority and effect
// reads must finish before entry. The request is read and the projection
// appended as separate fenced calls: cleanup either removes the projection
// after it is appended, or wins first and refuses the append, so no evidence
// is projected onto a request cleanup removed. The appends are deduplicated,
// so a projection repeated after a fence change does not duplicate evidence.
// Absence does not prove cleanup or execution outcome; receipts remain intact.
func withRetainedMCPApproval(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace, taskName string,
	expected approvals.Approval,
	project func(context.Context, approvals.Approval, []store.ExecutionEvent) error,
) (bool, error) {
	dataAccess, ok := eventStore.(store.TaskDataAccessStore)
	if !ok {
		return false, errors.New("approval projection requires fenced task data access")
	}
	var retained bool
	var err error
	for range maxMCPApprovalProjectionAttempts {
		retained, err = projectRetainedMCPApproval(ctx, dataAccess, eventStore, namespace, taskName, expected, project)
		if !errors.Is(err, store.ErrTaskDataCleanupChanged) && !errors.Is(err, errMCPApprovalHistoryChanged) {
			break
		}
	}
	return retained, err
}

func projectRetainedMCPApproval(
	ctx context.Context,
	dataAccess store.TaskDataAccessStore,
	eventStore store.ExecutionEventStore,
	namespace, taskName string,
	expected approvals.Approval,
	project func(context.Context, approvals.Approval, []store.ExecutionEvent) error,
) (bool, error) {
	retained := false
	err := dataAccess.WithTaskDataAccess(ctx, namespace, taskName, func(txCtx context.Context) error {
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
		// Derive decisions and the observed history position from this fenced
		// read, rather than authorizing appends with a cached projection.
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
