package store

import "context"

// MemoryProposalApply records an explicit proposal application request.
type MemoryProposalApply struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	AppliedBy string `json:"appliedBy"`
	// AuthorizeExistingMemory runs inside the apply transaction before returning
	// an existing memory or changing the proposal to reference it. New memories
	// are covered by the caller's separate create permission.
	AuthorizeExistingMemory func(context.Context, string, string) error `json:"-"`
}
