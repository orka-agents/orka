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

const maxMCPApprovalProjectionAttempts = 3

var errMCPApprovalHistoryChanged = errors.New("approval history changed during projection")

type mcpApprovalFence int

const (
	mcpApprovalTaskFence mcpApprovalFence = iota
	mcpApprovalNamespaceFence
)

func withRetainedMCPApproval(
	ctx context.Context,
	eventStore store.ExecutionEventStore,
	namespace, taskName string,
	fence mcpApprovalFence,
	expected approvals.Approval,
	project func(context.Context, approvals.Approval, []store.ExecutionEvent) (*store.ExecutionEvent, error),
) (bool, error) {
	dataAccess, ok := eventStore.(store.TaskDataAccessStore)
	if !ok {
		return false, errors.New("approval projection requires fenced task data access")
	}
	var retained bool
	var err error
	for range maxMCPApprovalProjectionAttempts {
		retained, err = projectRetainedMCPApproval(ctx, dataAccess, eventStore, namespace, taskName, fence, expected, project)
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
	fence mcpApprovalFence,
	expected approvals.Approval,
	project func(context.Context, approvals.Approval, []store.ExecutionEvent) (*store.ExecutionEvent, error),
) (bool, error) {
	fenceTask := taskName
	if fence == mcpApprovalNamespaceFence {
		fenceTask = ""
	}
	retained := false
	var request approvals.Approval
	var decision *store.ExecutionEvent
	err := dataAccess.WithTaskDataAccess(ctx, namespace, fenceTask, func(txCtx context.Context) error {
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
		for _, approval := range approvals.Derive(listed, time.Time{}) {
			if mcpApprovalProjectionBindingMatches(approval, expected) {
				request = approval
				var err error
				decision, err = project(txCtx, approval, listed)
				return err
			}
		}
		retained = false
		return nil
	})
	if err == nil {
		approvals.RecordDecision(request, decision)
	}
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
