package store

import (
	"context"
	"time"
)

// GatewayTaskCleanupReceipt preserves the exact event-to-Task ownership after
// retention compacts the event. It authorizes requesting ordinary Task deletion;
// it does not replace runtime, Session, or Task finalizer cleanup evidence.
type GatewayTaskCleanupReceipt struct {
	Namespace    string    `json:"namespace"`
	NamespaceUID string    `json:"namespaceUid"`
	GatewayName  string    `json:"gatewayName"`
	GatewayUID   string    `json:"gatewayUid"`
	BindingName  string    `json:"bindingName"`
	BindingUID   string    `json:"bindingUid"`
	EventID      string    `json:"eventId"`
	TaskName     string    `json:"taskName"`
	TaskUID      string    `json:"taskUid"`
	SessionName  string    `json:"sessionName"`
	CompactedAt  time.Time `json:"compactedAt"`
}

// GatewayTaskCleanupReceiptStore reads durable retention evidence. The only
// writer is event compaction; Task metadata cannot manufacture a receipt.
type GatewayTaskCleanupReceiptStore interface {
	GetGatewayTaskCleanupReceipt(context.Context, string, string, string) (*GatewayTaskCleanupReceipt, error)
}

// Validate checks the recorded ownership and the exact requested Task identity.
func (receipt *GatewayTaskCleanupReceipt) Validate(namespace, taskName, taskUID string) error {
	if receipt == nil {
		return ValidationErrorf("Gateway Task cleanup receipt is required")
	}
	for field, value := range map[string]string{
		"namespace": receipt.Namespace, "namespace UID": receipt.NamespaceUID,
		"Gateway name": receipt.GatewayName, "Gateway UID": receipt.GatewayUID,
		"GatewayBinding name": receipt.BindingName, "GatewayBinding UID": receipt.BindingUID,
		"event ID": receipt.EventID, "Task name": receipt.TaskName,
		"Task UID": receipt.TaskUID, "Session name": receipt.SessionName,
	} {
		if err := ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if receipt.CompactedAt.IsZero() {
		return ValidationErrorf("Gateway Task cleanup receipt compaction time is required")
	}
	if receipt.Namespace != namespace || receipt.TaskName != taskName || receipt.TaskUID != taskUID {
		return ConflictErrorf("Gateway Task cleanup receipt does not match the exact Task identity")
	}
	return nil
}
