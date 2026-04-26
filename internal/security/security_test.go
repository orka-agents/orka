package security

import (
	"encoding/json"
	"strings"
	"testing"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

func TestArtifactWorkspacePath(t *testing.T) {
	tests := []struct {
		name    string
		subPath string
		want    string
	}{
		{name: "root", subPath: "", want: ArtifactWorkspaceDir},
		{name: "single level", subPath: "services", want: "../" + ArtifactWorkspaceDir},
		{name: "nested", subPath: "services/api", want: "../../" + ArtifactWorkspaceDir},
		{name: "normalizes slashes", subPath: "/services/api/", want: "../../" + ArtifactWorkspaceDir},
		{name: "ignores traversal", subPath: "../services", want: ArtifactWorkspaceDir},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ArtifactWorkspacePath(tt.subPath); got != tt.want {
				t.Fatalf("ArtifactWorkspacePath(%q) = %q, want %q", tt.subPath, got, tt.want)
			}
		})
	}
}

func TestFindingsArtifactEvidenceRefsUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{name: "array", raw: `[{"kind":"artifact","name":"file.txt","label":"trace"}]`, want: 1},
		{name: "string shorthand", raw: `"inline evidence"`, want: 1},
		{name: "object shorthand", raw: `{"kind":"artifact","name":"file.txt"}`, want: 1},
		{name: "null", raw: `null`, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got FindingsArtifactEvidenceRefs
			if err := json.Unmarshal([]byte(tt.raw), &got); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("len(got) = %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestBuildThreatModelPromptRequiresThreatModelOnly(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/project",
			Branch:  "main",
		},
	}

	got := BuildThreatModelPrompt(scan, "manual", "", "", "# Existing")
	if !strings.Contains(got, "Your only job in this stage is to understand the repository and produce a strong, reusable threat model.") {
		t.Fatalf("BuildThreatModelPrompt() missing stage instruction:\n%s", got)
	}
	if !strings.Contains(got, "REQUIRED_SECURITY_ARTIFACTS: security-threat-model.md") {
		t.Fatalf("BuildThreatModelPrompt() missing required artifacts directive:\n%s", got)
	}
	if strings.Contains(got, "security-findings.json") {
		t.Fatalf("BuildThreatModelPrompt() unexpectedly references findings artifact:\n%s", got)
	}
}

func TestBuildDiscoveryPromptRequiresFindingsOnly(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/project",
			Branch:  "main",
		},
	}

	got := BuildDiscoveryPrompt(scan, "manual", "abc", "def", "# Threat Model", DiscoveryScopes()[0])
	if !strings.Contains(got, "Do not rewrite the threat model in this stage.") {
		t.Fatalf("BuildDiscoveryPrompt() missing discovery-stage instruction:\n%s", got)
	}
	if !strings.Contains(got, "REQUIRED_SECURITY_ARTIFACTS: security-findings.json") {
		t.Fatalf("BuildDiscoveryPrompt() missing findings directive:\n%s", got)
	}
	if !strings.Contains(got, "Shared threat model context:\n# Threat Model") {
		t.Fatalf("BuildDiscoveryPrompt() missing threat model context:\n%s", got)
	}
}

func TestBuildValidationPromptIncludesAttackPathAnalysis(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/project",
			Branch:  "main",
		},
	}

	finding := &store.Finding{
		ID:         "fnd_123",
		Title:      "Command injection",
		Severity:   "high",
		Confidence: "high",
		FilePath:   "cmd/run.go",
		Line:       42,
	}

	got := BuildValidationPrompt(scan, finding)
	if !strings.Contains(got, "REQUIRED_SECURITY_ARTIFACTS: security-validation.json") {
		t.Fatalf("BuildValidationPrompt() missing validation directive:\n%s", got)
	}
	if !strings.Contains(got, "attack_path_analysis") {
		t.Fatalf("BuildValidationPrompt() missing attack path schema:\n%s", got)
	}
}

func TestBuildPatchPromptRequiresWorkspaceEdit(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/project",
			Branch:  "main",
		},
	}

	finding := &store.Finding{
		ID:         "fnd_123",
		Title:      "Command injection",
		Severity:   "high",
		Confidence: "high",
	}

	got := BuildPatchPrompt(scan, finding)
	if !strings.Contains(got, "Apply the fix directly to the checked-out workspace files.") {
		t.Fatalf("BuildPatchPrompt() missing workspace-edit directive:\n%s", got)
	}
	if !strings.Contains(got, "Orka will commit and push the workspace changes for you.") {
		t.Fatalf("BuildPatchPrompt() missing push-handling directive:\n%s", got)
	}
}
