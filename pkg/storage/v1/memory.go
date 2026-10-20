package v1

import (
	"context"
	"time"
)

// Memory represents durable namespace-scoped memory captured from tasks,
// sessions, or explicit worker calls.
type Memory struct {
	ID               string     `json:"id"`
	Namespace        string     `json:"namespace"`
	SessionName      string     `json:"sessionName,omitempty"`
	AgentName        string     `json:"agentName,omitempty"`
	TaskName         string     `json:"taskName,omitempty"`
	ParentTask       string     `json:"parentTask,omitempty"`
	Source           string     `json:"source"`
	SourceProposalID string     `json:"sourceProposalId,omitempty"`
	Content          string     `json:"content"`
	Tags             []string   `json:"tags,omitempty"`
	Disabled         bool       `json:"disabled"`
	Deleted          bool       `json:"deleted"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	LastRecalledAt   *time.Time `json:"lastRecalledAt,omitempty"`
	RecalledCount    int        `json:"recalledCount"`
}

// MemoryFilter constrains memory list and recall queries.
type MemoryFilter struct {
	Namespace       string
	Query           string
	SessionName     string
	AgentName       string
	TaskName        string
	ParentTask      string
	Source          string
	Tags            []string
	IDs             []string
	IncludeDisabled bool
	IncludeDeleted  bool
	Limit           int
}

// MemoryProposal represents a proposed memory-adjacent change such as a reusable skill.
type MemoryProposal struct {
	ID              string     `json:"id"`
	Namespace       string     `json:"namespace"`
	TaskName        string     `json:"taskName,omitempty"`
	AgentName       string     `json:"agentName,omitempty"`
	Type            string     `json:"type"`
	SkillName       string     `json:"skillName,omitempty"`
	Title           string     `json:"title"`
	Description     string     `json:"description,omitempty"`
	Content         string     `json:"content,omitempty"`
	Patch           string     `json:"patch,omitempty"`
	Status          string     `json:"status"`
	Reviewer        string     `json:"reviewer,omitempty"`
	ReviewNote      string     `json:"reviewNote,omitempty"`
	AppliedMemoryID string     `json:"appliedMemoryId,omitempty"`
	AppliedBy       string     `json:"appliedBy,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	ReviewedAt      *time.Time `json:"reviewedAt,omitempty"`
	AppliedAt       *time.Time `json:"appliedAt,omitempty"`
}

// MemoryProposalFilter constrains proposal list queries.
type MemoryProposalFilter struct {
	Namespace string
	TaskName  string
	AgentName string
	Type      string
	Status    string
	Query     string
	Limit     int
}

// MemoryProposalReview records governance review decisions for proposals.
type MemoryProposalReview struct {
	Namespace  string
	ID         string
	Status     string
	Reviewer   string
	ReviewNote string
}

// Memory sources and memory proposal vocabulary.
const (
	// MemorySourceManual is recorded when a created memory names no source.
	MemorySourceManual = "manual"
	// MemorySourceProposal marks a memory created by applying a proposal.
	MemorySourceProposal = "memory_proposal"

	// MemoryProposalTypeSkill is the default proposal type.
	MemoryProposalTypeSkill = "skill"
	// MemoryProposalTypeMemory proposes a durable memory.
	MemoryProposalTypeMemory = "memory"

	MemoryProposalStatusPending  = "pending"
	MemoryProposalStatusAccepted = "accepted"
	MemoryProposalStatusRejected = "rejected"
	MemoryProposalStatusArchived = "archived"
	MemoryProposalStatusApplied  = "applied"
)

// MemoryStore holds durable namespace-scoped memories. Orka redacts memory
// content before a provider receives it.
//
//   - CreateMemory requires a namespace and non-blank content. It assigns an
//     ID when the record has none and records MemorySourceManual when it
//     names no source, writing both back to the caller's record.
//   - A memory is read, updated, disabled and deleted only in its namespace.
//     A missing memory returns ErrNotFound.
//   - ListMemories requires a namespace and returns memories most recently
//     updated first. It leaves out disabled and deleted memories unless the
//     filter includes them, matches Tags and Query case-insensitively, and
//     bounds the result: a zero Limit selects the provider's default.
//   - UpdateMemory replaces the mutable fields, including Disabled and
//     Deleted, and refreshes UpdatedAt with the provider's time.
//   - DeleteMemory keeps the record, marking it deleted and disabled.
//     SetMemoryDisabled on a deleted memory returns ErrNotFound.
type MemoryStore interface {
	CreateMemory(ctx context.Context, memory *Memory) error
	GetMemory(ctx context.Context, namespace, id string) (*Memory, error)
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	UpdateMemory(ctx context.Context, memory *Memory) error
	DeleteMemory(ctx context.Context, namespace, id string) error
	SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool) error
}

// MemoryProposalStore holds governance proposals for worker-proposed memory
// and skill changes. Orka redacts proposal and review text before a provider
// receives it. Applying an accepted proposal is not part of this contract
// version: it authorizes an existing memory inside the application
// transaction.
//
//   - CreateMemoryProposal requires a namespace and a title and creates a
//     pending proposal. It assigns an ID when the record has none, trims and
//     lower-cases Type (default MemoryProposalTypeSkill) and Status, and
//     writes them back to the caller's record. A new proposal carries no
//     review or apply outcome: a status other than
//     MemoryProposalStatusPending, or a reviewer, review note, review time,
//     applied memory ID, applier or application time, returns ErrValidation.
//   - A proposal is read, reviewed and archived only in its namespace. A
//     missing proposal returns ErrNotFound.
//   - ListMemoryProposals requires a namespace and returns proposals newest
//     first. Query matches the title, description, content and skill name
//     case-insensitively; a zero Limit selects the provider's default.
//   - Only proposal application, which is outside this contract version,
//     applies a proposal. A proposal counts as applied when its status is
//     MemoryProposalStatusApplied or it names an AppliedMemoryID.
//   - ReviewMemoryProposal records MemoryProposalStatusAccepted or
//     MemoryProposalStatusRejected, with the reviewer, note and review time,
//     once and only on a pending proposal that is not applied; any other
//     status, or any other proposal, returns ErrValidation. When two reviews
//     race, one succeeds and the other returns an error without changing the
//     proposal. A review never applies a proposal.
//   - ArchiveMemoryProposal is idempotent and refuses an applied proposal
//     with ErrValidation.
type MemoryProposalStore interface {
	CreateMemoryProposal(ctx context.Context, proposal *MemoryProposal) error
	GetMemoryProposal(ctx context.Context, namespace, id string) (*MemoryProposal, error)
	ListMemoryProposals(ctx context.Context, filter MemoryProposalFilter) ([]MemoryProposal, error)
	ReviewMemoryProposal(ctx context.Context, review MemoryProposalReview) error
	ArchiveMemoryProposal(ctx context.Context, namespace, id string) error
}
