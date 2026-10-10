package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

func unboundGatewaySoulMessages() []store.SessionMessage {
	return []store.SessionMessage{
		{ID: store.GatewayUserMessageID("failed"), Role: "user", SourceType: "gateway-event", SourceRef: "failed"},
		{ID: store.GatewayErrorMessageID("failed"), Role: "assistant", SourceType: "gateway-task", SourceRef: "failed-task",
			Metadata: map[string]string{store.SessionSoulUnboundMetadata: "true", "eventId": "failed", "taskName": "failed-task"}},
		{ID: store.GatewayUserMessageID("current"), Role: "user", SourceType: "gateway-event", SourceRef: "current"},
	}
}

func readSoulHistoryFixture(t *testing.T, sessionType string, messages []store.SessionMessage) (store.SessionSoulState, error) {
	t.Helper()
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Now().UTC()
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{
		Namespace: "default", Name: "session", SessionType: sessionType, ActiveTask: "current", ActiveTaskUID: "current-uid",
		MessageCount: len(messages), CreatedAt: now, UpdatedAt: now,
	}))
	for i, message := range messages {
		metadata, err := json.Marshal(message.Metadata)
		require.NoError(t, err)
		_, err = s.db.ExecContext(ctx, `INSERT INTO session_messages
		 (namespace, session_name, message_id, sort_order, role, content, source_type, source_ref, metadata_json, created_at)
		 VALUES ('default', 'session', ?, ?, ?, '', ?, ?, ?, ?)`,
			message.ID, i+1, message.Role, message.SourceType, message.SourceRef, string(metadata), now)
		require.NoError(t, err)
	}
	return s.ReadSessionSoul(ctx, "default", "session", "current", "current-uid")
}

func TestGatewaySessionSoulUnboundRequiresCanonicalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*store.SessionMessage)
		unbound bool
	}{
		{name: "explicit unbound canonical error", unbound: true},
		{name: "legacy error", mutate: func(m *store.SessionMessage) { delete(m.Metadata, store.SessionSoulUnboundMetadata) }},
		{name: "unknown marker", mutate: func(m *store.SessionMessage) { m.Metadata[store.SessionSoulUnboundMetadata] = "unknown" }},
		{name: "explicit no soul wins", mutate: func(m *store.SessionMessage) { m.Metadata[store.SessionSoulDigestMetadata] = "" }},
		{name: "bound error wins", mutate: func(m *store.SessionMessage) {
			m.Metadata[store.SessionSoulDigestMetadata] = agentcontext.Digest("revision")
		}},
		{name: "successful message", mutate: func(m *store.SessionMessage) { m.ID = store.GatewayAssistantMessageID("failed") }},
		{name: "legacy source", mutate: func(m *store.SessionMessage) { m.SourceType = "" }},
		{name: "expiry source", mutate: func(m *store.SessionMessage) { m.SourceType = "gateway-event" }},
		{name: "missing source Task", mutate: func(m *store.SessionMessage) { m.SourceRef = "" }},
		{name: "wrong source Task", mutate: func(m *store.SessionMessage) { m.SourceRef = "other" }},
		{name: "missing event", mutate: func(m *store.SessionMessage) { delete(m.Metadata, "eventId") }},
		{name: "wrong event", mutate: func(m *store.SessionMessage) { m.Metadata["eventId"] = "other" }},
		{name: "missing Task", mutate: func(m *store.SessionMessage) { delete(m.Metadata, "taskName") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := unboundGatewaySoulMessages()
			if tc.mutate != nil {
				tc.mutate(&messages[1])
			}
			state, err := readSoulHistoryFixture(t, store.SessionTypeGateway, messages)
			require.NoError(t, err)
			require.Equal(t, !tc.unbound, state.Established)
			require.Equal(t, messages[1].Metadata[store.SessionSoulDigestMetadata], state.Digest)
			first := messages[0].ID
			if tc.unbound {
				first = messages[2].ID
			}
			require.Equal(t, first, state.FirstMessageID)
		})
	}
}

func TestGatewaySessionSoulUnboundPreservesAmbiguousHistory(t *testing.T) {
	for _, tc := range []struct {
		name        string
		established bool
		messages    func([]store.SessionMessage) []store.SessionMessage
	}{
		{name: "earlier unmatched user", messages: func(ms []store.SessionMessage) []store.SessionMessage {
			return append([]store.SessionMessage{{ID: "legacy-user", Role: "user"}}, ms...)
		}},
		{name: "wrong user source", established: true, messages: func(ms []store.SessionMessage) []store.SessionMessage {
			ms[0].SourceType = "legacy"
			return ms
		}},
		{name: "wrong user event", established: true, messages: func(ms []store.SessionMessage) []store.SessionMessage {
			ms[0].SourceRef = "other"
			return ms
		}},
		{name: "wrong user ID", established: true, messages: func(ms []store.SessionMessage) []store.SessionMessage {
			ms[0].ID = "legacy-user"
			return ms
		}},
		{name: "wrong terminal slot", established: true, messages: func(ms []store.SessionMessage) []store.SessionMessage {
			return append(ms[:1], append([]store.SessionMessage{{ID: "other-user", Role: "user"}}, ms[1:]...)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := tc.messages(unboundGatewaySoulMessages())
			state, err := readSoulHistoryFixture(t, store.SessionTypeGateway, messages)
			require.NoError(t, err)
			require.Equal(t, tc.established, state.Established)
			require.Equal(t, messages[0].ID, state.FirstMessageID, "unknown history must still fence the next event")
		})
	}
	for _, digest := range []string{"", agentcontext.Digest("original revision")} {
		t.Run("established "+digest, func(t *testing.T) {
			messages := append([]store.SessionMessage{{ID: "original", Role: "assistant", Metadata: map[string]string{store.SessionSoulDigestMetadata: digest}}}, unboundGatewaySoulMessages()...)
			state, err := readSoulHistoryFixture(t, store.SessionTypeGateway, messages)
			require.NoError(t, err)
			require.True(t, state.Established)
			require.Equal(t, digest, state.Digest, "unbound errors must not reset an established conversation")
		})
	}
	state, err := readSoulHistoryFixture(t, "task", unboundGatewaySoulMessages())
	require.NoError(t, err)
	require.True(t, state.Established, "ordinary/legacy Sessions cannot opt into the exception")
	require.Empty(t, state.Digest)
	messages := unboundGatewaySoulMessages()
	messages[1].Metadata[store.SessionSoulDigestMetadata] = "invalid-digest"
	_, err = readSoulHistoryFixture(t, store.SessionTypeGateway, messages)
	require.Error(t, err, "unbound marker must not hide invalid digest metadata")
}

func TestGatewayRetentionDoesNotPinUnboundSoulFailure(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-2 * time.Hour)
	event := testGatewayEvent(old.Add(-time.Minute), "unbound-retention")
	event.BindingUID = testGatewayBindingUID
	_, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	require.NoError(t, err)
	require.NoError(t, s.ExpireGatewayEvent(ctx, event.Namespace, event.ID, "", "expired", old))
	attachDeliveredExpiryDelivery(t, s, ctx, event, old)
	// Replace the expiry fixture's metadata with the exact canonical pre-Job
	// error evidence emitted by Gateway terminal projection.
	metadata, err := json.Marshal(map[string]string{
		store.SessionSoulUnboundMetadata: "true", "eventId": event.ID, "taskName": event.TaskName,
	})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE session_messages SET source_type = 'gateway-task', source_ref = ?, metadata_json = ?
	 WHERE namespace = ? AND session_name = ? AND message_id = ?`,
		event.TaskName, string(metadata), event.Namespace, event.SessionName, store.GatewayErrorMessageID(event.ID))
	require.NoError(t, err)
	next := testGatewayEvent(now, "unbound-retention-next")
	next.BindingUID = event.BindingUID
	_, _, err = s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: next, AppendUserMessage: true})
	require.NoError(t, err)
	_, err = s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = s.ClaimNextGatewayEvent(ctx, next.Namespace, "dispatcher", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.MarkGatewayEventTaskCreated(ctx, next.Namespace, next.ID, next.TaskName, "next-uid", "dispatcher", now))
	state, err := s.ReadSessionSoul(ctx, next.Namespace, next.SessionName, next.TaskName, "next-uid")
	require.NoError(t, err)
	require.False(t, state.Established, "retention must not turn an unbound error into a no-soul anchor")
	require.Equal(t, store.GatewayUserMessageID(next.ID), state.FirstMessageID)
	require.Equal(t, 1, state.MessageCount)
	var anchors int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE source_type = ?`, store.SessionSoulAnchorSource).Scan(&anchors))
	require.Zero(t, anchors)
}
