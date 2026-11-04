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
	// location, with partial credit across reference locations.
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
// write "20m" back as "20m0s"; every other field is compared exactly.
// +kubebuilder:validation:XValidation:rule="self.subject == oldSelf.subject && self.cases == oldSelf.cases && self.trials == oldSelf.trials && self.graders == oldSelf.graders && has(self.maxConcurrentTrials) == has(oldSelf.maxConcurrentTrials) && (!has(self.maxConcurrentTrials) || self.maxConcurrentTrials == oldSelf.maxConcurrentTrials) && has(self.trialTimeout) == has(oldSelf.trialTimeout) && (!has(self.trialTimeout) || duration(self.trialTimeout) == duration(oldSelf.trialTimeout)) && has(self.maxInfraErrorPercent) == has(oldSelf.maxInfraErrorPercent) && (!has(self.maxInfraErrorPercent) || self.maxInfraErrorPercent == oldSelf.maxInfraErrorPercent) && has(self.ttlSecondsAfterFinished) == has(oldSelf.ttlSecondsAfterFinished) && (!has(self.ttlSecondsAfterFinished) || self.ttlSecondsAfterFinished == oldSelf.ttlSecondsAfterFinished)",message="EvalRun spec is immutable; create a new EvalRun"
// +kubebuilder:validation:XValidation:rule="!has(self.cases.ids) || size(self.cases.ids) * self.trials <= 500",message="cases × trials must be at most 500"
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
	// long after it finishes. Unset keeps them.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
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

	// CaseSetDigest is the digest of the selected cases.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	CaseSetDigest string `json:"caseSetDigest,omitempty"`
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
	// from it.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions represent the current state of the run.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
