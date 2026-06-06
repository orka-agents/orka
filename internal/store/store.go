package store

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a resource cannot be updated because it changed concurrently.
var ErrConflict = errors.New("conflict")

// ErrValidation is returned when supplied input fails store-level validation.
var ErrValidation = errors.New("validation error")

// ValidationError carries a client-safe validation message while remaining
// comparable with ErrValidation via errors.Is.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string {
	return e.Message
}

func (e ValidationError) Unwrap() error {
	return ErrValidation
}

// ValidationErrorf formats a store validation error.
func ValidationErrorf(format string, args ...any) error {
	return ValidationError{Message: fmt.Sprintf(format, args...)}
}

// HealthChecker can verify its underlying storage is reachable.
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// ResultStore handles task result persistence.
type ResultStore interface {
	SaveResult(ctx context.Context, namespace, taskName string, data []byte) error
	GetResult(ctx context.Context, namespace, taskName string) ([]byte, error)
	DeleteResult(ctx context.Context, namespace, taskName string) error
}

// SessionStore handles session transcript persistence.
type SessionStore interface {
	CreateSession(ctx context.Context, session *SessionRecord) error
	GetSession(ctx context.Context, namespace, name string) (*SessionRecord, error)
	ListSessions(ctx context.Context, namespace string) ([]SessionMetadata, error)
	DeleteSession(ctx context.Context, namespace, name string) error

	// Locking
	AcquireLock(ctx context.Context, namespace, name, taskName string) error
	ReleaseLock(ctx context.Context, namespace, name, taskName string) error
	IsLocked(ctx context.Context, namespace, name, currentTask string) (bool, error)

	// Transcript
	AppendMessages(ctx context.Context, namespace, name string, messages []SessionMessage) error
	LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]SessionMessage, error)
	SearchTranscript(ctx context.Context, filter TranscriptSearchFilter) ([]TranscriptSearchResult, error)

	// Token tracking
	UpdateTokenCounts(ctx context.Context, namespace, name string, inputTokens, outputTokens int) error
}

// MemoryStore handles durable namespace-scoped memory persistence.
type MemoryStore interface {
	CreateMemory(ctx context.Context, memory *Memory) error
	GetMemory(ctx context.Context, namespace, id string) (*Memory, error)
	ListMemories(ctx context.Context, filter MemoryFilter) ([]Memory, error)
	UpdateMemory(ctx context.Context, memory *Memory) error
	DeleteMemory(ctx context.Context, namespace, id string) error
	SetMemoryDisabled(ctx context.Context, namespace, id string, disabled bool) error
	MarkMemoriesRecalled(ctx context.Context, namespace string, ids []string) error
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

// PlanStore handles autonomous plan state persistence.
type PlanStore interface {
	SavePlan(ctx context.Context, namespace, taskName string, plan *PlanState) error
	GetPlan(ctx context.Context, namespace, taskName string) (*PlanState, error)
	DeletePlan(ctx context.Context, namespace, taskName string) error
}

// ArtifactStore handles task artifact persistence.
type ArtifactStore interface {
	SaveArtifact(ctx context.Context, namespace, taskName, filename, contentType string, data []byte) error
	GetArtifact(ctx context.Context, namespace, taskName, filename string) ([]byte, string, error)
	ListArtifacts(ctx context.Context, namespace, taskName string) ([]ArtifactMetadata, error)
	DeleteArtifacts(ctx context.Context, namespace, taskName string) error
}

// SecurityStore handles repository security scanning persistence.
type SecurityStore interface {
	CreateScanRun(ctx context.Context, run *ScanRun) error
	UpdateScanRun(ctx context.Context, run *ScanRun) error
	GetScanRun(ctx context.Context, namespace, id string) (*ScanRun, error)
	ListScanRuns(ctx context.Context, namespace, repositoryScan string, limit int, cursor string) ([]ScanRun, string, error)

	GetLatestThreatModel(ctx context.Context, namespace, repositoryScan string) (*ThreatModel, error)
	SaveThreatModel(ctx context.Context, model *ThreatModel) error

	UpsertFinding(ctx context.Context, finding *Finding) error
	GetFinding(ctx context.Context, namespace, id string) (*Finding, error)
	ListFindings(ctx context.Context, filter FindingFilter) ([]Finding, string, error)
	GetFindingCounts(ctx context.Context, namespace, repositoryScan string) (FindingCounts, error)
	UpdateFindingState(ctx context.Context, namespace, id, state string) error

	CreatePatchProposal(ctx context.Context, proposal *PatchProposal) error
	UpdatePatchProposal(ctx context.Context, proposal *PatchProposal) error
	ListPatchProposals(ctx context.Context, namespace, findingID string) ([]PatchProposal, error)
}

// RepositoryMonitorStore handles durable repository monitor state.
type RepositoryMonitorStore interface {
	UpsertRepositoryMonitor(ctx context.Context, monitor *RepositoryMonitorRecord) error
	GetRepositoryMonitor(ctx context.Context, namespace, name string) (*RepositoryMonitorRecord, error)
	ListRepositoryMonitors(ctx context.Context, namespace string, limit int, cursor string) ([]RepositoryMonitorRecord, string, error)
	DeleteRepositoryMonitor(ctx context.Context, namespace, name string) error

	CreateMonitorRun(ctx context.Context, run *MonitorRun) error
	UpdateMonitorRun(ctx context.Context, run *MonitorRun) error
	GetMonitorRun(ctx context.Context, namespace, id string) (*MonitorRun, error)
	ListMonitorRuns(ctx context.Context, filter MonitorRunFilter) ([]MonitorRun, string, error)

	UpsertMonitorItem(ctx context.Context, item *MonitorItem) error
	GetMonitorItem(ctx context.Context, namespace, monitorName, kind, itemKey string) (*MonitorItem, error)
	ListMonitorItems(ctx context.Context, filter MonitorItemFilter) ([]MonitorItem, string, error)

	CreateReviewRecord(ctx context.Context, record *ReviewRecord) error
	GetReviewRecord(ctx context.Context, namespace, id string) (*ReviewRecord, error)
	ListReviewRecords(ctx context.Context, filter ReviewRecordFilter) ([]ReviewRecord, string, error)

	CreateCommandEvent(ctx context.Context, event *CommandEvent) error
	UpdateCommandEvent(ctx context.Context, event *CommandEvent) error
	GetCommandEvent(ctx context.Context, namespace, id string) (*CommandEvent, error)

	CreateRepairJob(ctx context.Context, job *RepairJob) error
	UpdateRepairJob(ctx context.Context, job *RepairJob) error
	GetRepairJob(ctx context.Context, namespace, id string) (*RepairJob, error)
	ListRepairJobs(ctx context.Context, filter RepairJobFilter) ([]RepairJob, string, error)

	CreateMonitorEvent(ctx context.Context, event *MonitorEvent) error
	ListMonitorEvents(ctx context.Context, filter MonitorEventFilter) ([]MonitorEvent, string, error)
}

// Message represents an inter-agent message.
type Message struct {
	ID         int64  `json:"id"`
	Namespace  string `json:"namespace"`
	FromTask   string `json:"fromTask"`
	ToTask     string `json:"toTask"` // "*" for broadcast to all siblings
	ParentTask string `json:"parentTask"`
	Content    string `json:"content"`
	Read       bool   `json:"read"`
	CreatedAt  string `json:"createdAt"`
}

// MessageStore handles inter-agent message persistence.
type MessageStore interface {
	// SendMessage stores a new message. Use toTask="*" for broadcast.
	SendMessage(ctx context.Context, msg *Message) error

	// GetMessages returns unread messages for a task, optionally marking them as read.
	GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]Message, error)

	// DeleteTaskMessages deletes all messages involving a task (sent or received).
	DeleteTaskMessages(ctx context.Context, namespace, taskName string) error

	// DeleteParentMessages deletes all messages for children of a parent task.
	DeleteParentMessages(ctx context.Context, namespace, parentTask string) error
}
