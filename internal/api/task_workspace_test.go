package api

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRepositoryScanPatchTaskWorkspaceBindsTargetReadCredential(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:      "https://github.com/example/source.git",
			ForkRepo:     "https://github.com/example/fork.git",
			GitSecretRef: &corev1.LocalObjectReference{Name: "git-credentials"},
		},
	}

	workspace := repositoryScanPatchTaskWorkspace(scan, "orka/fix")
	if workspace == nil {
		t.Fatal("repositoryScanPatchTaskWorkspace() returned nil")
	}
	if workspace.PublicationReadCredentialRef == nil || workspace.PublicationReadCredentialRef.Name != "git-credentials" {
		t.Fatalf("publicationReadCredentialRef = %#v, want git-credentials", workspace.PublicationReadCredentialRef)
	}
	if workspace.PublicationCredentialRef == nil || workspace.PublicationCredentialRef.Name != "git-credentials" {
		t.Fatalf("publicationCredentialRef = %#v, want git-credentials", workspace.PublicationCredentialRef)
	}
}
