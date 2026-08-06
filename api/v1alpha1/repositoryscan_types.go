/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PolicyConfigMapKeyRef references a key in a same-namespace ConfigMap that contains scanner policy text.
type PolicyConfigMapKeyRef struct {
	// Name is the ConfigMap name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the ConfigMap data key. Defaults to "policy" when omitted.
	// +optional
	Key string `json:"key,omitempty"`
}

// RepositoryScanSpec defines the desired state of RepositoryScan.
type RepositoryScanSpec struct {
	// Provider is the source control provider. GitHub is the only supported v1 provider.
	// +kubebuilder:validation:Enum=github
	// +kubebuilder:default=github
	// +optional
	Provider string `json:"provider,omitempty"`

	// RepoURL is the repository URL to scan.
	// +kubebuilder:validation:Required
	RepoURL string `json:"repoURL"`

	// Owner is the repository owner or organization.
	// +optional
	Owner string `json:"owner,omitempty"`

	// Repository is the repository name.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Branch is the base branch to scan. Defaults to the literal main branch when omitted.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Ref is a specific git ref, tag, or commit SHA to checkout for scan tasks.
	// +optional
	Ref string `json:"ref,omitempty"`

	// SubPath scopes scanning to a subdirectory in a monorepo.
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// GitSecretRef references git credentials for private repositories.
	// +optional
	GitSecretRef *corev1.LocalObjectReference `json:"gitSecretRef,omitempty"`

	// ForkRepo is the writable fork repository URL used for patch proposals.
	// +optional
	ForkRepo string `json:"forkRepo,omitempty"`

	// PRBaseBranch is the pull request base branch for remediation.
	// +optional
	PRBaseBranch string `json:"prBaseBranch,omitempty"`

	// Schedule is the cron expression for incremental scans.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// TimeZone is the IANA time zone for the schedule.
	// +optional
	TimeZone *string `json:"timeZone,omitempty"`

	// HistoryDays controls how far back the initial scan should inspect repository history.
	// +optional
	HistoryDays *int32 `json:"historyDays,omitempty"`

	// ValidationMode controls how aggressively findings are validated.
	// +kubebuilder:validation:Enum=off;light;full
	// +kubebuilder:default=light
	// +optional
	ValidationMode string `json:"validationMode,omitempty"`

	// ValidationMaxFindingsPerRun bounds automatic validation tasks for light mode.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ValidationMaxFindingsPerRun *int32 `json:"validationMaxFindingsPerRun,omitempty"`

	// ValidationMinSeverity is the minimum severity eligible for automatic validation.
	// +kubebuilder:validation:Enum=critical;high;medium;low
	// +optional
	ValidationMinSeverity string `json:"validationMinSeverity,omitempty"`

	// ValidationMinConfidence is the minimum confidence eligible for automatic validation.
	// +kubebuilder:validation:Enum=high;medium;low
	// +optional
	ValidationMinConfidence string `json:"validationMinConfidence,omitempty"`

	// CustomScanInstructionsRef references additive scanner instructions in a same-namespace ConfigMap.
	// +optional
	CustomScanInstructionsRef *PolicyConfigMapKeyRef `json:"customScanInstructionsRef,omitempty"`

	// FalsePositivePolicyRef references additive false-positive policy in a same-namespace ConfigMap.
	// +optional
	FalsePositivePolicyRef *PolicyConfigMapKeyRef `json:"falsePositivePolicyRef,omitempty"`

	// AnalysisAgentRef is the agent used for scan runs.
	AnalysisAgentRef AgentReference `json:"analysisAgentRef"`

	// PatchAgentRef is the agent used for patch proposal runs.
	// +optional
	PatchAgentRef *AgentReference `json:"patchAgentRef,omitempty"`

	// MaxFindingsPerRun bounds scan output volume.
	// +optional
	MaxFindingsPerRun *int32 `json:"maxFindingsPerRun,omitempty"`

	// AnalysisIsolationPolicy controls whether repository-dependent analysis uses
	// Orka's verified read-only/runtime-auth-only execution profile. Empty is
	// legacy for backward compatibility.
	// +kubebuilder:validation:Enum=legacy;prefer-hardened;require-hardened
	// +optional
	AnalysisIsolationPolicy string `json:"analysisIsolationPolicy,omitempty"`

	// CompletionPolicy controls whether a successful discovery pipeline is
	// sufficient or whether full, verified assurance is required. Empty is
	// discovery for backward compatibility.
	// +kubebuilder:validation:Enum=discovery;validated
	// +optional
	CompletionPolicy string `json:"completionPolicy,omitempty"`

	// IncrementalBaselinePolicy selects which durable watermark may seed an
	// incremental scan. Empty remains legacy-discovery for existing resources.
	// +kubebuilder:validation:Enum=legacy-discovery;complete-coverage;assurance-qualified
	// +optional
	IncrementalBaselinePolicy string `json:"incrementalBaselinePolicy,omitempty"`

	// DeepScan configures opt-in bounded repeated discovery. The controller-wide
	// deepScanEnabled gate must also be enabled before this can dispatch work.
	// +optional
	DeepScan *RepositoryDeepScanSpec `json:"deepScan,omitempty"`

	// Suspend pauses scheduled incremental scans.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`
}

// RepositoryDeepScanSpec bounds opt-in repeated discovery.
type RepositoryDeepScanSpec struct {
	// Enabled opts this RepositoryScan into repeated discovery.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// MaxPasses is the hard discovery pass cap.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	MaxPasses *int32 `json:"maxPasses,omitempty"`

	// MaxWorkers is the hard concurrent discovery worker cap.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +optional
	MaxWorkers *int32 `json:"maxWorkers,omitempty"`

	// MaxCandidates is the hard accepted plus rejected candidate cap.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	MaxCandidates *int32 `json:"maxCandidates,omitempty"`

	// NoNewCandidatesThreshold stops after this many consecutive passes without
	// a new controller-derived canonical candidate.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +optional
	NoNewCandidatesThreshold *int32 `json:"noNewCandidatesThreshold,omitempty"`

	// Deadline is the hard wall-clock budget for repeated discovery.
	// +optional
	Deadline *metav1.Duration `json:"deadline,omitempty"`
}

// FindingCountsStatus summarizes open findings by severity.
type FindingCountsStatus struct {
	Total    int32 `json:"total,omitempty"`
	Critical int32 `json:"critical,omitempty"`
	High     int32 `json:"high,omitempty"`
	Medium   int32 `json:"medium,omitempty"`
	Low      int32 `json:"low,omitempty"`
}

// RepositoryScanQualityStatus is a bounded projection of run-scoped quality.
// Detailed receipts, paths, candidates, and evidence remain in the security store.
type RepositoryScanQualityStatus struct {
	// SchemaVersion identifies the quality reducer contract.
	// +optional
	SchemaVersion int32 `json:"schemaVersion,omitempty"`

	// ObservedRepositoryScanUID and Generation bind the projection to the object incarnation.
	// +optional
	ObservedRepositoryScanUID string `json:"observedRepositoryScanUID,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +kubebuilder:validation:Enum=not_started;pending;complete;partial;failed;unknown
	// +optional
	InventoryCoverageStatus string `json:"inventoryCoverageStatus,omitempty"`
	// +kubebuilder:validation:Enum=not_started;pending;complete;partial;failed;unknown
	// +optional
	CandidateCoverageStatus string `json:"candidateCoverageStatus,omitempty"`
	// +kubebuilder:validation:Enum=not_started;pending;complete;partial;failed;unknown
	// +optional
	CoverageStatus string `json:"coverageStatus,omitempty"`
	// +kubebuilder:validation:Enum=off;sampled;all;unknown
	// +optional
	ValidationScope string `json:"validationScope,omitempty"`
	// +kubebuilder:validation:Enum=not_started;pending;complete;partial;failed;deferred;unknown
	// +optional
	ValidationExecution string `json:"validationExecution,omitempty"`
	// +kubebuilder:validation:Enum=not_started;pending;complete;partial;failed;deferred;unknown
	// +optional
	AttackPathExecution string `json:"attackPathExecution,omitempty"`
	// +kubebuilder:validation:Enum=delivered;tool-observed;brokered;unverified
	// +optional
	AnalysisAttestationLevel string `json:"analysisAttestationLevel,omitempty"`
	// +kubebuilder:validation:Enum=pending;verified;unverified;mismatch
	// +optional
	TargetVerification string `json:"targetVerification,omitempty"`
	// +kubebuilder:validation:Enum=not_started;draft;sealing;sealed;retryable_failed;failed
	// +optional
	BundleStatus string `json:"bundleStatus,omitempty"`
	// +kubebuilder:validation:Enum=verified;admitted;delegated;legacy-unverified;revoked;expired
	// +optional
	AuthorizationStatus string `json:"authorizationStatus,omitempty"`
	// +kubebuilder:validation:Enum=legacy;hardened;fallback;unverified;failed
	// +optional
	IsolationStatus string `json:"isolationStatus,omitempty"`

	// ReasonCodes are stable low-cardinality degradation reasons.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	ReasonCodes []string `json:"reasonCodes,omitempty"`
}

// RepositoryScanStatus defines the observed state of RepositoryScan.
type RepositoryScanStatus struct {
	// Phase describes the high-level repository scan lifecycle state.
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastScanID is the most recent scan run identifier stored in SQLite.
	// +optional
	LastScanID string `json:"lastScanID,omitempty"`

	// LastScanTaskName is the most recent scan task name.
	// +optional
	LastScanTaskName string `json:"lastScanTaskName,omitempty"`

	// LastScanAt is the completion time of the most recent scan run, regardless of success or failure.
	// +optional
	LastScanAt *metav1.Time `json:"lastScanAt,omitempty"`

	// LastSuccessfulScanAt is the completion time of the most recent successful scan.
	// +optional
	LastSuccessfulScanAt *metav1.Time `json:"lastSuccessfulScanAt,omitempty"`

	// LastObservedHeadSHA is the latest repository head SHA seen by a completed scan.
	// +optional
	LastObservedHeadSHA string `json:"lastObservedHeadSHA,omitempty"`

	// LastProcessedCommit is the latest commit reached by the backward-compatible discovery pipeline.
	// It does not by itself claim complete coverage or validated assurance.
	// +optional
	LastProcessedCommit string `json:"lastProcessedCommit,omitempty"`

	// LastCompleteCoverageCommit is the latest commit with complete compatible inventory and candidate coverage.
	// +optional
	LastCompleteCoverageCommit string `json:"lastCompleteCoverageCommit,omitempty"`

	// LastBundleSealedCommit is the latest commit represented by an immutable sealed integrity bundle.
	// +optional
	LastBundleSealedCommit string `json:"lastBundleSealedCommit,omitempty"`

	// LastAssuranceQualifiedCommit is the latest commit satisfying validated completion.
	// +optional
	LastAssuranceQualifiedCommit string `json:"lastAssuranceQualifiedCommit,omitempty"`

	// Quality is the latest bounded run-quality projection.
	// +optional
	Quality *RepositoryScanQualityStatus `json:"quality,omitempty"`

	// ThreatModelVersion is the latest persisted threat model version.
	// +optional
	ThreatModelVersion int64 `json:"threatModelVersion,omitempty"`

	// FindingCounts summarizes open findings.
	// +optional
	FindingCounts FindingCountsStatus `json:"findingCounts,omitempty"`

	// Conditions represent the current state of the repository scan.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Findings",type=integer,JSONPath=`.status.findingCounts.total`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RepositoryScan is the Schema for the repository security scanning API.
type RepositoryScan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RepositoryScanSpec   `json:"spec,omitempty"`
	Status RepositoryScanStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryScanList contains a list of RepositoryScan resources.
type RepositoryScanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepositoryScan `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RepositoryScan{}, &RepositoryScanList{})
}
