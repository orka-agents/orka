package api

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
)

func taskWorkspaceCredentialReference(ref *corev1.LocalObjectReference) *corev1alpha1.WorkspaceCredentialReference {
	if ref == nil || strings.TrimSpace(ref.Name) == "" {
		return nil
	}
	return &corev1alpha1.WorkspaceCredentialReference{Name: strings.TrimSpace(ref.Name)}
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
		ReadCredentialRef: taskWorkspaceCredentialReference(scan.Spec.GitSecretRef),
		SubPath:           scan.Spec.SubPath,
		PRBaseBranch:      scan.Spec.PRBaseBranch,
	}
	if intent == corev1alpha1.WorkspaceIntentWrite {
		workspace.PublicationGitRepo = strings.TrimSpace(scan.Spec.ForkRepo)
		if workspace.PublicationGitRepo == "" {
			workspace.PublicationGitRepo = scan.Spec.RepoURL
		}
		workspace.PublicationReadCredentialRef = taskWorkspaceCredentialReference(scan.Spec.GitSecretRef)
		workspace.PublicationCredentialRef = taskWorkspaceCredentialReference(scan.Spec.GitSecretRef)
	}
	return workspace
}

func repositoryScanPatchTaskWorkspace(scan *corev1alpha1.RepositoryScan, branch string) *corev1alpha1.WorkspaceConfig {
	workspace := repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentWrite)
	if workspace != nil {
		workspace.PushBranch = strings.TrimSpace(branch)
	}
	return workspace
}
