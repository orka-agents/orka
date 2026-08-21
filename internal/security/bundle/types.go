// Package bundle builds and verifies deterministic repository security scan
// integrity bundles. It is intentionally independent from the scan store and
// controller so callers can decide when immutable inputs are ready to seal.
package bundle

import "time"

const SchemaVersion = 1

// Limits bounds canonical documents and evidence accepted by Build and Verify.
// Zero-valued fields use DefaultLimits. Negative values are invalid.
type Limits struct {
	MaxDocumentBytes      int
	MaxStringBytes        int
	MaxArrayItems         int
	MaxFindings           int
	MaxCoverageEntries    int
	MaxEvidenceBlobs      int
	MaxEvidenceBlobBytes  int
	MaxTotalEvidenceBytes int64
}

// DefaultLimits returns conservative in-process bundle limits.
func DefaultLimits() Limits {
	return Limits{
		MaxDocumentBytes:      4 << 20, // 4 MiB per canonical document
		MaxStringBytes:        1 << 20, // 1 MiB per string
		MaxArrayItems:         100_000,
		MaxFindings:           10_000,
		MaxCoverageEntries:    100_000,
		MaxEvidenceBlobs:      256,
		MaxEvidenceBlobBytes:  10 << 20, // 10 MiB per evidence blob
		MaxTotalEvidenceBytes: 64 << 20, // 64 MiB per bundle
	}
}

// Input is the complete typed input to Build.
type Input struct {
	Manifest ManifestInput
	Findings FindingsInput
	Coverage CoverageInput
	Evidence []EvidenceBlobInput
}

// ManifestInput contains semantic scan metadata plus the volatile run envelope.
// Pointer, slice, and map fields intentionally do not use omitempty: nil and
// explicitly empty values remain distinct in canonical JSON.
type ManifestInput struct {
	SchemaVersion      int
	Repository         RepositoryIdentity
	Target             TargetSnapshot
	ThreatModel        ThreatModelInput
	Quality            QualitySummary
	Versions           ComponentVersions
	OccurrenceIDs      []string
	AssessmentIDs      []string
	StageReceiptIDs    []string
	EvidenceReceiptIDs []string
	Metadata           map[string]string
	Run                RunEnvelope
}

// RepositoryIdentity identifies the stable repository and optional scan scope.
type RepositoryIdentity struct {
	Provider     string  `json:"provider"`
	RepositoryID string  `json:"repositoryId"`
	RepoURL      string  `json:"repoURL"`
	SubPath      *string `json:"subPath"`
}

// TargetSnapshot identifies the immutable repository target used for analysis.
type TargetSnapshot struct {
	CommitSHA     string  `json:"commitSHA"`
	TreeDigest    string  `json:"treeDigest"`
	TargetID      string  `json:"targetId"`
	OriginalRef   *string `json:"originalRef"`
	ReceiptID     string  `json:"receiptId"`
	ReceiptDigest string  `json:"receiptDigest"`
}

// ThreatModelInput is the exact threat model bound into the semantic bundle.
type ThreatModelInput struct {
	Version     string
	Content     string
	Scope       *string
	Assumptions []string
	Limitations []string
}

// QualitySummary contains bounded controller-derived scan quality state.
type QualitySummary struct {
	InventoryCoverage   string `json:"inventoryCoverage"`
	CandidateCoverage   string `json:"candidateCoverage"`
	Coverage            string `json:"coverage"`
	ValidationScope     string `json:"validationScope"`
	ValidationExecution string `json:"validationExecution"`
	AttackPathExecution string `json:"attackPathExecution"`
	AnalysisAttestation string `json:"analysisAttestation"`
	TargetVerification  string `json:"targetVerification"`
	Authorization       string `json:"authorization"`
	Isolation           string `json:"isolation"`
}

// ComponentVersions records the producers and schemas that shaped the bundle.
type ComponentVersions struct {
	Producer       *string           `json:"producer"`
	Schema         string            `json:"schema"`
	Controller     string            `json:"controller"`
	Mapper         *string           `json:"mapper"`
	Policy         *string           `json:"policy"`
	RuntimeAdapter *string           `json:"runtimeAdapter"`
	Additional     map[string]string `json:"additional"`
}

// RunEnvelope contains volatile identity and timestamps. These fields are
// excluded from ContentDigest and included in RunReceiptDigest.
type RunEnvelope struct {
	RunUID                   string
	PublicRunID              *string
	Namespace                string
	RepositoryScanName       string
	RepositoryScanUID        string
	RepositoryScanGeneration int64
	StartedAt                time.Time
	CompletedAt              *time.Time
	SealedAt                 time.Time
}

// FindingsInput is the canonical findings document input.
type FindingsInput struct {
	SchemaVersion int               `json:"schemaVersion"`
	Findings      []Finding         `json:"findings"`
	Metadata      map[string]string `json:"metadata"`
}

// Finding is one finalized occurrence in the canonical findings document.
type Finding struct {
	SemanticFingerprint string              `json:"semanticFingerprint"`
	OccurrenceID        string              `json:"occurrenceId"`
	LegacyFingerprint   *string             `json:"legacyFingerprint"`
	RuleID              string              `json:"ruleId"`
	IdentityAnchor      string              `json:"identityAnchor"`
	IdentityInstance    *string             `json:"identityInstance"`
	Title               string              `json:"title"`
	Summary             *string             `json:"summary"`
	Severity            string              `json:"severity"`
	Confidence          string              `json:"confidence"`
	Locations           []FindingLocation   `json:"locations"`
	Evidence            []EvidenceReference `json:"evidence"`
	AssessmentIDs       []string            `json:"assessmentIds"`
}

// FindingLocation is a normalized repository location.
type FindingLocation struct {
	Path      string  `json:"path"`
	StartLine int     `json:"startLine"`
	EndLine   int     `json:"endLine"`
	Symbol    *string `json:"symbol"`
}

// EvidenceReference links a finding to an immutable evidence blob.
type EvidenceReference struct {
	BlobName  string  `json:"blobName"`
	ReceiptID *string `json:"receiptId"`
	Label     *string `json:"label"`
}

// CoverageInput is the canonical controller-derived coverage document input.
type CoverageInput struct {
	SchemaVersion   int                      `json:"schemaVersion"`
	InventoryStatus string                   `json:"inventoryStatus"`
	CandidateStatus string                   `json:"candidateStatus"`
	CoverageStatus  string                   `json:"coverageStatus"`
	Inventory       []InventoryCoverageEntry `json:"inventory"`
	Candidates      []CandidateCoverageEntry `json:"candidates"`
	Stages          []StageCoverageEntry     `json:"stages"`
	Metadata        map[string]string        `json:"metadata"`
}

// InventoryCoverageEntry accounts for one repository path.
type InventoryCoverageEntry struct {
	Path           string   `json:"path"`
	Classification string   `json:"classification"`
	Reason         *string  `json:"reason"`
	SliceIDs       []string `json:"sliceIds"`
	ReceiptIDs     []string `json:"receiptIds"`
}

// CandidateCoverageEntry accounts for one controller-observed candidate.
type CandidateCoverageEntry struct {
	CandidateID  string   `json:"candidateId"`
	Disposition  string   `json:"disposition"`
	OccurrenceID *string  `json:"occurrenceId"`
	Reason       *string  `json:"reason"`
	ReceiptIDs   []string `json:"receiptIds"`
}

// StageCoverageEntry accounts for one terminal stage scope.
type StageCoverageEntry struct {
	Stage       string  `json:"stage"`
	ScopeID     *string `json:"scopeId"`
	Disposition string  `json:"disposition"`
	ReceiptID   string  `json:"receiptId"`
}

// EvidenceBlobInput supplies immutable evidence bytes. Textual media types are
// validated as UTF-8 and normalized to LF before hashing.
type EvidenceBlobInput struct {
	Name      string
	MediaType string
	Data      []byte
}

// EvidenceBlob is the canonical evidence returned by Build.
type EvidenceBlob struct {
	Name      string
	MediaType string
	Size      int
	Digest    string
	Data      []byte
}

// EvidenceDescriptor is embedded in the manifest and content root.
type EvidenceDescriptor struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Size      int    `json:"size"`
	Digest    string `json:"digest"`
}

// DigestSet contains canonical document and root digests.
type DigestSet struct {
	Manifest         string `json:"manifest"`
	SemanticManifest string `json:"semanticManifest"`
	Findings         string `json:"findings"`
	Coverage         string `json:"coverage"`
	Content          string `json:"content"`
	RunReceipt       string `json:"runReceipt"`
}

// RootDigests models controller-owned root state outside the documents.
type RootDigests struct {
	ContentDigest    string
	RunReceiptDigest string
}

// Bundle contains canonical bytes and immutable evidence. Returned byte slices
// do not alias the caller's input.
type Bundle struct {
	ManifestJSON []byte
	FindingsJSON []byte
	CoverageJSON []byte
	Evidence     []EvidenceBlob
	Roots        RootDigests
}
