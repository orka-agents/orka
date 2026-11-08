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
	// CaseSetDigest is the candidate's case set snapshot digest, for audit.
	CaseSetDigest string `json:"caseSetDigest"`
	// CaseKeys are the sorted, distinct keys of the held-out evidence the
	// candidate runs on. Budgets count per key, so relabeling, reordering
	// or subsetting the same cases never makes them fresh.
	CaseKeys   []string  `json:"caseKeys"`
	GateDigest string    `json:"gateDigest"`
	Hypothesis string    `json:"hypothesis"`
	ReservedAt time.Time `json:"reservedAt"`
}

// EvalExperimentLimits are the attempt budgets a reservation must fit.
type EvalExperimentLimits struct {
	// MaxPerBaseline bounds the candidates of one baseline incarnation.
	MaxPerBaseline int32 `json:"maxPerBaseline"`
	// MaxPerCase bounds the experiments any one held-out case key may be
	// part of, across every baseline and namespace.
	MaxPerCase int32 `json:"maxPerCase"`
}

// EvalExperimentReservation is the outcome of a reservation.
type EvalExperimentReservation struct {
	// Granted reports whether the candidate holds an attempt: a new one, or
	// the one an identical earlier reservation already holds.
	Granted bool `json:"granted"`
	// BaselineAttempts counts the attempts held on the baseline, and
	// CaseAttempts the most held on any one of the record's case keys, both
	// including this one when granted.
	BaselineAttempts int32 `json:"baselineAttempts"`
	CaseAttempts     int32 `json:"caseAttempts"`
}

// EvalExperimentOutcomeSchemaVersion is the schema of one stored experiment
// outcome.
const EvalExperimentOutcomeSchemaVersion = "orka.eval.experimentOutcome.v1"

// EvalDecisionSchemaVersion is the schema of one stored experiment decision.
const EvalDecisionSchemaVersion = "orka.eval.decision.v1"

// Experiment results, as EvalRun status reports them.
const (
	EvalExperimentResultPassed       = "Passed"
	EvalExperimentResultFailed       = "Failed"
	EvalExperimentResultIncomparable = "Incomparable"
)

// Experiment decisions.
const (
	EvalDecisionApproved = "approved"
	EvalDecisionRejected = "rejected"
)

// MaxEvalExperimentOutcomeBytes bounds one encoded experiment outcome.
const MaxEvalExperimentOutcomeBytes = 4 << 20

// MaxEvalDecisionReasonBytes bounds a decision's reason.
const MaxEvalDecisionReasonBytes = 2048

// EvalAgentIdentity identifies one Agent incarnation and generation.
type EvalAgentIdentity struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// EvalDecisionBinding is everything an approval depends on. The controller
// records it with the outcome; an approval binds to it, and stops matching
// when any element changes.
type EvalDecisionBinding struct {
	// CandidateAgent and CandidateDigest are the Agent the candidate
	// reviewed with and the digest of its promotable fields.
	CandidateAgent  EvalAgentIdentity `json:"candidateAgent"`
	CandidateDigest string            `json:"candidateDigest"`
	// BaselineAgent and BaselineDigest are the monitor's own reviewer as the
	// baseline ran it: the rollback target.
	BaselineAgent        EvalAgentIdentity `json:"baselineAgent"`
	BaselineDigest       string            `json:"baselineDigest"`
	MonitorUID           string            `json:"monitorUID"`
	MonitorGeneration    int64             `json:"monitorGeneration"`
	PromptTemplateDigest string            `json:"promptTemplateDigest"`
	CaseSetDigest        string            `json:"caseSetDigest"`
	GraderSetDigest      string            `json:"graderSetDigest"`
	GateDigest           string            `json:"gateDigest"`
}

// EvalExperimentTrialOutcome is one graded trial of an experiment arm.
type EvalExperimentTrialOutcome struct {
	CaseID     string                `json:"caseID"`
	Trial      int32                 `json:"trial"`
	Outcome    string                `json:"outcome"`
	InfraClass string                `json:"infraClass,omitempty"`
	Grades     []EvalExperimentGrade `json:"grades,omitempty"`
}

// EvalExperimentGrade is one grader's status for a trial.
type EvalExperimentGrade struct {
	Grader string `json:"grader"`
	Status string `json:"status"`
}

// EvalExperimentArmOutcome is one arm of a decided experiment.
type EvalExperimentArmOutcome struct {
	Run                string            `json:"run"`
	RunUID             string            `json:"runUID"`
	Agent              EvalAgentIdentity `json:"agent"`
	Model              string            `json:"model,omitempty"`
	SystemPromptDigest string            `json:"systemPromptDigest,omitempty"`
	RuntimeImageDigest string            `json:"runtimeImageDigest,omitempty"`
	Planned            int32             `json:"planned"`
	Completed          int32             `json:"completed"`
	Passed             int32             `json:"passed"`
	Failed             int32             `json:"failed"`
	// InfraError counts trials excluded from scores.
	InfraError   int32 `json:"infraError"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	// Trials hold grader statuses only: no review text or case content.
	Trials []EvalExperimentTrialOutcome `json:"trials,omitempty"`
}

// EvalExperimentTargetOutcome compares the target result of the two arms.
type EvalExperimentTargetOutcome struct {
	BaselinePassedTrials  int32  `json:"baselinePassedTrials"`
	BaselineTrials        int32  `json:"baselineTrials"`
	CandidatePassedTrials int32  `json:"candidatePassedTrials"`
	CandidateTrials       int32  `json:"candidateTrials"`
	ImprovedPairs         int32  `json:"improvedPairs"`
	RegressedPairs        int32  `json:"regressedPairs"`
	ImprovementPoints     string `json:"improvementPoints"`
	PValue                string `json:"pValue"`
}

// EvalExperimentOutcome is the evaluator's verdict on one experiment,
// written once when it is decided and kept after both EvalRuns are deleted.
type EvalExperimentOutcome struct {
	SchemaVersion   string                       `json:"schemaVersion"`
	Namespace       string                       `json:"namespace"`
	CandidateRun    string                       `json:"candidateRun"`
	CandidateRunUID string                       `json:"candidateRunUID"`
	Result          string                       `json:"result"`
	Reasons         []string                     `json:"reasons,omitempty"`
	Target          *EvalExperimentTargetOutcome `json:"target,omitempty"`
	// Binding is nil when the runs could not be compared.
	Binding   *EvalDecisionBinding     `json:"binding,omitempty"`
	Baseline  EvalExperimentArmOutcome `json:"baseline"`
	Candidate EvalExperimentArmOutcome `json:"candidate"`
	DecidedAt time.Time                `json:"decidedAt"`
}

// EvalDecisionActor is the person who decided.
type EvalDecisionActor struct {
	Username string `json:"username"`
	UID      string `json:"uid,omitempty"`
	// Groups are the promoter groups that admitted the actor.
	Groups []string `json:"groups,omitempty"`
}

// EvalDecisionRecord approves or rejects one decided experiment. Records are
// only appended; the latest one for an experiment is its effective decision.
type EvalDecisionRecord struct {
	SchemaVersion string `json:"schemaVersion"`
	// ID is chosen by the writer so a retried append is recognized.
	ID              string            `json:"id"`
	Namespace       string            `json:"namespace"`
	CandidateRun    string            `json:"candidateRun"`
	CandidateRunUID string            `json:"candidateRunUID"`
	Decision        string            `json:"decision"`
	Reason          string            `json:"reason"`
	Actor           EvalDecisionActor `json:"actor"`
	// Binding is what an approval binds to: the outcome's binding at the
	// time of the decision. A rejection may have none.
	Binding   *EvalDecisionBinding `json:"binding,omitempty"`
	DecidedAt time.Time            `json:"decidedAt"`
}

// EvalExperimentEntry is one experiment with everything recorded about it.
type EvalExperimentEntry struct {
	Experiment EvalExperimentRecord   `json:"experiment"`
	Outcome    *EvalExperimentOutcome `json:"outcome,omitempty"`
	// Decisions are ordered oldest first.
	Decisions []EvalDecisionRecord `json:"decisions,omitempty"`
}

// EvalExperimentStore keeps experiment attempts, outcomes and decisions.
// Only the EvalRun controller and its API write them, and nothing deletes
// them.
type EvalExperimentStore interface {
	// ReserveEvalExperiment atomically counts the attempts already held on
	// the record's baseline and on each of its case keys and adds the
	// record when all fit the limits. A repeat for the same candidate returns its earlier grant;
	// a different record for that candidate returns ErrDuplicateMismatch.
	ReserveEvalExperiment(ctx context.Context, record *EvalExperimentRecord, limits EvalExperimentLimits) (EvalExperimentReservation, error)
	// GetEvalExperiment returns the reservation a candidate incarnation
	// holds, or ErrNotFound.
	GetEvalExperiment(ctx context.Context, namespace, candidateRunUID string) (*EvalExperimentRecord, error)
	// RecordEvalExperimentOutcome stores a reserved experiment's outcome
	// once and returns the stored outcome: the first one written wins, so a
	// retried or repeated decision never changes it. It returns ErrNotFound
	// when the candidate holds no reservation.
	RecordEvalExperimentOutcome(ctx context.Context, outcome *EvalExperimentOutcome) (*EvalExperimentOutcome, error)
	// AppendEvalDecision appends a decision on a decided experiment. It
	// returns ErrNotFound without an outcome, and ErrConflict for an
	// approval of an outcome that did not pass or whose binding differs. A
	// repeat of the same ID is a no-op, and a different record under that
	// ID returns ErrDuplicateMismatch.
	AppendEvalDecision(ctx context.Context, decision *EvalDecisionRecord) error
	// ListEvalExperiments returns a namespace's experiments, or the one a
	// candidate incarnation holds when candidateRunUID is set, ordered by
	// reservation time.
	ListEvalExperiments(ctx context.Context, namespace, candidateRunUID string) ([]EvalExperimentEntry, error)
}
