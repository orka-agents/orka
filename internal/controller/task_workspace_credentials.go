package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func workspaceCredentialReference(ref *corev1.LocalObjectReference) *corev1alpha1.WorkspaceCredentialReference {
	if ref == nil || strings.TrimSpace(ref.Name) == "" {
		return nil
	}
	return &corev1alpha1.WorkspaceCredentialReference{Name: strings.TrimSpace(ref.Name)}
}

func repositoryScanReadCredentialRef(scan *corev1alpha1.RepositoryScan) *corev1.LocalObjectReference {
	if scan == nil {
		return nil
	}
	if scan.Spec.ReadCredentialRef != nil && strings.TrimSpace(scan.Spec.ReadCredentialRef.Name) != "" {
		return scan.Spec.ReadCredentialRef
	}
	return scan.Spec.GitSecretRef
}

func repositoryScanTaskWorkspace(scan *corev1alpha1.RepositoryScan, intent corev1alpha1.WorkspaceIntent) *corev1alpha1.WorkspaceConfig {
	return repositoryScanTaskWorkspaceAtCommit(scan, intent, "")
}

func repositoryScanTaskWorkspaceAtCommit(scan *corev1alpha1.RepositoryScan, intent corev1alpha1.WorkspaceIntent, pinnedCommit string) *corev1alpha1.WorkspaceConfig {
	if scan == nil {
		return nil
	}
	ref := security.EffectiveRef(scan)
	if candidate := strings.TrimSpace(pinnedCommit); candidate != "" && store.ValidateGitObjectID("pinned scan commit", candidate) == nil {
		ref = candidate
	}
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent:            intent,
		GitRepo:           security.CanonicalRepositoryCloneURL(scan.Spec.RepoURL),
		Branch:            security.EffectiveWorkspaceBranch(scan),
		Ref:               ref,
		ReadCredentialRef: workspaceCredentialReference(repositoryScanReadCredentialRef(scan)),
		SubPath:           scan.Spec.SubPath,
	}
	if intent == corev1alpha1.WorkspaceIntentWrite {
		workspace.PRBaseBranch = scan.Spec.PRBaseBranch
		workspace.PublicationGitRepo = security.CanonicalRepositoryCloneURL(scan.Spec.ForkRepo)
		if workspace.PublicationGitRepo == "" {
			workspace.PublicationGitRepo = workspace.GitRepo
		}
		workspace.PublicationReadCredentialRef = workspaceCredentialReference(scan.Spec.PublicationReadCredentialRef)
		workspace.PublicationCredentialRef = workspaceCredentialReference(scan.Spec.PublicationCredentialRef)
		workspace.ForgeCredentialRef = workspaceCredentialReference(scan.Spec.ForgeCredentialRef)
	}
	return workspace
}
