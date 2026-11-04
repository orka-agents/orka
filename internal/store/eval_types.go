package store

import (
	"context"
	"time"
)

// EvalTrialSchemaVersion is the schema of one stored eval trial record.
const EvalTrialSchemaVersion = "orka.eval.trial.v1"

// Eval trial outcomes. Infra errors are excluded from scores.
const (
	EvalTrialOutcomePass       = "pass"
	EvalTrialOutcomeFail       = "fail"
	EvalTrialOutcomeInfraError = "infraError"
)

// MaxEvalTrialRecordBytes bounds one encoded trial record.
const MaxEvalTrialRecordBytes = 64 << 10

// EvalTrialRecord is the graded result of one EvalRun trial. It holds
// identities, digests, grader results and counters only: never credentials,
// transaction tokens or transcripts. Transcripts stay with the trial Task's
// execution events.
type EvalTrialRecord struct {
	SchemaVersion string `json:"schemaVersion"`
	Namespace     string `json:"namespace"`
	EvalRun       string `json:"evalRun"`
	EvalRunUID    string `json:"evalRunUID"`
	CaseID        string `json:"caseID"`
	// Trial is the one-based trial number within the case.
	Trial    int32  `json:"trial"`
	TaskName string `json:"taskName"`
	TaskUID  string `json:"taskUID,omitempty"`
	// RuntimeSessionUID is the trial's own runtime Session. Trials never
	// share or resume a Session.
	RuntimeSessionUID string `json:"runtimeSessionUID,omitempty"`

	// Outcome is pass, fail or infraError.
	Outcome    string `json:"outcome"`
	InfraClass string `json:"infraClass,omitempty"`
	// Verdict is the verdict production would have recorded, after its gates.
	Verdict string `json:"verdict,omitempty"`
	// ReviewerVerdict is the verdict the reviewer reported, before gates.
	ReviewerVerdict string           `json:"reviewerVerdict,omitempty"`
	SkipReason      string           `json:"skipReason,omitempty"`
	GateReason      string           `json:"gateReason,omitempty"`
	Grades          []EvalTrialGrade `json:"grades,omitempty"`

	InputTokens  *int64 `json:"inputTokens,omitempty"`
	OutputTokens *int64 `json:"outputTokens,omitempty"`
	// ToolCalls counts the tool calls the trial started.
	ToolCalls *int64 `json:"toolCalls,omitempty"`
	// LatencyMillis runs from Task creation to its result.
	LatencyMillis *int64 `json:"latencyMillis,omitempty"`

	Snapshot EvalTrialSnapshot `json:"snapshot"`

	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt time.Time  `json:"completedAt"`
}

// EvalTrialGrade is one grader's result for one trial.
type EvalTrialGrade struct {
	Grader string  `json:"grader"`
	Status string  `json:"status"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason,omitempty"`
}

// EvalTrialSnapshot copies the run snapshot digests the trial was graded
// under, so a record stays comparable after its EvalRun is gone.
type EvalTrialSnapshot struct {
	MonitorUID           string `json:"monitorUID,omitempty"`
	MonitorGeneration    int64  `json:"monitorGeneration,omitempty"`
	ReviewerAgentUID     string `json:"reviewerAgentUID,omitempty"`
	ReviewerGeneration   int64  `json:"reviewerGeneration,omitempty"`
	RuntimeImageDigest   string `json:"runtimeImageDigest,omitempty"`
	Model                string `json:"model,omitempty"`
	PromptTemplateDigest string `json:"promptTemplateDigest,omitempty"`
	// ReviewerSystemPromptDigest is the digest of the reviewer's resolved
	// system prompt.
	ReviewerSystemPromptDigest string `json:"reviewerSystemPromptDigest,omitempty"`
	CaseSetDigest              string `json:"caseSetDigest,omitempty"`
	GraderSetDigest            string `json:"graderSetDigest,omitempty"`
	TrialPlanDigest            string `json:"trialPlanDigest,omitempty"`
}

// EvalTrialStore persists eval trial records. Only the EvalRun controller
// writes them.
type EvalTrialStore interface {
	// CreateEvalTrial records one completed trial. Records are immutable: an
	// identical repeat succeeds, a different record for the same trial
	// returns ErrDuplicateMismatch.
	CreateEvalTrial(ctx context.Context, record *EvalTrialRecord) error
	// ListEvalTrials returns the records of one EvalRun incarnation ordered
	// by case and trial.
	ListEvalTrials(ctx context.Context, namespace, evalRun, evalRunUID string) ([]EvalTrialRecord, error)
	// DeleteEvalTrials deletes the records of every incarnation of an
	// EvalRun name.
	DeleteEvalTrials(ctx context.Context, namespace, evalRun string) error
}
