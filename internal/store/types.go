package store

import "context"

type MemoryProposalApply struct {
	Namespace               string                                      `json:"namespace"`
	ID                      string                                      `json:"id"`
	AppliedBy               string                                      `json:"appliedBy"`
	AuthorizeExistingMemory func(context.Context, string, string) error `json:"-"`
	AuthorizeMemoryChange   func(context.Context, string, string) error `json:"-"`
}
