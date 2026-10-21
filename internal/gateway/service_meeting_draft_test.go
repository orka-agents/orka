package gateway

import (
	"encoding/json"
	"testing"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
)

func TestGatewayMeetingDraftExtensionAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, extensions string
		interim, allowed bool
	}{
		{"supported", `["meetingDraft.v1"]`, true, true},
		{"unrelated", `["future.v1"]`, true, false},
		{"wrong version", `["meetingDraft.v2"]`, true, false},
		{"wrong case", `["meetingdraft.v1"]`, true, false},
		{"not normalized", `[" meetingDraft.v1"]`, true, false},
		{"no interim", `["meetingDraft.v1"]`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _, task := newGatewayMessageFixture(t)
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
				require.NoError(t, json.Unmarshal([]byte(`{"extensions":`+tc.extensions+`}`), &g.Status.ObservedCapabilities.Capabilities))
				g.Status.ObservedCapabilities.Capabilities.InterimDelivery = tc.interim
			})
			draft := protocol.MeetingDraft{Subject: "Planning", Agenda: []string{"Next steps"}, Attendees: protocol.MeetingAttendees{Mode: "everyone"}}
			prepared, err := s.PrepareTaskMeetingDraft(t.Context(), task.Namespace, task.Name, string(task.UID), "meeting", "Draft above", draft)
			require.NoError(t, err)
			_, err = s.EnqueuePreparedTaskMessage(t.Context(), prepared)
			switch {
			case tc.allowed:
				require.NoError(t, err)
			case !tc.interim:
				require.ErrorIs(t, err, ErrInterimDeliveryUnsupported)
			default:
				require.ErrorIs(t, err, ErrMeetingDraftsUnsupported)
			}
		})
	}
}

func TestGatewayMeetingDraftCanonicalPersistenceReplay(t *testing.T) {
	for _, subject := range []string{" Planning ", "token is unit-test-placeholder"} {
		t.Run(subject, func(t *testing.T) {
			s, db, adapter, event, task := newGatewayMessageFixture(t)
			s.Config.InterimMessagesPerTask = 1
			updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
				g.Status.ObservedCapabilities.Capabilities.Extensions = []string{protocol.MeetingDraftExtension}
			})
			draft := protocol.MeetingDraft{Subject: subject, Agenda: []string{" Next steps "}, Attendees: protocol.MeetingAttendees{Mode: "named", Names: []string{" Alice Smith "}}, PreferredWindow: " Tomorrow "}
			enqueue := func() *TaskMessageReceipt {
				prepared, err := s.PrepareTaskMeetingDraft(t.Context(), task.Namespace, task.Name, string(task.UID), "meeting", "Draft above", draft)
				require.NoError(t, err)
				receipt, err := s.EnqueuePreparedTaskMessage(t.Context(), prepared)
				require.NoError(t, err)
				return receipt
			}
			first := enqueue()
			replay := enqueue()
			require.Equal(t, first.DeliveryID, replay.DeliveryID)
			require.False(t, replay.Created)
			row, err := db.GetGatewayDelivery(t.Context(), event.Namespace, first.DeliveryID)
			require.NoError(t, err)
			want := "Planning"
			if subject != " Planning " {
				want = "token is [REDACTED]"
			}
			require.Equal(t, want, row.Metadata["meeting.subject"])
			require.Equal(t, "Next steps", row.Metadata["meeting.agenda.0"])
			require.Equal(t, "Alice Smith", row.Metadata["meeting.name.0"])
			require.Equal(t, "Tomorrow", row.Metadata["meeting.preferredWindow"])
			require.NoError(t, s.DeliverOnce(t.Context()))
			require.Len(t, adapter.Deliveries(), 1)
			require.Equal(t, row.Metadata, adapter.Deliveries()[0].Metadata)
			require.NotContains(t, adapter.Deliveries()[0].Metadata["meeting.subject"], "unit-test-placeholder")
			replay = enqueue()
			require.False(t, replay.Created)
			rows, err := db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: event.Namespace})
			require.NoError(t, err)
			require.Len(t, rows, 1)
		})
	}
}

func TestGatewayMeetingDraftDispatchVersionAndExtension(t *testing.T) {
	s, db, _, event, task := newGatewayMessageFixture(t)
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
		g.Status.ObservedCapabilities.Capabilities.Extensions = []string{protocol.MeetingDraftExtension, "meetingDraft.v2"}
	})
	draft := protocol.MeetingDraft{Subject: "Planning", Agenda: []string{"Next steps"}, Attendees: protocol.MeetingAttendees{Mode: "everyone"}}
	prepared, err := s.PrepareTaskMeetingDraft(t.Context(), task.Namespace, task.Name, string(task.UID), "meeting", "Draft above", draft)
	require.NoError(t, err)
	receipt, err := s.EnqueuePreparedTaskMessage(t.Context(), prepared)
	require.NoError(t, err)
	row, err := db.GetGatewayDelivery(t.Context(), event.Namespace, receipt.DeliveryID)
	require.NoError(t, err)
	require.Equal(t, "1", row.Metadata["meeting.version"])
	require.NoError(t, s.validateMessageDelivery(t.Context(), row))
	for _, version := range []string{"", "2", "meetingDraft.v1"} {
		row.Metadata["meeting.version"] = version
		require.ErrorIs(t, s.validateMessageDelivery(t.Context(), row), ErrMeetingDraftsUnsupported)
	}
	row.Metadata["meeting.version"] = "1"
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
		g.Status.ObservedCapabilities.Capabilities.Extensions = []string{"meetingDraft.v2"}
	})
	require.ErrorIs(t, s.validateMessageDelivery(t.Context(), row), ErrMeetingDraftsUnsupported)
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
		g.Status.ObservedCapabilities.Capabilities.Extensions = []string{protocol.MeetingDraftExtension}
		g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false
	})
	require.ErrorIs(t, s.validateMessageDelivery(t.Context(), row), ErrInterimDeliveryUnsupported)
}

func TestGatewayMeetingDraftCapabilityWithdrawalAbandons(t *testing.T) {
	s, db, adapter, event, task := newGatewayMessageFixture(t)
	draft := protocol.MeetingDraft{Subject: "Planning", Agenda: []string{"Next steps"}, Attendees: protocol.MeetingAttendees{Mode: "everyone"}}
	denied, err := s.PrepareTaskMeetingDraft(t.Context(), task.Namespace, task.Name, string(task.UID), "meeting", "Draft above", draft)
	require.NoError(t, err)
	_, err = s.EnqueuePreparedTaskMessage(t.Context(), denied)
	require.ErrorIs(t, err, ErrMeetingDraftsUnsupported)
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) {
		g.Status.ObservedCapabilities.Capabilities.Extensions = []string{protocol.MeetingDraftExtension}
	})
	prepared, err := s.PrepareTaskMeetingDraft(t.Context(), task.Namespace, task.Name, string(task.UID), "meeting", "Draft above", draft)
	require.NoError(t, err)
	receipt, err := s.EnqueuePreparedTaskMessage(t.Context(), prepared)
	require.NoError(t, err)
	_, err = s.EnqueueTaskMessage(t.Context(), task.Namespace, task.Name, string(task.UID), "text", "ordinary progress")
	require.NoError(t, err)
	updateMessageGateway(t, s, func(g *gatewayv1alpha1.Gateway) { g.Status.ObservedCapabilities.Capabilities.Extensions = nil })
	require.NoError(t, s.DeliverOnce(t.Context()))
	row, err := db.GetGatewayDelivery(t.Context(), event.Namespace, receipt.DeliveryID)
	require.NoError(t, err)
	require.Equal(t, store.GatewayDeliveryDeadLettered, row.State)
	require.Empty(t, adapter.Deliveries())
	require.NoError(t, s.DeliverOnce(t.Context()))
	require.Len(t, adapter.Deliveries(), 1)
	require.Equal(t, "ordinary progress", adapter.Deliveries()[0].Text)
	prepared, err = s.PrepareTaskMeetingDraft(t.Context(), task.Namespace, task.Name, string(task.UID), "meeting", "Draft above", draft)
	require.NoError(t, err)
	replay, err := s.EnqueuePreparedTaskMessage(t.Context(), prepared)
	require.NoError(t, err)
	require.Equal(t, receipt.DeliveryID, replay.DeliveryID)
	require.False(t, replay.Created)
}
