package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

const (
	AgentResultSchemaVersion = 1

	AgentResultKindThreatModel = "orka.security.threat-model.v1"
	AgentResultKindFindings    = "orka.security.findings.v1"
	AgentResultKindValidation  = "orka.security.validation.v1"
	AgentResultKindPatch       = "orka.security.patch.v1"

	maxThreatModelResultBytes    = 1 << 20
	maxFindingsResultBytes       = 512 << 10
	maxValidationResultBytes     = 256 << 10
	maxPatchResultBytes          = 64 << 10
	MaxPatchSummaryArtifactBytes = maxPatchResultBytes
	maxPatchSummaryBytes         = 16 << 10
	maxPatchChangedFiles         = 64
	maxPatchTestsRun             = 64
	maxReviewContextBytes        = 1 << 20
	maxThreatModelBytes          = 768 << 10
	maxFindingTextBytes          = 64 << 10
	maxFindingSummaryBytes       = 16 << 10
	maxFindingEvidenceRefs       = 32
	maxFindingsPerSlice          = 3
	maxValidationListItems       = 64
	maxValidationItemBytes       = 8 << 10
	maxValidationEvidenceRefs    = 32
	maxReviewedFiles             = 256
	maxResolvedQuestions         = 32
	maxResolvedQuestionBytes     = 4 << 10

	MaxReviewContextReferenceFiles = 4000
)

type AgentResultBinding struct {
	RepositoryScan string
	ScanID         string
	PolicyDigest   string
	ContextDigest  string
}

func MarshalReviewContext(manifest ReviewContextManifest) ([]byte, error) {
	data, err := json.Marshal(manifest)
	if err != nil || len(data) <= maxReviewContextBytes {
		return data, err
	}
	excess := len(data) - maxReviewContextBytes
	references := manifest.ReferenceFiles
	for excess > 0 && len(references) > 0 {
		last, err := json.Marshal(references[len(references)-1])
		if err != nil {
			return nil, err
		}
		excess -= len(last) + 1
		references = references[:len(references)-1]
	}
	manifest.ReferenceFiles = references
	return json.Marshal(manifest)
}

const MaxReviewContextsBytes = 40 << 20

func MarshalReviewContexts(manifests []ReviewContextManifest, budget int) ([][]byte, error) {
	encoded := make([][]byte, len(manifests))
	total := 0
	for i := range manifests {
		data, err := MarshalReviewContext(manifests[i])
		if err != nil {
			return nil, err
		}
		encoded[i] = data
		total += len(data)
	}
	for total > budget {
		longest := -1
		for i := range manifests {
			if len(manifests[i].ReferenceFiles) > 0 && (longest < 0 || len(manifests[i].ReferenceFiles) > len(manifests[longest].ReferenceFiles)) {
				longest = i
			}
		}
		if longest < 0 {
			return nil, fmt.Errorf("review contexts total %d bytes even without reference files, above the %d byte limit", total, budget)
		}
		references := manifests[longest].ReferenceFiles
		manifests[longest].ReferenceFiles = references[:len(references)/2]
		data, err := MarshalReviewContext(manifests[longest])
		if err != nil {
			return nil, err
		}
		total += len(data) - len(encoded[longest])
		encoded[longest] = data
	}
	return encoded, nil
}

const minScaledReviewContextBytes = 2 << 10

func BuildReviewContexts(
	root string,
	slices []store.ReviewSlice,
	references map[string][]ReviewContextReferenceFile,
	budget int,
) ([][]byte, error) {
	maxBytes := defaultMaxReviewContextBytes
	for {
		manifests := make([]ReviewContextManifest, 0, len(slices))
		bare := 0
		for i := range slices {
			_, manifest, err := BuildReviewContext(root, slices[i], ReviewContextOptions{MaxBytes: maxBytes})
			if err != nil {
				return nil, fmt.Errorf("build security review context for %s: %w", slices[i].ID, err)
			}
			data, err := MarshalReviewContext(manifest)
			if err != nil {
				return nil, err
			}
			bare += len(data)
			manifest.ReferenceFiles = references[slices[i].ID]
			manifests = append(manifests, manifest)
		}
		scaled := maxBytes < defaultMaxReviewContextBytes
		if !scaled && bare <= budget || scaled && bare <= budget/2 || maxBytes == minScaledReviewContextBytes {
			return MarshalReviewContexts(manifests, budget)
		}
		next := int(int64(maxBytes) * int64(budget/2) / int64(bare))
		maxBytes = max(minScaledReviewContextBytes, min(next, maxBytes*3/4))
	}
}

func ParseTrustedReviewContextManifest(data []byte) (*ReviewContextManifest, string, error) {
	var manifest ReviewContextManifest
	if err := decodeStrictResult(data, maxReviewContextBytes, &manifest); err != nil {
		return nil, "", err
	}
	if manifest.SchemaVersion != SchemaVersionReviewContext {
		return nil, "", fmt.Errorf("unsupported review context schemaVersion %d", manifest.SchemaVersion)
	}
	if strings.TrimSpace(manifest.SliceID) == "" {
		return nil, "", fmt.Errorf("review context sliceId is required")
	}
	if manifest.Prompt == "" {
		return nil, "", fmt.Errorf("review context prompt is required")
	}
	if len(manifest.Prompt) > defaultMaxReviewContextBytes || manifest.PromptBytes != len(manifest.Prompt) {
		return nil, "", fmt.Errorf("review context prompt does not match bounded promptBytes")
	}
	if manifest.ApproximateTokens != (manifest.PromptBytes+3)/4 {
		return nil, "", fmt.Errorf("review context approximateTokens does not match promptBytes")
	}
	if len(manifest.IncludedFiles) > defaultMaxReviewContextFiles {
		return nil, "", fmt.Errorf("review context includes more than %d files", defaultMaxReviewContextFiles)
	}
	if len(manifest.ChangedFiles) > maxReviewContextChangedFiles || len(manifest.ChangedLineRanges) > maxReviewContextChangedLineRanges {
		return nil, "", fmt.Errorf("review context changed-code metadata exceeds bounds")
	}
	if len(manifest.OmittedFiles) > 1024 {
		return nil, "", fmt.Errorf("review context omitted file metadata exceeds bounds")
	}
	parsed, err := ParseReviewContextManifest(data)
	if err != nil {
		return nil, "", err
	}
	digest, err := ReviewContextDigest(*parsed)
	if err != nil {
		return nil, "", err
	}
	return parsed, digest, nil
}

type ThreatModelResultEnvelope struct {
	SchemaVersion  int                     `json:"schemaVersion"`
	Kind           string                  `json:"kind"`
	RepositoryScan string                  `json:"repositoryScan"`
	ScanID         string                  `json:"scanId"`
	PolicyDigest   string                  `json:"policyDigest"`
	ThreatModel    string                  `json:"threatModel"`
	Investigations []SecurityInvestigation `json:"investigations,omitempty"`
}

type FindingsResultEnvelope struct {
	SchemaVersion  int                `json:"schemaVersion"`
	Kind           string             `json:"kind"`
	RepositoryScan string             `json:"repositoryScan"`
	ScanID         string             `json:"scanId"`
	SliceID        string             `json:"sliceId"`
	PolicyDigest   string             `json:"policyDigest"`
	ContextDigest  string             `json:"contextDigest"`
	Findings       FindingsV2Artifact `json:"findings"`
}

type ValidationResultEnvelope struct {
	SchemaVersion  int                `json:"schemaVersion"`
	Kind           string             `json:"kind"`
	RepositoryScan string             `json:"repositoryScan"`
	ScanID         string             `json:"scanId"`
	FindingID      string             `json:"findingId"`
	PolicyDigest   string             `json:"policyDigest"`
	Validation     ValidationArtifact `json:"validation"`
}

type FindingsResultExpectation struct {
	Binding    AgentResultBinding
	SliceID    string
	Mode       string
	Repository FindingsV2Repository
}

type ValidationResultExpectation struct {
	Binding AgentResultBinding
	Finding *store.Finding
}

func ParseThreatModelResult(data []byte, expected AgentResultBinding) (string, error) {
	var result ThreatModelResultEnvelope
	if err := decodeStrictResult(data, maxThreatModelResultBytes, &result); err != nil {
		return "", err
	}
	if result.SchemaVersion != AgentResultSchemaVersion {
		return "", fmt.Errorf("unsupported security result schemaVersion %d", result.SchemaVersion)
	}
	if result.Kind != AgentResultKindThreatModel {
		return "", fmt.Errorf("security result kind %q is not a threat model", result.Kind)
	}
	if err := validateAgentResultBinding(result.RepositoryScan, result.ScanID, result.PolicyDigest, "", expected); err != nil {
		return "", err
	}
	content := strings.TrimSpace(result.ThreatModel)
	if len(content) > maxThreatModelBytes {
		return "", fmt.Errorf("threatModel exceeds %d bytes", maxThreatModelBytes)
	}
	content = strings.TrimSpace(stripUnsafeTextRunes(content))
	if content == "" {
		return "", fmt.Errorf("threatModel is required")
	}
	if isPlaceholderThreatModel(content) {
		return "", fmt.Errorf("threatModel is a prompt placeholder")
	}
	if !strings.HasPrefix(content, "#") {
		return "", fmt.Errorf("threatModel must be markdown beginning with a heading")
	}
	if securityResultLooksLikeToolTranscript(content) {
		return "", fmt.Errorf("threatModel looks like a tool transcript")
	}
	if result.Investigations != nil {
		content = RenderThreatModelInvestigations(content, result.Investigations)
	}
	content = SanitizeThreatModel(content)
	if len(content) > maxThreatModelBytes {
		return "", fmt.Errorf("threatModel exceeds %d bytes", maxThreatModelBytes)
	}
	return content, nil
}

func SanitizeThreatModel(content string) string {
	return redact.SensitiveText(stripUnsafeTextRunes(content))
}

func securityResultLooksLikeToolTranscript(content string) bool {
	for _, marker := range []string{
		"<tool_call>",
		"</tool_call>",
		"<tool_name>",
		"</tool_name>",
		"<parameters>",
		"</parameters>",
		"<command>",
		"</command>",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

func ParseFindingsResult(data []byte, expected FindingsResultExpectation) (*FindingsV2Artifact, error) {
	var result FindingsResultEnvelope
	if err := decodeStrictResult(data, maxFindingsResultBytes, &result); err != nil {
		return nil, err
	}
	if result.SchemaVersion != AgentResultSchemaVersion {
		return nil, fmt.Errorf("unsupported security result schemaVersion %d", result.SchemaVersion)
	}
	if result.Kind != AgentResultKindFindings {
		return nil, fmt.Errorf("security result kind %q is not findings", result.Kind)
	}
	if err := validateAgentResultBinding(result.RepositoryScan, result.ScanID, result.PolicyDigest, result.ContextDigest, expected.Binding); err != nil {
		return nil, err
	}
	if strings.TrimSpace(result.SliceID) != strings.TrimSpace(expected.SliceID) {
		return nil, fmt.Errorf("security result sliceId does not match task slice")
	}
	normalizeFindingsCoverage(&result.Findings.Scan)
	if err := validateFindingsResultArtifact(result.Findings, expected); err != nil {
		return nil, err
	}
	return &result.Findings, nil
}

func ParseValidationResult(data []byte, expected ValidationResultExpectation) (*ValidationArtifact, error) {
	if expected.Finding == nil {
		return nil, fmt.Errorf("expected finding is required")
	}
	var result ValidationResultEnvelope
	if err := decodeStrictResult(data, maxValidationResultBytes, &result); err != nil {
		return nil, err
	}
	if result.SchemaVersion != AgentResultSchemaVersion {
		return nil, fmt.Errorf("unsupported security result schemaVersion %d", result.SchemaVersion)
	}
	if result.Kind != AgentResultKindValidation {
		return nil, fmt.Errorf("security result kind %q is not validation", result.Kind)
	}
	if err := validateAgentResultBinding(result.RepositoryScan, result.ScanID, result.PolicyDigest, "", expected.Binding); err != nil {
		return nil, err
	}
	findingID := strings.TrimSpace(expected.Finding.ID)
	if strings.TrimSpace(result.FindingID) != findingID || strings.TrimSpace(result.Validation.FindingID) != findingID {
		return nil, fmt.Errorf("security validation result findingId does not match task finding")
	}
	result.Validation.Evidence = usableValidationEvidence(result.Validation.Evidence, expected.Finding.Evidence)
	if err := validateValidationArtifact(result.Validation, expected.Finding); err != nil {
		return nil, err
	}
	return &result.Validation, nil
}

func decodeStrictResult(data []byte, maxBytes int, target any) error {
	if len(data) == 0 {
		return fmt.Errorf("security task result is empty")
	}
	if len(data) > maxBytes {
		return fmt.Errorf("security task result exceeds %d bytes", maxBytes)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("security task result is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode security task result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errorsIsEOF(err) {
		if err == nil {
			return fmt.Errorf("security task result contains trailing JSON")
		}
		return fmt.Errorf("decode trailing security task result: %w", err)
	}
	return nil
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}

func validateAgentResultBinding(repositoryScan, scanID, policyDigest, contextDigest string, expected AgentResultBinding) error {
	if strings.TrimSpace(repositoryScan) != strings.TrimSpace(expected.RepositoryScan) {
		return fmt.Errorf("security result repositoryScan does not match task owner")
	}
	if strings.TrimSpace(scanID) != strings.TrimSpace(expected.ScanID) {
		return fmt.Errorf("security result scanId does not match task run")
	}
	if strings.TrimSpace(policyDigest) != strings.TrimSpace(expected.PolicyDigest) {
		return fmt.Errorf("security result policyDigest does not match scan policy")
	}
	if strings.TrimSpace(contextDigest) != strings.TrimSpace(expected.ContextDigest) {
		return fmt.Errorf("security result contextDigest does not match mapper context")
	}
	return nil
}

func normalizeFindingsCoverage(scan *FindingsV2Scan) {
	files := make([]string, 0, min(len(scan.ReviewedFiles), maxReviewedFiles))
	seen := map[string]struct{}{}
	for _, file := range scan.ReviewedFiles {
		file = strings.TrimSpace(file)
		if _, ok := seen[file]; ok || !SafeRepoPath(file) {
			continue
		}
		seen[file] = struct{}{}
		files = append(files, file)
		if len(files) == maxReviewedFiles {
			break
		}
	}
	scan.ReviewedFiles = files
	questions := make([]FindingsV2ResolvedQuestion, 0, min(len(scan.ResolvedQuestions), maxResolvedQuestions))
	for _, question := range scan.ResolvedQuestions {
		question.Question = truncateUTF8(strings.TrimSpace(question.Question), maxResolvedQuestionBytes)
		question.Resolution = truncateUTF8(strings.TrimSpace(question.Resolution), maxResolvedQuestionBytes)
		if question.Question == "" {
			continue
		}
		questions = append(questions, question)
		if len(questions) == maxResolvedQuestions {
			break
		}
	}
	scan.ResolvedQuestions = questions
	limitations := make([]string, 0, min(len(scan.Limitations), maxResolvedQuestions))
	for _, limitation := range scan.Limitations {
		if limitation = truncateUTF8(strings.TrimSpace(limitation), maxResolvedQuestionBytes); limitation != "" {
			limitations = append(limitations, limitation)
		}
		if len(limitations) == maxResolvedQuestions {
			break
		}
	}
	scan.Limitations = limitations
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func isPlaceholderResultText(value string) bool {
	trimmed := strings.TrimSpace(stripUnsafeTextRunes(value))
	return trimmed == truncationSuffix || trimmed == "…"
}

func isPlaceholderThreatModel(content string) bool {
	if isPlaceholderResultText(content) {
		return true
	}
	lines := strings.Split(content, "\n")
	nonEmpty := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) == 2 && strings.EqualFold(nonEmpty[0], "# Threat Model") && isPlaceholderResultText(nonEmpty[1]) {
		return true
	}
	return false
}

func ReviewResultIsInconclusive(artifact FindingsV2Artifact) bool {
	if len(artifact.Findings) > 0 {
		return false
	}
	for _, file := range artifact.Scan.ReviewedFiles {
		if trimmed := strings.TrimSpace(file); trimmed != "" && !isPlaceholderResultText(trimmed) {
			return false
		}
	}
	for _, question := range artifact.Scan.ResolvedQuestions {
		q := strings.TrimSpace(question.Question)
		r := strings.TrimSpace(question.Resolution)
		if q != "" && !isPlaceholderResultText(q) && r != "" && !isPlaceholderResultText(r) {
			return false
		}
	}
	summary := strings.TrimSpace(stripUnsafeTextRunes(artifact.Scan.Summary))
	return summary == "" || isPlaceholderResultText(summary)
}

func validateFindingsResultArtifact(artifact FindingsV2Artifact, expected FindingsResultExpectation) error {
	if artifact.SchemaVersion != SchemaVersionFindingsV2 {
		return fmt.Errorf("unsupported findings schemaVersion %d", artifact.SchemaVersion)
	}
	if artifact.Findings == nil {
		return fmt.Errorf("findings must be an array")
	}
	if len(artifact.Findings) > maxFindingsPerSlice {
		return fmt.Errorf("findings exceeds per-slice limit %d", maxFindingsPerSlice)
	}
	if artifact.Repository != expected.Repository {
		return fmt.Errorf("findings repository identity does not match scan run")
	}
	if strings.TrimSpace(artifact.Scan.Mode) != strings.TrimSpace(expected.Mode) {
		return fmt.Errorf("findings scan.mode does not match scan run")
	}
	if strings.TrimSpace(artifact.Scan.SliceID) != strings.TrimSpace(expected.SliceID) {
		return fmt.Errorf("findings scan.sliceId does not match task slice")
	}
	if err := boundedString("findings scan.summary", artifact.Scan.Summary, maxFindingSummaryBytes); err != nil {
		return err
	}
	if isPlaceholderResultText(artifact.Scan.Summary) {
		return fmt.Errorf("findings scan.summary is a prompt placeholder")
	}
	for i := range artifact.Findings {
		if err := validateBoundedFinding(i, artifact.Findings[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateBoundedFinding(index int, finding FindingsV2Finding) error {
	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"title", finding.Title, maxFindingSummaryBytes},
		{"category", finding.Category, maxFindingSummaryBytes},
		{"severity", finding.Severity, 32},
		{"confidence", finding.Confidence, 32},
		{"triage", finding.Triage, maxFindingSummaryBytes},
		{summaryField, finding.Summary, maxFindingTextBytes},
		{"rootCause", finding.RootCause, maxFindingTextBytes},
		{"reproduction", finding.Reproduction, maxFindingTextBytes},
		{"remediation", finding.Remediation, maxFindingTextBytes},
		{"suggestedAction", finding.SuggestedAction, maxFindingTextBytes},
		{"whyTestsDoNotAlreadyCoverThis", finding.WhyTestsDoNotAlreadyCoverThis, maxFindingTextBytes},
		{"suggestedRegressionTest", finding.SuggestedRegressionTest, maxFindingTextBytes},
		{"minimumFixScope", finding.MinimumFixScope, maxFindingTextBytes},
	}
	for _, field := range fields {
		if err := boundedString(fmt.Sprintf("findings[%d].%s", index, field.name), field.value, field.limit); err != nil {
			return err
		}
	}
	if len(finding.Evidence) > maxFindingEvidenceRefs {
		return fmt.Errorf("findings[%d].evidence exceeds %d entries", index, maxFindingEvidenceRefs)
	}
	for evidenceIndex, ref := range finding.Evidence {
		if err := boundedString(fmt.Sprintf("findings[%d].evidence[%d].path", index, evidenceIndex), ref.Path, 4096); err != nil {
			return err
		}
		if ref.Symbol != nil {
			if err := boundedString(fmt.Sprintf("findings[%d].evidence[%d].symbol", index, evidenceIndex), *ref.Symbol, maxFindingSummaryBytes); err != nil {
				return err
			}
		}
		if ref.Quote != nil {
			if err := boundedString(fmt.Sprintf("findings[%d].evidence[%d].quote", index, evidenceIndex), *ref.Quote, maxFindingTextBytes); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateValidationArtifact(artifact ValidationArtifact, finding *store.Finding) error {
	if artifact.Version != 1 {
		return fmt.Errorf("unsupported validation version %d", artifact.Version)
	}
	switch strings.TrimSpace(artifact.Status) {
	case "validated", "failed", "skipped":
	default:
		return fmt.Errorf("unsupported validation status %q", artifact.Status)
	}
	if strings.TrimSpace(artifact.Summary) == "" {
		return fmt.Errorf("validation summary is required")
	}
	if isPlaceholderResultText(artifact.Summary) {
		return fmt.Errorf("validation summary is a prompt placeholder")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{summaryField, artifact.Summary},
		{"reproduction", string(artifact.Reproduction)},
		{"attack_path_analysis", string(artifact.AttackPathAnalysis)},
		{"likelihood", artifact.Likelihood},
		{"impact", artifact.Impact},
	} {
		if err := boundedString("validation."+field.name, field.value, maxFindingTextBytes); err != nil {
			return err
		}
	}
	for name, values := range map[string][]string{
		"validation_steps": artifact.ValidationSteps,
		"assumptions":      artifact.Assumptions,
		"controls":         artifact.Controls,
		"blindspots":       artifact.Blindspots,
	} {
		if len(values) > maxValidationListItems {
			return fmt.Errorf("validation.%s exceeds %d entries", name, maxValidationListItems)
		}
		for i, value := range values {
			if err := boundedString(fmt.Sprintf("validation.%s[%d]", name, i), value, maxValidationItemBytes); err != nil {
				return err
			}
		}
	}
	if len(artifact.Evidence) > maxValidationEvidenceRefs {
		return fmt.Errorf("validation.evidence exceeds %d entries", maxValidationEvidenceRefs)
	}
	for i, ref := range artifact.Evidence {
		if err := validateValidationEvidenceRef(i, ref, finding.Evidence); err != nil {
			return err
		}
	}
	return nil
}

func usableValidationEvidence(refs ValidationArtifactEvidenceRefs, accepted []store.FindingEvidenceRef) ValidationArtifactEvidenceRefs {
	out := make(ValidationArtifactEvidenceRefs, 0, min(len(refs), maxValidationEvidenceRefs))
	for _, ref := range refs {
		if validateValidationEvidenceRef(len(out), ref, accepted) != nil {
			continue
		}
		out = append(out, ref)
		if len(out) == maxValidationEvidenceRefs {
			break
		}
	}
	return out
}

func validateValidationEvidenceRef(index int, ref store.FindingEvidenceRef, accepted []store.FindingEvidenceRef) error {
	if strings.TrimSpace(ref.TaskName) != "" || strings.TrimSpace(ref.Name) != "" {
		return fmt.Errorf("validation.evidence[%d] may not reference task artifacts", index)
	}
	switch strings.TrimSpace(ref.Kind) {
	case fileLocationKind:
		if !SafeRepoPath(ref.Path) || ref.StartLine <= 0 || ref.EndLine < ref.StartLine {
			return fmt.Errorf("validation.evidence[%d] has invalid file range", index)
		}
		if !validationEvidenceWithinAcceptedFinding(ref, accepted) {
			return fmt.Errorf("validation.evidence[%d] is outside accepted finding evidence", index)
		}
	case "note":
		if strings.TrimSpace(ref.Label) == "" {
			return fmt.Errorf("validation.evidence[%d] note label is required", index)
		}
	default:
		return fmt.Errorf("validation.evidence[%d] kind %q is not allowed", index, ref.Kind)
	}
	for name, value := range map[string]string{
		"label":  ref.Label,
		"path":   ref.Path,
		"symbol": ref.Symbol,
		"quote":  ref.Quote,
	} {
		if err := boundedString(fmt.Sprintf("validation.evidence[%d].%s", index, name), value, maxFindingTextBytes); err != nil {
			return err
		}
	}
	return nil
}

func validationEvidenceWithinAcceptedFinding(ref store.FindingEvidenceRef, accepted []store.FindingEvidenceRef) bool {
	for _, candidate := range accepted {
		if candidate.Kind != fileLocationKind || candidate.Path != ref.Path {
			continue
		}
		if ref.StartLine >= candidate.StartLine && ref.EndLine <= candidate.EndLine {
			return true
		}
	}
	return false
}

func boundedString(name, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxBytes)
	}
	if strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid text", name)
	}
	return nil
}

func ReviewContextDigest(manifest ReviewContextManifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type PatchResultEnvelope struct {
	SchemaVersion  int            `json:"schemaVersion"`
	Kind           string         `json:"kind"`
	RepositoryScan string         `json:"repositoryScan"`
	FindingID      string         `json:"findingId"`
	Summary        string         `json:"summary"`
	ChangedFiles   []string       `json:"changedFiles"`
	TestsRun       []PatchTestRun `json:"testsRun,omitempty"`
	Risk           string         `json:"risk"`
}

type PatchResultExpectation struct {
	RepositoryScan string
	FindingID      string
}

func ParsePatchResult(data []byte, expected PatchResultExpectation) (*PatchSummaryArtifact, error) {
	var result PatchResultEnvelope
	if err := decodeStrictResult(data, maxPatchResultBytes, &result); err != nil {
		return nil, err
	}
	if result.SchemaVersion != AgentResultSchemaVersion {
		return nil, fmt.Errorf("unsupported security result schemaVersion %d", result.SchemaVersion)
	}
	if result.Kind != AgentResultKindPatch {
		return nil, fmt.Errorf("security result kind %q is not a patch", result.Kind)
	}
	if strings.TrimSpace(expected.RepositoryScan) == "" || result.RepositoryScan != expected.RepositoryScan {
		return nil, fmt.Errorf("patch result repositoryScan does not match the expected scan")
	}
	if strings.TrimSpace(expected.FindingID) == "" || result.FindingID != expected.FindingID {
		return nil, fmt.Errorf("patch result findingId does not match the expected finding")
	}
	return NormalizePatchSummaryArtifact(PatchSummaryArtifact{
		SchemaVersion: SchemaVersionPatchSummary,
		FindingID:     result.FindingID,
		Summary:       result.Summary,
		ChangedFiles:  result.ChangedFiles,
		TestsRun:      result.TestsRun,
		Risk:          result.Risk,
	})
}

func NormalizePatchSummaryArtifact(artifact PatchSummaryArtifact) (*PatchSummaryArtifact, error) {
	if artifact.SchemaVersion != SchemaVersionPatchSummary {
		return nil, fmt.Errorf("unsupported patch summary schemaVersion %d", artifact.SchemaVersion)
	}
	if strings.TrimSpace(artifact.FindingID) == "" {
		return nil, fmt.Errorf("patch summary findingId is required")
	}
	summary := strings.TrimSpace(stripUnsafeTextRunes(artifact.Summary))
	if summary == "" {
		return nil, fmt.Errorf("patch summary is required")
	}
	if len(summary) > maxPatchSummaryBytes {
		return nil, fmt.Errorf("patch summary exceeds %d bytes", maxPatchSummaryBytes)
	}
	if securityResultLooksLikeToolTranscript(summary) {
		return nil, fmt.Errorf("patch summary looks like a tool transcript")
	}
	if LooksLikeSecret(summary) {
		return nil, fmt.Errorf("patch summary contains a credential-shaped value")
	}
	if len(artifact.ChangedFiles) == 0 {
		return nil, fmt.Errorf("patch changedFiles is required")
	}
	if len(artifact.ChangedFiles) > maxPatchChangedFiles {
		return nil, fmt.Errorf("patch changedFiles exceeds %d entries", maxPatchChangedFiles)
	}
	changed := make([]string, 0, len(artifact.ChangedFiles))
	seen := make(map[string]struct{}, len(artifact.ChangedFiles))
	for _, file := range artifact.ChangedFiles {
		file = strings.TrimSpace(strings.ReplaceAll(stripUnsafeTextRunes(file), "\\", "/"))
		for strings.HasPrefix(file, "./") {
			file = strings.TrimPrefix(file, "./")
		}
		if file == "" || !SafeRepoPath(file) {
			return nil, fmt.Errorf("patch changedFiles contains an unsafe path")
		}
		if LooksLikeSecret(file) {
			return nil, fmt.Errorf("patch changedFiles contains a credential-shaped path")
		}
		if _, duplicate := seen[file]; duplicate {
			continue
		}
		seen[file] = struct{}{}
		changed = append(changed, file)
	}
	if len(artifact.TestsRun) > maxPatchTestsRun {
		return nil, fmt.Errorf("patch testsRun exceeds %d entries", maxPatchTestsRun)
	}
	var testsRun []PatchTestRun
	if len(artifact.TestsRun) > 0 {
		testsRun = make([]PatchTestRun, len(artifact.TestsRun))
	}
	for i, run := range artifact.TestsRun {
		command := strings.TrimSpace(stripUnsafeTextRunes(run.Command))
		if command == "" || len(command) > maxValidationItemBytes {
			return nil, fmt.Errorf("patch testsRun contains an invalid command")
		}
		if LooksLikeSecret(command) {
			return nil, fmt.Errorf("patch testsRun contains a credential-shaped command")
		}
		testsRun[i] = PatchTestRun{Command: command, ExitCode: run.ExitCode}
	}
	risk := strings.ToLower(strings.TrimSpace(artifact.Risk))
	switch risk {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("patch risk must be low, medium, or high")
	}
	return &PatchSummaryArtifact{
		SchemaVersion: SchemaVersionPatchSummary,
		FindingID:     artifact.FindingID,
		Summary:       summary,
		ChangedFiles:  changed,
		TestsRun:      testsRun,
		Risk:          risk,
	}, nil
}
