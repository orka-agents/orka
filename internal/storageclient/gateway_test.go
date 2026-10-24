package storageclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
)

func (r *recordingStore) AdmitGatewayEvent(ctx context.Context, admission store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	r.record("AdmitGatewayEvent", admission)
	return r.Store.AdmitGatewayEvent(ctx, admission)
}

func (r *recordingStore) GetGatewayEventDuplicate(ctx context.Context, candidate *store.GatewayEvent, now time.Time) (*store.GatewayEvent, error) {
	r.record("GetGatewayEventDuplicate", candidate)
	return r.Store.GetGatewayEventDuplicate(ctx, candidate, now)
}

func (r *recordingStore) RetryGatewayEvent(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	r.record("RetryGatewayEvent", reason)
	return r.Store.RetryGatewayEvent(ctx, namespace, id, owner, reason, nextAttemptAt)
}

func (r *recordingStore) ExpireGatewayEvent(ctx context.Context, namespace, id, owner, reason string, now time.Time) error {
	r.record("ExpireGatewayEvent", reason)
	return r.Store.ExpireGatewayEvent(ctx, namespace, id, owner, reason, now)
}

func (r *recordingStore) ExpireGatewayEventWithDelivery(ctx context.Context, projection store.GatewayExpiryProjection) (*store.GatewayDelivery, bool, error) {
	r.record("ExpireGatewayEventWithDelivery", projection)
	return r.Store.ExpireGatewayEventWithDelivery(ctx, projection)
}

func (r *recordingStore) MarkExpiredGatewayEventDeadLettered(ctx context.Context, namespace, id, reason string, now time.Time) error {
	r.record("MarkExpiredGatewayEventDeadLettered", reason)
	return r.Store.MarkExpiredGatewayEventDeadLettered(ctx, namespace, id, reason, now)
}

func (r *recordingStore) ProjectGatewayTerminal(ctx context.Context, projection store.GatewayTerminalProjection) (*store.GatewayDelivery, bool, error) {
	r.record("ProjectGatewayTerminal", projection)
	return r.Store.ProjectGatewayTerminal(ctx, projection)
}

func (r *recordingStore) CreateGatewayDelivery(ctx context.Context, delivery *store.GatewayDelivery) (*store.GatewayDelivery, bool, error) {
	r.record("CreateGatewayDelivery", delivery)
	return r.Store.CreateGatewayDelivery(ctx, delivery)
}

func (r *recordingStore) MarkGatewayDeliveryDelivered(ctx context.Context, namespace, id, owner, providerMessageID string, now time.Time) error {
	r.record("MarkGatewayDeliveryDelivered", providerMessageID)
	return r.Store.MarkGatewayDeliveryDelivered(ctx, namespace, id, owner, providerMessageID, now)
}

func (r *recordingStore) ScheduleGatewayDeliveryRetry(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	r.record("ScheduleGatewayDeliveryRetry", reason)
	return r.Store.ScheduleGatewayDeliveryRetry(ctx, namespace, id, owner, reason, nextAttemptAt)
}

func (r *recordingStore) MarkGatewayDeliveryTerminal(
	ctx context.Context, namespace, id, owner string, state store.GatewayDeliveryState, reason string, now time.Time,
) error {
	r.record("MarkGatewayDeliveryTerminal", reason)
	return r.Store.MarkGatewayDeliveryTerminal(ctx, namespace, id, owner, state, reason, now)
}

// TestProviderReceivesOnlySanitizedGatewayText sends secrets through every
// gateway write whose text Orka sanitizes. The recorder captures each request
// before the provider runs it, so a call the provider then refuses for its
// state still proves what it received.
func TestProviderReceivesOnlySanitizedGatewayText(t *testing.T) {
	secrets := protectedSecrets()
	openAI, github, bearer := secrets["OpenAI key"], secrets["GitHub token"], secrets["bearer value"]
	recorder := newRecordingStore(t)
	client := storageclienttest.NewEmbedded(t, recorder)
	ctx := t.Context()
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	event := store.GatewayEvent{
		ID: "gwe-1", Namespace: "tenant", NamespaceUID: "tenant-uid", GatewayUID: "gateway-uid", GatewayGeneration: 1,
		GatewayName: "chat", BindingName: "room", ExternalEventID: "external-1", ProtocolVersion: "v1",
		EventType: "message", AccountID: "account", ContextID: "context", SenderID: "sender",
		SenderDisplayName: "token is " + github, Text: "hello", SessionName: "session", TaskName: "task",
		StateMessage: "Authorization: Bearer " + bearer, Metadata: map[string]string{"note": "api_key=" + openAI},
		ExpiresAt: now.Add(time.Hour),
	}
	_, _, err := client.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
	require.NoError(t, err)
	_, err = client.GetGatewayEventDuplicate(ctx, &event, now)
	require.NoError(t, err)

	delivery := func(id string) store.GatewayDelivery {
		return store.GatewayDelivery{
			ID: id, IdempotencyID: id, Namespace: "tenant", NamespaceUID: "tenant-uid", GatewayUID: "gateway-uid",
			GatewayGeneration: 1, GatewayName: "chat", EventID: event.ID, Kind: "final", AccountID: "account",
			ContextID: "context", ReplyTarget: "target", Text: "reply", LastError: "token is " + github,
			ProviderMessageID: "api_key=" + openAI, Metadata: map[string]string{"note": "token is " + github},
			ExpiresAt: now.Add(time.Hour),
		}
	}
	created := delivery("gdl-created")
	_, _, _ = client.CreateGatewayDelivery(ctx, &created)
	require.Equal(t, "token is [REDACTED]", created.LastError, "the caller's record matches what the provider received")
	_ = client.RetryGatewayEvent(ctx, "tenant", event.ID, "owner", "api_key="+openAI, now)
	_ = client.ExpireGatewayEvent(ctx, "tenant", event.ID, "owner", "token is "+github, now)
	_, _, _ = client.ExpireGatewayEventWithDelivery(ctx, store.GatewayExpiryProjection{
		EventID: event.ID, Owner: "owner", Reason: "Authorization: Bearer " + bearer, Delivery: delivery("gdl-expired"), CompletedAt: now,
	})
	_ = client.MarkExpiredGatewayEventDeadLettered(ctx, "tenant", event.ID, "api_key="+openAI, now)
	_, _, _ = client.ProjectGatewayTerminal(ctx, store.GatewayTerminalProjection{
		EventID: event.ID, Delivery: delivery("gdl-final"), CompletedAt: now,
		Message: store.SessionMessage{ID: "message", Role: "assistant", Content: "done",
			Metadata: map[string]string{"note": "token is " + github}},
	})
	_ = client.MarkGatewayDeliveryDelivered(ctx, "tenant", "gdl-created", "owner", "api_key="+openAI, now)
	_ = client.ScheduleGatewayDeliveryRetry(ctx, "tenant", "gdl-created", "owner", "token is "+github, now)
	_ = client.MarkGatewayDeliveryTerminal(ctx, "tenant", "gdl-created", "owner", store.GatewayDeliveryFailed, "api_key="+openAI, now)

	recorder.requireCalled(t,
		"AdmitGatewayEvent", "GetGatewayEventDuplicate", "CreateGatewayDelivery", "RetryGatewayEvent", "ExpireGatewayEvent",
		"ExpireGatewayEventWithDelivery", "MarkExpiredGatewayEventDeadLettered", "ProjectGatewayTerminal",
		"MarkGatewayDeliveryDelivered", "ScheduleGatewayDeliveryRetry", "MarkGatewayDeliveryTerminal",
	)
	recorder.requireNoSecret(t, secrets)
	recorder.requireReceived(t, "AdmitGatewayEvent", "Authorization:")
	recorder.requireReceived(t, "ProjectGatewayTerminal", "token is")

	// The provider stores what it received: the admitted event and its
	// transcript message carry only the sanitized text.
	stored, err := client.GetGatewayEvent(ctx, "tenant", event.ID)
	require.NoError(t, err)
	require.Equal(t, "token is [REDACTED]", stored.SenderDisplayName)
	require.Equal(t, "api_key=[REDACTED]", stored.Metadata["note"])
}
