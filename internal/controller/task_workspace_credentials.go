package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
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
	if scan == nil {
		return nil
	}
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent:            intent,
		GitRepo:           scan.Spec.RepoURL,
		Branch:            security.EffectiveWorkspaceBranch(scan),
		Ref:               security.EffectiveRef(scan),
		ReadCredentialRef: workspaceCredentialReference(repositoryScanReadCredentialRef(scan)),
		SubPath:           scan.Spec.SubPath,
	}
	if intent == corev1alpha1.WorkspaceIntentWrite {
		// prBaseBranch is a publication field: the ACP workspace preflight
		// rejects it on non-write intents.
		workspace.PRBaseBranch = scan.Spec.PRBaseBranch
		workspace.PublicationGitRepo = strings.TrimSpace(scan.Spec.ForkRepo)
		if workspace.PublicationGitRepo == "" {
			workspace.PublicationGitRepo = scan.Spec.RepoURL
		}
		workspace.PublicationReadCredentialRef = workspaceCredentialReference(scan.Spec.PublicationReadCredentialRef)
		workspace.PublicationCredentialRef = workspaceCredentialReference(scan.Spec.PublicationCredentialRef)
		workspace.ForgeCredentialRef = workspaceCredentialReference(scan.Spec.ForgeCredentialRef)
	}
	return workspace
}
