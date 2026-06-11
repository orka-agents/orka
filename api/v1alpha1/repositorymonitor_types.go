/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RepositoryMonitorSpec defines the desired state of RepositoryMonitor.
type RepositoryMonitorSpec struct {
	// Provider is the source control provider. GitHub is the only supported v1 provider.
	// +kubebuilder:validation:Enum=github
	// +kubebuilder:default=github
	// +optional
	Provider string `json:"provider,omitempty"`

	// RepoURL is the repository URL to monitor.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(https://github[.]com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+([.]git)?/?|git@github[.]com:[A-Za-z0-9._-]+/[A-Za-z0-9._-]+([.]git)?)$`
	RepoURL string `json:"repoURL"`

	// Owner is the repository owner or organization.
	// +optional
	Owner string `json:"owner,omitempty"`

	// Repository is the repository name.
	// +optional
	Repository string `json:"repository,omitempty"`

	// Branch is the default base branch for repository-wide monitoring decisions.
	// +kubebuilder:default=main
	// +optional
	Branch string `json:"branch,omitempty"`

	// GitSecretRef references GitHub credentials for repository monitor operations.
	// +optional
	GitSecretRef *corev1.LocalObjectReference `json:"gitSecretRef,omitempty"`

	// Schedule is the cron expression for background monitor runs.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// TimeZone is the IANA time zone for the schedule.
	// +optional
	TimeZone *string `json:"timeZone,omitempty"`

	// Suspend pauses scheduled monitor runs.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// Targets selects the repository item types covered by this monitor.
	// +optional
	Targets RepositoryMonitorTargets `json:"targets,omitempty"`

	// Agents configures the agents used by monitor review and repair tasks.
	// +optional
	Agents RepositoryMonitorAgents `json:"agents,omitempty"`

	// Review controls pull-request review behavior.
	// +optional
	Review RepositoryMonitorReviewSpec `json:"review,omitempty"`

	// Repair controls bounded repair behavior.
	// +optional
	Repair RepositoryMonitorRepairSpec `json:"repair,omitempty"`

	// Automerge controls deterministic merge behavior.
	// +optional
	Automerge RepositoryMonitorAutomergeSpec `json:"automerge,omitempty"`

	// Policy contains authorization and safety policy for monitor operations.
	// +optional
	Policy RepositoryMonitorPolicySpec `json:"policy,omitempty"`

	// Validation configures deterministic validation commands for repair.
	// +optional
	Validation RepositoryMonitorValidationSpec `json:"validation,omitempty"`
}

// RepositoryMonitorTargets configures monitored repository item types.
type RepositoryMonitorTargets struct {
	// PullRequests configures pull request monitoring.
	// +optional
	PullRequests RepositoryMonitorPullRequestTarget `json:"pullRequests,omitempty"`

	// Issues configures issue monitoring.
	// +optional
	Issues RepositoryMonitorIssueTarget `json:"issues,omitempty"`

	// Commits configures commit monitoring.
	// +optional
	Commits RepositoryMonitorCommitTarget `json:"commits,omitempty"`
}

// RepositoryMonitorPullRequestTarget configures pull request monitoring.
type RepositoryMonitorPullRequestTarget struct {
	// Enabled enables pull request monitoring.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// IncludeDrafts allows draft pull requests to be selected for review.
	// +optional
	IncludeDrafts bool `json:"includeDrafts,omitempty"`

	// MaxPerRun limits pull requests selected by one background run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	// +optional
	MaxPerRun *int32 `json:"maxPerRun,omitempty"`
}

// RepositoryMonitorIssueTarget configures issue monitoring.
type RepositoryMonitorIssueTarget struct {
	// Enabled enables issue monitoring.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// MaxPerRun limits issues selected by one background run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxPerRun *int32 `json:"maxPerRun,omitempty"`
}

// RepositoryMonitorCommitTarget configures commit monitoring.
type RepositoryMonitorCommitTarget struct {
	// Enabled enables commit monitoring.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// MaxPerRun limits commits selected by one background run.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxPerRun *int32 `json:"maxPerRun,omitempty"`
}

// RepositoryMonitorAgents configures task agents for monitor workflows.
type RepositoryMonitorAgents struct {
	// Reviewer is the agent used for pull-request review tasks.
	// +optional
	Reviewer *AgentReference `json:"reviewer,omitempty"`

	// Repairer is the agent used for repair tasks.
	// +optional
	Repairer *AgentReference `json:"repairer,omitempty"`

	// Implementer is the agent used for guarded issue implementation tasks.
	// +optional
	Implementer *AgentReference `json:"implementer,omitempty"`
}

// RepositoryMonitorReviewSpec configures review behavior.
type RepositoryMonitorReviewSpec struct {
	// Event is the legacy/default GitHub review event value included in review task input.
	// It does not control RepositoryMonitor GitHub publishing; use Publish.Event.
	// +kubebuilder:validation:Enum=COMMENT;APPROVE;REQUEST_CHANGES
	// +kubebuilder:default=COMMENT
	// +optional
	Event string `json:"event,omitempty"`

	// RequireGreenCI requires acceptable CI before background review selection.
	// +optional
	RequireGreenCI bool `json:"requireGreenCI,omitempty"`

	// StaleReviewTTL bounds how long an unchanged head review remains fresh.
	// +optional
	StaleReviewTTL *metav1.Duration `json:"staleReviewTTL,omitempty"`

	// ExactEventEnabled enables exact-head review from repository events.
	// +optional
	ExactEventEnabled bool `json:"exactEventEnabled,omitempty"`

	// Publish controls deterministic GitHub pull request review publishing after review ingestion.
	// Publishing is disabled by default and V1 only supports neutral COMMENT reviews.
	// +optional
	Publish RepositoryMonitorReviewPublishSpec `json:"publish,omitempty"`
}

// RepositoryMonitorReviewPublishSpec configures safe GitHub review publishing.
type RepositoryMonitorReviewPublishSpec struct {
	// Enabled enables GitHub pull request review publishing. Defaults to false.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Mode selects whether Orka publishes only a deterministic summary or also eligible inline findings.
	// +kubebuilder:validation:Enum=summary_only;summary_with_inline_findings
	// +kubebuilder:default=summary_only
	// +optional
	Mode string `json:"mode,omitempty"`

	// Event is the GitHub review event to submit. V1 only supports COMMENT.
	// +kubebuilder:validation:Enum=COMMENT
	// +kubebuilder:default=COMMENT
	// +optional
	Event string `json:"event,omitempty"`

	// PostPassed controls whether clean/passed reviews are posted. Defaults to false.
	// +optional
	PostPassed *bool `json:"postPassed,omitempty"`

	// PostNeedsChanges controls whether needs_changes reviews are posted. Defaults to true.
	// +optional
	PostNeedsChanges *bool `json:"postNeedsChanges,omitempty"`

	// PostNeedsHuman controls whether needs_human reviews are posted. Defaults to true.
	// +optional
	PostNeedsHuman *bool `json:"postNeedsHuman,omitempty"`

	// PostSecuritySensitive allows public publishing of security_sensitive findings when true.
	// Defaults to false.
	// +optional
	PostSecuritySensitive bool `json:"postSecuritySensitive,omitempty"`

	// SameHeadPolicy controls duplicate handling for one monitor, PR, and exact head SHA.
	// V1 only supports skip.
	// +kubebuilder:validation:Enum=skip
	// +kubebuilder:default=skip
	// +optional
	SameHeadPolicy string `json:"sameHeadPolicy,omitempty"`

	// Inline controls optional inline review comments for eligible findings.
	// +optional
	Inline RepositoryMonitorReviewPublishInlineSpec `json:"inline,omitempty"`
}

// RepositoryMonitorReviewPublishInlineSpec configures optional inline GitHub review comments.
type RepositoryMonitorReviewPublishInlineSpec struct {
	// Enabled enables inline comments when Mode is summary_with_inline_findings.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// MinPriority is the lowest finding priority eligible for inline comments. Defaults to P2.
	// +kubebuilder:validation:Enum=P0;P1;P2;P3
	// +kubebuilder:default=P2
	// +optional
	MinPriority string `json:"minPriority,omitempty"`

	// MaxComments caps inline comments per review. Defaults to 10.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +optional
	MaxComments *int32 `json:"maxComments,omitempty"`

	// OnlyChangedLines restricts inline comments to RIGHT-side changed lines. V1 treats this as true.
	// +optional
	OnlyChangedLines *bool `json:"onlyChangedLines,omitempty"`
}

// RepositoryMonitorRepairSpec configures bounded repair behavior.
type RepositoryMonitorRepairSpec struct {
	// Enabled enables repair jobs.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// RequireMaintainerOptIn requires an explicit maintainer command or label.
	// +kubebuilder:default=true
	// +optional
	RequireMaintainerOptIn *bool `json:"requireMaintainerOptIn,omitempty"`

	// MaxRepairsPerPR bounds total automated repairs per PR.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxRepairsPerPR *int32 `json:"maxRepairsPerPR,omitempty"`

	// MaxRepairsPerHead bounds automated repairs per PR head SHA.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxRepairsPerHead *int32 `json:"maxRepairsPerHead,omitempty"`

	// MaxValidationRetries bounds validation retries for one repair job.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxValidationRetries *int32 `json:"maxValidationRetries,omitempty"`

	// MaxReviewFixRetries bounds review-fix loop retries.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxReviewFixRetries *int32 `json:"maxReviewFixRetries,omitempty"`
}

// RepositoryMonitorAutomergeSpec configures automerge behavior.
type RepositoryMonitorAutomergeSpec struct {
	// Enabled enables automerge jobs.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// RequireMaintainerOptIn requires explicit maintainer opt-in.
	// +kubebuilder:default=true
	// +optional
	RequireMaintainerOptIn *bool `json:"requireMaintainerOptIn,omitempty"`

	// RequireGlobalMergeGate requires the controller-wide merge gate.
	// +kubebuilder:default=true
	// +optional
	RequireGlobalMergeGate *bool `json:"requireGlobalMergeGate,omitempty"`

	// AllowedMergeMethods lists merge methods allowed by policy.
	// +kubebuilder:validation:items:Enum=merge;squash;rebase
	// +listType=set
	// +optional
	AllowedMergeMethods []string `json:"allowedMergeMethods,omitempty"`
}

// RepositoryMonitorPolicySpec configures monitor safety policy.
type RepositoryMonitorPolicySpec struct {
	// ProtectedLabels block automated review, repair, or merge according to policy.
	// +listType=set
	// +optional
	ProtectedLabels []string `json:"protectedLabels,omitempty"`

	// PauseLabels block further automation while present.
	// +listType=set
	// +optional
	PauseLabels []string `json:"pauseLabels,omitempty"`

	// OptInLabels names labels that opt a PR into repair or automerge.
	// +optional
	OptInLabels RepositoryMonitorOptInLabels `json:"optInLabels,omitempty"`

	// AdvisoryLabels controls advisory label synchronization.
	// +optional
	AdvisoryLabels RepositoryMonitorAdvisoryLabels `json:"advisoryLabels,omitempty"`

	// AllowedRepositoryPermissions lists GitHub permissions allowed to issue write commands.
	// +kubebuilder:validation:items:Enum=admin;maintain;write
	// +listType=set
	// +optional
	AllowedRepositoryPermissions []string `json:"allowedRepositoryPermissions,omitempty"`
}

// RepositoryMonitorOptInLabels configures opt-in label names.
type RepositoryMonitorOptInLabels struct {
	// Autofix opts a PR into autofix.
	// +optional
	Autofix string `json:"autofix,omitempty"`

	// Automerge opts a PR into automerge.
	// +optional
	Automerge string `json:"automerge,omitempty"`
}

// RepositoryMonitorAdvisoryLabels controls advisory label sync.
type RepositoryMonitorAdvisoryLabels struct {
	// Enabled enables advisory label sync.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// RepositoryMonitorValidationSpec configures deterministic validation.
type RepositoryMonitorValidationSpec struct {
	// Mode selects validation scope.
	// +kubebuilder:validation:Enum=off;changed;full
	// +kubebuilder:default=changed
	// +optional
	Mode string `json:"mode,omitempty"`

	// Commands are validation commands repair jobs must run.
	// +optional
	Commands []string `json:"commands,omitempty"`
}

// RepositoryMonitorStatus defines the observed state of RepositoryMonitor.
type RepositoryMonitorStatus struct {
	// Phase describes the high-level monitor lifecycle state.
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastRunID is the most recent monitor run identifier stored in SQLite.
	// +optional
	LastRunID string `json:"lastRunID,omitempty"`

	// LastRunTime is the completion time of the most recent run, regardless of success.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// LastSuccessfulRunTime is the completion time of the most recent successful run.
	// +optional
	LastSuccessfulRunTime *metav1.Time `json:"lastSuccessfulRunTime,omitempty"`

	// ObservedGeneration is the latest spec generation reflected in status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// OpenPullRequests is the current count of open pull requests seen by the monitor.
	// +optional
	OpenPullRequests int32 `json:"openPullRequests,omitempty"`

	// PendingReviews is the count of items waiting for review.
	// +optional
	PendingReviews int32 `json:"pendingReviews,omitempty"`

	// ActiveRepairs is the count of repair jobs currently active.
	// +optional
	ActiveRepairs int32 `json:"activeRepairs,omitempty"`

	// BlockedItems is the count of items blocked by policy, failures, or human action.
	// +optional
	BlockedItems int32 `json:"blockedItems,omitempty"`

	// MergeReadyItems is the count of items ready for merge.
	// +optional
	MergeReadyItems int32 `json:"mergeReadyItems,omitempty"`

	// Conditions represent the current state of the repository monitor.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branch`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pending",type=integer,JSONPath=`.status.pendingReviews`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RepositoryMonitor is the Schema for repository maintainer automation.
type RepositoryMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RepositoryMonitorSpec   `json:"spec,omitempty"`
	Status RepositoryMonitorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryMonitorList contains a list of RepositoryMonitor resources.
type RepositoryMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RepositoryMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RepositoryMonitor{}, &RepositoryMonitorList{})
}
