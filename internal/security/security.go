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
	ArtifactValidation     = "security-validation.json"
	ArtifactValidationText = "security-validation.txt"
	// ArtifactWorkspaceDir is the repo-root symlink the worker exposes for
	// writing security artifacts from inside the agent workspace.
	ArtifactWorkspaceDir = ".orka-artifacts"
	maxGeneratedTaskName = 63
)

const (
	StageThreatModel = "threat-model"
	StageMapper      = "mapper"
	StageReview      = "review"
	StageValidation  = "validation"
	StagePatch       = "patch"
)

// ValidationArtifact captures the per-finding validator/repro payload.
type ValidationArtifact struct {
	Version            int                            `json:"version"`
	FindingID          string                         `json:"finding_id"`
	Status             string                         `json:"status"`
	Summary            string                         `json:"summary"`
	ValidationSteps    []string                       `json:"validation_steps,omitempty"`
	Reproduction       string                         `json:"reproduction,omitempty"`
	AttackPathAnalysis string                         `json:"attack_path_analysis,omitempty"`
	Likelihood         string                         `json:"likelihood,omitempty"`
	Impact             string                         `json:"impact,omitempty"`
	Assumptions        []string                       `json:"assumptions,omitempty"`
	Controls           []string                       `json:"controls,omitempty"`
	Blindspots         []string                       `json:"blindspots,omitempty"`
	Evidence           ValidationArtifactEvidenceRefs `json:"evidence,omitempty"`
}

// ValidationArtifactEvidenceRefs accepts the structured validation evidence
// array and the existing shorthand forms used by validation agents.
type ValidationArtifactEvidenceRefs []store.FindingEvidenceRef

func (e *ValidationArtifactEvidenceRefs) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	switch {
	case trimmed == "", trimmed == "null":
		*e = nil
		return nil
	case strings.HasPrefix(trimmed, `"`):
		ref, ok, err := validationArtifactEvidenceRefFromJSON(data)
		if err != nil {
			return err
		}
		if !ok {
			*e = nil
			return nil
		}
		*e = ValidationArtifactEvidenceRefs{ref}
		return nil
	case strings.HasPrefix(trimmed, "{"):
		ref, _, err := validationArtifactEvidenceRefFromJSON(data)
		if err != nil {
			return err
		}
		*e = ValidationArtifactEvidenceRefs{ref}
		return nil
	default:
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		refs := make([]store.FindingEvidenceRef, 0, len(items))
		for _, item := range items {
			ref, ok, err := validationArtifactEvidenceRefFromJSON(item)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			refs = append(refs, ref)
		}
		*e = ValidationArtifactEvidenceRefs(refs)
		return nil
	}
}

func validationArtifactEvidenceRefFromJSON(data []byte) (store.FindingEvidenceRef, bool, error) {
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

// EffectiveValidationMode returns the configured validation mode or the default.
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

// EffectiveRef returns the configured checkout ref, if any.
func EffectiveRef(scan *corev1alpha1.RepositoryScan) string {
	return strings.TrimSpace(scan.Spec.Ref)
}

// EffectiveWorkspaceBranch returns the branch to pass to git clone for scan workspaces.
// Ref-only scans must not force the default branch before the worker can check out the ref.
func EffectiveWorkspaceBranch(scan *corev1alpha1.RepositoryScan) string {
	if scan.Spec.Branch != "" {
		return scan.Spec.Branch
	}
	if EffectiveRef(scan) != "" {
		return ""
	}
	return EffectiveBranch(scan)
}

// IsSuspended returns whether scheduled scans are paused.
func IsSuspended(scan *corev1alpha1.RepositoryScan) bool {
	return scan.Spec.Suspend != nil && *scan.Spec.Suspend
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

// BuildReviewPrompt returns the prompt for one deterministic review slice.
func BuildReviewPrompt(scan *corev1alpha1.RepositoryScan, mode, baseCommit, headCommit, threatModel string, slice store.ReviewSlice) string {
	var prompt strings.Builder
	artifactDir := ArtifactWorkspacePath(scan.Spec.SubPath)
	contextArtifact := ReviewContextArtifactName(slice.ID)
	sliceJSON, err := json.MarshalIndent(slice, "", "  ")
	if err != nil {
		sliceJSON = []byte("{}")
	}

	fmt.Fprintf(&prompt, "You are reviewing one deterministic security slice for %s on branch %s.\n", scan.Spec.RepoURL, EffectiveBranch(scan))
	fmt.Fprintf(&prompt, "Scan mode: %s\n", mode)
	fmt.Fprintf(&prompt, "Slice ID: %s\n", slice.ID)
	fmt.Fprintf(&prompt, "Slice title: %s\n", slice.Title)
	fmt.Fprintf(&prompt, "Slice kind: %s\n", slice.Kind)
	fmt.Fprintf(&prompt, "Max findings for this slice: %d\n", min(EffectiveMaxFindingsPerRun(scan), 3))
	if scan.Spec.SubPath != "" {
		fmt.Fprintf(&prompt, "Sub-path focus: %s\n", scan.Spec.SubPath)
	}
	if baseCommit != "" || headCommit != "" {
		fmt.Fprintf(&prompt, "Commit focus: base=%s head=%s\n", baseCommit, headCommit)
	}

	prompt.WriteString("\nYour job in this stage is to review only the bounded slice below and produce evidence-backed findings.\n")
	prompt.WriteString("Do not rewrite the threat model. Do not edit code, commit, push, or create pull requests.\n")
	prompt.WriteString("Inspect owned files first, then context files and tests. Avoid unrelated repository exploration unless absolutely necessary to understand a cited line.\n")
	prompt.WriteString("Prefer a small number of high-signal findings over broad speculation. If you cannot support a claim from the included slice files, omit it.\n")
	prompt.WriteString("Every finding must cite repo-relative file evidence with startLine and endLine from the files recorded in the review context manifest.\n")
	prompt.WriteString("Quote fields are optional; use them only when you can copy the cited file text exactly.\n")

	fmt.Fprintf(&prompt, "\nWrite these artifacts under %s/:\n", artifactDir)
	fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, ArtifactFindingsV2)
	appendRequiredArtifactsDirective(&prompt, ArtifactFindingsV2)
	prompt.WriteString("The stage will be treated as failed if the findings artifact is missing, empty, or invalid.\n")
	prompt.WriteString("Prefer Bash heredocs or shell redirection when writing artifact files so they are persisted on disk.\n")

	fmt.Fprintf(&prompt, "\nOrka generated and will upload %s before and after model execution.\n", contextArtifact)
	prompt.WriteString("Do not create, edit, or replace the review context manifest. Findings that cite paths or line ranges outside the generated manifest will be dropped.\n")

	prompt.WriteString("\nsecurity-findings.v2.json must be valid JSON with this top-level shape:\n")
	prompt.WriteString(`{"schemaVersion":2,"repository":{"repoURL":"...","branch":"...","subPath":"...","baseSHA":"...","headSHA":"..."},"scan":{"mode":"initial|incremental|manual","sliceId":"...","summary":"..."},"findings":[]}` + "\n")
	prompt.WriteString("Each finding object must use these keys: title, category, severity, confidence, triage, evidence, summary, rootCause, reproduction, remediation, suggestedAction, whyTestsDoNotAlreadyCoverThis, suggestedRegressionTest, minimumFixScope.\n")
	prompt.WriteString("Use severity exactly one of: critical, high, medium, low. Use confidence exactly one of: high, medium, low.\n")
	prompt.WriteString("Set scan.sliceId exactly to the slice ID above. Even when this slice has zero findings, write valid JSON with an empty findings array.\n")

	prompt.WriteString("\nReview slice metadata:\n")
	prompt.Write(sliceJSON)
	prompt.WriteString("\n")
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
	diffArtifact := fmt.Sprintf("security-patch-%s.diff", finding.ID)
	summaryArtifact := fmt.Sprintf("security-patch-%s.json", finding.ID)
	fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, diffArtifact)
	fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, summaryArtifact)
	appendRequiredArtifactsDirective(&prompt, diffArtifact, summaryArtifact)
	prompt.WriteString("The JSON patch summary must be valid JSON with this exact shape:\n")
	fmt.Fprintf(
		&prompt,
		`{"schemaVersion":%d,"findingId":%q,"summary":"...","changedFiles":["path/to/changed-file"],"testsRun":[{"command":"go test ./...","exitCode":0}],"risk":"low|medium|high"}`+"\n",
		SchemaVersionPatchSummary,
		finding.ID,
	)
	prompt.WriteString("The changedFiles array must exactly match the files changed in the workspace diff.\n")
	prompt.WriteString("Prefer Bash heredocs or shell redirection when writing artifact files so they are persisted on disk.\n")
	return prompt.String()
}
