package store

import "context"

// MemoryProposalApply records an explicit proposal application request.
type MemoryProposalApply struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	AppliedBy string `json:"appliedBy"`
	// AuthorizeExistingMemory authorizes the exact existing memory an
	// application would return or link, before the storage client commits it.
	// No provider transaction is open while it runs, and it runs again for
	// each new target after a concurrent change. New memories are covered by
	// the caller's separate create permission.
	AuthorizeExistingMemory func(context.Context, string, string) error `json:"-"`
}
