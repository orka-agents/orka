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

func TestParseGitHubRepositoryURL(t *testing.T) {
	tests := []struct {
		name      string
		repoURL   string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{
			name:      "HTTPS URL",
			repoURL:   "https://github.com/example/project",
			wantOwner: "example",
			wantRepo:  "project",
		},
		{
			name:      "HTTPS URL with git suffix and trailing slash",
			repoURL:   "https://github.com/example/project.git/",
			wantOwner: "example",
			wantRepo:  "project",
		},
		{
			name:      "SSH URL",
			repoURL:   "git@github.com:example/project.git",
			wantOwner: "example",
			wantRepo:  "project",
		},
		{
			name:    "rejects credentials",
			repoURL: "https://token@github.com/example/project",
			wantErr: true,
		},
		{
			name:    "rejects SSH URL query",
			repoURL: "git@github.com:example/project?token=secret",
			wantErr: true,
		},
		{
			name:    "rejects SSH URL credential-like repo",
			repoURL: "git@github.com:example/project@secret",
			wantErr: true,
		},
		{
			name:    "rejects non GitHub host",
			repoURL: "https://example.com/example/project",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := ParseGitHubRepositoryURL(tt.repoURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseGitHubRepositoryURL(%q) succeeded, want error", tt.repoURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGitHubRepositoryURL(%q) error = %v", tt.repoURL, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Fatalf("ParseGitHubRepositoryURL(%q) = %q/%q, want %q/%q", tt.repoURL, owner, repo, tt.wantOwner, tt.wantRepo)
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
		{name: "string array shorthand", raw: `["inline evidence"]`, want: 1},
		{name: "array of strings", raw: `["note one","note two"]`, want: 2},
		{name: "mixed array shorthand", raw: `["inline evidence", {"kind":"artifact","name":"file.txt"}]`, want: 2},
		{name: "mixed array with blanks", raw: `["inline evidence",{"kind":"artifact","name":"file.txt","label":"trace"},null,"  "]`, want: 2},
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

	var got FindingsArtifactEvidenceRefs
	if err := json.Unmarshal([]byte(`["inline evidence"]`), &got); err != nil {
		t.Fatalf("json.Unmarshal(array shorthand) error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Kind != "note" || got[0].Label != "inline evidence" {
		t.Fatalf("got[0] = %#v, want note shorthand normalization", got[0])
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

func TestBuildPatchPromptRequiresWorkspaceEditAndManagedPush(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/project",
			Branch:  "main",
		},
	}

	finding := &store.Finding{
		ID:         "fnd_123",
		Title:      "Command injection",
		Severity:   "critical",
		Confidence: "high",
	}

	got := BuildPatchPrompt(scan, finding, "orka/security/fnd-123")
	if !strings.Contains(got, "patch branch orka/security/fnd-123") {
		t.Fatalf("BuildPatchPrompt() missing patch branch guidance:\n%s", got)
	}
	if !strings.Contains(got, "Apply the fix directly to the checked-out workspace files.") {
		t.Fatalf("BuildPatchPrompt() missing workspace-edit directive:\n%s", got)
	}
	if !strings.Contains(got, "Do not commit, push, or open a pull request directly.") {
		t.Fatalf("BuildPatchPrompt() missing no-manual-push instruction:\n%s", got)
	}
	if !strings.Contains(got, "Orka can create the commit and push it to the patch branch automatically.") {
		t.Fatalf("BuildPatchPrompt() missing Orka-managed push instruction:\n%s", got)
	}
}

func TestGeneratedSecurityTaskNamesStayLabelSafe(t *testing.T) {
	scanName := "demo-security-repository-security1-1776034262"

	names := []string{
		ScanTaskName(scanName, "initial"),
		ScanStageTaskName(scanName, "initial", "threat-model", ""),
		ScanStageTaskName(scanName, "initial", "discovery", "ci-cd-supply-chain"),
		ScanStageTaskName(scanName, "initial", "discovery", "ci-cd-supply-chain-4"),
		PatchTaskName(scanName, "fnd_1234567890abcdef"),
	}

	for _, name := range names {
		if len(name) > 63 {
			t.Fatalf("generated task name %q has length %d, want <= 63", name, len(name))
		}
		if strings.Contains(name, "--") {
			t.Fatalf("generated task name %q should not contain duplicate separators", name)
		}
	}
}

func TestPatchBranchUsesUniqueTaskHash(t *testing.T) {
	branchA := PatchBranch("fnd_1234567890abcdef", "demo-security-repository-patch-a")
	branchB := PatchBranch("fnd_1234567890abcdef", "demo-security-repository-patch-b")

	if !strings.HasPrefix(branchA, "orka/security/fnd-1234567890abcdef-") {
		t.Fatalf("PatchBranch() = %q, want finding prefix preserved", branchA)
	}
	if branchA == branchB {
		t.Fatalf("PatchBranch() should vary by task name, got identical branches %q", branchA)
	}
}
