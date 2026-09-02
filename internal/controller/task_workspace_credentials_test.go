package controller

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testCanonicalScanSourceCloneURL = "https://github.com/example/source"

func TestRepositoryScanTaskWorkspaceCanonicalizesSSHRepositoryURLs(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		RepoURL:  "git@github.com:example/source.git",
		ForkRepo: "git@github.com:example/fork.git",
	}}

	read := repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentRead)
	if read == nil || read.GitRepo != testCanonicalScanSourceCloneURL {
		t.Fatalf("read workspace gitRepo = %#v, want canonical HTTPS clone URL", read)
	}
	if read.PublicationGitRepo != "" {
		t.Fatalf("read workspace publication repo = %q, want empty", read.PublicationGitRepo)
	}

	write := repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentWrite)
	if write == nil || write.GitRepo != testCanonicalScanSourceCloneURL ||
		write.PublicationGitRepo != "https://github.com/example/fork" {
		t.Fatalf("write workspace = %#v, want canonical HTTPS clone URLs", write)
	}

	scan.Spec.ForkRepo = ""
	write = repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentWrite)
	if write == nil || write.PublicationGitRepo != testCanonicalScanSourceCloneURL {
		t.Fatalf("fallback publication repo = %#v, want canonical source clone URL", write)
	}
}

func TestRepositoryScanTaskWorkspacePassesNonGitHubURLsThrough(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		RepoURL: "https://git.example.com/example/source.git",
	}}

	workspace := repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentRead)
	if workspace == nil || workspace.GitRepo != "https://git.example.com/example/source.git" {
		t.Fatalf("workspace gitRepo = %#v, want unchanged non-GitHub URL", workspace)
	}
}
