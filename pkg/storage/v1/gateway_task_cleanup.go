package v1

import (
	"context"
	"time"
)

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

type GatewayTaskCleanupReceiptStore interface {
	GetGatewayTaskCleanupReceipt(context.Context, string, string, string) (*GatewayTaskCleanupReceipt, error)
}

type GatewayTaskCleanupReceiptFilter struct {
	Namespace      string
	AfterNamespace string
	AfterTaskUID   string
	Limit          int
}

type GatewayTaskCleanupReceiptMaintenanceStore interface {
	ListGatewayTaskCleanupReceipts(context.Context, GatewayTaskCleanupReceiptFilter) ([]GatewayTaskCleanupReceipt, error)
	DeleteGatewayTaskCleanupReceipt(context.Context, string, string, string) error
}

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
