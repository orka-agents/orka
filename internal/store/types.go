package store

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
