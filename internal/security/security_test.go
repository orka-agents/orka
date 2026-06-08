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

func TestEffectiveWorkspaceBranch(t *testing.T) {
	tests := []struct {
		name string
		spec corev1alpha1.RepositoryScanSpec
		want string
	}{
		{
			name: "explicit branch wins",
			spec: corev1alpha1.RepositoryScanSpec{Branch: "release", Ref: "v1.2.3"},
			want: "release",
		},
		{
			name: "ref only omits implicit branch",
			spec: corev1alpha1.RepositoryScanSpec{Ref: "v1.2.3"},
			want: "",
		},
		{
			name: "default branch without ref",
			spec: corev1alpha1.RepositoryScanSpec{},
			want: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := &corev1alpha1.RepositoryScan{Spec: tt.spec}
			if got := EffectiveWorkspaceBranch(scan); got != tt.want {
				t.Fatalf("EffectiveWorkspaceBranch() = %q, want %q", got, tt.want)
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
	if strings.Contains(got, "security-findings") {
		t.Fatalf("BuildThreatModelPrompt() unexpectedly references findings artifact:\n%s", got)
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

func TestValidationArtifactEvidenceRefsUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []store.FindingEvidenceRef
	}{
		{
			name: "string shorthand",
			raw:  `"validation transcript"`,
			want: []store.FindingEvidenceRef{{Kind: "note", Label: "validation transcript"}},
		},
		{
			name: "object shorthand",
			raw:  `{"kind":"artifact","name":"security-validation.txt","label":"trace"}`,
			want: []store.FindingEvidenceRef{{Kind: "artifact", Name: "security-validation.txt", Label: "trace"}},
		},
		{
			name: "mixed array",
			raw:  `["validation transcript",{"kind":"artifact","name":"security-validation.txt"},null,"  "]`,
			want: []store.FindingEvidenceRef{
				{Kind: "note", Label: "validation transcript"},
				{Kind: "artifact", Name: "security-validation.txt"},
			},
		},
		{
			name: "null",
			raw:  `null`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ValidationArtifactEvidenceRefs
			if err := json.Unmarshal([]byte(tt.raw), &got); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(got) = %d, want %d: %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d] = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
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
	if !strings.Contains(got, "REQUIRED_SECURITY_ARTIFACTS: security-patch-fnd_123.diff, security-patch-fnd_123.json") {
		t.Fatalf("BuildPatchPrompt() missing required patch artifacts directive:\n%s", got)
	}
	if !strings.Contains(got, `"schemaVersion":1,"findingId":"fnd_123"`) {
		t.Fatalf("BuildPatchPrompt() missing patch summary schema:\n%s", got)
	}
	if !strings.Contains(got, "changedFiles array must exactly match") {
		t.Fatalf("BuildPatchPrompt() missing changedFiles verification guidance:\n%s", got)
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
