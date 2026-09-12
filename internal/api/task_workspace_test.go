package api

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRepositoryScanPatchTaskWorkspaceBindsRoleSeparatedCredentials(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:                      "https://github.com/example/source.git",
			ForkRepo:                     "https://github.com/example/fork.git",
			GitSecretRef:                 &corev1.LocalObjectReference{Name: "legacy-read"},
			ReadCredentialRef:            &corev1.LocalObjectReference{Name: "source-read"},
			PublicationReadCredentialRef: &corev1.LocalObjectReference{Name: "target-read"},
			PublicationCredentialRef:     &corev1.LocalObjectReference{Name: "target-write"},
			ForgeCredentialRef:           &corev1.LocalObjectReference{Name: "forge"},
		},
	}

	workspace := repositoryScanPatchTaskWorkspace(scan, "orka/fix")
	if workspace == nil {
		t.Fatal("repositoryScanPatchTaskWorkspace() returned nil")
	}
	if workspace.ReadCredentialRef == nil || workspace.ReadCredentialRef.Name != "source-read" {
		t.Fatalf("readCredentialRef = %#v, want source-read", workspace.ReadCredentialRef)
	}
	if workspace.PublicationReadCredentialRef == nil || workspace.PublicationReadCredentialRef.Name != "target-read" {
		t.Fatalf("publicationReadCredentialRef = %#v, want target-read", workspace.PublicationReadCredentialRef)
	}
	if workspace.PublicationCredentialRef == nil || workspace.PublicationCredentialRef.Name != "target-write" {
		t.Fatalf("publicationCredentialRef = %#v, want target-write", workspace.PublicationCredentialRef)
	}
	if workspace.ForgeCredentialRef == nil || workspace.ForgeCredentialRef.Name != "forge" {
		t.Fatalf("forgeCredentialRef = %#v, want forge", workspace.ForgeCredentialRef)
	}
	if !workspace.CreatePR {
		t.Fatal("createPR = false, want true")
	}
	if workspace.PRBaseBranch != "main" {
		t.Fatalf("prBaseBranch = %q, want main", workspace.PRBaseBranch)
	}
}

func TestRepositoryScanTaskWorkspaceUsesLegacyCredentialForReadOnlyFallback(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		RepoURL:      "https://github.com/example/source.git",
		GitSecretRef: &corev1.LocalObjectReference{Name: "legacy-read"},
	}}

	workspace := repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentRead)
	if workspace == nil || workspace.ReadCredentialRef == nil || workspace.ReadCredentialRef.Name != "legacy-read" {
		t.Fatalf("read workspace = %#v, want legacy read credential", workspace)
	}
	if workspace.PublicationReadCredentialRef != nil || workspace.PublicationCredentialRef != nil || workspace.ForgeCredentialRef != nil || workspace.CreatePR {
		t.Fatalf("read workspace gained publication authority: %#v", workspace)
	}
}

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

func TestRepositoryScanPatchCredentialRefsRequireDistinctExplicitRoles(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		GitSecretRef:                 &corev1.LocalObjectReference{Name: "legacy-read"},
		ReadCredentialRef:            &corev1.LocalObjectReference{Name: "source-read"},
		PublicationReadCredentialRef: &corev1.LocalObjectReference{Name: "target-read"},
		PublicationCredentialRef:     &corev1.LocalObjectReference{Name: "target-write"},
		ForgeCredentialRef:           &corev1.LocalObjectReference{Name: "forge"},
	}}
	if _, err := repositoryScanPatchCredentialRefs(scan); err != nil {
		t.Fatalf("repositoryScanPatchCredentialRefs() error = %v", err)
	}

	scan.Spec.ReadCredentialRef = nil
	if _, err := repositoryScanPatchCredentialRefs(scan); err == nil || !strings.Contains(err.Error(), "spec.readCredentialRef is required") {
		t.Fatalf("missing read credential error = %v", err)
	}

	scan.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: "source-read"}
	scan.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: "target-write"}
	if _, err := repositoryScanPatchCredentialRefs(scan); err == nil || !strings.Contains(err.Error(), "must reference distinct Secrets") {
		t.Fatalf("duplicate credential error = %v", err)
	}
}
