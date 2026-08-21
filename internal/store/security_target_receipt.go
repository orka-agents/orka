package store

import (
	"context"
	"encoding/json"
	"time"
)

// SecurityTargetReceipt stores controller-accepted immutable mapper target bytes.
type SecurityTargetReceipt struct {
	ID              string          `json:"id"`
	Namespace       string          `json:"namespace"`
	RepositoryScan  string          `json:"repositoryScan"`
	ScanRunID       string          `json:"scanRunID"`
	RunUID          string          `json:"runUID"`
	TargetID        string          `json:"targetID"`
	HeadSHA         string          `json:"headSHA"`
	ObjectFormat    string          `json:"objectFormat"`
	SnapshotDigest  string          `json:"snapshotDigest"`
	TreeDigest      string          `json:"treeDigest"`
	ReceiptJSON     json.RawMessage `json:"receipt"`
	InventoryJSON   json.RawMessage `json:"inventory,omitempty"`
	InventoryDigest string          `json:"inventoryDigest,omitempty"`
	PayloadDigest   string          `json:"payloadDigest"`
	RecordDigest    string          `json:"recordDigest"`
	CreatedAt       time.Time       `json:"createdAt"`
}

// SecurityTargetReceiptStore persists immutable target receipts separately from mutable staging.
type SecurityTargetReceiptStore interface {
	SaveSecurityTargetReceipt(ctx context.Context, receipt *SecurityTargetReceipt) (bool, error)
	GetSecurityTargetReceipt(ctx context.Context, namespace, id string) (*SecurityTargetReceipt, error)
}
