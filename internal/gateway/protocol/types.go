/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package protocol defines the provider-neutral orka.gateway.v1 HTTP contract.
package protocol

import "time"

const (
	// Version is the frozen gateway adapter contract version.
	Version = "orka.gateway.v1"

	// EventTypeText is the only inbound event type supported by V1.
	EventTypeText = "text"

	// DeliveryKindFinal is a successful terminal assistant reply.
	DeliveryKindFinal = "final"
	// DeliveryKindError is a sanitized terminal or denial reply.
	DeliveryKindError = "error"

	// DeliveryStatusDelivered indicates that the provider accepted a delivery.
	DeliveryStatusDelivered = "delivered"
	// DeliveryStatusRetryableError indicates that the same delivery ID should be retried.
	DeliveryStatusRetryableError = "retryableError"
	// DeliveryStatusNonRetryableError indicates a permanent adapter rejection.
	DeliveryStatusNonRetryableError = "nonRetryableError"

	MaxHTTPBodyBytes        = 256 << 10
	MaxTextBytes            = 64 << 10
	MaxIdentityBytes        = 256
	MaxMetadataEntries      = 32
	MaxMetadataKeyBytes     = 256
	MaxMetadataValueBytes   = 256
	MaxAdapterResponseBytes = 64 << 10
)

// Sender identifies a stable external principal without provider credentials.
type Sender struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
}

// EventEnvelope is the normalized inbound adapter payload.
type EventEnvelope struct {
	ProtocolVersion string            `json:"protocolVersion"`
	ExternalEventID string            `json:"externalEventId"`
	EventType       string            `json:"eventType"`
	AccountID       string            `json:"accountId"`
	ContextID       string            `json:"contextId"`
	ThreadID        string            `json:"threadId,omitempty"`
	Sender          Sender            `json:"sender"`
	Text            string            `json:"text"`
	ReplyTarget     string            `json:"replyTarget,omitempty"`
	OccurredAt      *time.Time        `json:"occurredAt,omitempty"`
	ReceivedAt      *time.Time        `json:"receivedAt,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// IngressResponse acknowledges durable admission without waiting for execution.
type IngressResponse struct {
	Status  string `json:"status"`
	EventID string `json:"eventId"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// CapabilitiesResponse is returned by GET /v1/capabilities.
type CapabilitiesResponse struct {
	ProtocolVersion string       `json:"protocolVersion"`
	AdapterName     string       `json:"adapterName"`
	AdapterVersion  string       `json:"adapterVersion"`
	Capabilities    Capabilities `json:"capabilities"`
}

// Capabilities are provider-neutral adapter behaviors.
type Capabilities struct {
	InboundText        bool `json:"inboundText"`
	OutboundText       bool `json:"outboundText"`
	Threads            bool `json:"threads,omitempty"`
	SenderIdentity     bool `json:"senderIdentity,omitempty"`
	ExplicitSessions   bool `json:"explicitSessions,omitempty"`
	IdempotentDelivery bool `json:"idempotentDelivery"`
}

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status string `json:"status"`
}

// ResourceReference identifies an Orka object without embedding it.
type ResourceReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// DeliveryRequest is sent by Orka to POST /v1/deliveries.
type DeliveryRequest struct {
	ProtocolVersion  string             `json:"protocolVersion"`
	DeliveryID       string             `json:"deliveryId"`
	IdempotencyID    string             `json:"idempotencyId"`
	OriginatingEvent string             `json:"originatingEventId"`
	TaskRef          *ResourceReference `json:"taskRef,omitempty"`
	SessionRef       *ResourceReference `json:"sessionRef,omitempty"`
	Kind             string             `json:"kind"`
	AccountID        string             `json:"accountId"`
	ContextID        string             `json:"contextId"`
	ThreadID         string             `json:"threadId,omitempty"`
	ReplyTarget      string             `json:"replyTarget"`
	Text             string             `json:"text"`
	Metadata         map[string]string  `json:"metadata,omitempty"`
}

// DeliveryResponse is the synchronous terminal adapter response.
type DeliveryResponse struct {
	Status            string `json:"status"`
	ProviderMessageID string `json:"providerMessageId,omitempty"`
	Message           string `json:"message,omitempty"`
}
