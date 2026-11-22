package storageclient

import (
	"context"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

const (
	gatewayReasonBytes   = 1024
	gatewayIdentityBytes = 256
)

func sanitizeGatewayText(value string, limit int) string {
	value = redact.SensitiveText(strings.TrimSpace(value))
	if limit > 0 && len(value) > limit {
		value = value[:limit]
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func sanitizeGatewayMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	sanitized := make(map[string]string, len(metadata))
	for key, value := range metadata {
		sanitized[key] = sanitizeGatewayText(value, gatewayIdentityBytes)
	}
	return sanitized
}

func sanitizeGatewayDelivery(delivery *store.GatewayDelivery) {
	if delivery == nil {
		return
	}
	delivery.LastError = sanitizeGatewayText(delivery.LastError, gatewayReasonBytes)
	delivery.ProviderMessageID = sanitizeGatewayText(delivery.ProviderMessageID, gatewayIdentityBytes)
	delivery.Metadata = sanitizeGatewayMetadata(delivery.Metadata)
}

func protectGatewayText(ctx context.Context, method, field, value string, limit int) (string, error) {
	sanitized := sanitizeGatewayText(value, limit)
	if sanitized != value && callerOwnsEnvelope(ctx) {
		return "", unprotected(method, field)
	}
	return sanitized, nil
}

func protectGatewayMetadata(ctx context.Context, method, field string, metadata map[string]string) (map[string]string, error) {
	sanitized := sanitizeGatewayMetadata(metadata)
	if !callerOwnsEnvelope(ctx) {
		return sanitized, nil
	}
	if !maps.Equal(sanitized, metadata) {
		return nil, unprotected(method, field)
	}
	return metadata, nil
}

func protectGatewayDelivery(ctx context.Context, method string, delivery *store.GatewayDelivery) error {
	if delivery == nil {
		return nil
	}
	if !callerOwnsEnvelope(ctx) {
		sanitizeGatewayDelivery(delivery)
		return nil
	}
	if _, err := protectGatewayText(ctx, method, "the delivery's last error", delivery.LastError, gatewayReasonBytes); err != nil {
		return err
	}
	if _, err := protectGatewayText(ctx, method, "the delivery's provider message ID", delivery.ProviderMessageID, gatewayIdentityBytes); err != nil {
		return err
	}
	_, err := protectGatewayMetadata(ctx, method, "the delivery's metadata", delivery.Metadata)
	return err
}

func (c *Client) AdmitGatewayEvent(ctx context.Context, admission store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	const method = "AdmitGatewayEvent"
	var err error
	if admission.Event.StateMessage, err = protectGatewayText(ctx, method, "the event's state message",
		admission.Event.StateMessage, gatewayReasonBytes); err != nil {
		return nil, false, err
	}
	if admission.Event.SenderDisplayName, err = protectGatewayText(ctx, method, "the event's sender display name",
		admission.Event.SenderDisplayName, gatewayIdentityBytes); err != nil {
		return nil, false, err
	}
	if admission.Event.Metadata, err = protectGatewayMetadata(ctx, method, "the event's metadata", admission.Event.Metadata); err != nil {
		return nil, false, err
	}
	return c.StateProvider.AdmitGatewayEvent(ctx, admission)
}

func (c *Client) GetGatewayEventDuplicate(ctx context.Context, candidate *store.GatewayEvent, now time.Time) (*store.GatewayEvent, error) {
	if candidate == nil {
		return c.StateProvider.GetGatewayEventDuplicate(ctx, nil, now)
	}
	sanitized := *candidate
	sanitized.StateMessage = sanitizeGatewayText(sanitized.StateMessage, gatewayReasonBytes)
	sanitized.SenderDisplayName = sanitizeGatewayText(sanitized.SenderDisplayName, gatewayIdentityBytes)
	sanitized.Metadata = sanitizeGatewayMetadata(sanitized.Metadata)
	return c.StateProvider.GetGatewayEventDuplicate(ctx, &sanitized, now)
}

func (c *Client) RetryGatewayEvent(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	reason, err := protectGatewayText(ctx, "RetryGatewayEvent", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.RetryGatewayEvent(ctx, namespace, id, owner, reason, nextAttemptAt)
}

func (c *Client) ExpireGatewayEvent(ctx context.Context, namespace, id, owner, reason string, now time.Time) error {
	reason, err := protectGatewayText(ctx, "ExpireGatewayEvent", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.ExpireGatewayEvent(ctx, namespace, id, owner, reason, now)
}

func (c *Client) ExpireGatewayEventWithDelivery(
	ctx context.Context, projection store.GatewayExpiryProjection,
) (*store.GatewayDelivery, bool, error) {
	const method = "ExpireGatewayEventWithDelivery"
	var err error
	if projection.Reason, err = protectGatewayText(ctx, method, "the reason", projection.Reason, gatewayReasonBytes); err != nil {
		return nil, false, err
	}
	if err := protectGatewayDelivery(ctx, method, &projection.Delivery); err != nil {
		return nil, false, err
	}
	return c.StateProvider.ExpireGatewayEventWithDelivery(ctx, projection)
}

func (c *Client) MarkExpiredGatewayEventDeadLettered(ctx context.Context, namespace, id, reason string, now time.Time) error {
	reason, err := protectGatewayText(ctx, "MarkExpiredGatewayEventDeadLettered", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.MarkExpiredGatewayEventDeadLettered(ctx, namespace, id, reason, now)
}

func (c *Client) ProjectGatewayTerminal(
	ctx context.Context, projection store.GatewayTerminalProjection,
) (*store.GatewayDelivery, bool, error) {
	const method = "ProjectGatewayTerminal"
	if err := protectGatewayDelivery(ctx, method, &projection.Delivery); err != nil {
		return nil, false, err
	}
	var err error
	if projection.Message.Metadata, err = protectGatewayMetadata(ctx, method, "the message's metadata",
		projection.Message.Metadata); err != nil {
		return nil, false, err
	}
	return c.StateProvider.ProjectGatewayTerminal(ctx, projection)
}

func (c *Client) CreateGatewayDelivery(ctx context.Context, delivery *store.GatewayDelivery) (*store.GatewayDelivery, bool, error) {
	if err := protectGatewayDelivery(ctx, "CreateGatewayDelivery", delivery); err != nil {
		return nil, false, err
	}
	return c.StateProvider.CreateGatewayDelivery(ctx, delivery)
}

func (c *Client) MarkGatewayDeliveryDelivered(ctx context.Context, namespace, id, owner, providerMessageID string, now time.Time) error {
	providerMessageID, err := protectGatewayText(ctx, "MarkGatewayDeliveryDelivered", "the provider message ID", providerMessageID, gatewayIdentityBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.MarkGatewayDeliveryDelivered(ctx, namespace, id, owner, providerMessageID, now)
}

func (c *Client) ScheduleGatewayDeliveryRetry(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	reason, err := protectGatewayText(ctx, "ScheduleGatewayDeliveryRetry", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.ScheduleGatewayDeliveryRetry(ctx, namespace, id, owner, reason, nextAttemptAt)
}

func (c *Client) MarkGatewayDeliveryTerminal(
	ctx context.Context, namespace, id, owner string, state store.GatewayDeliveryState, reason string, now time.Time,
) error {
	reason, err := protectGatewayText(ctx, "MarkGatewayDeliveryTerminal", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.MarkGatewayDeliveryTerminal(ctx, namespace, id, owner, state, reason, now)
}

func (c *Client) EnqueueGatewayMessage(ctx context.Context, request store.GatewayMessageEnqueue) (*store.GatewayDelivery, bool, error) {
	if len(request.Text) > protocol.MaxInterimTextBytes || !utf8.ValidString(request.Text) {
		return nil, false, store.ValidationErrorf("gateway message text is invalid")
	}
	sanitized := protocol.SanitizeMessage(request.Text, 0)
	if sanitized != request.Text && callerOwnsEnvelope(ctx) {
		return nil, false, unprotected("EnqueueGatewayMessage", "the message text")
	}
	request.Text = sanitized
	if len(request.Text) > protocol.MaxInterimTextBytes || !utf8.ValidString(request.Text) {
		return nil, false, store.ValidationErrorf("gateway message text is invalid after sanitization")
	}
	return c.StateProvider.EnqueueGatewayMessage(ctx, request)
}
