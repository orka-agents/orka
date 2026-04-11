package store

import "time"

// ScanRun represents a single repository security scan execution.
type ScanRun struct {
	ID             string     `json:"id"`
	Namespace      string     `json:"namespace"`
	RepositoryScan string     `json:"repositoryScan"`
	TaskName       string     `json:"taskName"`
	Mode           string     `json:"mode"`
	Phase          string     `json:"phase"`
	StartedAt      time.Time  `json:"startedAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	BaseCommit     string     `json:"baseCommit,omitempty"`
	HeadCommit     string     `json:"headCommit,omitempty"`
	CommitCount    int        `json:"commitCount"`
	Summary        string     `json:"summary,omitempty"`
	ErrorMessage   string     `json:"errorMessage,omitempty"`
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
	Kind     string `json:"kind"`
	TaskName string `json:"taskName,omitempty"`
	Name     string `json:"name,omitempty"`
	Label    string `json:"label,omitempty"`
}

// Finding represents a security finding associated with a repository scan.
type Finding struct {
	ID               string               `json:"id"`
	Namespace        string               `json:"namespace"`
	RepositoryScan   string               `json:"repositoryScan"`
	ScanRunID        string               `json:"scanRunID"`
	ScanTaskName     string               `json:"scanTaskName,omitempty"`
	Fingerprint      string               `json:"fingerprint"`
	Title            string               `json:"title"`
	Summary          string               `json:"summary"`
	Severity         string               `json:"severity"`
	Confidence       string               `json:"confidence"`
	ValidationStatus string               `json:"validationStatus"`
	State            string               `json:"state"`
	FilePath         string               `json:"filePath,omitempty"`
	Line             int                  `json:"line,omitempty"`
	CommitSHA        string               `json:"commitSHA,omitempty"`
	RootCause        string               `json:"rootCause,omitempty"`
	Remediation      string               `json:"remediation,omitempty"`
	SuggestedAction  string               `json:"suggestedAction,omitempty"`
	Evidence         []FindingEvidenceRef `json:"evidence,omitempty"`
	ValidationJSON   string               `json:"validationJSON,omitempty"`
	PatchProposalID  string               `json:"patchProposalID,omitempty"`
	PRNumber         *int                 `json:"prNumber,omitempty"`
	PRURL            string               `json:"prURL,omitempty"`
	CreatedAt        time.Time            `json:"createdAt"`
	UpdatedAt        time.Time            `json:"updatedAt"`
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

// FindingFeedback records user feedback for a finding.
type FindingFeedback struct {
	ID        string    `json:"id"`
	Namespace string    `json:"namespace"`
	FindingID string    `json:"findingID"`
	Decision  string    `json:"decision"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
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
	Severity         string
	ValidationStatus string
	State            string
	Recommended      bool
	Limit            int
	Cursor           string
}
