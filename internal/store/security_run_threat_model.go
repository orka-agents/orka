package store

import (
	"context"
	"time"
)

// SecurityRunThreatModel is the immutable threat-model snapshot accepted for one run.
type SecurityRunThreatModel struct {
	RunUID          string    `json:"runUID"`
	Namespace       string    `json:"namespace"`
	RepositoryScan  string    `json:"repositoryScan"`
	ScanRunID       string    `json:"scanRunID"`
	Version         int64     `json:"version"`
	Content         string    `json:"content"`
	ContentDigest   string    `json:"contentDigest"`
	SourceReceiptID string    `json:"sourceReceiptID,omitempty"`
	RecordDigest    string    `json:"recordDigest"`
	CreatedAt       time.Time `json:"createdAt"`
}

// SecurityRunThreatModelStore persists immutable per-run threat-model snapshots.
type SecurityRunThreatModelStore interface {
	SaveSecurityRunThreatModel(ctx context.Context, model *SecurityRunThreatModel) (bool, error)
	GetSecurityRunThreatModel(ctx context.Context, namespace, runUID string) (*SecurityRunThreatModel, error)
}
