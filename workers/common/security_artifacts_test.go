package common

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/security"
)

func TestEnsureRequiredSecurityArtifactsFollowUpWritesMissingArtifact(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	cfg := &AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: security-findings.v2.json",
	}
	findings := security.FindingsV2Artifact{
		SchemaVersion: security.SchemaVersionFindingsV2,
		Repository: security.FindingsV2Repository{
			RepoURL: "https://github.com/sozercan/actions-test.git",
			Branch:  "main",
		},
		Scan:     security.FindingsV2Scan{Mode: "initial", SliceID: "slice_app", Summary: "No findings in scope"},
		Findings: []security.FindingsV2Finding{},
	}
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	result, err := EnsureRequiredSecurityArtifacts(
		context.Background(),
		cfg,
		"analysis summary only",
		func(_ context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "security-findings.v2.json") {
				t.Fatalf("follow-up prompt = %q, want security-findings.v2.json", prompt)
			}
			if err := WriteArtifactFile(security.ArtifactFindingsV2, data); err != nil {
				return "", err
			}
			return "SECURITY_ARTIFACTS_WRITTEN", nil
		},
	)
	if err != nil {
		t.Fatalf("EnsureRequiredSecurityArtifacts() error = %v", err)
	}
	if !strings.Contains(result, "SECURITY_ARTIFACTS_WRITTEN") {
		t.Fatalf("result = %q, want follow-up confirmation appended", result)
	}

	saved, err := os.ReadFile(filepath.Join(artifactsDir(), security.ArtifactFindingsV2))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(saved) != string(data) {
		t.Fatalf("saved artifact = %s, want %s", string(saved), string(data))
	}
}

func TestEnsureRequiredSecurityArtifactsRecoversFromTranscript(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	cfg := &AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: security-findings.v2.json",
	}

	transcript := "cat > /workspace/.orka-artifacts/security-findings.v2.json << 'EOF'\n" +
		`{"schemaVersion":2,"repository":{` +
		`"repoURL":"https://github.com/sozercan/actions-test.git",` +
		`"branch":"main","subPath":"","headSHA":"","baseSHA":""},` +
		`"scan":{"mode":"initial","sliceId":"slice_app","summary":"empty"},` +
		`"findings":[]}` +
		"\nEOF"

	if _, err := EnsureRequiredSecurityArtifacts(context.Background(), cfg, transcript, nil); err != nil {
		t.Fatalf("EnsureRequiredSecurityArtifacts() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(artifactsDir(), security.ArtifactFindingsV2)); err != nil {
		t.Fatalf("artifact not recovered: %v", err)
	}
}

func TestEnsureRequiredSecurityArtifactsRecoversPatchArtifactsFromTranscript(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	findingID := "fnd_patch_123"
	diffName := "security-patch-" + findingID + ".diff"
	summaryName := "security-patch-" + findingID + ".json"
	cfg := &AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: " + diffName + ", " + summaryName,
	}

	diff := strings.Join([]string{
		"diff --git a/app.py b/app.py",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	summary := security.PatchSummaryArtifact{
		SchemaVersion: security.SchemaVersionPatchSummary,
		FindingID:     findingID,
		Summary:       "patched",
		ChangedFiles:  []string{"app.py"},
		Risk:          "low",
	}
	summaryData, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("json.Marshal(summary) error = %v", err)
	}
	transcript := "cat > /workspace/.orka-artifacts/" + diffName + " << 'EOF'\n" +
		diff +
		"EOF\n" +
		"cat > /workspace/.orka-artifacts/" + summaryName + " << 'EOF'\n" +
		string(summaryData) +
		"\nEOF"

	if _, err := EnsureRequiredSecurityArtifacts(context.Background(), cfg, transcript, nil); err != nil {
		t.Fatalf("EnsureRequiredSecurityArtifacts() error = %v", err)
	}

	savedDiff, err := os.ReadFile(filepath.Join(artifactsDir(), diffName))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if string(savedDiff) != strings.TrimSuffix(diff, "\n") {
		t.Fatalf("saved diff = %q, want %q", string(savedDiff), strings.TrimSuffix(diff, "\n"))
	}
	savedSummary, err := os.ReadFile(filepath.Join(artifactsDir(), summaryName))
	if err != nil {
		t.Fatalf("ReadFile(summary) error = %v", err)
	}
	if string(savedSummary) != string(summaryData) {
		t.Fatalf("saved summary = %s, want %s", string(savedSummary), string(summaryData))
	}
}

func TestValidArtifactCandidateRejectsPatchSummaryFindingMismatch(t *testing.T) {
	if validArtifactCandidate("security-patch-fnd_expected.json", []byte(
		`{"schemaVersion":1,"findingId":"fnd_other","summary":"patched","changedFiles":["app.py"],"risk":"low"}`,
	)) {
		t.Fatal("validArtifactCandidate() = true, want false for mismatched patch summary findingId")
	}
}

func TestEnsureRequiredSecurityArtifactsRestoresGeneratedReviewContextManifest(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	contextArtifact := security.ReviewContextArtifactName("slice_app")
	generatedManifest := []byte(
		`{"schemaVersion":1,"sliceId":"slice_app","includedFiles":[` +
			`{"path":"app.go","role":"owned","bytes":20,"includedBytes":20,` +
			`"includedLineRanges":[{"startLine":1,"endLine":2}],` +
			`"truncated":false,"readable":true,"skippedReason":null}` +
			`],"promptBytes":80,"approximateTokens":20}`,
	)
	modelManifest := `{"schemaVersion":1,"sliceId":"slice_app","includedFiles":[` +
		`{"path":"extra.go","role":"owned","bytes":20,"includedBytes":20,` +
		`"includedLineRanges":[{"startLine":1,"endLine":50}],` +
		`"truncated":false,"readable":true,"skippedReason":null}` +
		`],"promptBytes":80,"approximateTokens":20}`
	findings := `{"schemaVersion":2,` +
		`"repository":{"repoURL":"https://github.com/example/repo","branch":"main",` +
		`"subPath":"","baseSHA":"","headSHA":""},` +
		`"scan":{"mode":"manual","sliceId":"slice_app","summary":"empty"},` +
		`"findings":[]}`

	cfg := &AgentConfig{
		Prompt:                        "REQUIRED_SECURITY_ARTIFACTS: security-findings.v2.json",
		securityReviewContextArtifact: contextArtifact,
		securityReviewContextManifest: generatedManifest,
	}
	transcript := "cat > /workspace/.orka-artifacts/" + contextArtifact + " << 'EOF'\n" +
		modelManifest +
		"\nEOF\n" +
		"cat > /workspace/.orka-artifacts/security-findings.v2.json << 'EOF'\n" +
		findings +
		"\nEOF"

	if _, err := EnsureRequiredSecurityArtifacts(context.Background(), cfg, transcript, nil); err != nil {
		t.Fatalf("EnsureRequiredSecurityArtifacts() error = %v", err)
	}

	manifestData, err := os.ReadFile(filepath.Join(artifactsDir(), contextArtifact))
	if err != nil {
		t.Fatalf("ReadFile(context artifact) error = %v", err)
	}
	manifest, err := security.ParseReviewContextManifest(manifestData)
	if err != nil {
		t.Fatalf("ParseReviewContextManifest() error = %v", err)
	}
	if len(manifest.IncludedFiles) != 1 || manifest.IncludedFiles[0].Path != "app.go" {
		t.Fatalf("restored manifest = %#v, want worker-generated app.go manifest", manifest)
	}
}

func cleanupSecurityArtifactsDir(t *testing.T) {
	t.Helper()
	t.Setenv(artifactsDirEnv, filepath.Join(t.TempDir(), "artifacts"))
	if err := os.RemoveAll(artifactsDir()); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", artifactsDir(), err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(artifactsDir())
	})
}
