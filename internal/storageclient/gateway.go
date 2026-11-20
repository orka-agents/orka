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
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
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

// The protect helpers below sanitize gateway text as the functions above do.
// A caller that supplies its own write envelope sends its request unchanged
// (see identity.go): they return its value as it is when sanitizing would not
// change it, and refuse it with ErrValidation when sanitizing would.

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

// protectGatewayDelivery sanitizes delivery in place, or leaves a
// caller-owned delivery as it is and refuses one sanitizing would change.
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

// AdmitGatewayEvent sanitizes the event's state message, sender display name
// and metadata before admission.
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
	reason, err := protectGatewayText(ctx, "RetryGatewayEvent", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.RetryGatewayEvent(ctx, namespace, id, owner, reason, nextAttemptAt)
}

// ExpireGatewayEvent sanitizes the expiry reason, which also becomes the
// visible transcript error.
func (c *Client) ExpireGatewayEvent(ctx context.Context, namespace, id, owner, reason string, now time.Time) error {
	reason, err := protectGatewayText(ctx, "ExpireGatewayEvent", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.ExpireGatewayEvent(ctx, namespace, id, owner, reason, now)
}

// ExpireGatewayEventWithDelivery sanitizes the expiry reason and the error
// delivery.
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

// MarkExpiredGatewayEventDeadLettered sanitizes the dead-letter reason.
func (c *Client) MarkExpiredGatewayEventDeadLettered(ctx context.Context, namespace, id, reason string, now time.Time) error {
	reason, err := protectGatewayText(ctx, "MarkExpiredGatewayEventDeadLettered", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.MarkExpiredGatewayEventDeadLettered(ctx, namespace, id, reason, now)
}

// ProjectGatewayTerminal sanitizes the final delivery and the metadata of the
// terminal transcript message.
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

// CreateGatewayDelivery sanitizes the delivery in place, so the caller's
// record matches the stored one.
func (c *Client) CreateGatewayDelivery(ctx context.Context, delivery *store.GatewayDelivery) (*store.GatewayDelivery, bool, error) {
	if err := protectGatewayDelivery(ctx, "CreateGatewayDelivery", delivery); err != nil {
		return nil, false, err
	}
	return c.StateProvider.CreateGatewayDelivery(ctx, delivery)
}

// MarkGatewayDeliveryDelivered sanitizes the provider message ID.
func (c *Client) MarkGatewayDeliveryDelivered(ctx context.Context, namespace, id, owner, providerMessageID string, now time.Time) error {
	providerMessageID, err := protectGatewayText(ctx, "MarkGatewayDeliveryDelivered", "the provider message ID", providerMessageID, gatewayIdentityBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.MarkGatewayDeliveryDelivered(ctx, namespace, id, owner, providerMessageID, now)
}

// ScheduleGatewayDeliveryRetry sanitizes the retry reason.
func (c *Client) ScheduleGatewayDeliveryRetry(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	reason, err := protectGatewayText(ctx, "ScheduleGatewayDeliveryRetry", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.ScheduleGatewayDeliveryRetry(ctx, namespace, id, owner, reason, nextAttemptAt)
}

// MarkGatewayDeliveryTerminal sanitizes the terminal reason.
func (c *Client) MarkGatewayDeliveryTerminal(
	ctx context.Context, namespace, id, owner string, state store.GatewayDeliveryState, reason string, now time.Time,
) error {
	reason, err := protectGatewayText(ctx, "MarkGatewayDeliveryTerminal", "the reason", reason, gatewayReasonBytes)
	if err != nil {
		return err
	}
	return c.StateProvider.MarkGatewayDeliveryTerminal(ctx, namespace, id, owner, state, reason, now)
}

// EnqueueGatewayMessage redacts the message text before the provider stores
// it, as the Gateway service does before it enqueues, so a direct caller
// cannot store a credential either. Like the service, it refuses text that is
// over the interim text limit or not valid UTF-8, before or after redaction,
// instead of truncating it. Text the service already redacted is unchanged.
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
