package security

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

const (
	ArtifactScanSummary = "security-scan-summary.json"
	ArtifactThreatModel = "security-threat-model.md"
	ArtifactFindings    = "security-findings.json"
)

// FindingsArtifact captures the v1 security-findings.json payload.
type FindingsArtifact struct {
	Version    int                       `json:"version"`
	Repository FindingsArtifactRepo      `json:"repository"`
	Scan       FindingsArtifactScan      `json:"scan"`
	Findings   []FindingsArtifactFinding `json:"findings"`
}

type FindingsArtifactRepo struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
}

type FindingsArtifactScan struct {
	Mode        string `json:"mode"`
	CommitCount int    `json:"commit_count"`
	Summary     string `json:"summary"`
}

type FindingsArtifactFinding struct {
	Fingerprint      string                     `json:"fingerprint"`
	Title            string                     `json:"title"`
	Summary          string                     `json:"summary"`
	Severity         string                     `json:"severity"`
	Confidence       string                     `json:"confidence"`
	ValidationStatus string                     `json:"validation_status"`
	FilePath         string                     `json:"file_path"`
	Line             int                        `json:"line"`
	CommitSHA        string                     `json:"commit_sha"`
	RootCause        string                     `json:"root_cause"`
	Remediation      string                     `json:"remediation"`
	SuggestedAction  string                     `json:"suggested_action"`
	Evidence         []store.FindingEvidenceRef `json:"evidence"`
}

// ParseRepositoryURL extracts owner and repo name from GitHub URLs.
func ParseRepositoryURL(repoURL string) (owner string, repository string) {
	if strings.HasPrefix(repoURL, "git@") {
		parts := strings.SplitN(strings.TrimPrefix(repoURL, "git@"), ":", 2)
		if len(parts) == 2 {
			repoPath := strings.TrimSuffix(parts[1], ".git")
			segments := strings.Split(strings.Trim(repoPath, "/"), "/")
			if len(segments) >= 2 {
				return segments[0], segments[1]
			}
		}
		return "", ""
	}

	u, err := url.Parse(repoURL)
	if err != nil {
		return "", ""
	}
	segments := strings.Split(strings.Trim(path.Clean(u.Path), "/"), "/")
	if len(segments) < 2 {
		return "", ""
	}
	return segments[0], strings.TrimSuffix(segments[1], ".git")
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func sanitizeName(value string) string {
	value = strings.ToLower(value)
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				out.WriteRune('-')
				lastDash = true
			}
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "security"
	}
	return result
}

// FindingID returns a stable finding identifier derived from its fingerprint.
func FindingID(fingerprint string) string {
	return "fnd_" + shortHash(fingerprint)
}

// ScanRunID returns a scan run ID derived from task identity.
func ScanRunID(taskName string) string {
	return "scan_" + shortHash(taskName)
}

// PatchProposalID returns a stable patch proposal ID for a task.
func PatchProposalID(taskName string) string {
	return "patch_" + shortHash(taskName)
}

// ScanTaskName returns a task name for a scan run.
func ScanTaskName(repositoryScanName, mode string) string {
	return fmt.Sprintf("%s-%s-%d", sanitizeName(repositoryScanName), sanitizeName(mode), time.Now().Unix())
}

// PatchTaskName returns a task name for a patch proposal.
func PatchTaskName(repositoryScanName, findingID string) string {
	return fmt.Sprintf("%s-patch-%s-%d", sanitizeName(repositoryScanName), sanitizeName(findingID), time.Now().Unix())
}

// PatchBranch returns the default branch name for a security patch proposal.
func PatchBranch(findingID string) string {
	return fmt.Sprintf("orka/security/%s", sanitizeName(findingID))
}

// EffectiveProvider returns the configured provider or the v1 default.
func EffectiveProvider(scan *corev1alpha1.RepositoryScan) string {
	if scan.Spec.Provider != "" {
		return scan.Spec.Provider
	}
	return "github"
}

// EffectiveValidationMode returns the configured validation mode or the v1 default.
func EffectiveValidationMode(scan *corev1alpha1.RepositoryScan) string {
	if scan.Spec.ValidationMode != "" {
		return scan.Spec.ValidationMode
	}
	return "light"
}

// EffectiveHistoryDays returns the configured history window or a conservative default.
func EffectiveHistoryDays(scan *corev1alpha1.RepositoryScan) int32 {
	if scan.Spec.HistoryDays != nil && *scan.Spec.HistoryDays > 0 {
		return *scan.Spec.HistoryDays
	}
	return 30
}

// EffectiveMaxFindingsPerRun returns the configured cap or a conservative default.
func EffectiveMaxFindingsPerRun(scan *corev1alpha1.RepositoryScan) int32 {
	if scan.Spec.MaxFindingsPerRun != nil && *scan.Spec.MaxFindingsPerRun > 0 {
		return *scan.Spec.MaxFindingsPerRun
	}
	return 10
}

// EffectiveBranch returns the configured branch or the standard default.
func EffectiveBranch(scan *corev1alpha1.RepositoryScan) string {
	if scan.Spec.Branch != "" {
		return scan.Spec.Branch
	}
	return "main"
}

// IsSuspended returns whether scheduled scans are paused.
func IsSuspended(scan *corev1alpha1.RepositoryScan) bool {
	return scan.Spec.Suspend != nil && *scan.Spec.Suspend
}

// ToFinding converts an artifact finding into a stored finding.
func ToFinding(namespace, repositoryScan, scanRunID string, item FindingsArtifactFinding) *store.Finding {
	validationStatus := item.ValidationStatus
	if validationStatus == "" {
		validationStatus = "unvalidated"
	}
	return &store.Finding{
		ID:               FindingID(item.Fingerprint),
		Namespace:        namespace,
		RepositoryScan:   repositoryScan,
		ScanRunID:        scanRunID,
		Fingerprint:      item.Fingerprint,
		Title:            item.Title,
		Summary:          item.Summary,
		Severity:         item.Severity,
		Confidence:       item.Confidence,
		ValidationStatus: validationStatus,
		State:            "open",
		FilePath:         item.FilePath,
		Line:             item.Line,
		CommitSHA:        item.CommitSHA,
		RootCause:        item.RootCause,
		Remediation:      item.Remediation,
		SuggestedAction:  item.SuggestedAction,
		Evidence:         item.Evidence,
	}
}

// BuildScanPrompt returns the prompt for repository scan tasks.
func BuildScanPrompt(scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit, threatModel string) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "You are running a repository security scan for %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	fmt.Fprintf(&prompt, "Mode: %s\n", mode)
	fmt.Fprintf(&prompt, "Provider: %s\n", EffectiveProvider(scan))
	fmt.Fprintf(&prompt, "Validation mode: %s\n", EffectiveValidationMode(scan))
	fmt.Fprintf(&prompt, "History window: %d days\n", EffectiveHistoryDays(scan))
	fmt.Fprintf(&prompt, "Max findings: %d\n", EffectiveMaxFindingsPerRun(scan))
	if scan.Spec.SubPath != "" {
		fmt.Fprintf(&prompt, "Sub-path focus: %s\n", scan.Spec.SubPath)
	}
	if baseCommit != "" || headCommit != "" {
		fmt.Fprintf(&prompt, "Commit focus: base=%s head=%s\n", baseCommit, headCommit)
	}
	prompt.WriteString("\nTasks:\n")
	prompt.WriteString("1. Inspect the current repository state and recent commit history for likely vulnerabilities.\n")
	prompt.WriteString("2. Generate or update a concise threat model.\n")
	prompt.WriteString("3. Prefer high-confidence findings over speculative broad coverage.\n")
	prompt.WriteString("4. Validate findings only when safe and practical.\n")
	prompt.WriteString("5. Do not edit code, commit, or push during scan runs.\n")
	prompt.WriteString("\nWrite artifacts under /tmp/artifacts using these exact filenames:\n")
	prompt.WriteString("- security-scan-summary.json\n")
	prompt.WriteString("- security-threat-model.md\n")
	prompt.WriteString("- security-findings.json\n")
	prompt.WriteString("Optional validation artifacts may use flat filenames like security-validation-<finding-id>.txt and .json.\n")
	prompt.WriteString("Keep security-findings.json compact and avoid large code excerpts.\n")
	if strings.TrimSpace(threatModel) != "" {
		prompt.WriteString("\nExisting threat model context:\n")
		prompt.WriteString(threatModel)
		prompt.WriteString("\n")
	}
	return prompt.String()
}

// BuildPatchPrompt returns the prompt for patch proposal tasks.
func BuildPatchPrompt(scan *corev1alpha1.RepositoryScan, finding *store.Finding) string {
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Generate a minimal security patch for repository %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	fmt.Fprintf(&prompt, "Finding ID: %s\nTitle: %s\nSeverity: %s\nConfidence: %s\n", finding.ID, finding.Title, finding.Severity, finding.Confidence)
	if finding.FilePath != "" {
		fmt.Fprintf(&prompt, "Primary file: %s:%d\n", finding.FilePath, finding.Line)
	}
	if finding.RootCause != "" {
		fmt.Fprintf(&prompt, "Root cause: %s\n", finding.RootCause)
	}
	if finding.Remediation != "" {
		fmt.Fprintf(&prompt, "Remediation guidance: %s\n", finding.Remediation)
	}
	prompt.WriteString("\nRequirements:\n")
	prompt.WriteString("1. Fix only this finding.\n")
	prompt.WriteString("2. Keep the diff as small and reviewable as possible.\n")
	prompt.WriteString("3. Preserve existing behavior unless the vulnerability requires a behavior change.\n")
	prompt.WriteString("4. Run focused tests when available.\n")
	prompt.WriteString("5. Do not open a pull request directly.\n")
	prompt.WriteString("\nWrite these artifacts under /tmp/artifacts:\n")
	fmt.Fprintf(&prompt, "- security-patch-%s.diff\n", finding.ID)
	fmt.Fprintf(&prompt, "- security-patch-%s.json\n", finding.ID)
	return prompt.String()
}
