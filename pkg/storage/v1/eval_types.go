package v1

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

// EvalExperimentSchemaVersion is the schema of one stored experiment
// reservation.
const EvalExperimentSchemaVersion = "orka.eval.experiment.v1"

// EvalExperimentRecord reserves one attempt of an improvement experiment: a
// candidate EvalRun paired with a baseline over a held-out case set. Records
// outlive their EvalRuns, so deleting a candidate never returns the attempt
// it spent.
type EvalExperimentRecord struct {
	SchemaVersion   string `json:"schemaVersion"`
	Namespace       string `json:"namespace"`
	CandidateRun    string `json:"candidateRun"`
	CandidateRunUID string `json:"candidateRunUID"`
	BaselineRun     string `json:"baselineRun"`
	BaselineRunUID  string `json:"baselineRunUID"`
	// CaseSetDigest identifies the held-out case set by its case content.
	CaseSetDigest string    `json:"caseSetDigest"`
	GateDigest    string    `json:"gateDigest"`
	Hypothesis    string    `json:"hypothesis"`
	ReservedAt    time.Time `json:"reservedAt"`
}

// EvalExperimentLimits are the attempt budgets a reservation must fit.
type EvalExperimentLimits struct {
	// MaxPerBaseline bounds the candidates of one baseline incarnation.
	MaxPerBaseline int32 `json:"maxPerBaseline"`
	// MaxPerCaseSet bounds the experiments on one held-out case set across
	// every baseline and namespace.
	MaxPerCaseSet int32 `json:"maxPerCaseSet"`
}

// EvalExperimentReservation is the outcome of a reservation.
type EvalExperimentReservation struct {
	// Granted reports whether the candidate holds an attempt: a new one, or
	// the one an identical earlier reservation already holds.
	Granted bool `json:"granted"`
	// BaselineAttempts and CaseSetAttempts count the attempts already held
	// on the baseline and on the case set, including this one when granted.
	BaselineAttempts int32 `json:"baselineAttempts"`
	CaseSetAttempts  int32 `json:"caseSetAttempts"`
}

// EvalExperimentStore reserves experiment attempts. Only the EvalRun
// controller writes them, and nothing deletes them.
type EvalExperimentStore interface {
	// ReserveEvalExperiment atomically counts the attempts already held on
	// the record's baseline and case set and adds the record when both fit
	// the limits. A repeat for the same candidate returns its earlier grant;
	// a different record for that candidate returns ErrDuplicateMismatch.
	ReserveEvalExperiment(ctx context.Context, record *EvalExperimentRecord, limits EvalExperimentLimits) (EvalExperimentReservation, error)
	// GetEvalExperiment returns the reservation a candidate incarnation
	// holds, or ErrNotFound.
	GetEvalExperiment(ctx context.Context, namespace, candidateRunUID string) (*EvalExperimentRecord, error)
}
