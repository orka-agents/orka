package store

import (
	"encoding/json"
	"time"
)

const (
	// SecurityQualitySchemaVersion is the current durable scan-quality projection schema.
	SecurityQualitySchemaVersion = 1

	IdentityQualityCanonical        = "canonical"
	IdentityQualityProducerProposed = "producer-proposed"
	IdentityQualityLegacy           = "legacy"

	IdentityAlgorithmLegacyV2 = "legacy-v2"

	FindingHistoryCanonical           = "canonical"
	FindingHistoryLegacyUnrebuildable = "legacy-unrebuildable"
)

// CoverageStatus describes controller-derived inventory or candidate coverage.
type CoverageStatus string

const (
	CoverageStatusNotStarted CoverageStatus = "not_started"
	CoverageStatusPending    CoverageStatus = "pending"
	CoverageStatusComplete   CoverageStatus = "complete"
	CoverageStatusPartial    CoverageStatus = "partial"
	CoverageStatusFailed     CoverageStatus = "failed"
	CoverageStatusUnknown    CoverageStatus = "unknown"
)

// ValidationScope describes the intended validation selection policy.
type ValidationScope string

const (
	ValidationScopeOff     ValidationScope = "off"
	ValidationScopeSampled ValidationScope = "sampled"
	ValidationScopeAll     ValidationScope = "all"
	ValidationScopeUnknown ValidationScope = "unknown"
)

// QualityExecutionStatus describes execution of validation or attack-path work.
type QualityExecutionStatus string

const (
	QualityExecutionNotStarted QualityExecutionStatus = "not_started"
	QualityExecutionPending    QualityExecutionStatus = "pending"
	QualityExecutionComplete   QualityExecutionStatus = "complete"
	QualityExecutionPartial    QualityExecutionStatus = "partial"
	QualityExecutionFailed     QualityExecutionStatus = "failed"
	QualityExecutionDeferred   QualityExecutionStatus = "deferred"
	QualityExecutionUnknown    QualityExecutionStatus = "unknown"
)

// AnalysisAttestationLevel describes controller-observed analysis access.
type AnalysisAttestationLevel string

const (
	AnalysisAttestationDelivered    AnalysisAttestationLevel = "delivered"
	AnalysisAttestationToolObserved AnalysisAttestationLevel = "tool-observed"
	AnalysisAttestationBrokered     AnalysisAttestationLevel = "brokered"
	AnalysisAttestationUnverified   AnalysisAttestationLevel = "unverified"
)

// TargetVerificationStatus describes whether the analyzed target matches the trusted target receipt.
type TargetVerificationStatus string

const (
	TargetVerificationPending    TargetVerificationStatus = "pending"
	TargetVerificationVerified   TargetVerificationStatus = "verified"
	TargetVerificationUnverified TargetVerificationStatus = "unverified"
	TargetVerificationMismatch   TargetVerificationStatus = "mismatch"
)

// BundleStatus describes the integrity-bundle lifecycle.
type BundleStatus string

const (
	BundleStatusNotStarted      BundleStatus = "not_started"
	BundleStatusDraft           BundleStatus = "draft"
	BundleStatusSealing         BundleStatus = "sealing"
	BundleStatusSealed          BundleStatus = "sealed"
	BundleStatusRetryableFailed BundleStatus = "retryable_failed"
	BundleStatusFailed          BundleStatus = "failed"
)

// AuthorizationStatus describes controller-derived execution authorization.
type AuthorizationStatus string

const (
	AuthorizationStatusVerified         AuthorizationStatus = "verified"
	AuthorizationStatusAdmitted         AuthorizationStatus = "admitted"
	AuthorizationStatusDelegated        AuthorizationStatus = "delegated"
	AuthorizationStatusLegacyUnverified AuthorizationStatus = "legacy-unverified"
	AuthorizationStatusRevoked          AuthorizationStatus = "revoked"
	AuthorizationStatusExpired          AuthorizationStatus = "expired"
)

// IsolationStatus describes the effective analysis isolation mode.
type IsolationStatus string

const (
	IsolationStatusLegacy     IsolationStatus = "legacy"
	IsolationStatusHardened   IsolationStatus = "hardened"
	IsolationStatusFallback   IsolationStatus = "fallback"
	IsolationStatusUnverified IsolationStatus = "unverified"
	IsolationStatusFailed     IsolationStatus = "failed"
)

// ScanQuality is the controller-owned, run-scoped quality projection.
type ScanQuality struct {
	SchemaVersion            int                      `json:"qualitySchemaVersion"`
	InventoryCoverageStatus  CoverageStatus           `json:"inventoryCoverageStatus"`
	CandidateCoverageStatus  CoverageStatus           `json:"candidateCoverageStatus"`
	CoverageStatus           CoverageStatus           `json:"coverageStatus"`
	ValidationScope          ValidationScope          `json:"validationScope"`
	ValidationExecution      QualityExecutionStatus   `json:"validationExecution"`
	AttackPathExecution      QualityExecutionStatus   `json:"attackPathExecution"`
	AnalysisAttestationLevel AnalysisAttestationLevel `json:"analysisAttestationLevel"`
	TargetVerification       TargetVerificationStatus `json:"targetVerification"`
	BundleStatus             BundleStatus             `json:"bundleStatus"`
	AuthorizationStatus      AuthorizationStatus      `json:"authorizationStatus"`
	IsolationStatus          IsolationStatus          `json:"isolationStatus"`
	ReasonCodes              []string                 `json:"reasonCodes,omitempty"`
}

// LegacyScanQuality returns the explicit projection used for pre-integrity rows.
func LegacyScanQuality() ScanQuality {
	return ScanQuality{
		SchemaVersion:            SecurityQualitySchemaVersion,
		InventoryCoverageStatus:  CoverageStatusUnknown,
		CandidateCoverageStatus:  CoverageStatusUnknown,
		CoverageStatus:           CoverageStatusUnknown,
		ValidationScope:          ValidationScopeUnknown,
		ValidationExecution:      QualityExecutionUnknown,
		AttackPathExecution:      QualityExecutionUnknown,
		AnalysisAttestationLevel: AnalysisAttestationUnverified,
		TargetVerification:       TargetVerificationUnverified,
		BundleStatus:             BundleStatusNotStarted,
		AuthorizationStatus:      AuthorizationStatusLegacyUnverified,
		IsolationStatus:          IsolationStatusLegacy,
		ReasonCodes:              []string{},
	}
}

// RepositoryScanIdentity identifies one immutable RepositoryScan incarnation.
type RepositoryScanIdentity struct {
	Name       string
	UID        string
	Generation int64
}

// ScanRun represents a single repository security scan execution.
type ScanRun struct {
	ID                       string      `json:"id"`
	RunUID                   string      `json:"runUID,omitempty"`
	Namespace                string      `json:"namespace"`
	RepositoryScan           string      `json:"repositoryScan"`
	RepositoryScanUID        string      `json:"repositoryScanUID,omitempty"`
	RepositoryScanGeneration int64       `json:"repositoryScanGeneration,omitempty"`
	TaskName                 string      `json:"taskName"`
	Mode                     string      `json:"mode"`
	Phase                    string      `json:"phase"`
	StartedAt                time.Time   `json:"startedAt"`
	CompletedAt              *time.Time  `json:"completedAt,omitempty"`
	BaseCommit               string      `json:"baseCommit,omitempty"`
	HeadCommit               string      `json:"headCommit,omitempty"`
	CommitCount              int         `json:"commitCount"`
	SliceCount               int         `json:"sliceCount"`
	ReviewedSliceCount       int         `json:"reviewedSliceCount"`
	SkippedSliceCount        int         `json:"skippedSliceCount"`
	AcceptedFindings         int         `json:"acceptedFindings"`
	DroppedFindings          int         `json:"droppedFindings"`
	ScannerPolicyVersion     string      `json:"scannerPolicyVersion,omitempty"`
	PolicyDigest             string      `json:"policyDigest,omitempty"`
	RequestIdempotencyKey    string      `json:"requestIdempotencyKey,omitempty"`
	ResolvedTargetKey        string      `json:"resolvedTargetKey,omitempty"`
	TargetReceiptID          string      `json:"targetReceiptID,omitempty"`
	IdempotencyKey           string      `json:"idempotencyKey,omitempty"`
	Quality                  ScanQuality `json:"quality"`
	Summary                  string      `json:"summary,omitempty"`
	ErrorMessage             string      `json:"errorMessage,omitempty"`
}

// ThreatModel stores the latest generated or user-edited threat model.
type ThreatModel struct {
	Namespace                string    `json:"namespace"`
	RepositoryScan           string    `json:"repositoryScan"`
	RepositoryScanUID        string    `json:"repositoryScanUID,omitempty"`
	RepositoryScanGeneration int64     `json:"repositoryScanGeneration,omitempty"`
	Version                  int64     `json:"version"`
	Content                  string    `json:"content"`
	Source                   string    `json:"source"`
	GeneratedByScan          string    `json:"generatedByScan,omitempty"`
	CreatedAt                time.Time `json:"createdAt"`
	UpdatedAt                time.Time `json:"updatedAt"`
}

// FindingEvidenceRef points to supporting evidence for a finding.
type FindingEvidenceRef struct {
	Kind          string `json:"kind"`
	TaskName      string `json:"taskName,omitempty"`
	Name          string `json:"name,omitempty"`
	Label         string `json:"label,omitempty"`
	Path          string `json:"path,omitempty"`
	StartLine     int    `json:"startLine,omitempty"`
	EndLine       int    `json:"endLine,omitempty"`
	Symbol        string `json:"symbol,omitempty"`
	Quote         string `json:"quote,omitempty"`
	ContentSHA256 string `json:"contentSHA256,omitempty"`
	ContentSize   int64  `json:"contentSize,omitempty"`
}

// Finding represents a security finding associated with a repository scan.
type Finding struct {
	ID                            string               `json:"id"`
	Namespace                     string               `json:"namespace"`
	RepositoryScan                string               `json:"repositoryScan"`
	ScanRunID                     string               `json:"scanRunID"`
	ScanTaskName                  string               `json:"scanTaskName,omitempty"`
	SliceID                       string               `json:"sliceID,omitempty"`
	Fingerprint                   string               `json:"fingerprint"`
	IdentityQuality               string               `json:"identityQuality,omitempty"`
	IdentityAlgorithmVersion      string               `json:"identityAlgorithmVersion,omitempty"`
	SemanticFingerprint           string               `json:"semanticFingerprint,omitempty"`
	LegacyFingerprint             string               `json:"legacyFingerprint,omitempty"`
	HistoryStatus                 string               `json:"historyStatus,omitempty"`
	CurrentOccurrenceID           string               `json:"currentOccurrenceID,omitempty"`
	DecisionVersion               int64                `json:"decisionVersion,omitempty"`
	Title                         string               `json:"title"`
	Category                      string               `json:"category,omitempty"`
	Summary                       string               `json:"summary"`
	Severity                      string               `json:"severity"`
	Confidence                    string               `json:"confidence"`
	Triage                        string               `json:"triage,omitempty"`
	ValidationStatus              string               `json:"validationStatus"`
	State                         string               `json:"state"`
	FilePath                      string               `json:"filePath,omitempty"`
	Line                          int                  `json:"line,omitempty"`
	CommitSHA                     string               `json:"commitSHA,omitempty"`
	RootCause                     string               `json:"rootCause,omitempty"`
	Reproduction                  string               `json:"reproduction,omitempty"`
	Remediation                   string               `json:"remediation,omitempty"`
	SuggestedAction               string               `json:"suggestedAction,omitempty"`
	WhyTestsDoNotAlreadyCoverThis string               `json:"whyTestsDoNotAlreadyCoverThis,omitempty"`
	SuggestedRegressionTest       string               `json:"suggestedRegressionTest,omitempty"`
	MinimumFixScope               string               `json:"minimumFixScope,omitempty"`
	Evidence                      []FindingEvidenceRef `json:"evidence,omitempty"`
	ValidationJSON                string               `json:"validationJSON,omitempty"`
	PatchProposalID               string               `json:"patchProposalID,omitempty"`
	PRNumber                      *int                 `json:"prNumber,omitempty"`
	PRURL                         string               `json:"prURL,omitempty"`
	CreatedAt                     time.Time            `json:"createdAt"`
	UpdatedAt                     time.Time            `json:"updatedAt"`
}

// PatchProposal represents a patch generation attempt for a finding.
type PatchProposal struct {
	ID              string    `json:"id"`
	Namespace       string    `json:"namespace"`
	RepositoryScan  string    `json:"repositoryScan"`
	FindingID       string    `json:"findingID"`
	OccurrenceID    string    `json:"occurrenceID,omitempty"`
	SourceScanRunID string    `json:"sourceScanRunID,omitempty"`
	SourceHeadSHA   string    `json:"sourceHeadSHA,omitempty"`
	TaskName        string    `json:"taskName"`
	Branch          string    `json:"branch"`
	DiffArtifact    string    `json:"diffArtifact,omitempty"`
	SummaryArtifact string    `json:"summaryArtifact,omitempty"`
	Status          string    `json:"status"`
	PRNumber        *int      `json:"prNumber,omitempty"`
	PRURL           string    `json:"prURL,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// FindingCounts summarizes finding counts by severity.
type FindingCounts struct {
	Total    int32 `json:"total"`
	Critical int32 `json:"critical"`
	High     int32 `json:"high"`
	Medium   int32 `json:"medium"`
	Low      int32 `json:"low"`
}

// FindingFilter constrains finding queries.
type FindingFilter struct {
	Namespace        string
	RepositoryScan   string
	SliceID          string
	Category         string
	Severity         string
	ValidationStatus string
	State            string
	Recommended      bool
	Limit            int
	Cursor           string
}

// ChangedLineRange identifies lines introduced or modified between two scan commits.
type ChangedLineRange struct {
	Path      string `json:"path"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
}

// ReviewSliceFile references a repository file included in review slice metadata.
type ReviewSliceFile struct {
	Path    string `json:"path"`
	Reason  string `json:"reason,omitempty"`
	Symbol  string `json:"symbol,omitempty"`
	Route   string `json:"route,omitempty"`
	Command string `json:"command,omitempty"`
}

// ReviewSliceTest references a test file and optional command associated with a slice.
type ReviewSliceTest struct {
	Path    string `json:"path"`
	Command string `json:"command,omitempty"`
}

// ReviewSlice describes a deterministic, bounded repository review unit.
type ReviewSlice struct {
	SchemaVersion     int                `json:"schemaVersion"`
	ID                string             `json:"id"`
	Namespace         string             `json:"namespace,omitempty"`
	RepositoryScan    string             `json:"repositoryScan"`
	Source            string             `json:"source"`
	Title             string             `json:"title"`
	Summary           string             `json:"summary"`
	Kind              string             `json:"kind"`
	Entrypoints       []ReviewSliceFile  `json:"entrypoints,omitempty"`
	OwnedFiles        []ReviewSliceFile  `json:"ownedFiles,omitempty"`
	ContextFiles      []ReviewSliceFile  `json:"contextFiles,omitempty"`
	Tests             []ReviewSliceTest  `json:"tests,omitempty"`
	ChangedFiles      []string           `json:"changedFiles,omitempty"`
	ChangedLineRanges []ChangedLineRange `json:"changedLineRanges,omitempty"`
	Tags              []string           `json:"tags,omitempty"`
	TrustBoundaries   []string           `json:"trustBoundaries,omitempty"`
	Confidence        string             `json:"confidence"`
	Status            string             `json:"status"`
	LastScanRunID     string             `json:"lastScanRunID,omitempty"`
	LastReviewedAt    *time.Time         `json:"lastReviewedAt,omitempty"`
	CreatedAt         time.Time          `json:"createdAt,omitempty"`
	UpdatedAt         time.Time          `json:"updatedAt,omitempty"`
}

// ReviewSliceFilter constrains review slice queries.
type ReviewSliceFilter struct {
	Namespace      string
	RepositoryScan string
	Status         string
	LastScanRunID  string
	Limit          int
	Cursor         string
}

// DroppedFinding records model output rejected before finding ingestion.
type DroppedFinding struct {
	ID             string    `json:"id"`
	Namespace      string    `json:"namespace"`
	RepositoryScan string    `json:"repositoryScan"`
	ScanRunID      string    `json:"scanRunID"`
	TaskName       string    `json:"taskName"`
	SliceID        string    `json:"sliceID,omitempty"`
	Reason         string    `json:"reason"`
	Layer          string    `json:"layer,omitempty"`
	SampleJSON     string    `json:"sampleJSON,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// DroppedFindingFilter constrains dropped finding diagnostic queries.
type DroppedFindingFilter struct {
	Namespace      string
	RepositoryScan string
	ScanRunID      string
	SliceID        string
	Layer          string
	Reason         string
	ReasonContains string
	Limit          int
	Cursor         string
}

// ExecutionProvenanceKind selects one trusted execution identity union member.
type ExecutionProvenanceKind string

const (
	ExecutionProvenanceKubernetes      ExecutionProvenanceKind = "kubernetes"
	ExecutionProvenanceHarness         ExecutionProvenanceKind = "harness"
	ExecutionProvenanceExternalAdapter ExecutionProvenanceKind = "external-adapter"
)

// KubernetesExecutionProvenance binds output to one Task attempt and workload.
type KubernetesExecutionProvenance struct {
	TaskName string `json:"taskName"`
	TaskUID  string `json:"taskUID"`
	Attempt  int64  `json:"attempt"`
	JobName  string `json:"jobName,omitempty"`
	JobUID   string `json:"jobUID,omitempty"`
	PodName  string `json:"podName,omitempty"`
	PodUID   string `json:"podUID,omitempty"`
}

// HarnessExecutionProvenance binds output to a trusted runtime turn.
type HarnessExecutionProvenance struct {
	RuntimeSessionID string `json:"runtimeSessionID"`
	TurnID           string `json:"turnID"`
	CorrelationID    string `json:"correlationID"`
	RuntimeName      string `json:"runtimeName,omitempty"`
	RuntimeVersion   string `json:"runtimeVersion,omitempty"`
}

// ExternalAdapterExecutionProvenance binds output to an adapter invocation.
type ExternalAdapterExecutionProvenance struct {
	RuntimeName       string `json:"runtimeName"`
	RuntimeUID        string `json:"runtimeUID,omitempty"`
	RuntimeGeneration int64  `json:"runtimeGeneration,omitempty"`
	InvocationID      string `json:"invocationID"`
	AdapterVersion    string `json:"adapterVersion,omitempty"`
	Attestation       string `json:"attestation,omitempty"`
}

// ExecutionProvenance is a tagged union. Exactly one member must match Kind.
type ExecutionProvenance struct {
	Kind            ExecutionProvenanceKind             `json:"kind"`
	Kubernetes      *KubernetesExecutionProvenance      `json:"kubernetes,omitempty"`
	Harness         *HarnessExecutionProvenance         `json:"harness,omitempty"`
	ExternalAdapter *ExternalAdapterExecutionProvenance `json:"externalAdapter,omitempty"`
}

// StageReceiptDisposition is the controller ingestion disposition for one attempt.
type StageReceiptDisposition string

const (
	StageReceiptAccepted   StageReceiptDisposition = "accepted"
	StageReceiptRejected   StageReceiptDisposition = "rejected"
	StageReceiptDeferred   StageReceiptDisposition = "deferred"
	StageReceiptSuperseded StageReceiptDisposition = "superseded"
)

// StageReceipt is an immutable controller-owned execution and ingestion record.
type StageReceipt struct {
	ID                         string                   `json:"id"`
	Namespace                  string                   `json:"namespace"`
	RepositoryScan             string                   `json:"repositoryScan"`
	ScanRunID                  string                   `json:"scanRunID"`
	RunUID                     string                   `json:"runUID"`
	Stage                      string                   `json:"stage"`
	ScopeKind                  string                   `json:"scopeKind,omitempty"`
	ScopeID                    string                   `json:"scopeID,omitempty"`
	Provenance                 ExecutionProvenance      `json:"provenance"`
	ExpectedTargetSHA          string                   `json:"expectedTargetSHA,omitempty"`
	ObservedTargetSHA          string                   `json:"observedTargetSHA,omitempty"`
	TargetReceiptID            string                   `json:"targetReceiptID,omitempty"`
	AgentName                  string                   `json:"agentName,omitempty"`
	AgentUID                   string                   `json:"agentUID,omitempty"`
	AgentGeneration            int64                    `json:"agentGeneration,omitempty"`
	ProducerImage              string                   `json:"producerImage,omitempty"`
	ProducerVersion            string                   `json:"producerVersion,omitempty"`
	AttestationLevel           AnalysisAttestationLevel `json:"attestationLevel"`
	ScannerPolicyDigest        string                   `json:"scannerPolicyDigest,omitempty"`
	NormalizedInputDigest      string                   `json:"normalizedInputDigest,omitempty"`
	SourceArtifactName         string                   `json:"sourceArtifactName,omitempty"`
	SourceArtifactMediaType    string                   `json:"sourceArtifactMediaType,omitempty"`
	SourceArtifactSize         int64                    `json:"sourceArtifactSize,omitempty"`
	SourceArtifactGeneration   int64                    `json:"sourceArtifactGeneration,omitempty"`
	SourceArtifactDigest       string                   `json:"sourceArtifactDigest,omitempty"`
	ControllerIngestionVersion string                   `json:"controllerIngestionVersion"`
	NormalizedOutputDigest     string                   `json:"normalizedOutputDigest,omitempty"`
	EvidenceDigest             string                   `json:"evidenceDigest,omitempty"`
	Disposition                StageReceiptDisposition  `json:"disposition"`
	ReasonCode                 string                   `json:"reasonCode,omitempty"`
	Reason                     string                   `json:"reason,omitempty"`
	StartedAt                  time.Time                `json:"startedAt"`
	IngestedAt                 time.Time                `json:"ingestedAt"`
	CompletedAt                *time.Time               `json:"completedAt,omitempty"`
	RecordDigest               string                   `json:"recordDigest"`
	CreatedAt                  time.Time                `json:"createdAt"`
}

// StageReceiptFilter constrains receipt queries.
type StageReceiptFilter struct {
	Namespace      string
	RepositoryScan string
	ScanRunID      string
	Stage          string
	ScopeID        string
	Disposition    StageReceiptDisposition
	Limit          int
	Cursor         string
}

// FindingObservationDisposition describes controller handling of one producer candidate.
type FindingObservationDisposition string

const (
	FindingObservationAccepted FindingObservationDisposition = "accepted"
	FindingObservationRejected FindingObservationDisposition = "rejected"
	FindingObservationDeferred FindingObservationDisposition = "deferred"
)

// FindingObservation is an immutable normalized producer candidate.
type FindingObservation struct {
	ID                       string                        `json:"id"`
	Namespace                string                        `json:"namespace"`
	RepositoryScan           string                        `json:"repositoryScan"`
	ScanRunID                string                        `json:"scanRunID"`
	RunUID                   string                        `json:"runUID"`
	StageReceiptID           string                        `json:"stageReceiptID"`
	TargetReceiptID          string                        `json:"targetReceiptID,omitempty"`
	SliceID                  string                        `json:"sliceID,omitempty"`
	PassID                   string                        `json:"passID,omitempty"`
	CandidateKey             string                        `json:"candidateKey,omitempty"`
	ProducerFindingID        string                        `json:"producerFindingID,omitempty"`
	SourceArtifactName       string                        `json:"sourceArtifactName,omitempty"`
	SourceArtifactGeneration int64                         `json:"sourceArtifactGeneration,omitempty"`
	SourceArtifactDigest     string                        `json:"sourceArtifactDigest,omitempty"`
	PolicyDigest             string                        `json:"policyDigest,omitempty"`
	Ordinal                  int                           `json:"ordinal"`
	Disposition              FindingObservationDisposition `json:"disposition"`
	ReasonCode               string                        `json:"reasonCode,omitempty"`
	Reason                   string                        `json:"reason,omitempty"`
	RuleID                   string                        `json:"ruleID,omitempty"`
	IdentityAnchor           string                        `json:"identityAnchor,omitempty"`
	IdentityInstance         string                        `json:"identityInstance,omitempty"`
	IdentityQuality          string                        `json:"identityQuality,omitempty"`
	IdentityAlgorithmVersion string                        `json:"identityAlgorithmVersion,omitempty"`
	SemanticFingerprint      string                        `json:"semanticFingerprint,omitempty"`
	LegacyFingerprint        string                        `json:"legacyFingerprint,omitempty"`
	NormalizedPayload        json.RawMessage               `json:"normalizedPayload,omitempty"`
	PayloadDigest            string                        `json:"payloadDigest,omitempty"`
	RecordDigest             string                        `json:"recordDigest"`
	CreatedAt                time.Time                     `json:"createdAt"`
}

// FindingObservationFilter constrains observation queries.
type FindingObservationFilter struct {
	Namespace           string
	RepositoryScan      string
	ScanRunID           string
	StageReceiptID      string
	SemanticFingerprint string
	Disposition         FindingObservationDisposition
	Limit               int
	Cursor              string
}

const (
	FindingObservationRelationshipContributor = "contributor"
	FindingObservationRelationshipAbsorbed    = "absorbed"
)

// FindingOccurrenceObservation links one finalized occurrence to a contributor.
type FindingOccurrenceObservation struct {
	ObservationID string `json:"observationID"`
	Relationship  string `json:"relationship"`
	Ordinal       int    `json:"ordinal"`
}

// FindingOccurrence is immutable discovery state for one semantic finding in one run.
type FindingOccurrence struct {
	ID                       string                         `json:"id"`
	Namespace                string                         `json:"namespace"`
	RepositoryScan           string                         `json:"repositoryScan"`
	ScanRunID                string                         `json:"scanRunID"`
	RunUID                   string                         `json:"runUID"`
	PublicFindingID          string                         `json:"publicFindingID"`
	SemanticFindingID        string                         `json:"semanticFindingID"`
	SemanticFingerprint      string                         `json:"semanticFingerprint"`
	IdentityQuality          string                         `json:"identityQuality"`
	IdentityAlgorithmVersion string                         `json:"identityAlgorithmVersion"`
	LegacyFingerprint        string                         `json:"legacyFingerprint,omitempty"`
	RuleID                   string                         `json:"ruleID,omitempty"`
	IdentityAnchor           string                         `json:"identityAnchor,omitempty"`
	IdentityInstance         string                         `json:"identityInstance,omitempty"`
	TargetReceiptID          string                         `json:"targetReceiptID,omitempty"`
	TargetSHA                string                         `json:"targetSHA,omitempty"`
	DiscoveryPayload         json.RawMessage                `json:"discoveryPayload,omitempty"`
	PayloadDigest            string                         `json:"payloadDigest,omitempty"`
	ObservationLinks         []FindingOccurrenceObservation `json:"observationLinks,omitempty"`
	RecordDigest             string                         `json:"recordDigest"`
	CreatedAt                time.Time                      `json:"createdAt"`
}

// FindingOccurrenceFinalization atomically writes occurrence history and its materialized projection.
type FindingOccurrenceFinalization struct {
	Occurrence       FindingOccurrence              `json:"occurrence"`
	ObservationLinks []FindingOccurrenceObservation `json:"observationLinks"`
	Projection       Finding                        `json:"projection"`
}

// FindingOccurrenceFilter constrains occurrence queries.
type FindingOccurrenceFilter struct {
	Namespace           string
	RepositoryScan      string
	ScanRunID           string
	PublicFindingID     string
	SemanticFingerprint string
	Limit               int
	Cursor              string
}

// FindingAlias maps canonical semantic identity to the compatibility finding ID.
type FindingAlias struct {
	Namespace                string    `json:"namespace"`
	RepositoryScan           string    `json:"repositoryScan"`
	SemanticFingerprint      string    `json:"semanticFingerprint"`
	SemanticFindingID        string    `json:"semanticFindingID"`
	PublicFindingID          string    `json:"publicFindingID"`
	IdentityQuality          string    `json:"identityQuality"`
	IdentityAlgorithmVersion string    `json:"identityAlgorithmVersion"`
	CreatedAt                time.Time `json:"createdAt"`
}

// FindingAssessmentKind separates validation from attack-path analysis.
type FindingAssessmentKind string

const (
	FindingAssessmentValidation FindingAssessmentKind = "validation"
	FindingAssessmentAttackPath FindingAssessmentKind = "attack_path"
)

// FindingAssessment is an immutable assessment bound to one occurrence.
type FindingAssessment struct {
	ID                         string                `json:"id"`
	Namespace                  string                `json:"namespace"`
	RepositoryScan             string                `json:"repositoryScan"`
	ScanRunID                  string                `json:"scanRunID"`
	RunUID                     string                `json:"runUID"`
	OccurrenceID               string                `json:"occurrenceID"`
	PublicFindingID            string                `json:"publicFindingID"`
	Kind                       FindingAssessmentKind `json:"kind"`
	StageReceiptID             string                `json:"stageReceiptID"`
	TargetReceiptID            string                `json:"targetReceiptID,omitempty"`
	TargetSHA                  string                `json:"targetSHA,omitempty"`
	Method                     string                `json:"method,omitempty"`
	Outcome                    string                `json:"outcome"`
	FailureClass               string                `json:"failureClass,omitempty"`
	Summary                    string                `json:"summary,omitempty"`
	ProofGap                   string                `json:"proofGap,omitempty"`
	EvidenceReceiptIDs         []string              `json:"evidenceReceiptIDs,omitempty"`
	NormalizedPayload          json.RawMessage       `json:"normalizedPayload,omitempty"`
	PayloadDigest              string                `json:"payloadDigest,omitempty"`
	ProjectionValidationStatus string                `json:"projectionValidationStatus,omitempty"`
	ProjectionEvidence         []FindingEvidenceRef  `json:"projectionEvidence,omitempty"`
	RecordDigest               string                `json:"recordDigest"`
	CreatedAt                  time.Time             `json:"createdAt"`
}

// FindingAssessmentFilter constrains assessment queries.
type FindingAssessmentFilter struct {
	Namespace       string
	RepositoryScan  string
	OccurrenceID    string
	PublicFindingID string
	Kind            FindingAssessmentKind
	Limit           int
	Cursor          string
}

// FindingDecisionScope selects occurrence-specific or logical-finding applicability.
type FindingDecisionScope string

const (
	FindingDecisionOccurrence     FindingDecisionScope = "occurrence"
	FindingDecisionLogicalFinding FindingDecisionScope = "logical_finding"
)

// FindingDecisionAction is an append-only lifecycle transition.
type FindingDecisionAction string

const (
	FindingDecisionReopen             FindingDecisionAction = "reopen"
	FindingDecisionCloseFixed         FindingDecisionAction = "close_fixed"
	FindingDecisionCloseFalsePositive FindingDecisionAction = "close_false_positive"
	FindingDecisionCloseWontFix       FindingDecisionAction = "close_wont_fix"
	FindingDecisionSuppress           FindingDecisionAction = "suppress"
)

// FindingDecisionApplicability bounds logical-finding decisions.
type FindingDecisionApplicability struct {
	TargetLineage   string     `json:"targetLineage"`
	Scope           string     `json:"scope"`
	PolicyVersion   string     `json:"policyVersion"`
	PredicateDigest string     `json:"predicateDigest"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
}

// FindingDecision is an immutable authenticated lifecycle decision.
type FindingDecision struct {
	ID                      string                        `json:"decisionID"`
	Namespace               string                        `json:"namespace"`
	RepositoryScan          string                        `json:"repositoryScan"`
	PublicFindingID         string                        `json:"publicFindingID"`
	Scope                   FindingDecisionScope          `json:"scope"`
	OccurrenceID            string                        `json:"occurrenceID,omitempty"`
	Action                  FindingDecisionAction         `json:"action"`
	ReasonCode              string                        `json:"reasonCode,omitempty"`
	Reason                  string                        `json:"reason,omitempty"`
	EvidenceReceiptIDs      []string                      `json:"evidenceReceiptIDs,omitempty"`
	SupersedesDecisionID    string                        `json:"supersedesDecisionID,omitempty"`
	ExpectedDecisionVersion int64                         `json:"expectedDecisionVersion"`
	DecisionVersion         int64                         `json:"decisionVersion"`
	Applicability           *FindingDecisionApplicability `json:"applicability,omitempty"`
	ActorSubject            string                        `json:"actorSubject"`
	ActorIssuer             string                        `json:"actorIssuer,omitempty"`
	AuthenticationSource    string                        `json:"authenticationSource"`
	Source                  string                        `json:"source,omitempty"`
	FeedbackEligible        bool                          `json:"feedbackEligible"`
	RecordDigest            string                        `json:"recordDigest"`
	CreatedAt               time.Time                     `json:"createdAt"`
}

// FindingDecisionFilter constrains decision queries.
type FindingDecisionFilter struct {
	Namespace       string
	RepositoryScan  string
	PublicFindingID string
	OccurrenceID    string
	Scope           FindingDecisionScope
	Limit           int
	Cursor          string
}
