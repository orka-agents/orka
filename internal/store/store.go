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

// MemoryStore handles durable namespace-scoped memory persistence.
type MemoryStore interface {
	CreateMemory(ctx context.Context, memory *Memory) error
	GetMemory(ctx context.Context, namespace, id string) (*Memory, error)
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	UpdateMemory(ctx context.Context, memory *Memory) error
	DeleteMemory(ctx context.Context, namespace, id string) error
	SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool) error
}

// MemoryProposalStore handles governance for worker-proposed memory/skill changes.
type MemoryProposalStore interface {
	CreateMemoryProposal(ctx context.Context, proposal *MemoryProposal) error
	GetMemoryProposal(ctx context.Context, namespace, id string) (*MemoryProposal, error)
	ListMemoryProposals(ctx context.Context, filter MemoryProposalFilter) ([]MemoryProposal, error)
	ReviewMemoryProposal(ctx context.Context, review MemoryProposalReview) error
	ApplyMemoryProposal(ctx context.Context, apply MemoryProposalApply) (*Memory, error)
	ArchiveMemoryProposal(ctx context.Context, namespace, id string) error
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
