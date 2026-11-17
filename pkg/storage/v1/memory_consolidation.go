package v1

import (
	"context"
	"time"
)

// MemoryConsolidationRun records one memory consolidation run (ADR 0051): the
// window of Sessions it reads and, once it finished, its report. Each mode of
// a namespace has its own cursor, so a dry run never consumes the Sessions a
// proposing run has yet to read.
type MemoryConsolidationRun struct {
	Namespace string `json:"namespace"`
	// TaskUID identifies the run: one run per Task.
	TaskUID  string `json:"taskUID"`
	TaskName string `json:"taskName"`
	Mode     string `json:"mode"`
	// Status is MemoryConsolidationRunning until the run completes or is
	// skipped.
	Status      string    `json:"status"`
	WindowStart time.Time `json:"windowStart"`
	WindowEnd   time.Time `json:"windowEnd"`
	// Input is the JSON input the run read, recorded when it began, at most
	// MaxMemoryConsolidationInputBytes. A resumed worker reads the same
	// input, and completion is checked against it. It holds transcript
	// excerpts, so the provider clears it once the run completes, is skipped
	// or is abandoned, and the excerpts never outlive their run.
	Input []byte `json:"input,omitempty"`
	// Report is the run's JSON report, set when it finishes, at most
	// MaxMemoryConsolidationReportBytes.
	Report      []byte     `json:"report,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// MemoryConsolidationCompletion finishes a running run.
type MemoryConsolidationCompletion struct {
	Namespace string
	TaskUID   string
	// Status is MemoryConsolidationCompleted or MemoryConsolidationSkipped.
	Status      string
	Report      []byte
	CompletedAt time.Time
	// Proposals are the pending memory proposals the run creates, at most
	// MaxMemoryConsolidationProposals. Orka redacts them first.
	Proposals []MemoryProposal
}

// Memory consolidation run statuses and bounds.
const (
	MemoryConsolidationRunning   = "running"
	MemoryConsolidationCompleted = "completed"
	MemoryConsolidationSkipped   = "skipped"
	// MemoryConsolidationAbandoned marks a running run whose lease ended and
	// that another run took over. It can neither resume nor complete.
	MemoryConsolidationAbandoned = "abandoned"

	MaxMemoryConsolidationReportBytes = 256 << 10
	MaxMemoryConsolidationInputBytes  = 1 << 20
	MaxMemoryConsolidationProposals   = 20
	// MaxMemoryConsolidationSessions bounds a ListMemoryConsolidationSessions
	// page.
	MaxMemoryConsolidationSessions = 200

	// MemoryConsolidationRunLease is how long a running run keeps another
	// from beginning in its namespace. A run whose Task died stops blocking
	// once it is older.
	MemoryConsolidationRunLease = 2 * time.Hour
)

// MemoryConsolidationStore holds memory consolidation runs.
//
//   - BeginMemoryConsolidationRun requires a namespace, a TaskUID, a
//     TaskName, a mode, status MemoryConsolidationRunning, a CreatedAt and a
//     window that does not end before it starts, and an Input of at most
//     MaxMemoryConsolidationInputBytes, and returns ErrValidation otherwise.
//     When a run already exists for the namespace and TaskUID it changes
//     nothing. Otherwise it records the run, unless another run in the
//     namespace, of any mode, is running and began less than
//     MemoryConsolidationRunLease before CreatedAt: then it returns
//     ErrConflict, so two runs never propose over the same memories at once.
//     It returns ErrPreconditionFailed, which also matches ErrConflict, when
//     a run in the same mode completed with a window end after WindowStart:
//     the window was chosen from a cursor that has since moved, and the
//     caller chooses it again.
//     A running run whose lease ended is marked MemoryConsolidationAbandoned
//     in the same transaction, so it can never complete. It writes the stored
//     run back to the caller's record, so a retried worker keeps its first
//     window and input.
//   - CompleteMemoryConsolidationRun finishes the run the namespace and
//     TaskUID name, which must exist (ErrNotFound otherwise). Status must be
//     completed or skipped, and the report non-empty and at most
//     MaxMemoryConsolidationReportBytes. A running run records the status,
//     report and completion time, clears its Input, and creates every
//     proposal, each validated
//     as CreateMemoryProposal validates it, all in one transaction: an
//     invalid or duplicate proposal changes nothing. A skipped run creates no
//     proposals. A finished or abandoned run is returned unchanged and
//     creates nothing. It returns the stored run.
//   - ReleaseMemoryConsolidationRun releases the run of a namespace and
//     TaskUID whose Task ended: a running run becomes
//     MemoryConsolidationAbandoned, and the run's Input is cleared, so it
//     keeps no transcripts. With deleted, for a Task being deleted, its
//     Report is cleared too, and a run the namespace's cursors no longer
//     need is removed, as is any earlier deleted run in its mode that a later
//     completed run supersedes, so runs do not accumulate with Task history.
//     Neither mode's cursor moves. A missing run is left alone. It requires a
//     namespace and a TaskUID.
//   - GetMemoryConsolidationRun returns the run of a namespace and TaskUID,
//     or ErrNotFound.
//   - ListMemoryConsolidationSessions returns the namespace's chat and task
//     Sessions, never gateway ones, whose UpdatedAt is after after and no
//     later than through: the most recently updated first, then by name, at
//     most limit (1 to MaxMemoryConsolidationSessions), and how many there
//     are in all.
//   - GetMemoryConsolidationCursor returns where the namespace's next run in
//     mode starts: the latest window end of a completed run in that mode, or,
//     before any completed, the earliest window start of a run in that mode,
//     so skipped and failed runs keep the first run's start. It returns nil
//     when the mode has no run.
//
// Begin, Complete, GetMemoryConsolidationRun and
// ListMemoryConsolidationSessions check a Task data fence in their context
// (see TaskDataFenceStore), since a running run's Input holds transcripts
// and its completion creates proposals. A run's Input and Report never
// outlive the run's own row: an operation receipt for
// BeginMemoryConsolidationRun or CompleteMemoryConsolidationRun keeps no copy
// of either, so a replay returns the run as it is now.
type MemoryConsolidationStore interface {
	BeginMemoryConsolidationRun(ctx context.Context, run *MemoryConsolidationRun) error
	CompleteMemoryConsolidationRun(ctx context.Context, completion MemoryConsolidationCompletion) (*MemoryConsolidationRun, error)
	ReleaseMemoryConsolidationRun(ctx context.Context, namespace, taskUID string, deleted bool) error
	GetMemoryConsolidationRun(ctx context.Context, namespace, taskUID string) (*MemoryConsolidationRun, error)
	GetMemoryConsolidationCursor(ctx context.Context, namespace, mode string) (*time.Time, error)
	ListMemoryConsolidationSessions(ctx context.Context, namespace string, after, through time.Time, limit int) ([]SessionMetadata, int, error)
}
