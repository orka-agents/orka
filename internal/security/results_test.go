package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestParseThreatModelResultRequiresExactEnvelopeAndBinding(t *testing.T) {
	expected := AgentResultBinding{
		RepositoryScan: "vekil",
		ScanID:         "scan_123",
		PolicyDigest:   "sha256:policy",
	}
	result := ThreatModelResultEnvelope{
		SchemaVersion:  AgentResultSchemaVersion,
		Kind:           AgentResultKindThreatModel,
		RepositoryScan: expected.RepositoryScan,
		ScanID:         expected.ScanID,
		PolicyDigest:   expected.PolicyDigest,
		ThreatModel:    "# Threat Model\n\nTrusted boundaries.",
	}
	data := mustMarshalSecurityResult(t, result)
	got, err := ParseThreatModelResult(data, expected)
	if err != nil {
		t.Fatalf("ParseThreatModelResult() error = %v", err)
	}
	if got != result.ThreatModel {
		t.Fatalf("ParseThreatModelResult() = %q, want %q", got, result.ThreatModel)
	}

	result.ScanID = "scan_stale"
	if _, err := ParseThreatModelResult(mustMarshalSecurityResult(t, result), expected); err == nil || !strings.Contains(err.Error(), "scanId") {
		t.Fatalf("ParseThreatModelResult(stale) error = %v, want scan binding rejection", err)
	}
	if _, err := ParseThreatModelResult(append(data, []byte(` {}`)...), expected); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("ParseThreatModelResult(trailing) error = %v, want trailing JSON rejection", err)
	}
}

func TestParseFindingsResultRequiresExactRepositoryAndContext(t *testing.T) {
	repository := FindingsV2Repository{
		RepoURL: "https://github.com/sozercan/vekil",
		Branch:  "main",
		HeadSHA: "0123456789abcdef",
	}
	expected := FindingsResultExpectation{
		Binding: AgentResultBinding{
			RepositoryScan: "vekil",
			ScanID:         "scan_123",
			PolicyDigest:   "sha256:policy",
			ContextDigest:  "sha256:context",
		},
		SliceID:    "slice_api",
		Mode:       "initial",
		Repository: repository,
	}
	result := FindingsResultEnvelope{
		SchemaVersion:  AgentResultSchemaVersion,
		Kind:           AgentResultKindFindings,
		RepositoryScan: expected.Binding.RepositoryScan,
		ScanID:         expected.Binding.ScanID,
		SliceID:        expected.SliceID,
		PolicyDigest:   expected.Binding.PolicyDigest,
		ContextDigest:  expected.Binding.ContextDigest,
		Findings: FindingsV2Artifact{
			SchemaVersion: SchemaVersionFindingsV2,
			Repository:    repository,
			Scan:          FindingsV2Scan{Mode: expected.Mode, SliceID: expected.SliceID, Summary: "reviewed"},
			Findings: []FindingsV2Finding{{
				Title:       "Authorization bypass",
				Category:    "authz",
				Severity:    "high",
				Confidence:  "high",
				Summary:     "A trusted boundary is bypassed.",
				Remediation: "Enforce authorization.",
				Evidence: []FindingsV2EvidenceRef{{
					Path:      "internal/api/server.go",
					StartLine: 10,
					EndLine:   12,
				}},
			}},
		},
	}
	if _, err := ParseFindingsResult(mustMarshalSecurityResult(t, result), expected); err != nil {
		t.Fatalf("ParseFindingsResult() error = %v", err)
	}

	result.ContextDigest = "sha256:stale"
	if _, err := ParseFindingsResult(mustMarshalSecurityResult(t, result), expected); err == nil || !strings.Contains(err.Error(), "contextDigest") {
		t.Fatalf("ParseFindingsResult(stale context) error = %v, want context rejection", err)
	}
	result.ContextDigest = expected.Binding.ContextDigest
	result.Findings.Repository.HeadSHA = "different"
	if _, err := ParseFindingsResult(mustMarshalSecurityResult(t, result), expected); err == nil || !strings.Contains(err.Error(), "repository identity") {
		t.Fatalf("ParseFindingsResult(repository mismatch) error = %v, want repository rejection", err)
	}
}

func TestParseValidationResultRestrictsEvidenceToAcceptedFinding(t *testing.T) {
	finding := &store.Finding{
		ID: "fnd_123",
		Evidence: []store.FindingEvidenceRef{{
			Kind:      "file",
			Path:      "internal/api/server.go",
			StartLine: 10,
			EndLine:   20,
		}},
	}
	expected := ValidationResultExpectation{
		Binding: AgentResultBinding{RepositoryScan: "vekil", ScanID: "scan_123", PolicyDigest: "sha256:policy"},
		Finding: finding,
	}
	result := ValidationResultEnvelope{
		SchemaVersion:  AgentResultSchemaVersion,
		Kind:           AgentResultKindValidation,
		RepositoryScan: expected.Binding.RepositoryScan,
		ScanID:         expected.Binding.ScanID,
		FindingID:      finding.ID,
		PolicyDigest:   expected.Binding.PolicyDigest,
		Validation: ValidationArtifact{
			Version:   1,
			FindingID: finding.ID,
			Status:    "validated",
			Summary:   "Confirmed.",
			Evidence: ValidationArtifactEvidenceRefs{{
				Kind:      "file",
				Path:      "internal/api/server.go",
				StartLine: 12,
				EndLine:   15,
			}},
		},
	}
	if _, err := ParseValidationResult(mustMarshalSecurityResult(t, result), expected); err != nil {
		t.Fatalf("ParseValidationResult() error = %v", err)
	}

	result.Validation.Evidence[0].StartLine = 1
	if _, err := ParseValidationResult(mustMarshalSecurityResult(t, result), expected); err == nil || !strings.Contains(err.Error(), "outside accepted finding evidence") {
		t.Fatalf("ParseValidationResult(out of range) error = %v, want evidence rejection", err)
	}
	result.Validation.Evidence[0] = store.FindingEvidenceRef{Kind: "artifact", Name: "transcript.txt"}
	if _, err := ParseValidationResult(mustMarshalSecurityResult(t, result), expected); err == nil || !strings.Contains(err.Error(), "task artifacts") {
		t.Fatalf("ParseValidationResult(artifact) error = %v, want artifact rejection", err)
	}
}

func TestParseTrustedReviewContextManifestBindsExactPrompt(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "api", "server.go"), []byte("package api\n\nfunc Serve() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	slice := store.ReviewSlice{
		ID:         "slice_api",
		Title:      "API",
		Kind:       "package",
		OwnedFiles: []store.ReviewSliceFile{{Path: "internal/api/server.go"}},
	}
	prompt, manifest, err := BuildReviewContext(root, slice, ReviewContextOptions{})
	if err != nil {
		t.Fatalf("BuildReviewContext() error = %v", err)
	}
	if manifest.Prompt != prompt {
		t.Fatal("manifest prompt does not match generated prompt")
	}
	data := mustMarshalSecurityResult(t, manifest)
	parsed, digest, err := ParseTrustedReviewContextManifest(data)
	if err != nil {
		t.Fatalf("ParseTrustedReviewContextManifest() error = %v", err)
	}
	if parsed.Prompt != prompt || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("trusted context prompt/digest = %q/%q", parsed.Prompt, digest)
	}

	manifest.PromptBytes++
	if _, _, err := ParseTrustedReviewContextManifest(mustMarshalSecurityResult(t, manifest)); err == nil || !strings.Contains(err.Error(), "promptBytes") {
		t.Fatalf("ParseTrustedReviewContextManifest(tampered) error = %v, want prompt binding rejection", err)
	}
}

func TestHarnessV2SecurityPromptsRequireTerminalJSONWithoutArtifacts(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{}
	scan.Name = "vekil"
	scan.Spec.RepoURL = "https://github.com/sozercan/vekil"
	scan.Spec.Branch = "main"
	binding := AgentResultBinding{RepositoryScan: scan.Name, ScanID: "scan_123", PolicyDigest: "sha256:policy", ContextDigest: "sha256:context"}
	manifest := ReviewContextManifest{SchemaVersion: SchemaVersionReviewContext, SliceID: "slice_api", Prompt: "bounded context\n", PromptBytes: len("bounded context\n"), ApproximateTokens: 4}
	slice := store.ReviewSlice{ID: "slice_api", Title: "API", Kind: "package"}
	finding := &store.Finding{ID: "fnd_123", Title: "Finding", Severity: "high", Confidence: "high"}

	prompts := []string{
		BuildThreatModelResultPrompt(scan, "initial", "", "", "", binding),
		BuildReviewResultPrompt(scan, "initial", "", "", "", slice, binding, manifest, FindingsV2Repository{RepoURL: scan.Spec.RepoURL, Branch: "main"}),
		BuildValidationResultPrompt(scan, finding, binding),
	}
	for i, prompt := range prompts {
		if !strings.Contains(prompt, "Return exactly one JSON object") {
			t.Fatalf("prompt[%d] missing terminal JSON contract:\n%s", i, prompt)
		}
		if strings.Contains(prompt, "REQUIRED_SECURITY_ARTIFACTS") || strings.Contains(prompt, "Write these artifacts") {
			t.Fatalf("prompt[%d] retained artifact contract:\n%s", i, prompt)
		}
	}
}

func mustMarshalSecurityResult(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return data
}
