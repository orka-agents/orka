package controller

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestWorkspaceRepositoryCanonicalizesGitHubOwnerRepoCasing(t *testing.T) {
	const wantID = "github.com/orka-agents/orka"
	variants := []string{
		"https://github.com/orka-agents/orka.git",
		"https://GitHub.COM/Orka-Agents/Orka.git",
		"https://github.com/ORKA-AGENTS/ORKA.git",
	}

	for _, rawURL := range variants {
		t.Run(rawURL, func(t *testing.T) {
			repository, err := workspaceRepository(&corev1alpha1.WorkspaceConfig{
				GitRepo: rawURL,
				SourceRepository: &corev1alpha1.RepositoryIdentity{
					Provider: workspaceRepositoryProviderGitHub,
					ID:       wantID,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if repository.ID != wantID {
				t.Fatalf("workspaceRepository().ID = %q, want %q", repository.ID, wantID)
			}
			if !sameCanonicalWorkspaceRepository(variants[0], rawURL) {
				t.Fatalf("sameCanonicalWorkspaceRepository(%q, %q) = false, want true", variants[0], rawURL)
			}
		})
	}
}

func TestCanonicalWorkspaceRepositoryURLPreservesNonGitHubPathCasing(t *testing.T) {
	upperURL := "https://git.example.com/Acme/Repo.git"
	lowerURL := "https://git.example.com/acme/repo.git"

	upperParsed, upperID, err := canonicalWorkspaceRepositoryURL(upperURL)
	if err != nil {
		t.Fatal(err)
	}
	lowerParsed, lowerID, err := canonicalWorkspaceRepositoryURL(lowerURL)
	if err != nil {
		t.Fatal(err)
	}

	if upperParsed.String() != upperURL || upperID != "git.example.com/Acme/Repo" {
		t.Fatalf("canonicalWorkspaceRepositoryURL(%q) = (%q, %q), want (%q, %q)", upperURL, upperParsed.String(), upperID, upperURL, "git.example.com/Acme/Repo")
	}
	if lowerParsed.String() != lowerURL || lowerID != "git.example.com/acme/repo" {
		t.Fatalf("canonicalWorkspaceRepositoryURL(%q) = (%q, %q), want (%q, %q)", lowerURL, lowerParsed.String(), lowerID, lowerURL, "git.example.com/acme/repo")
	}
	if upperID == lowerID {
		t.Fatalf("non-GitHub repository identities unexpectedly ignored path casing: %q", upperID)
	}
	if sameCanonicalWorkspaceRepository(upperURL, lowerURL) {
		t.Fatalf("sameCanonicalWorkspaceRepository(%q, %q) = true, want false", upperURL, lowerURL)
	}
}

func TestWorkspaceRepositoryAcceptsLegacyMixedCaseGitHubIdentity(t *testing.T) {
	workspace := &corev1alpha1.WorkspaceConfig{
		GitRepo: "https://github.com/Orka-Agents/Orka.git",
		SourceRepository: &corev1alpha1.RepositoryIdentity{
			Provider: workspaceRepositoryProviderGitHub,
			ID:       "github.com/Orka-Agents/Orka",
		},
		PublicationGitRepo: "https://github.com/SozerCan/Orka.git",
		PublicationRepository: &corev1alpha1.RepositoryIdentity{
			Provider: workspaceRepositoryProviderGitHub,
			ID:       "github.com/SozerCan/Orka",
		},
	}

	source, err := workspaceRepository(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if source.ID != "github.com/orka-agents/orka" {
		t.Fatalf("workspaceRepository().ID = %q, want canonical lowercase identity", source.ID)
	}
	target, err := workspacePublicationRepository(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "github.com/sozercan/orka" {
		t.Fatalf("workspacePublicationRepository().ID = %q, want canonical lowercase identity", target.ID)
	}
}

func TestWorkspaceRepositoryIdentityCompatibilityIsGitHubOnly(t *testing.T) {
	if !sameWorkspaceRepositoryIdentity("github.com/Orka-Agents/Orka", "github.com/orka-agents/orka") {
		t.Fatal("legacy mixed-case GitHub identity was not accepted")
	}
	if sameWorkspaceRepositoryIdentity("git.example.com/Acme/Repo", "git.example.com/acme/repo") {
		t.Fatal("non-GitHub repository identity unexpectedly ignored path casing")
	}
}
