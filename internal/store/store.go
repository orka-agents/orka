package store

import (
	"context"
	"errors"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// ErrControllerEpochMutationContention marks an exhausted mutation-lock
// acquisition retry window. Errors carrying it also wrap ErrConflict;
// definitive epoch authority failures do not carry this marker.
var ErrControllerEpochMutationContention = errors.New("controller epoch mutation contention")

// MemoryProposalStore adds proposal application to the provider contract's
// proposal family. Application runs Orka's authorization callback inside the
// embedded store's transaction.
type MemoryProposalStore interface {
	storagev1.MemoryProposalStore
	ApplyMemoryProposal(ctx context.Context, apply MemoryProposalApply) (*Memory, error)
}

// SecurityStore handles repository security scanning persistence. It extends
// the provider contract with scan ingestion, which still runs Orka validation
// and application callbacks inside the embedded SQLite transaction.
type SecurityStore interface {
	storagev1.SecurityStore
	// ApplyScanTaskIngestion atomically applies results, updates the current run,
	// and records the receipt. It skips already ingested Tasks and terminal runs.
	// If supplied, validate runs before application and immediately before commit.
	// The callback must use the supplied store and must not perform external mutations.
	ApplyScanTaskIngestion(ctx context.Context, ingestion *ScanTaskIngestion, validate func(*ScanRun) error, apply func(SecurityStore, *ScanRun) error) (bool, error)
}
