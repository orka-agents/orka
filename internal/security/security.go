package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

const (
	ArtifactThreatModel    = "security-threat-model.md"
	ArtifactFindings       = "security-findings.json"
	ArtifactValidation     = "security-validation.json"
	ArtifactValidationText = "security-validation.txt"
	// ArtifactWorkspaceDir is the repo-root symlink the worker exposes for
	// writing security artifacts from inside the agent workspace.
	ArtifactWorkspaceDir = ".orka-artifacts"
	maxGeneratedTaskName = 63
)

const (
	StageCombined    = "combined"
	StageThreatModel = "threat-model"
	StageDiscovery   = "discovery"
	StageValidation  = "validation"
	StagePatch       = "patch"
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
	Fingerprint      string                       `json:"fingerprint"`
	Title            string                       `json:"title"`
	Summary          string                       `json:"summary"`
	Severity         string                       `json:"severity"`
	Confidence       string                       `json:"confidence"`
	ValidationStatus string                       `json:"validation_status"`
	FilePath         string                       `json:"file_path"`
	Line             int                          `json:"line"`
	CommitSHA        string                       `json:"commit_sha"`
	RootCause        string                       `json:"root_cause"`
	Remediation      string                       `json:"remediation"`
	SuggestedAction  string                       `json:"suggested_action"`
	Evidence         FindingsArtifactEvidenceRefs `json:"evidence"`
}

// ValidationArtifact captures the per-finding validator/repro payload.
type ValidationArtifact struct {
	Version            int                          `json:"version"`
	FindingID          string                       `json:"finding_id"`
	Status             string                       `json:"status"`
	Summary            string                       `json:"summary"`
	ValidationSteps    []string                     `json:"validation_steps,omitempty"`
	Reproduction       string                       `json:"reproduction,omitempty"`
	AttackPathAnalysis string                       `json:"attack_path_analysis,omitempty"`
	Likelihood         string                       `json:"likelihood,omitempty"`
	Impact             string                       `json:"impact,omitempty"`
	Assumptions        []string                     `json:"assumptions,omitempty"`
	Controls           []string                     `json:"controls,omitempty"`
	Blindspots         []string                     `json:"blindspots,omitempty"`
	Evidence           FindingsArtifactEvidenceRefs `json:"evidence,omitempty"`
}

// DiscoveryScope describes a focused discovery lens for an independent finding task.
type DiscoveryScope struct {
	Name         string
	Label        string
	Instructions string
}

// DiscoveryScopes returns the default independent finding lenses for a scan.
func DiscoveryScopes() []DiscoveryScope {
	return []DiscoveryScope{
		{
			Name:         "app-logic-inputs",
			Label:        "Application logic, input handling, and injection risk",
			Instructions: "Focus on untrusted inputs, parser edges, command execution, template use, unsafe file access, path traversal, SSRF, and code paths where user or repo-controlled data changes behavior.",
		},
		{
			Name:         "auth-secrets-privilege",
			Label:        "Authentication, secrets, and privilege boundaries",
			Instructions: "Focus on auth flows, token handling, session trust, key or secret exposure, privilege escalation, entitlement scope, and components that cross trust boundaries or run with elevated rights.",
		},
		{
			Name:         "ci-cd-supply-chain",
			Label:        "CI/CD, release, and supply-chain surfaces",
			Instructions: "Focus on GitHub Actions, release scripts, packaging, update channels, build pipelines, artifact integrity, dependency execution, and automation that could be influenced by untrusted inputs.",
		},
		{
			Name:         "data-exposure-logging",
			Label:        "Data exposure, privacy, and logging",
			Instructions: "Focus on logging, telemetry, diagnostics, debug tooling, persistence, caches, and any paths that might leak secrets, tokens, sensitive user data, or internal trust assumptions.",
		},
		{
			Name:         "recent-commits-history",
			Label:        "Recent commits and security-relevant history",
			Instructions: "Focus on the commit range in scope, newly introduced trust-boundary changes, security regressions, missing validation, and risky deltas that matter more than older architectural issues.",
		},
	}
}

// FindingsArtifactEvidenceRefs accepts either the structured v1 evidence array
// or a legacy shorthand string/object and normalizes it to evidence refs.
type FindingsArtifactEvidenceRefs []store.FindingEvidenceRef

func (e *FindingsArtifactEvidenceRefs) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "", trimmed == "null":
		*e = nil
		return nil
	case strings.HasPrefix(trimmed, `"`):
		ref, ok, err := findingsArtifactEvidenceRefFromJSON(data)
		if err != nil {
			return err
		}
		if !ok {
			*e = nil
			return nil
		}
		*e = FindingsArtifactEvidenceRefs{ref}
		return nil
	case strings.HasPrefix(trimmed, "{"):
		ref, _, err := findingsArtifactEvidenceRefFromJSON(data)
		if err != nil {
			return err
		}
		*e = FindingsArtifactEvidenceRefs{ref}
		return nil
	default:
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		refs := make([]store.FindingEvidenceRef, 0, len(items))
		for _, item := range items {
			ref, ok, err := findingsArtifactEvidenceRefFromJSON(item)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			refs = append(refs, ref)
		}
		*e = FindingsArtifactEvidenceRefs(refs)
		return nil
	}
}

func findingsArtifactEvidenceRefFromJSON(data []byte) (store.FindingEvidenceRef, bool, error) {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "", trimmed == "null":
		return store.FindingEvidenceRef{}, false, nil
	case strings.HasPrefix(trimmed, `"`):
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return store.FindingEvidenceRef{}, false, err
		}
		if strings.TrimSpace(text) == "" {
			return store.FindingEvidenceRef{}, false, nil
		}
		return store.FindingEvidenceRef{Kind: "note", Label: text}, true, nil
	default:
		var ref store.FindingEvidenceRef
		if err := json.Unmarshal(data, &ref); err != nil {
			return store.FindingEvidenceRef{}, false, err
		}
		return ref, true, nil
	}
}

// ParseRepositoryURL extracts owner and repo name from GitHub URLs.
func ParseRepositoryURL(repoURL string) (owner string, repository string) {
	if trimmed, ok := strings.CutPrefix(repoURL, "git@"); ok {
		parts := strings.SplitN(trimmed, ":", 2)
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

// ParseGitHubRepositoryURL validates a credential-free GitHub repository URL
// and returns its owner and repository name.
func ParseGitHubRepositoryURL(repoURL string) (owner string, repository string, err error) {
	value := strings.TrimSpace(repoURL)
	if value == "" {
		return "", "", fmt.Errorf("repository URL is required")
	}
	if after, ok := strings.CutPrefix(value, "git@"); ok {
		trimmed := after
		host, repoPath, ok := strings.Cut(trimmed, ":")
		if !ok || !strings.EqualFold(host, "github.com") {
			return "", "", fmt.Errorf("repository URL must be a GitHub repository URL")
		}
		return githubOwnerRepoFromPath(repoPath)
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", fmt.Errorf("repository URL must be a valid GitHub repository URL")
	}
	if parsed.User != nil {
		return "", "", fmt.Errorf("repository URL must not include credentials")
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("repository URL must be a GitHub repository URL")
	}
	return githubOwnerRepoFromPath(parsed.Path)
}

func githubOwnerRepoFromPath(repoPath string) (string, string, error) {
	repoPath = strings.Trim(repoPath, "/")
	segments := strings.Split(strings.TrimSuffix(repoPath, ".git"), "/")
	if len(segments) != 2 || !githubRepositoryPathSegmentIsSafe(segments[0]) || !githubRepositoryPathSegmentIsSafe(segments[1]) {
		return "", "", fmt.Errorf("repository URL must include GitHub owner and repository")
	}
	return segments[0], segments[1], nil
}

func githubRepositoryPathSegmentIsSafe(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
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
	return boundedTaskName(
		sanitizeName(repositoryScanName),
		sanitizeName(mode),
		fmt.Sprintf("%d", time.Now().Unix()),
	)
}

// ScanStageTaskName returns a task name for a specific scan stage and optional scope.
func ScanStageTaskName(repositoryScanName, mode, stage, scope string) string {
	parts := []string{sanitizeName(repositoryScanName), sanitizeName(mode), sanitizeName(stage)}
	if strings.TrimSpace(scope) != "" {
		parts = append(parts, sanitizeName(scope))
	}
	parts = append(parts, fmt.Sprintf("%d", time.Now().Unix()))
	return boundedTaskName(parts...)
}

// PatchTaskName returns a task name for a patch proposal.
func PatchTaskName(repositoryScanName, findingID string) string {
	return boundedTaskName(
		sanitizeName(repositoryScanName),
		"patch",
		sanitizeName(findingID),
		fmt.Sprintf("%d", time.Now().Unix()),
	)
}

// PatchBranch returns the default branch name for a security patch proposal.
func PatchBranch(findingID, taskName string) string {
	return fmt.Sprintf("orka/security/%s-%s", sanitizeName(findingID), shortHash(taskName))
}

func boundedTaskName(parts ...string) string {
	base := strings.Join(parts, "-")
	if len(base) <= maxGeneratedTaskName {
		return base
	}

	visibleParts := parts
	if len(visibleParts) > 3 {
		visibleParts = visibleParts[:3]
	}
	visible := strings.Join(visibleParts, "-")
	hash := shortHash(base)
	maxVisible := max(maxGeneratedTaskName-len(hash)-1, 1)
	if len(visible) > maxVisible {
		visible = strings.Trim(visible[:maxVisible], "-")
		if visible == "" {
			visible = "security"
		}
	}

	return visible + "-" + hash
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

// DiscoveryMaxFindingsPerScope keeps the per-agent scope output bounded while
// still allowing a few strong findings from each lens.
func DiscoveryMaxFindingsPerScope(scan *corev1alpha1.RepositoryScan) int32 {
	total := EffectiveMaxFindingsPerRun(scan)
	scopeCount := int32(len(DiscoveryScopes()))
	if scopeCount <= 0 {
		return total
	}
	perScope := total / scopeCount
	if total%scopeCount != 0 {
		perScope++
	}
	if perScope < 2 {
		perScope = 2
	}
	return perScope
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

func withTaskNameEvidenceRefs(refs FindingsArtifactEvidenceRefs, taskName string) []store.FindingEvidenceRef {
	out := make([]store.FindingEvidenceRef, 0, len(refs))
	for _, ref := range refs {
		normalized := ref
		if normalized.Kind == "artifact" && normalized.TaskName == "" {
			normalized.TaskName = taskName
		}
		out = append(out, normalized)
	}
	return out
}

// ToFinding converts an artifact finding into a stored finding.
func ToFinding(namespace, repositoryScan, scanRunID, taskName string, item FindingsArtifactFinding) *store.Finding {
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
		Evidence:         withTaskNameEvidenceRefs(item.Evidence, taskName),
	}
}

// ArtifactWorkspacePath returns the relative path from the agent working
// directory to the repo-root artifacts symlink.
func ArtifactWorkspacePath(subPath string) string {
	cleaned := strings.TrimSpace(strings.ReplaceAll(subPath, "\\", "/"))
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "" {
		return ArtifactWorkspaceDir
	}

	cleaned = path.Clean(cleaned)
	if cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "..") {
		return ArtifactWorkspaceDir
	}

	depth := 0
	for segment := range strings.SplitSeq(cleaned, "/") {
		if segment == "" || segment == "." {
			continue
		}
		depth++
	}
	if depth == 0 {
		return ArtifactWorkspaceDir
	}
	return strings.Repeat("../", depth) + ArtifactWorkspaceDir
}

// BuildThreatModelPrompt returns the prompt for the threat-model-first stage of a scan run.
func BuildThreatModelPrompt(scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit, threatModel string) string {
	var prompt strings.Builder
	artifactDir := ArtifactWorkspacePath(scan.Spec.SubPath)
	hasExistingThreatModel := strings.TrimSpace(threatModel) != ""

	fmt.Fprintf(&prompt, "You are generating the canonical repository threat model for %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	fmt.Fprintf(&prompt, "Scan mode: %s\n", mode)
	fmt.Fprintf(&prompt, "Validation mode: %s\n", EffectiveValidationMode(scan))
	fmt.Fprintf(&prompt, "History window: %d days\n", EffectiveHistoryDays(scan))
	if scan.Spec.SubPath != "" {
		fmt.Fprintf(&prompt, "Sub-path focus: %s\n", scan.Spec.SubPath)
	}
	if baseCommit != "" || headCommit != "" {
		fmt.Fprintf(&prompt, "Commit focus: base=%s head=%s\n", baseCommit, headCommit)
	}

	prompt.WriteString("\nYour only job in this stage is to understand the repository and produce a strong, reusable threat model.\n")
	prompt.WriteString("Do not create findings in this stage. Do not edit code, commit, or push.\n")
	prompt.WriteString("Ground the model in the actual repository structure, workflows, secrets, auth paths, network boundaries, privileged components, and attack surfaces you can support from the repo.\n")
	prompt.WriteString("\nThreat model requirements for security-threat-model.md:\n")
	prompt.WriteString("- Produce a substantial, engineering-grade markdown document that future finding agents can use as shared context.\n")
	prompt.WriteString("- Include these sections when applicable:\n")
	prompt.WriteString("  1. System Overview and deployment/runtime context\n")
	prompt.WriteString("  2. Key Assets, Trust Boundaries, and sensitive operations\n")
	prompt.WriteString("  3. Attacker-controlled inputs, operator-controlled inputs, and assumptions\n")
	prompt.WriteString("  4. Security-relevant data flows and entry points\n")
	prompt.WriteString("  5. Attack surface and existing mitigations by subsystem/component\n")
	prompt.WriteString("  6. Concrete attacker stories or abuse cases tied to this repository\n")
	prompt.WriteString("  7. Non-applicable or low-relevance vulnerability classes when helpful\n")
	prompt.WriteString("  8. Criticality calibration for what would count as critical, high, medium, and low impact here\n")
	if mode == "incremental" || mode == "manual" {
		prompt.WriteString("- Include a short section on security-relevant change analysis for the commits in scope and explain what changed versus what remains unchanged.\n")
	}
	prompt.WriteString("- Call out important uncertainties explicitly instead of inventing details.\n")
	if hasExistingThreatModel {
		prompt.WriteString("- Treat the existing threat model as baseline context to refine and extend. Do not replace it with a shorter version unless the repository is genuinely tiny.\n")
	}
	fmt.Fprintf(&prompt, "\nWrite these artifacts under %s/:\n", artifactDir)
	fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, ArtifactThreatModel)
	appendRequiredArtifactsDirective(&prompt, ArtifactThreatModel)
	prompt.WriteString("The stage will be treated as failed if the threat model artifact is missing or empty.\n")
	prompt.WriteString("Prefer Bash heredocs or shell redirection when writing artifact files so they are persisted on disk.\n")
	if hasExistingThreatModel {
		prompt.WriteString("\nExisting threat model context:\n")
		prompt.WriteString(threatModel)
		prompt.WriteString("\n")
	}
	return prompt.String()
}

// BuildDiscoveryPrompt returns the prompt for an independent finding-discovery stage.
func BuildDiscoveryPrompt(scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit, threatModel string, scope DiscoveryScope) string {
	var prompt strings.Builder
	artifactDir := ArtifactWorkspacePath(scan.Spec.SubPath)

	fmt.Fprintf(&prompt, "You are an independent security finding agent for %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	fmt.Fprintf(&prompt, "Scan mode: %s\n", mode)
	fmt.Fprintf(&prompt, "Discovery scope: %s\n", scope.Label)
	fmt.Fprintf(&prompt, "Validation mode: %s\n", EffectiveValidationMode(scan))
	fmt.Fprintf(&prompt, "Max findings for this scope: %d\n", DiscoveryMaxFindingsPerScope(scan))
	if scan.Spec.SubPath != "" {
		fmt.Fprintf(&prompt, "Sub-path focus: %s\n", scan.Spec.SubPath)
	}
	if baseCommit != "" || headCommit != "" {
		fmt.Fprintf(&prompt, "Commit focus: base=%s head=%s\n", baseCommit, headCommit)
	}

	prompt.WriteString("\nYour job in this stage is to discover candidate findings only for the assigned security lens.\n")
	prompt.WriteString("Scope instructions: ")
	prompt.WriteString(scope.Instructions)
	prompt.WriteString("\n")
	prompt.WriteString("Reuse the shared threat model below as canonical repository context.\n")
	prompt.WriteString("Do not rewrite the threat model in this stage. Do not edit code, commit, or push.\n")
	prompt.WriteString("Prefer a small number of high-signal findings over broad speculation.\n")
	prompt.WriteString("Do not do deep reproduction here; reserve heavy validation and repro work for the validator agent.\n")
	prompt.WriteString("If you cannot support a claim from the code or recent commits, omit it.\n")

	fmt.Fprintf(&prompt, "\nWrite these artifacts under %s/:\n", artifactDir)
	fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, ArtifactFindings)
	appendRequiredArtifactsDirective(&prompt, ArtifactFindings)
	prompt.WriteString("The stage will be treated as failed if the findings artifact is missing, empty, or invalid.\n")
	fmt.Fprintf(&prompt, "Even when this scope has zero findings, still write %s/%s with valid JSON and an empty findings array.\n", artifactDir, ArtifactFindings)
	prompt.WriteString("Prefer Bash heredocs or shell redirection when writing artifact files so they are persisted on disk.\n")
	prompt.WriteString("security-findings.json must be valid JSON with this top-level shape:\n")
	prompt.WriteString(`{"version":1,"repository":{"repo_url":"...","branch":"...","head_sha":"...","base_sha":"..."},"scan":{"mode":"initial|incremental|manual","commit_count":0,"summary":"..."},"findings":[]}` + "\n")
	prompt.WriteString("Each finding object must use these keys: fingerprint, title, summary, severity, confidence, validation_status, file_path, line, commit_sha, root_cause, remediation, suggested_action, evidence.\n")
	prompt.WriteString("Set validation_status to unvalidated unless you safely and directly validated the issue in this stage.\n")
	prompt.WriteString("Keep security-findings.json compact and avoid large code excerpts.\n")
	if strings.TrimSpace(threatModel) != "" {
		prompt.WriteString("\nShared threat model context:\n")
		prompt.WriteString(threatModel)
		prompt.WriteString("\n")
	}
	return prompt.String()
}

// BuildValidationPrompt returns the prompt for the dedicated validator/repro stage for a finding.
func BuildValidationPrompt(scan *corev1alpha1.RepositoryScan, finding *store.Finding) string {
	var prompt strings.Builder
	artifactDir := ArtifactWorkspacePath(scan.Spec.SubPath)

	fmt.Fprintf(&prompt, "You are validating and, when safe, attempting to reproduce a single security finding for %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	fmt.Fprintf(&prompt, "Finding ID: %s\n", finding.ID)
	fmt.Fprintf(&prompt, "Title: %s\n", finding.Title)
	fmt.Fprintf(&prompt, "Severity: %s\n", finding.Severity)
	fmt.Fprintf(&prompt, "Confidence: %s\n", finding.Confidence)
	if finding.FilePath != "" {
		fmt.Fprintf(&prompt, "Primary location: %s:%d\n", finding.FilePath, finding.Line)
	}
	if finding.CommitSHA != "" {
		fmt.Fprintf(&prompt, "Commit: %s\n", finding.CommitSHA)
	}
	if finding.RootCause != "" {
		fmt.Fprintf(&prompt, "Root cause hypothesis: %s\n", finding.RootCause)
	}
	if finding.Remediation != "" {
		fmt.Fprintf(&prompt, "Suggested remediation: %s\n", finding.Remediation)
	}
	prompt.WriteString("\nRequirements:\n")
	prompt.WriteString("1. Validate only this finding. Do not look for unrelated vulnerabilities.\n")
	prompt.WriteString("2. Prefer safe, focused reproduction steps. Do not perform destructive actions.\n")
	prompt.WriteString("3. Tighten or lower confidence when the code or environment does not support the original claim.\n")
	prompt.WriteString("4. Capture a concrete attack-path analysis for how the issue could be exploited, what assumptions it depends on, and which controls already limit it.\n")
	prompt.WriteString("5. Do not edit code, commit, or push during validation.\n")

	fmt.Fprintf(&prompt, "\nWrite these artifacts under %s/:\n", artifactDir)
	fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, ArtifactValidation)
	fmt.Fprintf(&prompt, "- %s/%s (optional but strongly preferred)\n", artifactDir, ArtifactValidationText)
	appendRequiredArtifactsDirective(&prompt, ArtifactValidation)
	prompt.WriteString("Prefer Bash heredocs or shell redirection when writing artifact files so they are persisted on disk.\n")
	prompt.WriteString("security-validation.json must be valid JSON with this shape:\n")
	prompt.WriteString(`{"version":1,"finding_id":"fnd_...","status":"validated|failed|skipped","summary":"...","validation_steps":["..."],"reproduction":"...","attack_path_analysis":"...","likelihood":"...","impact":"...","assumptions":["..."],"controls":["..."],"blindspots":["..."],"evidence":[]}` + "\n")
	prompt.WriteString("Use status=validated when the code path and validation strongly support the issue.\n")
	prompt.WriteString("Use status=failed when the original claim does not hold after review or reproduction attempts.\n")
	prompt.WriteString("Use status=skipped when the environment or safety constraints prevent meaningful validation.\n")
	prompt.WriteString("If you create additional evidence or transcript artifacts, reference them in the evidence array.\n")
	return prompt.String()
}

func appendRequiredArtifactsDirective(prompt *strings.Builder, artifacts ...string) {
	if len(artifacts) == 0 {
		return
	}
	prompt.WriteString("REQUIRED_SECURITY_ARTIFACTS: ")
	prompt.WriteString(strings.Join(artifacts, ", "))
	prompt.WriteString("\n")
}

// BuildPatchPrompt returns the prompt for patch proposal tasks.
func BuildPatchPrompt(scan *corev1alpha1.RepositoryScan, finding *store.Finding, patchBranch string) string {
	var prompt strings.Builder
	artifactDir := ArtifactWorkspacePath(scan.Spec.SubPath)
	fmt.Fprintf(&prompt, "Generate a minimal security patch for repository %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	if strings.TrimSpace(patchBranch) != "" {
		fmt.Fprintf(&prompt, "Orka will push the final diff to patch branch %s after the task finishes.\n", patchBranch)
	}
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
	prompt.WriteString("2. Apply the fix directly to the checked-out workspace files. Do not stop at a diff artifact or a written description.\n")
	prompt.WriteString("3. Keep the code diff as small and reviewable as possible.\n")
	prompt.WriteString("4. Preserve existing behavior unless the vulnerability requires a behavior change.\n")
	prompt.WriteString("5. Run focused tests when available.\n")
	prompt.WriteString("6. The diff artifact must match the actual workspace changes after your edit.\n")
	prompt.WriteString("7. Do not commit, push, or open a pull request directly. Leave the final file changes in the workspace so Orka can create the commit and push it to the patch branch automatically.\n")
	fmt.Fprintf(&prompt, "\nWrite these artifacts under %s/:\n", artifactDir)
	fmt.Fprintf(&prompt, "- %s/security-patch-%s.diff\n", artifactDir, finding.ID)
	fmt.Fprintf(&prompt, "- %s/security-patch-%s.json\n", artifactDir, finding.ID)
	prompt.WriteString("Prefer Bash heredocs or shell redirection when writing artifact files so they are persisted on disk.\n")
	return prompt.String()
}
