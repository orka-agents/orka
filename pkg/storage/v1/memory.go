package v1

import (
	"context"
	"time"
)

// Memory represents durable namespace-scoped memory captured from tasks,
// sessions, or explicit worker calls. LastRecalledAt and RecalledCount record
// agent recalls of the memory; only RecordMemoryRecall changes them.
//
// Generation is the version of the record: 1 when it is created, and one more
// for every change to its content, tags, provenance, Disabled or Deleted; a
// recall does not change it. A consolidation proposal names the generation it
// read, so applying it fails once the memory changed. SupersededBy names the
// memory that replaced this one when a merge or supersede proposal was
// applied.
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

// MemoryRecall records that an agent's recall returned the named memories.
// Only a recall counts: Orka records one when an agent's recall_memory call
// returns memories, never when it injects memories into a prompt or when an
// operator lists them.
type MemoryRecall struct {
	Namespace string
	// IDs names the memories the recall returned, at most MaxMemoryRecallIDs.
	IDs []string
	// RecalledAt is when the recall returned them.
	RecalledAt time.Time
}

// MaxMemoryRecallIDs bounds the memories one recall records. It equals the
// largest recall a provider returns.
const MaxMemoryRecallIDs = 200

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
	// Targets names the memories a consolidation proposal changes, each at the
	// generation the proposal read. Only consolidation types carry targets.
	Targets []MemoryProposalTarget `json:"targets,omitempty"`
	// Sources cites the Session messages a proposal drew on.
	Sources []MemoryProposalSource `json:"sources,omitempty"`
}

// MemoryProposalTarget names one memory a consolidation proposal changes and
// the generation of that memory the proposal read.
type MemoryProposalTarget struct {
	MemoryID   string `json:"memoryId"`
	Generation uint64 `json:"generation"`
}

// MemoryProposalSource cites a Session, and optionally one of its messages,
// that a proposal drew on.
type MemoryProposalSource struct {
	SessionName string `json:"sessionName"`
	MessageID   string `json:"messageId,omitempty"`
}

// MaxMemoryProposalTargets and MaxMemoryProposalSources bound the targets and
// sources of one proposal.
const (
	MaxMemoryProposalTargets = 20
	MaxMemoryProposalSources = 20
)

// IsMemoryConsolidationProposalType reports whether proposalType is one of
// the consolidation types, which change existing memories.
func IsMemoryConsolidationProposalType(proposalType string) bool {
	switch proposalType {
	case MemoryProposalTypeMerge, MemoryProposalTypeSupersede, MemoryProposalTypeRetire:
		return true
	default:
		return false
	}
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

// MemoryProposalApplyTarget names what applying a proposal would return,
// without the content of any memory. Orka authorizes MemoryID, when it is
// set, before it commits the application.
type MemoryProposalApplyTarget struct {
	Namespace  string
	ProposalID string
	// ProposalStatus and AppliedMemoryID are the proposal state the target
	// was resolved from. They are the version the commit checks.
	ProposalStatus  string
	AppliedMemoryID string
	// MemoryID is the existing memory the application returns: the memory
	// the proposal names, or the memory already created from it. It is empty
	// when the application creates a memory.
	MemoryID string
}

// MemoryProposalApplication commits a resolved proposal application.
type MemoryProposalApplication struct {
	// Target is the target ResolveMemoryProposalApplication returned.
	Target    MemoryProposalApplyTarget
	AppliedBy string
	// AppliedAt is recorded as the application time.
	AppliedAt time.Time
	// Memory is the memory to create when Target.MemoryID is empty, and must
	// be nil otherwise. Orka builds it from the proposal and redacts it. The
	// provider sets its namespace, source and source proposal, and assigns an
	// ID when it has none.
	Memory *Memory
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
	// MemoryProposalTypeMerge proposes one memory, from the proposal's
	// content, that replaces two or more target memories.
	MemoryProposalTypeMerge = "memory_merge"
	// MemoryProposalTypeSupersede proposes one memory, from the proposal's
	// content, that replaces one target memory.
	MemoryProposalTypeSupersede = "memory_supersede"
	// MemoryProposalTypeRetire proposes disabling one or more target memories.
	MemoryProposalTypeRetire = "memory_retire"

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
//     names no source, writing both back to the caller's record. Memory IDs
//     are unique across namespaces: an ID already in use returns ErrConflict,
//     as does a SourceProposalID already used by a memory in the namespace.
//     It stores RecalledCount 0, no LastRecalledAt, Generation 1 and no
//     SupersededBy, whatever the record names, and writes them back.
//   - A memory is read, updated, disabled and deleted only in its namespace.
//     A missing memory returns ErrNotFound.
//   - ListMemories requires a namespace and returns memories most recently
//     updated first. It leaves out disabled and deleted memories unless the
//     filter includes them, matches Tags and Query case-insensitively, and
//     bounds the result: a zero Limit selects the provider's default.
//   - UpdateMemory replaces the mutable fields, including Disabled, Deleted
//     and SourceProposalID, and refreshes UpdatedAt with the provider's time.
//     A SourceProposalID another memory in the namespace uses returns
//     ErrConflict. It ignores the record's Generation and SupersededBy, and
//     adds one to the stored Generation when any field it replaces changes;
//     an update that changes none keeps it.
//   - DeleteMemory keeps the record, marking it deleted and disabled.
//     SetMemoryDisabled on a deleted memory returns ErrNotFound. Each adds one
//     to Generation when it changes Deleted or Disabled, and leaves it
//     unchanged otherwise.
//   - RecordMemoryRecall requires a namespace and at most MaxMemoryRecallIDs
//     IDs, and returns ErrValidation otherwise. For each distinct ID that
//     names a memory in the namespace that is not deleted, it adds one to
//     RecalledCount and sets LastRecalledAt to RecalledAt unless the stored
//     time is later. It ignores any other ID, because a memory can be
//     deleted between a recall and its record, and changes no other field:
//     a recall is not an update, so UpdatedAt and list order stay as they
//     were. A repeat of the same operation is safe through its receipt; a new
//     operation for the same recall counts it again.
type MemoryStore interface {
	CreateMemory(ctx context.Context, memory *Memory) error
	GetMemory(ctx context.Context, namespace, id string) (*Memory, error)
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	UpdateMemory(ctx context.Context, memory *Memory) error
	DeleteMemory(ctx context.Context, namespace, id string) error
	SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool) error
	RecordMemoryRecall(ctx context.Context, recall MemoryRecall) error
}

// MemoryProposalStore holds governance proposals for worker-proposed memory
// and skill changes. Orka redacts proposal and review text before a provider
// receives it.
//
//   - CreateMemoryProposal requires a namespace and a title and creates a
//     pending proposal. Proposal IDs are unique across namespaces: an ID
//     already in use returns ErrConflict. It assigns an ID when the record
//     has none, trims and lower-cases Type (default
//     MemoryProposalTypeSkill) and Status, and writes them back to the
//     caller's record. A new proposal carries no review or apply outcome:
//     a status other than MemoryProposalStatusPending, or a reviewer, review
//     note, review time, applied memory ID, applier or application time,
//     returns ErrValidation. A blank reviewer, review note or applier counts
//     as absent; any nonempty AppliedMemoryID, even whitespace, is an apply
//     outcome, as it is for review and archive. Targets and Sources are
//     validated, returning ErrValidation: a consolidation type
//     (IsMemoryConsolidationProposalType) needs its targets: at least two for
//     MemoryProposalTypeMerge, exactly one for MemoryProposalTypeSupersede and
//     at least one for MemoryProposalTypeRetire, each a distinct nonblank
//     memory ID with a generation of at least 1, at most
//     MaxMemoryProposalTargets. Merge and supersede also need nonblank
//     content. Any other type carries no targets. Every type may cite at most
//     MaxMemoryProposalSources sources, each naming a Session.
//   - A proposal is read, reviewed and archived only in its namespace. A
//     missing proposal returns ErrNotFound.
//   - ListMemoryProposals requires a namespace and returns proposals newest
//     first. Query matches the title, description, content and skill name
//     case-insensitively; a zero Limit selects the provider's default.
//   - Only CommitMemoryProposalApplication applies a proposal. A proposal
//     counts as applied when its status is MemoryProposalStatusApplied or it
//     names an AppliedMemoryID.
//   - ReviewMemoryProposal records MemoryProposalStatusAccepted or
//     MemoryProposalStatusRejected, with the reviewer, note and review time,
//     once and only on a pending proposal that is not applied; any other
//     status, or any other proposal, returns ErrValidation. When reviews
//     race, one succeeds and the others return ErrValidation without
//     changing the proposal. A review never applies a proposal.
//   - ArchiveMemoryProposal is idempotent and refuses an applied proposal
//     with ErrValidation.
//
// Applying a proposal is a conditional command in two steps, so that Orka can
// authorize the exact memory it would return before any memory content
// leaves the provider:
//
//   - ResolveMemoryProposalApplication returns the target without memory
//     content. An applied proposal targets the memory it names; a proposal
//     that names none returns an error. An accepted proposal of type
//     MemoryProposalTypeMemory targets the memory already created from it
//     (its SourceProposalID), or no memory when applying would create one.
//     So does an accepted merge or supersede proposal. An accepted retire
//     proposal targets its first target memory. Any other type or status
//     returns ErrValidation. A missing proposal returns ErrNotFound.
//   - CommitMemoryProposalApplication resolves the target again and applies
//     only if it equals Target, returning ErrPreconditionFailed with no
//     memory and no change otherwise. For an applied proposal it returns the
//     named memory unchanged. For an existing memory created from the
//     proposal it records the proposal as applied to that memory, keeping an
//     earlier application time. Otherwise it creates Memory and records the
//     proposal as applied to it, in the same transaction. Two applications
//     that race create one memory: the loser returns ErrPreconditionFailed
//     and its retry resolves the winner's memory.
//   - Applying a consolidation proposal changes its targets in the same
//     transaction, and only while every target is in the namespace, is not
//     deleted and is still at the generation the proposal names. Otherwise
//     it returns ErrConflict and changes nothing: the proposal is stale, and
//     a new resolve would not change that. A merge or supersede creates
//     Memory, then marks every target deleted and disabled, superseded by
//     it. It never adopts a memory that already names it as its source:
//     such a memory was not created by applying it, so it returns
//     ErrConflict. A retire creates nothing and disables every target,
//     returning the first, and leaves a target already disabled at its
//     generation. Each target that changes gains one generation and the
//     application time as UpdatedAt.
type MemoryProposalStore interface {
	CreateMemoryProposal(ctx context.Context, proposal *MemoryProposal) error
	GetMemoryProposal(ctx context.Context, namespace, id string) (*MemoryProposal, error)
	ListMemoryProposals(ctx context.Context, filter MemoryProposalFilter) ([]MemoryProposal, error)
	ReviewMemoryProposal(ctx context.Context, review MemoryProposalReview) error
	ArchiveMemoryProposal(ctx context.Context, namespace, id string) error
	ResolveMemoryProposalApplication(ctx context.Context, namespace, id string) (MemoryProposalApplyTarget, error)
	CommitMemoryProposalApplication(ctx context.Context, application MemoryProposalApplication) (*Memory, error)
}
