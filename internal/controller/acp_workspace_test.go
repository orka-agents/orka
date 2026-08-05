package controller

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	canonicalWorkspaceRepositoryTestURL = "https://github.com/orka-agents/orka.git"
	canonicalWorkspaceRepositoryTestID  = "github.com/orka-agents/orka"
)

func TestRuntimeWorkspaceForSessionContinuationUsesPublicationTargetReadCredential(t *testing.T) {
	sourceRead := &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"}
	targetRead := &corev1alpha1.WorkspaceCredentialReference{Name: "target-read"}
	workspace := &corev1alpha1.WorkspaceConfig{
		GitRepo:                      "https://github.com/orka-agents/source.git",
		SourceRepository:             &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka-agents/source"},
		Branch:                       "main",
		ReadCredentialRef:            sourceRead,
		PublicationGitRepo:           "https://github.com/orka-agents/target.git",
		PublicationRepository:        &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka-agents/target"},
		PublicationReadCredentialRef: targetRead,
	}
	baseline := &store.VerifiedBranchBaseline{
		RepositoryID: "github.com/orka-agents/target",
		Ref:          "refs/heads/orka/session-session-uid",
		SHA:          strings.Repeat("a", 40),
	}

	got, err := runtimeWorkspaceForSessionContinuation(workspace, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if got == workspace {
		t.Fatal("session continuation mutated the original workspace instead of using a copy")
	}
	if got.GitRepo != workspace.PublicationGitRepo || got.SourceRepository == nil || got.SourceRepository.ID != workspace.PublicationRepository.ID {
		t.Fatalf("continuation repository = %#v, want publication target", got)
	}
	if got.ReadCredentialRef == nil || got.ReadCredentialRef.Name != targetRead.Name {
		t.Fatalf("continuation read credential = %#v, want target read credential", got.ReadCredentialRef)
	}
	if got.Ref != baseline.Ref || got.Branch != "" {
		t.Fatalf("continuation source = ref %q branch %q, want verified ref %q", got.Ref, got.Branch, baseline.Ref)
	}
	if workspace.ReadCredentialRef == nil || workspace.ReadCredentialRef.Name != sourceRead.Name || workspace.GitRepo == workspace.PublicationGitRepo {
		t.Fatalf("source workspace was mutated: %#v", workspace)
	}

	conflicting := *workspace
	conflicting.ExpectedRemoteSHA = strings.Repeat("b", 40)
	if _, err := runtimeWorkspaceForSessionContinuation(&conflicting, baseline); err == nil {
		t.Fatal("conflicting expectedRemoteSHA unexpectedly accepted before prompt execution")
	}
}

func TestRuntimeWorkspaceForSessionContinuationClearsSourceIdentityForCrossRepositoryPublication(t *testing.T) {
	sourceIdentity := &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka-agents/source"}
	workspace := &corev1alpha1.WorkspaceConfig{
		GitRepo:            "https://github.com/orka-agents/source.git",
		SourceRepository:   sourceIdentity,
		PublicationGitRepo: "https://github.com/orka-agents/target.git",
	}
	baseline := &store.VerifiedBranchBaseline{
		RepositoryID: "github.com/orka-agents/target",
		Ref:          "refs/heads/orka/session-session-uid",
		SHA:          strings.Repeat("a", 40),
	}

	got, err := runtimeWorkspaceForSessionContinuation(workspace, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitRepo != workspace.PublicationGitRepo {
		t.Fatalf("continuation gitRepo = %q, want %q", got.GitRepo, workspace.PublicationGitRepo)
	}
	if got.SourceRepository != nil {
		t.Fatalf("continuation sourceRepository = %#v, want derived publication identity", got.SourceRepository)
	}
	if workspace.SourceRepository != sourceIdentity {
		t.Fatalf("source workspace identity was mutated: %#v", workspace.SourceRepository)
	}
}

func TestRuntimeWorkspaceForSessionContinuationPreservesSourceIdentityForSameRepositoryPublication(t *testing.T) {
	sourceIdentity := &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka-agents/source"}
	workspace := &corev1alpha1.WorkspaceConfig{
		GitRepo:            "https://GitHub.COM/orka-agents/source.git",
		SourceRepository:   sourceIdentity,
		PublicationGitRepo: "https://github.com/orka-agents/source",
	}
	baseline := &store.VerifiedBranchBaseline{
		RepositoryID: sourceIdentity.ID,
		Ref:          "refs/heads/orka/session-session-uid",
		SHA:          strings.Repeat("a", 40),
	}

	got, err := runtimeWorkspaceForSessionContinuation(workspace, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitRepo != workspace.PublicationGitRepo {
		t.Fatalf("continuation gitRepo = %q, want %q", got.GitRepo, workspace.PublicationGitRepo)
	}
	if got.SourceRepository == nil || *got.SourceRepository != *sourceIdentity {
		t.Fatalf("continuation sourceRepository = %#v, want %#v", got.SourceRepository, sourceIdentity)
	}
}

func TestRuntimeWorkspaceSourceRefDoesNotAssumeMain(t *testing.T) {
	tests := []struct {
		name      string
		workspace *corev1alpha1.WorkspaceConfig
		want      string
		wantErr   string
	}{
		{name: "explicit branch", workspace: &corev1alpha1.WorkspaceConfig{Branch: "trunk"}, want: "refs/heads/trunk"},
		{name: "full branch ref", workspace: &corev1alpha1.WorkspaceConfig{Branch: "refs/heads/release/v2"}, want: "refs/heads/release/v2"},
		{name: "exact ref", workspace: &corev1alpha1.WorkspaceConfig{Ref: "v2.0.0"}, want: "v2.0.0"},
		{name: "default resolved by publisher", workspace: &corev1alpha1.WorkspaceConfig{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := runtimeWorkspaceSourceRef(test.workspace)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("runtimeWorkspaceSourceRef() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("runtimeWorkspaceSourceRef() = %q, %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestCanonicalWorkspaceRepositoryURLAllowsOnlyDefaultHTTPSPort(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantURL string
		wantID  string
		wantErr string
	}{
		{
			name:    "implicit default port retains canonical host behavior",
			rawURL:  "https://GitHub.COM/orka-agents/orka.git",
			wantURL: canonicalWorkspaceRepositoryTestURL,
			wantID:  canonicalWorkspaceRepositoryTestID,
		},
		{
			name:    "explicit default port is accepted",
			rawURL:  "https://GitHub.COM:443/orka-agents/orka.git",
			wantURL: "https://github.com:443/orka-agents/orka.git",
			wantID:  canonicalWorkspaceRepositoryTestID,
		},
		{
			name:    "explicit non-default port is rejected",
			rawURL:  "https://github.com:8443/orka-agents/orka.git",
			wantErr: errWorkspaceRepositoryHTTPSPort.Error(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, id, err := canonicalWorkspaceRepositoryURL(test.rawURL)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("canonicalWorkspaceRepositoryURL() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parsed.String() != test.wantURL || id != test.wantID {
				t.Fatalf("canonicalWorkspaceRepositoryURL() = (%q, %q), want (%q, %q)", parsed.String(), id, test.wantURL, test.wantID)
			}
		})
	}
}
