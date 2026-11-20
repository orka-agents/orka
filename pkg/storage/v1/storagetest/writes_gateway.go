package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// gatewayCaseNow is the request time of the gateway write cases. Gateway
// leases and claims are decided on request times, so a fixed time works.
var gatewayCaseNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const (
	gatewayCaseOwner    = "dispatcher"
	gatewayCaseSender   = "sender"
	gatewayCaseTaskUID  = "task-uid"
	gatewayCaseDelivery = "gdl-case"
)

func gatewayCaseEvent() storagev1.GatewayEvent {
	return gatewayEvent(gatewayCaseNow, eventsTenant, "case")
}

// gatewayRetentionEvent is old enough for retention to compact.
func gatewayRetentionEvent() storagev1.GatewayEvent {
	return gatewayEvent(gatewayCaseNow.Add(-2*time.Hour), eventsTenant, "case")
}

func gatewayCaseDeliveryRecord() storagev1.GatewayDelivery {
	return gatewayDelivery(gatewayCaseEvent(), gatewayCaseDelivery, "final", gatewayCaseNow)
}

func admitGatewayCase(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	admitGateway(t, p, gatewayCaseEvent())
}

func claimGatewayCase(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	admitGatewayCase(t, p)
	claimed, err := p.ClaimNextGatewayEvent(t.Context(), eventsTenant, gatewayCaseOwner, gatewayCaseNow, time.Minute)
	require.NoError(t, err)
	require.Equal(t, gatewayCaseEvent().ID, claimed.ID)
}

func linkGatewayCase(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	linkGatewayTask(t, p, gatewayCaseEvent(), gatewayCaseTaskUID)
}

func expireGatewayCase(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	claimGatewayCase(t, p)
	require.NoError(t, p.ExpireGatewayEvent(t.Context(), eventsTenant, gatewayCaseEvent().ID, gatewayCaseOwner, "expired", gatewayCaseNow))
}

func createGatewayCaseDelivery(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	delivery := gatewayCaseDeliveryRecord()
	_, created, err := p.CreateGatewayDelivery(t.Context(), &delivery)
	require.NoError(t, err)
	require.True(t, created)
}

func claimGatewayCaseDelivery(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	createGatewayCaseDelivery(t, p)
	claimed, err := p.ClaimNextGatewayDelivery(t.Context(), eventsTenant, gatewayCaseSender, gatewayCaseNow, time.Minute)
	require.NoError(t, err)
	require.Equal(t, gatewayCaseDelivery, claimed.ID)
}

func failGatewayCaseDelivery(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	claimGatewayCaseDelivery(t, p)
	require.NoError(t, p.MarkGatewayDeliveryTerminal(t.Context(), eventsTenant, gatewayCaseDelivery, gatewayCaseSender,
		storagev1.GatewayDeliveryFailed, "permanent", gatewayCaseNow))
}

// expireGatewayRetentionCase links an old event and expires it with a
// delivered reply an hour before gatewayCaseNow, so retention can compact it.
func expireGatewayRetentionCase(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	event := linkGatewayTask(t, p, gatewayRetentionEvent(), gatewayCaseTaskUID)
	cutoff := gatewayCaseNow.Add(-time.Hour)
	delivered := gatewayDelivery(event, gatewayCaseDelivery, "error", cutoff)
	delivered.State, delivered.ProviderMessageID, delivered.DeliveredAt = storagev1.GatewayDeliveryDelivered, "provider", &cutoff
	_, _, err := p.ExpireGatewayEventWithDelivery(t.Context(), storagev1.GatewayExpiryProjection{
		EventID: event.ID, Reason: "expired", CompletedAt: cutoff, Delivery: delivered,
	})
	require.NoError(t, err)
}

func compactGatewayRetentionCase(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	expireGatewayRetentionCase(t, p)
	result, err := p.MaintainGatewayRecords(t.Context(), eventsTenant, gatewayCaseNow, gatewayCaseNow.Add(-time.Hour).Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
}

// observeGatewayCase reads the case event, its delivery and its Session.
func observeGatewayCase(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	ctx := t.Context()
	event := gatewayCaseEvent()
	stored, err := p.GetGatewayEvent(ctx, event.Namespace, event.ID)
	delivery, deliveryErr := p.GetGatewayDelivery(ctx, event.Namespace, gatewayCaseDelivery)
	session, sessionErr := p.GetSession(ctx, event.Namespace, event.SessionName)
	return []any{observed(t, stored, err), observed(t, delivery, deliveryErr), observed(t, session, sessionErr)}
}

// observeGatewayRetention reads what retention compaction changes.
func observeGatewayRetention(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	ctx := t.Context()
	event := gatewayRetentionEvent()
	stored, err := p.GetGatewayEvent(ctx, event.Namespace, event.ID)
	receipt, receiptErr := p.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, gatewayCaseTaskUID)
	tombstoned, tombstoneErr := p.HasGatewayTaskTombstone(ctx, event.Namespace, event.TaskName, gatewayCaseTaskUID)
	require.NoError(t, tombstoneErr)
	return []any{observed(t, stored, err), observed(t, receipt, receiptErr), tombstoned}
}

// observeGatewayMessages reads the case event's message budget and deliveries.
func observeGatewayMessages(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	ctx := t.Context()
	event := gatewayCaseEvent()
	budget, err := p.GetGatewayMessageBudget(ctx, storagev1.GatewayMessageBudgetQuery{
		Namespace: event.Namespace, NamespaceUID: event.NamespaceUID, EventID: event.ID,
		TaskName: event.TaskName, TaskUID: gatewayCaseTaskUID, RequestID: "operation-1",
	})
	require.NoError(t, err)
	deliveries, err := p.ListGatewayDeliveries(ctx, storagev1.GatewayDeliveryFilter{Namespace: event.Namespace, EventID: event.ID})
	require.NoError(t, err)
	return []any{budget, deliveries}
}

func init() {
	event := gatewayCaseEvent()
	ns, id := event.Namespace, event.ID
	stateWriteCases = append(stateWriteCases,
		writeCase[storagev1.StateProvider]{
			method: "AdmitGatewayEvent",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{storagev1.GatewayEventAdmission{Event: gatewayCaseEvent(), AppendUserMessage: true, PendingLimit: 100}}
			},
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ClaimNextGatewayEvent",
			setup:   admitGatewayCase,
			args:    fixed(ns, gatewayCaseOwner, gatewayCaseNow, time.Minute),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "RenewGatewayEventClaim",
			setup:   claimGatewayCase,
			args:    fixed(ns, id, gatewayCaseOwner, gatewayCaseNow.Add(30*time.Second), time.Minute),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method: "FreezeGatewayEventTaskRuntimeAllowedTools",
			setup:  claimGatewayCase,
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{ns, id, gatewayCaseOwner, []string{"tool"}, gatewayCaseNow}
			},
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "MarkGatewayEventTaskCreated",
			setup:   claimGatewayCase,
			args:    fixed(ns, id, event.TaskName, gatewayCaseTaskUID, gatewayCaseOwner, gatewayCaseNow),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "RetryGatewayEvent",
			setup:   claimGatewayCase,
			args:    fixed(ns, id, gatewayCaseOwner, "temporary", gatewayCaseNow.Add(time.Minute)),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "DeferGatewayEventProjection",
			setup:   linkGatewayCase,
			args:    fixed(ns, id, gatewayCaseNow.Add(time.Minute)),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ExpireGatewayEvent",
			setup:   claimGatewayCase,
			args:    fixed(ns, id, gatewayCaseOwner, "expired", gatewayCaseNow),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method: "ExpireGatewayEventWithDelivery",
			setup:  linkGatewayCase,
			args: func(storagev1.ControllerEpochFence) []any {
				event := gatewayCaseEvent()
				event.TaskUID = gatewayCaseTaskUID
				return []any{storagev1.GatewayExpiryProjection{
					EventID: event.ID, Reason: "expired", CompletedAt: gatewayCaseNow,
					Delivery: gatewayDelivery(event, gatewayCaseDelivery, "error", gatewayCaseNow),
				}}
			},
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "MarkExpiredGatewayEventDeadLettered",
			setup:   expireGatewayCase,
			args:    fixed(ns, id, "no reply route", gatewayCaseNow),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method: "ProjectGatewayTerminal",
			setup:  linkGatewayCase,
			args: func(storagev1.ControllerEpochFence) []any {
				event := gatewayCaseEvent()
				event.TaskUID = gatewayCaseTaskUID
				return []any{storagev1.GatewayTerminalProjection{
					EventID: event.ID, CompletedAt: gatewayCaseNow,
					Message: storagev1.SessionMessage{
						ID: storagev1.GatewayAssistantMessageID(event.ID), Role: "assistant", Content: "done",
						SourceType: "gateway-task", SourceRef: event.TaskName, Timestamp: gatewayCaseNow,
					},
					Delivery: gatewayDelivery(event, gatewayCaseDelivery, "final", gatewayCaseNow),
				}}
			},
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method: "CreateGatewayDelivery",
			args: func(storagev1.ControllerEpochFence) []any {
				delivery := gatewayCaseDeliveryRecord()
				return []any{&delivery}
			},
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ClaimNextGatewayDelivery",
			setup:   createGatewayCaseDelivery,
			args:    fixed(ns, gatewayCaseSender, gatewayCaseNow, time.Minute),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "MarkGatewayDeliveryDelivered",
			setup:   claimGatewayCaseDelivery,
			args:    fixed(ns, gatewayCaseDelivery, gatewayCaseSender, "provider-1", gatewayCaseNow),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "ScheduleGatewayDeliveryRetry",
			setup:   claimGatewayCaseDelivery,
			args:    fixed(ns, gatewayCaseDelivery, gatewayCaseSender, "temporary", gatewayCaseNow.Add(time.Second)),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method: "MarkGatewayDeliveryTerminal",
			setup:  claimGatewayCaseDelivery,
			args: fixed(ns, gatewayCaseDelivery, gatewayCaseSender, storagev1.GatewayDeliveryFailed,
				"permanent", gatewayCaseNow),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "RetryGatewayDelivery",
			setup:   failGatewayCaseDelivery,
			args:    fixed(ns, gatewayCaseDelivery, gatewayCaseNow.Add(time.Minute), gatewayCaseNow.Add(time.Hour)),
			observe: observeGatewayCase,
		},
		writeCase[storagev1.StateProvider]{
			method:  "MaintainGatewayRecords",
			setup:   expireGatewayRetentionCase,
			args:    fixed(ns, gatewayCaseNow, gatewayCaseNow.Add(-time.Hour).Add(time.Second)),
			observe: observeGatewayRetention,
		},
		writeCase[storagev1.StateProvider]{
			method: "EnqueueGatewayMessage",
			setup:  linkGatewayCase,
			args: func(storagev1.ControllerEpochFence) []any {
				event := gatewayCaseEvent()
				return []any{storagev1.GatewayMessageEnqueue{
					Namespace: event.Namespace, NamespaceUID: event.NamespaceUID, EventID: event.ID,
					TaskName: event.TaskName, TaskUID: gatewayCaseTaskUID, RequestID: "operation-1",
					Text: "working", MaxMessages: 2, MaxAttempts: 3, Now: gatewayCaseNow, ExpiresAt: event.ExpiresAt,
				}}
			},
			observe: observeGatewayMessages,
		},
		writeCase[storagev1.StateProvider]{
			method:  "DeleteGatewayTaskCleanupReceipt",
			setup:   compactGatewayRetentionCase,
			args:    fixed(ns, gatewayRetentionEvent().TaskName, gatewayCaseTaskUID),
			observe: observeGatewayRetention,
		},
	)
}
