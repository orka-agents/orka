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

// MemoryProposalStore adds Orka's proposal application to the provider
// contract's proposal family. The storage client implements
// ApplyMemoryProposal with the contract's resolve and conditional commit, and
// runs the authorization callback in Orka between them.
type MemoryProposalStore interface {
	storagev1.MemoryProposalStore
	ApplyMemoryProposal(ctx context.Context, apply MemoryProposalApply) (*Memory, error)
}

// SecurityStore handles repository security scanning persistence. It adds
// Orka's scan ingestion to the provider contract; the storage client
// implements it with the contract's conditional ingestion command.
type SecurityStore interface {
	storagev1.SecurityStore
	// ApplyScanTaskIngestion atomically applies results, updates the current
	// run, and records the receipt. It skips already ingested Tasks and
	// terminal or cancelled runs. If supplied, validate runs before
	// application and again just before the commit. Neither function runs
	// inside a store transaction. apply must use the supplied store, which
	// records its reads and writes for the commit, and must not perform
	// external mutations. Its writes take effect only when the commit
	// succeeds, and their errors may surface at a later read or at the
	// commit. When another writer changed a record apply read, the
	// ingestion returns ErrPreconditionFailed and changes nothing; the
	// caller repeats it from fresh state rather than re-running apply on
	// state it mutated.
	ApplyScanTaskIngestion(ctx context.Context, ingestion *ScanTaskIngestion, validate func(*ScanRun) error, apply func(SecurityStore, *ScanRun) error) (bool, error)
}
