package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type GatewayEventState string

const (
	GatewayEventAccepted     GatewayEventState = "Accepted"
	GatewayEventQueued       GatewayEventState = "Queued"
	GatewayEventDispatching  GatewayEventState = "Dispatching"
	GatewayEventTaskCreated  GatewayEventState = "TaskCreated"
	GatewayEventCompleted    GatewayEventState = "Completed"
	GatewayEventRejected     GatewayEventState = "Rejected"
	GatewayEventDeadLettered GatewayEventState = "DeadLettered"
	GatewayEventExpired      GatewayEventState = "Expired"
)

type GatewayDeliveryState string

const (
	GatewayDeliveryPending        GatewayDeliveryState = "Pending"
	GatewayDeliverySending        GatewayDeliveryState = "Sending"
	GatewayDeliveryDelivered      GatewayDeliveryState = "Delivered"
	GatewayDeliveryRetryScheduled GatewayDeliveryState = "RetryScheduled"
	GatewayDeliveryFailed         GatewayDeliveryState = "Failed"
	GatewayDeliveryDeadLettered   GatewayDeliveryState = "DeadLettered"
	GatewayDeliveryExpired        GatewayDeliveryState = "Expired"
)

type GatewayEvent struct {
	ID                string            `json:"id"`
	Namespace         string            `json:"namespace"`
	NamespaceUID      string            `json:"namespaceUid"`
	GatewayUID        string            `json:"gatewayUid"`
	GatewayGeneration int64             `json:"gatewayGeneration"`
	GatewayName       string            `json:"gatewayName"`
	BindingName       string            `json:"bindingName,omitempty"`
	BindingUID        string            `json:"bindingUid,omitempty"`
	BindingGeneration int64             `json:"bindingGeneration,omitempty"`
	AgentName         string            `json:"agentName,omitempty"`
	AgentUID          string            `json:"agentUid,omitempty"`
	ExternalEventID   string            `json:"externalEventId"`
	ProtocolVersion   string            `json:"protocolVersion"`
	EventType         string            `json:"eventType"`
	State             GatewayEventState `json:"state"`
	StateMessage      string            `json:"stateMessage,omitempty"`
	AccountID         string            `json:"accountId"`
	ContextID         string            `json:"contextId"`
	ThreadID          string            `json:"threadId,omitempty"`
	SenderID          string            `json:"senderId"`
	SenderDisplayName string            `json:"senderDisplayName,omitempty"`
	Text              string            `json:"text,omitempty"`
	ReplyTarget       string            `json:"replyTarget,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	SessionName       string            `json:"sessionName,omitempty"`
	TaskName          string            `json:"taskName,omitempty"`
	TaskUID           string            `json:"taskUid,omitempty"`
	TaskAllowedTools  []string          `json:"-"`
	TaskPolicyFrozen  bool              `json:"-"`
	DeliveryID        string            `json:"deliveryId,omitempty"`
	ProviderMessageID string            `json:"providerMessageId,omitempty"`
	TraceParent       string            `json:"-"`
	TraceState        string            `json:"-"`
	TranscriptOrder   int64             `json:"transcriptOrder,omitempty"`
	AttemptCount      int               `json:"attemptCount"`
	ClaimOwner        string            `json:"-"`
	ClaimUntil        *time.Time        `json:"-"`
	NextAttemptAt     time.Time         `json:"nextAttemptAt"`
	OccurredAt        *time.Time        `json:"occurredAt,omitempty"`
	ReceivedAt        time.Time         `json:"receivedAt"`
	ExpiresAt         time.Time         `json:"expiresAt"`
	CreatedAt         time.Time         `json:"createdAt"`
	UpdatedAt         time.Time         `json:"updatedAt"`
	CompletedAt       *time.Time        `json:"completedAt,omitempty"`
}

type GatewayEventAdmission struct {
	Event               GatewayEvent
	AppendUserMessage   bool
	PendingLimit        int
	GatewayRecordLimit  int
	RejectedRecordLimit int
}

type GatewayEventFilter struct {
	Namespace          string
	NamespaceUID       string
	GatewayUID         string
	GatewayUIDs        []string
	ID                 string
	GatewayName        string
	BindingName        string
	SessionName        string
	TaskName           string
	States             []GatewayEventState
	DueBefore          *time.Time
	ExpiresBefore      *time.Time
	OrderByNextAttempt bool
	OrderByExpiry      bool
	SessionHeadOnly    bool
	MissingDelivery    bool
	BeforeCreatedAt    *time.Time
	BeforeID           string
	Limit              int
}

type GatewayTerminalProjection struct {
	EventID     string
	Message     SessionMessage
	Delivery    GatewayDelivery
	CompletedAt time.Time
}

type GatewayExpiryProjection struct {
	EventID     string
	Owner       string
	Reason      string
	Delivery    GatewayDelivery
	CompletedAt time.Time
}

type GatewayMessageBudgetQuery struct {
	Namespace    string
	NamespaceUID string
	EventID      string
	TaskName     string
	TaskUID      string
	RequestID    string
}

type GatewayMessageBudget struct {
	Accepted      int  `json:"accepted"`
	RequestExists bool `json:"requestExists"`
}

type GatewayMessageEnqueue struct {
	Namespace    string
	NamespaceUID string
	EventID      string
	TaskName     string
	TaskUID      string
	RequestID    string
	Text         string
	ReplayOnly   bool
	MaxMessages  int
	MaxAttempts  int
	Now          time.Time
	ExpiresAt    time.Time
}

type GatewayDelivery struct {
	ID                string               `json:"id"`
	IdempotencyID     string               `json:"idempotencyId"`
	Namespace         string               `json:"namespace"`
	NamespaceUID      string               `json:"namespaceUid"`
	GatewayUID        string               `json:"gatewayUid"`
	GatewayGeneration int64                `json:"gatewayGeneration"`
	GatewayName       string               `json:"gatewayName"`
	BindingName       string               `json:"bindingName,omitempty"`
	EventID           string               `json:"eventId"`
	TaskName          string               `json:"taskName,omitempty"`
	SessionName       string               `json:"sessionName,omitempty"`
	Kind              string               `json:"kind"`
	State             GatewayDeliveryState `json:"state"`
	AccountID         string               `json:"accountId"`
	ContextID         string               `json:"contextId"`
	ThreadID          string               `json:"threadId,omitempty"`
	ReplyTarget       string               `json:"replyTarget"`
	Text              string               `json:"text"`
	Metadata          map[string]string    `json:"metadata,omitempty"`
	AttemptCount      int                  `json:"attemptCount"`
	MaxAttempts       int                  `json:"maxAttempts"`
	ManualRetryCount  int                  `json:"manualRetryCount"`
	NextAttemptAt     time.Time            `json:"nextAttemptAt"`
	ExpiresAt         time.Time            `json:"expiresAt"`
	ProviderMessageID string               `json:"providerMessageId,omitempty"`
	TraceParent       string               `json:"-"`
	TraceState        string               `json:"-"`
	LastError         string               `json:"lastError,omitempty"`
	ClaimOwner        string               `json:"-"`
	ClaimUntil        *time.Time           `json:"-"`
	CreatedAt         time.Time            `json:"createdAt"`
	UpdatedAt         time.Time            `json:"updatedAt"`
	DeliveredAt       *time.Time           `json:"deliveredAt,omitempty"`
}

type GatewayDeliveryFilter struct {
	Namespace       string
	NamespaceUID    string
	GatewayUID      string
	GatewayUIDs     []string
	ID              string
	GatewayName     string
	BindingName     string
	EventID         string
	SessionName     string
	TaskName        string
	States          []GatewayDeliveryState
	BeforeCreatedAt *time.Time
	BeforeID        string
	Limit           int
}

type GatewayQueueStats struct {
	PendingEvents       int
	OldestEventReceived *time.Time
	PendingDeliveries   int
	OldestDeliveryDue   *time.Time
}

type GatewayMaintenanceResult struct {
	ExpiredEvents          int `json:"expiredEvents"`
	ExpiredDeliveries      int `json:"expiredDeliveries"`
	DeletedEvents          int `json:"deletedEvents"`
	DeletedDeliveries      int `json:"deletedDeliveries"`
	UpsertedTombstones     int `json:"upsertedTombstones"`
	DeletedTombstones      int `json:"deletedTombstones"`
	DeletedSessionMessages int `json:"deletedSessionMessages"`
	DeletedSessions        int `json:"deletedSessions"`
}

func GatewayUserMessageID(eventID string) string { return "gateway:" + eventID + ":user" }

func GatewayAssistantMessageID(eventID string) string { return "gateway:" + eventID + ":assistant" }

func GatewayErrorMessageID(eventID string) string { return "gateway:" + eventID + ":error" }

func GatewayEventEnvelopeDigest(event *GatewayEvent) string {
	if event == nil {
		return ""
	}
	occurredAt := ""
	if event.OccurredAt != nil {
		occurredAt = event.OccurredAt.UTC().Format(time.RFC3339Nano)
	}
	metadata := event.Metadata
	if len(metadata) == 0 {
		metadata = nil
	}
	payload := struct {
		Namespace         string            `json:"namespace"`
		NamespaceUID      string            `json:"namespaceUid"`
		GatewayUID        string            `json:"gatewayUid"`
		GatewayName       string            `json:"gatewayName"`
		ExternalEventID   string            `json:"externalEventId"`
		ProtocolVersion   string            `json:"protocolVersion"`
		EventType         string            `json:"eventType"`
		AccountID         string            `json:"accountId"`
		ContextID         string            `json:"contextId"`
		ThreadID          string            `json:"threadId"`
		SenderID          string            `json:"senderId"`
		SenderDisplayName string            `json:"senderDisplayName"`
		Text              string            `json:"text"`
		ReplyTarget       string            `json:"replyTarget"`
		OccurredAt        string            `json:"occurredAt"`
		Metadata          map[string]string `json:"metadata"`
	}{
		Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
		GatewayUID: event.GatewayUID, GatewayName: event.GatewayName,
		ExternalEventID: event.ExternalEventID, ProtocolVersion: event.ProtocolVersion, EventType: event.EventType,
		AccountID: event.AccountID, ContextID: event.ContextID, ThreadID: event.ThreadID,
		SenderID: event.SenderID, SenderDisplayName: event.SenderDisplayName, Text: event.Text,
		ReplyTarget: event.ReplyTarget, OccurredAt: occurredAt, Metadata: metadata,
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func IsValidGatewayEventState(state GatewayEventState) bool {
	switch state {
	case GatewayEventAccepted, GatewayEventQueued, GatewayEventDispatching, GatewayEventTaskCreated,
		GatewayEventCompleted, GatewayEventRejected, GatewayEventDeadLettered, GatewayEventExpired:
		return true
	default:
		return false
	}
}

func IsValidGatewayDeliveryState(state GatewayDeliveryState) bool {
	switch state {
	case GatewayDeliveryPending, GatewayDeliverySending, GatewayDeliveryDelivered,
		GatewayDeliveryRetryScheduled, GatewayDeliveryFailed, GatewayDeliveryDeadLettered, GatewayDeliveryExpired:
		return true
	default:
		return false
	}
}
