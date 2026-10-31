package storageclient

import (
	"context"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

// Gateway text limits, in bytes.
const (
	gatewayReasonBytes   = 1024
	gatewayIdentityBytes = 256
)

// sanitizeGatewayText redacts credentials from adapter- or Orka-supplied
// gateway text and bounds it before a provider stores it.
func sanitizeGatewayText(value string, limit int) string {
	value = redact.SensitiveText(strings.TrimSpace(value))
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}

// sanitizeGatewayMetadata returns a sanitized copy of metadata, leaving the
// caller's map unchanged. Empty metadata becomes nil.
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

// sanitizeGatewayDelivery sanitizes a delivery's error, provider message ID
// and metadata in place.
func sanitizeGatewayDelivery(delivery *store.GatewayDelivery) {
	if delivery == nil {
		return
	}
	delivery.LastError = sanitizeGatewayText(delivery.LastError, gatewayReasonBytes)
	delivery.ProviderMessageID = sanitizeGatewayText(delivery.ProviderMessageID, gatewayIdentityBytes)
	delivery.Metadata = sanitizeGatewayMetadata(delivery.Metadata)
}

// AdmitGatewayEvent sanitizes the event's state message, sender display name
// and metadata before admission.
func (c *Client) AdmitGatewayEvent(ctx context.Context, admission store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	admission.Event.StateMessage = sanitizeGatewayText(admission.Event.StateMessage, gatewayReasonBytes)
	admission.Event.SenderDisplayName = sanitizeGatewayText(admission.Event.SenderDisplayName, gatewayIdentityBytes)
	admission.Event.Metadata = sanitizeGatewayMetadata(admission.Event.Metadata)
	return c.StateProvider.AdmitGatewayEvent(ctx, admission)
}

// GetGatewayEventDuplicate looks up a candidate sanitized the same way an
// admitted event is stored.
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

// RetryGatewayEvent sanitizes the retry reason.
func (c *Client) RetryGatewayEvent(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	return c.StateProvider.RetryGatewayEvent(ctx, namespace, id, owner, sanitizeGatewayText(reason, gatewayReasonBytes), nextAttemptAt)
}

// ExpireGatewayEvent sanitizes the expiry reason, which also becomes the
// visible transcript error.
func (c *Client) ExpireGatewayEvent(ctx context.Context, namespace, id, owner, reason string, now time.Time) error {
	return c.StateProvider.ExpireGatewayEvent(ctx, namespace, id, owner, sanitizeGatewayText(reason, gatewayReasonBytes), now)
}

// ExpireGatewayEventWithDelivery sanitizes the expiry reason and the error
// delivery.
func (c *Client) ExpireGatewayEventWithDelivery(
	ctx context.Context, projection store.GatewayExpiryProjection,
) (*store.GatewayDelivery, bool, error) {
	projection.Reason = sanitizeGatewayText(projection.Reason, gatewayReasonBytes)
	sanitizeGatewayDelivery(&projection.Delivery)
	return c.StateProvider.ExpireGatewayEventWithDelivery(ctx, projection)
}

// MarkExpiredGatewayEventDeadLettered sanitizes the dead-letter reason.
func (c *Client) MarkExpiredGatewayEventDeadLettered(ctx context.Context, namespace, id, reason string, now time.Time) error {
	return c.StateProvider.MarkExpiredGatewayEventDeadLettered(ctx, namespace, id, sanitizeGatewayText(reason, gatewayReasonBytes), now)
}

// ProjectGatewayTerminal sanitizes the final delivery and the metadata of the
// terminal transcript message.
func (c *Client) ProjectGatewayTerminal(
	ctx context.Context, projection store.GatewayTerminalProjection,
) (*store.GatewayDelivery, bool, error) {
	sanitizeGatewayDelivery(&projection.Delivery)
	projection.Message.Metadata = sanitizeGatewayMetadata(projection.Message.Metadata)
	return c.StateProvider.ProjectGatewayTerminal(ctx, projection)
}

// CreateGatewayDelivery sanitizes the delivery in place, so the caller's
// record matches the stored one.
func (c *Client) CreateGatewayDelivery(ctx context.Context, delivery *store.GatewayDelivery) (*store.GatewayDelivery, bool, error) {
	sanitizeGatewayDelivery(delivery)
	return c.StateProvider.CreateGatewayDelivery(ctx, delivery)
}

// MarkGatewayDeliveryDelivered sanitizes the provider message ID.
func (c *Client) MarkGatewayDeliveryDelivered(ctx context.Context, namespace, id, owner, providerMessageID string, now time.Time) error {
	return c.StateProvider.MarkGatewayDeliveryDelivered(ctx, namespace, id, owner,
		sanitizeGatewayText(providerMessageID, gatewayIdentityBytes), now)
}

// ScheduleGatewayDeliveryRetry sanitizes the retry reason.
func (c *Client) ScheduleGatewayDeliveryRetry(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	return c.StateProvider.ScheduleGatewayDeliveryRetry(ctx, namespace, id, owner, sanitizeGatewayText(reason, gatewayReasonBytes), nextAttemptAt)
}

// MarkGatewayDeliveryTerminal sanitizes the terminal reason.
func (c *Client) MarkGatewayDeliveryTerminal(
	ctx context.Context, namespace, id, owner string, state store.GatewayDeliveryState, reason string, now time.Time,
) error {
	return c.StateProvider.MarkGatewayDeliveryTerminal(ctx, namespace, id, owner, state, sanitizeGatewayText(reason, gatewayReasonBytes), now)
}
