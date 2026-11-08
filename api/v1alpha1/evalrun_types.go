/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// EvalCaseSchemaVersion is the schema of one eval case line.
	EvalCaseSchemaVersion = "orka.eval.case.v1"

	// EvalRunMaxTrials is the largest number of trials per case.
	EvalRunMaxTrials = 20
	// EvalRunMaxCaseTrials caps cases × trials for one run. Admission checks
	// it when spec.cases.ids lists the cases; otherwise the controller checks
	// it against the case set it loads.
	EvalRunMaxCaseTrials = 500
	// EvalRunMaxResults caps status.results: one result per grader overall
	// and per distinct case tag.
	EvalRunMaxResults = 256
	// EvalGraderCount is the number of EvalGraderName values.
	EvalGraderCount = 4
)

// EvalSubjectType names what an EvalRun evaluates. Each value has one Subject
// implementation; an unknown value is rejected at admission.
// +kubebuilder:validation:Enum=repositoryMonitorReview
type EvalSubjectType string

const (
	// EvalSubjectRepositoryMonitorReview evaluates the RepositoryMonitor pull
	// request review path.
	EvalSubjectRepositoryMonitorReview EvalSubjectType = "repositoryMonitorReview"
)

// EvalGraderName names one code grader. Each value has one Grader
// implementation; an unknown value is rejected at admission.
// +kubebuilder:validation:Enum=verdict;localization;contract;honesty
type EvalGraderName string

const (
	// EvalGraderVerdict passes when the verdict matches the case reference.
	EvalGraderVerdict EvalGraderName = "verdict"
	// EvalGraderLocalization passes when a finding falls within a reference
	// location, with partial credit across reference locations. A finding on
	// a line the change removed counts where the removal happened.
	EvalGraderLocalization EvalGraderName = "localization"
	// EvalGraderContract passes when the result honors the review contract.
	EvalGraderContract EvalGraderName = "contract"
	// EvalGraderHonesty passes unless the review claims passing tests without
	// a validation image.
	EvalGraderHonesty EvalGraderName = "honesty"
)

// EvalRunPhase is the lifecycle phase of an EvalRun.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Invalid;Failed
type EvalRunPhase string

const (
	EvalRunPhasePending   EvalRunPhase = "Pending"
	EvalRunPhaseRunning   EvalRunPhase = "Running"
	EvalRunPhaseSucceeded EvalRunPhase = "Succeeded"
	// EvalRunPhaseInvalid means the results must not be used: the snapshot
	// changed mid-run or infra errors exceeded maxInfraErrorPercent.
	EvalRunPhaseInvalid EvalRunPhase = "Invalid"
	EvalRunPhaseFailed  EvalRunPhase = "Failed"
)

// EvalObjectReference names an object in the EvalRun's namespace.
type EvalObjectReference struct {
	// Name is the object name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// EvalConfigMapKeyRef names a key of a ConfigMap in the EvalRun's namespace.
type EvalConfigMapKeyRef struct {
	// Name is the ConfigMap name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Key is the ConfigMap data key holding orka.eval.case.v1 JSONL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// EvalRepositoryMonitorReviewSubject evaluates the review path of a
// RepositoryMonitor. The monitor supplies the reviewer Agent and review
// settings; trials never receive its forge credentials.
type EvalRepositoryMonitorReviewSubject struct {
	// MonitorRef names a RepositoryMonitor in the EvalRun's namespace.
	MonitorRef EvalObjectReference `json:"monitorRef"`

	// AgentRef names an Agent in the EvalRun's namespace to review with
	// instead of the monitor's reviewer, such as a candidate in an
	// experiment. The monitor still supplies every other review setting.
	// +optional
	AgentRef *EvalObjectReference `json:"agentRef,omitempty"`
}

// EvalExperimentTarget names the result an improvement experiment must raise:
// one grader, overall or on one case tag.
type EvalExperimentTarget struct {
	// Grader is the grader whose pass@1 must improve. It must be one of the
	// run's graders.
	Grader EvalGraderName `json:"grader"`

	// Tag limits the target to cases with this tag. Empty means every case.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`
	// +optional
	Tag string `json:"tag,omitempty"`
}

// EvalExperiment makes this run the candidate arm of an improvement
// experiment paired with a baseline run over the same cases and trials.
type EvalExperiment struct {
	// Hypothesis states in one sentence what the candidate should improve.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Hypothesis string `json:"hypothesis"`

	// BaselineRunRef names the baseline EvalRun in this namespace: the same
	// monitor, cases, trials and graders with the stable reviewer.
	BaselineRunRef EvalObjectReference `json:"baselineRunRef"`

	// Target is the result the candidate must raise.
	Target EvalExperimentTarget `json:"target"`
}

// EvalSubject selects what an EvalRun evaluates. Exactly the member named by
// type is set.
// +kubebuilder:validation:XValidation:rule="has(self.repositoryMonitorReview) == (self.type == 'repositoryMonitorReview')",message="subject.repositoryMonitorReview is required for, and only allowed with, type repositoryMonitorReview"
type EvalSubject struct {
	// Type names the subject.
	Type EvalSubjectType `json:"type"`

	// RepositoryMonitorReview configures the repositoryMonitorReview subject.
	// +optional
	RepositoryMonitorReview *EvalRepositoryMonitorReviewSubject `json:"repositoryMonitorReview,omitempty"`
}

// EvalCaseSource selects the orka.eval.case.v1 cases to run. Exactly one
// source is set; configMapRef is the only source today.
// +kubebuilder:validation:XValidation:rule="has(self.configMapRef)",message="cases.configMapRef is required"
type EvalCaseSource struct {
	// ConfigMapRef names the ConfigMap key holding the cases, one JSON object
	// per line.
	// +optional
	ConfigMapRef *EvalConfigMapKeyRef `json:"configMapRef,omitempty"`

	// IDs selects a subset of the cases by id. Empty runs every case.
	// +kubebuilder:validation:MaxItems=500
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`
	// +listType=set
	// +optional
	IDs []string `json:"ids,omitempty"`
}

// EvalRunSpec defines an eval run. It is immutable; create a new EvalRun to
// run again. trialTimeout is compared as a duration because typed clients
// write "20m" back as "20m0s", and an empty cases.ids equals an omitted one
// because typed clients drop the empty list; every other field is compared
// exactly.
// +kubebuilder:validation:XValidation:rule="self.subject == oldSelf.subject && self.cases.configMapRef == oldSelf.cases.configMapRef && (has(self.cases.ids) ? self.cases.ids : []) == (has(oldSelf.cases.ids) ? oldSelf.cases.ids : []) && self.trials == oldSelf.trials && self.graders == oldSelf.graders && has(self.maxConcurrentTrials) == has(oldSelf.maxConcurrentTrials) && (!has(self.maxConcurrentTrials) || self.maxConcurrentTrials == oldSelf.maxConcurrentTrials) && has(self.trialTimeout) == has(oldSelf.trialTimeout) && (!has(self.trialTimeout) || duration(self.trialTimeout) == duration(oldSelf.trialTimeout)) && has(self.maxInfraErrorPercent) == has(oldSelf.maxInfraErrorPercent) && (!has(self.maxInfraErrorPercent) || self.maxInfraErrorPercent == oldSelf.maxInfraErrorPercent) && has(self.ttlSecondsAfterFinished) == has(oldSelf.ttlSecondsAfterFinished) && (!has(self.ttlSecondsAfterFinished) || self.ttlSecondsAfterFinished == oldSelf.ttlSecondsAfterFinished) && has(self.experiment) == has(oldSelf.experiment) && (!has(self.experiment) || self.experiment == oldSelf.experiment)",message="EvalRun spec is immutable; create a new EvalRun"
// +kubebuilder:validation:XValidation:rule="!has(self.cases.ids) || size(self.cases.ids) * self.trials <= 500",message="cases × trials must be at most 500"
// +kubebuilder:validation:XValidation:rule="!has(self.experiment) || self.experiment.target.grader in self.graders",message="experiment.target.grader must be one of graders"
type EvalRunSpec struct {
	// Subject selects what is evaluated.
	Subject EvalSubject `json:"subject"`

	// Cases selects the cases to run.
	Cases EvalCaseSource `json:"cases"`

	// Trials is the number of trials per case (k in pass@k and pass^k).
	// cases × trials is at most 500.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	Trials int32 `json:"trials"`

	// MaxConcurrentTrials bounds how many trials run at once.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentTrials int32 `json:"maxConcurrentTrials,omitempty"`

	// TrialTimeout bounds one trial, from Task creation to its result.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('2h')",message="trialTimeout must be from 1m to 2h"
	// +kubebuilder:default="20m"
	// +optional
	TrialTimeout *metav1.Duration `json:"trialTimeout,omitempty"`

	// Graders lists the code graders applied to every trial.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	Graders []EvalGraderName `json:"graders"`

	// MaxInfraErrorPercent is the share of trials that may end in an infra
	// error before the run is Invalid. Infra errors are excluded from scores.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	// +optional
	MaxInfraErrorPercent *int32 `json:"maxInfraErrorPercent,omitempty"`

	// TTLSecondsAfterFinished deletes the EvalRun and its trial records this
	// long after it finishes, or, for an experiment candidate, after its
	// experiment is decided. Unset keeps them.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`

	// Experiment makes this run the candidate arm of an improvement
	// experiment. The controller compares it with the baseline run once both
	// finish and records the gate's result in status.experiment.
	// +optional
	Experiment *EvalExperiment `json:"experiment,omitempty"`
}

// EvalObjectIdentity records the exact identity of an object a run depends on.
type EvalObjectIdentity struct {
	// Name is the object name.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Name string `json:"name,omitempty"`

	// UID is the object UID.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// Generation is the object generation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Generation int64 `json:"generation,omitempty"`
}

// EvalRunSnapshot freezes, at start, the identities that decide outcomes. A
// change to any of them mid-run makes the run Invalid.
type EvalRunSnapshot struct {
	// MonitorUID is the RepositoryMonitor UID, so a recreated monitor with
	// the same name and generation is still a change.
	// +optional
	MonitorUID types.UID `json:"monitorUID,omitempty"`

	// MonitorGeneration is the RepositoryMonitor generation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MonitorGeneration int64 `json:"monitorGeneration,omitempty"`

	// ReviewerAgent is the reviewer Agent identity.
	// +optional
	ReviewerAgent *EvalObjectIdentity `json:"reviewerAgent,omitempty"`

	// RuntimeImageDigest is the reviewer runtime image digest.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	RuntimeImageDigest string `json:"runtimeImageDigest,omitempty"`

	// Model is the model route the reviewer uses.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Model string `json:"model,omitempty"`

	// PromptTemplateDigest is the digest of the review prompt template.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	PromptTemplateDigest string `json:"promptTemplateDigest,omitempty"`

	// ReviewerSystemPromptDigest is the digest of the reviewer's system
	// prompt as trials receive it, after resolving a ConfigMap source, so an
	// edit to a ConfigMap-backed prompt is a change.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	ReviewerSystemPromptDigest string `json:"reviewerSystemPromptDigest,omitempty"`

	// CaseSetDigest is the digest of the selected cases.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	CaseSetDigest string `json:"caseSetDigest,omitempty"`

	// GraderSetDigest is the digest of the graders' names and versions.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	GraderSetDigest string `json:"graderSetDigest,omitempty"`

	// TrialPlanDigest is the digest of the ordered (case id, trial) plan.
	// Two runs with the same digest assign the same trials.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	TrialPlanDigest string `json:"trialPlanDigest,omitempty"`
}

// EvalRunCounts counts trials. Infra errors are neither passed nor failed.
type EvalRunCounts struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	Planned int32 `json:"planned,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Completed int32 `json:"completed,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Passed int32 `json:"passed,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	InfraError int32 `json:"infraError,omitempty"`
}

// EvalGraderResult aggregates one grader over the cases with one tag. Values
// are integers only; clients derive pass@1 (passedTrials / trials), pass@k
// (casesAnyPassed / cases) and pass^k (casesAllPassed / cases).
type EvalGraderResult struct {
	// Grader is the grader name.
	Grader EvalGraderName `json:"grader"`

	// Tag is the case tag aggregated; empty aggregates every case.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Tag string `json:"tag,omitempty"`

	// Trials is the number of graded trials, excluding infra errors and
	// trials the grader does not apply to.
	// +kubebuilder:validation:Minimum=0
	Trials int32 `json:"trials"`

	// PassedTrials is the number of graded trials that passed.
	// +kubebuilder:validation:Minimum=0
	PassedTrials int32 `json:"passedTrials"`

	// Cases is the number of cases with at least one graded trial.
	// +kubebuilder:validation:Minimum=0
	Cases int32 `json:"cases"`

	// CasesAnyPassed is the number of cases with at least one passed trial.
	// +kubebuilder:validation:Minimum=0
	CasesAnyPassed int32 `json:"casesAnyPassed"`

	// CasesAllPassed is the number of cases whose graded trials all passed.
	// +kubebuilder:validation:Minimum=0
	CasesAllPassed int32 `json:"casesAllPassed"`
}

// EvalRunStatus is the observed state of an EvalRun. Per-trial records live in
// Orka's durable store, not here.
type EvalRunStatus struct {
	// Phase is the run phase.
	// +optional
	Phase EvalRunPhase `json:"phase,omitempty"`

	// Snapshot is frozen when the run starts.
	// +optional
	Snapshot *EvalRunSnapshot `json:"snapshot,omitempty"`

	// Counts counts trials.
	// +optional
	Counts EvalRunCounts `json:"counts,omitempty"`

	// Results aggregates every grader overall and per case tag. A case file
	// has at most 63 distinct tags, so this fits every grader.
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	// +optional
	Results []EvalGraderResult `json:"results,omitempty"`

	// SuspectCases lists cases that passed no trial. They are presumed broken
	// until a transcript shows otherwise.
	// +kubebuilder:validation:MaxItems=500
	// +kubebuilder:validation:items:MaxLength=63
	// +listType=set
	// +optional
	SuspectCases []string `json:"suspectCases,omitempty"`

	// StartTime is when the snapshot was frozen.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run finished; ttlSecondsAfterFinished counts
	// from it, or from status.experiment.decidedAt for a candidate.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Experiment is the experiment gate's result for a candidate run.
	// +optional
	Experiment *EvalExperimentStatus `json:"experiment,omitempty"`

	// Conditions represent the current state of the run.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EvalExperimentResult is the experiment gate's verdict.
// +kubebuilder:validation:Enum=Pending;Passed;Failed;Incomparable
type EvalExperimentResult string

const (
	// EvalExperimentPending waits for the candidate and baseline to finish.
	EvalExperimentPending EvalExperimentResult = "Pending"
	// EvalExperimentPassed means the candidate cleared every gate check.
	EvalExperimentPassed EvalExperimentResult = "Passed"
	// EvalExperimentFailed means the runs were comparable and the candidate
	// failed a gate check.
	EvalExperimentFailed EvalExperimentResult = "Failed"
	// EvalExperimentIncomparable means the two runs differ in something other
	// than the candidate's promotable fields, or one of them did not succeed.
	EvalExperimentIncomparable EvalExperimentResult = "Incomparable"
)

// EvalExperimentGate is the protected set's gate as the experiment froze it.
type EvalExperimentGate struct {
	MaxInfraErrorPercent        int32 `json:"maxInfraErrorPercent"`
	MaxCandidatesPerBaseline    int32 `json:"maxCandidatesPerBaseline"`
	MaxExperimentsPerHeldOutSet int32 `json:"maxExperimentsPerHeldOutSet"`
	MinTargetImprovementPoints  int32 `json:"minTargetImprovementPoints"`
	// Significance is the one-sided test level before it is divided by
	// MaxExperimentsPerHeldOutSet, as a decimal string such as "0.05".
	// +kubebuilder:validation:MaxLength=32
	Significance string `json:"significance"`
}

// EvalExperimentTargetResult compares the target result of the two arms.
type EvalExperimentTargetResult struct {
	BaselinePassedTrials  int32 `json:"baselinePassedTrials"`
	BaselineTrials        int32 `json:"baselineTrials"`
	CandidatePassedTrials int32 `json:"candidatePassedTrials"`
	CandidateTrials       int32 `json:"candidateTrials"`
	// ImprovedPairs counts paired trials the baseline failed and the
	// candidate passed; RegressedPairs counts the reverse.
	ImprovedPairs  int32 `json:"improvedPairs"`
	RegressedPairs int32 `json:"regressedPairs"`
	// ImprovementPoints is candidate minus baseline pass@1 in percentage
	// points, as a decimal string.
	// +kubebuilder:validation:MaxLength=32
	ImprovementPoints string `json:"improvementPoints"`
	// PValue is the one-sided exact sign test over the paired trials, as a
	// decimal string.
	// +kubebuilder:validation:MaxLength=32
	PValue string `json:"pValue"`
}

// EvalExperimentStatus records an experiment's pairing and gate result.
type EvalExperimentStatus struct {
	// Result is the gate's verdict.
	Result EvalExperimentResult `json:"result"`

	// Reasons explain a result other than Passed, and the checks a Passed
	// result cleared.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Reasons []string `json:"reasons,omitempty"`

	// BaselineRun is the baseline run's identity, frozen when the candidate
	// started, so a recreated baseline is not mistaken for it.
	// +optional
	BaselineRun *EvalObjectIdentity `json:"baselineRun,omitempty"`

	// CandidateDigest digests the candidate's promotable fields: the
	// reviewer Agent's identity, resolved system prompt, model and runtime
	// image.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	CandidateDigest string `json:"candidateDigest,omitempty"`

	// GateDigest digests the frozen gate.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	GateDigest string `json:"gateDigest,omitempty"`

	// DecidedAt is when the experiment left Pending.
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`

	// Gate is the gate the experiment was judged by.
	// +optional
	Gate *EvalExperimentGate `json:"gate,omitempty"`

	// Target compares the target result of the two arms.
	// +optional
	Target *EvalExperimentTargetResult `json:"target,omitempty"`
}

// EvalRun runs a set of cases through an Orka agent path several times and
// grades every trial with code.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Planned",type=integer,JSONPath=`.status.counts.planned`
// +kubebuilder:printcolumn:name="Completed",type=integer,JSONPath=`.status.counts.completed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EvalRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   EvalRunSpec   `json:"spec"`
	Status EvalRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EvalRunList contains a list of EvalRun.
type EvalRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvalRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EvalRun{}, &EvalRunList{})
}
