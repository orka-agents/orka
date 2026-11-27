package v1

import (
	"context"
	"time"
)

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
	Generation       uint64     `json:"generation"`
	SupersededBy     string     `json:"supersededBy,omitempty"`
}

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

type MemoryRecall struct {
	Namespace  string
	IDs        []string
	RecalledAt time.Time
}

const MaxMemoryRecallIDs = 200

type MemoryProposal struct {
	ID              string                 `json:"id"`
	Namespace       string                 `json:"namespace"`
	TaskName        string                 `json:"taskName,omitempty"`
	AgentName       string                 `json:"agentName,omitempty"`
	Type            string                 `json:"type"`
	SkillName       string                 `json:"skillName,omitempty"`
	Title           string                 `json:"title"`
	Description     string                 `json:"description,omitempty"`
	Content         string                 `json:"content,omitempty"`
	Patch           string                 `json:"patch,omitempty"`
	Status          string                 `json:"status"`
	Reviewer        string                 `json:"reviewer,omitempty"`
	ReviewNote      string                 `json:"reviewNote,omitempty"`
	AppliedMemoryID string                 `json:"appliedMemoryId,omitempty"`
	AppliedBy       string                 `json:"appliedBy,omitempty"`
	CreatedAt       time.Time              `json:"createdAt"`
	UpdatedAt       time.Time              `json:"updatedAt"`
	ReviewedAt      *time.Time             `json:"reviewedAt,omitempty"`
	AppliedAt       *time.Time             `json:"appliedAt,omitempty"`
	Targets         []MemoryProposalTarget `json:"targets,omitempty"`
	Sources         []MemoryProposalSource `json:"sources,omitempty"`
}

type MemoryProposalTarget struct {
	MemoryID   string `json:"memoryId"`
	Generation uint64 `json:"generation"`
}

type MemoryProposalSource struct {
	SessionName string `json:"sessionName"`
	MessageID   string `json:"messageId,omitempty"`
}

const (
	MaxMemoryProposalTargets = 20
	MaxMemoryProposalSources = 20
)

func IsMemoryConsolidationProposalType(proposalType string) bool {
	switch proposalType {
	case MemoryProposalTypeMerge, MemoryProposalTypeSupersede, MemoryProposalTypeRetire:
		return true
	default:
		return false
	}
}

type MemoryProposalFilter struct {
	Namespace string
	TaskName  string
	AgentName string
	Type      string
	Status    string
	Query     string
	Limit     int
}

type MemoryProposalReview struct {
	Namespace  string
	ID         string
	Status     string
	Reviewer   string
	ReviewNote string
}

type MemoryProposalApplyTarget struct {
	Namespace       string
	ProposalID      string
	ProposalStatus  string
	AppliedMemoryID string
	MemoryID        string
}

type MemoryProposalApplication struct {
	Target    MemoryProposalApplyTarget
	AppliedBy string
	AppliedAt time.Time
	Memory    *Memory
}

const (
	MemorySourceManual   = "manual"
	MemorySourceProposal = "memory_proposal"

	MemoryProposalTypeSkill     = "skill"
	MemoryProposalTypeMemory    = "memory"
	MemoryProposalTypeMerge     = "memory_merge"
	MemoryProposalTypeSupersede = "memory_supersede"
	MemoryProposalTypeRetire    = "memory_retire"

	MemoryProposalStatusPending  = "pending"
	MemoryProposalStatusAccepted = "accepted"
	MemoryProposalStatusRejected = "rejected"
	MemoryProposalStatusArchived = "archived"
	MemoryProposalStatusApplied  = "applied"
)

type MemoryStore interface {
	CreateMemory(ctx context.Context, memory *Memory) error
	GetMemory(ctx context.Context, namespace, id string) (*Memory, error)
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	UpdateMemory(ctx context.Context, memory *Memory) error
	DeleteMemory(ctx context.Context, namespace, id string) error
	SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool) error
	RecordMemoryRecall(ctx context.Context, recall MemoryRecall) error
}

type MemoryProposalStore interface {
	CreateMemoryProposal(ctx context.Context, proposal *MemoryProposal) error
	GetMemoryProposal(ctx context.Context, namespace, id string) (*MemoryProposal, error)
	ListMemoryProposals(ctx context.Context, filter MemoryProposalFilter) ([]MemoryProposal, error)
	ReviewMemoryProposal(ctx context.Context, review MemoryProposalReview) error
	ArchiveMemoryProposal(ctx context.Context, namespace, id string) error
	ResolveMemoryProposalApplication(ctx context.Context, namespace, id string) (MemoryProposalApplyTarget, error)
	CommitMemoryProposalApplication(ctx context.Context, application MemoryProposalApplication) (*Memory, error)
}
