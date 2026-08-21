package store

import (
	"context"
	"time"
)

// SecurityScanBundle is one atomically published immutable canonical run bundle.
type SecurityScanBundle struct {
	ID                       string    `json:"id"`
	Namespace                string    `json:"namespace"`
	RepositoryScan           string    `json:"repositoryScan"`
	RepositoryScanUID        string    `json:"repositoryScanUID"`
	RepositoryScanGeneration int64     `json:"repositoryScanGeneration"`
	ScanRunID                string    `json:"scanRunID"`
	RunUID                   string    `json:"runUID"`
	Version                  int       `json:"version"`
	ManifestJSON             []byte    `json:"manifest"`
	FindingsJSON             []byte    `json:"findings"`
	CoverageJSON             []byte    `json:"coverage"`
	EvidenceJSON             []byte    `json:"evidence"`
	ContentDigest            string    `json:"contentDigest"`
	RunReceiptDigest         string    `json:"runReceiptDigest"`
	SealedAt                 time.Time `json:"sealedAt"`
	RecordDigest             string    `json:"recordDigest"`
}

// SecurityBundleStore publishes and reads immutable scan bundles.
type SecurityBundleStore interface {
	SealSecurityScanBundle(ctx context.Context, bundle *SecurityScanBundle) (bool, error)
	GetSecurityScanBundle(ctx context.Context, namespace, scanRunID string) (*SecurityScanBundle, error)
}
