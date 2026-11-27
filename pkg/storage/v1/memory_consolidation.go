package v1

import (
	"context"
	"time"
)

type MemoryConsolidationRun struct {
	Namespace   string     `json:"namespace"`
	TaskUID     string     `json:"taskUID"`
	TaskName    string     `json:"taskName"`
	Mode        string     `json:"mode"`
	Status      string     `json:"status"`
	WindowStart time.Time  `json:"windowStart"`
	WindowEnd   time.Time  `json:"windowEnd"`
	Input       []byte     `json:"input,omitempty"`
	Report      []byte     `json:"report,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type MemoryConsolidationCompletion struct {
	Namespace   string
	TaskUID     string
	Status      string
	Report      []byte
	CompletedAt time.Time
	Proposals   []MemoryProposal
}

const (
	MemoryConsolidationRunning   = "running"
	MemoryConsolidationCompleted = "completed"
	MemoryConsolidationSkipped   = "skipped"
	MemoryConsolidationAbandoned = "abandoned"

	MemoryConsolidationModePropose = "Propose"

	MaxMemoryConsolidationReportBytes = 256 << 10
	MaxMemoryConsolidationInputBytes  = 1 << 20
	MaxMemoryConsolidationProposals   = 20
	MaxMemoryConsolidationSessions    = 200

	MemoryConsolidationRunLease = 2 * time.Hour
)

type MemoryConsolidationStore interface {
	BeginMemoryConsolidationRun(ctx context.Context, run *MemoryConsolidationRun) error
	CompleteMemoryConsolidationRun(ctx context.Context, completion MemoryConsolidationCompletion) (*MemoryConsolidationRun, error)
	ReleaseMemoryConsolidationRun(ctx context.Context, namespace, taskUID string, deleted bool) error
	GetMemoryConsolidationRun(ctx context.Context, namespace, taskUID string) (*MemoryConsolidationRun, error)
	GetMemoryConsolidationCursor(ctx context.Context, namespace, mode string) (*time.Time, error)
	ListMemoryConsolidationSessions(ctx context.Context, namespace string, after, through time.Time, limit int) ([]SessionMetadata, int, error)
}
