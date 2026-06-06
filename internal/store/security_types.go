package store

import "time"

// ScanRun represents a single repository security scan execution.
type ScanRun struct {
	ID                 string     `json:"id"`
	Namespace          string     `json:"namespace"`
	RepositoryScan     string     `json:"repositoryScan"`
	TaskName           string     `json:"taskName"`
	Mode               string     `json:"mode"`
	Phase              string     `json:"phase"`
	StartedAt          time.Time  `json:"startedAt"`
	CompletedAt        *time.Time `json:"completedAt,omitempty"`
	BaseCommit         string     `json:"baseCommit,omitempty"`
	HeadCommit         string     `json:"headCommit,omitempty"`
	CommitCount        int        `json:"commitCount"`
	SliceCount         int        `json:"sliceCount"`
	ReviewedSliceCount int        `json:"reviewedSliceCount"`
	SkippedSliceCount  int        `json:"skippedSliceCount"`
	AcceptedFindings   int        `json:"acceptedFindings"`
	DroppedFindings    int        `json:"droppedFindings"`
	Summary            string     `json:"summary,omitempty"`
	ErrorMessage       string     `json:"errorMessage,omitempty"`
}

// ThreatModel stores the latest generated or user-edited threat model.
type ThreatModel struct {
	Namespace       string    `json:"namespace"`
	RepositoryScan  string    `json:"repositoryScan"`
	Version         int64     `json:"version"`
	Content         string    `json:"content"`
	Source          string    `json:"source"`
	GeneratedByScan string    `json:"generatedByScan,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// FindingEvidenceRef points to supporting evidence for a finding.
type FindingEvidenceRef struct {
	Kind      string `json:"kind"`
	TaskName  string `json:"taskName,omitempty"`
	Name      string `json:"name,omitempty"`
	Label     string `json:"label,omitempty"`
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Quote     string `json:"quote,omitempty"`
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
	SchemaVersion   int               `json:"schemaVersion"`
	ID              string            `json:"id"`
	Namespace       string            `json:"namespace,omitempty"`
	RepositoryScan  string            `json:"repositoryScan"`
	Source          string            `json:"source"`
	Title           string            `json:"title"`
	Summary         string            `json:"summary"`
	Kind            string            `json:"kind"`
	Entrypoints     []ReviewSliceFile `json:"entrypoints,omitempty"`
	OwnedFiles      []ReviewSliceFile `json:"ownedFiles,omitempty"`
	ContextFiles    []ReviewSliceFile `json:"contextFiles,omitempty"`
	Tests           []ReviewSliceTest `json:"tests,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	TrustBoundaries []string          `json:"trustBoundaries,omitempty"`
	Confidence      string            `json:"confidence"`
	Status          string            `json:"status"`
	LastScanRunID   string            `json:"lastScanRunID,omitempty"`
	LastReviewedAt  *time.Time        `json:"lastReviewedAt,omitempty"`
	CreatedAt       time.Time         `json:"createdAt,omitempty"`
	UpdatedAt       time.Time         `json:"updatedAt,omitempty"`
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
	SampleJSON     string    `json:"sampleJSON,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// DroppedFindingFilter constrains dropped finding diagnostic queries.
type DroppedFindingFilter struct {
	Namespace      string
	RepositoryScan string
	ScanRunID      string
	SliceID        string
	Limit          int
	Cursor         string
}
