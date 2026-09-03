package api

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
)

type repositoryScanCredentialRef struct {
	field string
	ref   *corev1.LocalObjectReference
}

func taskWorkspaceCredentialReference(ref *corev1.LocalObjectReference) *corev1alpha1.WorkspaceCredentialReference {
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

func repositoryScanConfiguredCredentialRefs(scan *corev1alpha1.RepositoryScan) []repositoryScanCredentialRef {
	if scan == nil {
		return nil
	}
	return []repositoryScanCredentialRef{
		{field: "spec.gitSecretRef", ref: scan.Spec.GitSecretRef},
		{field: "spec.readCredentialRef", ref: scan.Spec.ReadCredentialRef},
		{field: "spec.publicationReadCredentialRef", ref: scan.Spec.PublicationReadCredentialRef},
		{field: "spec.publicationCredentialRef", ref: scan.Spec.PublicationCredentialRef},
		{field: "spec.forgeCredentialRef", ref: scan.Spec.ForgeCredentialRef},
	}
}

func repositoryScanPatchCredentialRefs(scan *corev1alpha1.RepositoryScan) ([]repositoryScanCredentialRef, error) {
	if scan == nil {
		return nil, fmt.Errorf("repository scan is required")
	}
	refs := []repositoryScanCredentialRef{
		{field: "spec.readCredentialRef", ref: scan.Spec.ReadCredentialRef},
		{field: "spec.publicationReadCredentialRef", ref: scan.Spec.PublicationReadCredentialRef},
		{field: "spec.publicationCredentialRef", ref: scan.Spec.PublicationCredentialRef},
		{field: "spec.forgeCredentialRef", ref: scan.Spec.ForgeCredentialRef},
	}
	seen := make(map[string]string, len(refs))
	for _, credential := range refs {
		name := ""
		if credential.ref != nil {
			name = strings.TrimSpace(credential.ref.Name)
		}
		if name == "" {
			return nil, fmt.Errorf("%s is required for repository scan patch publication", credential.field)
		}
		if previous := seen[name]; previous != "" {
			return nil, fmt.Errorf("%s and %s must reference distinct Secrets", previous, credential.field)
		}
		seen[name] = credential.field
	}
	return refs, nil
}

func repositoryScanTaskWorkspace(scan *corev1alpha1.RepositoryScan, intent corev1alpha1.WorkspaceIntent) *corev1alpha1.WorkspaceConfig {
	if scan == nil {
		return nil
	}
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent: intent,
		// The ACP workspace preflight admits only credential-free HTTPS URLs,
		// so accepted GitHub-style SSH roots are canonicalized here.
		GitRepo:           security.CanonicalRepositoryCloneURL(scan.Spec.RepoURL),
		Branch:            security.EffectiveWorkspaceBranch(scan),
		Ref:               security.EffectiveRef(scan),
		ReadCredentialRef: taskWorkspaceCredentialReference(repositoryScanReadCredentialRef(scan)),
		SubPath:           scan.Spec.SubPath,
	}
	if intent == corev1alpha1.WorkspaceIntentWrite {
		// prBaseBranch is a publication field: the ACP workspace preflight
		// rejects it on non-write intents.
		workspace.PRBaseBranch = scan.Spec.PRBaseBranch
		workspace.PublicationGitRepo = security.CanonicalRepositoryCloneURL(scan.Spec.ForkRepo)
		if workspace.PublicationGitRepo == "" {
			workspace.PublicationGitRepo = workspace.GitRepo
		}
		if strings.TrimSpace(workspace.PRBaseBranch) == "" {
			workspace.PRBaseBranch = security.EffectiveBranch(scan)
		}
		workspace.PublicationReadCredentialRef = taskWorkspaceCredentialReference(scan.Spec.PublicationReadCredentialRef)
		workspace.PublicationCredentialRef = taskWorkspaceCredentialReference(scan.Spec.PublicationCredentialRef)
		workspace.ForgeCredentialRef = taskWorkspaceCredentialReference(scan.Spec.ForgeCredentialRef)
		workspace.CreatePR = true
	}
	return workspace
}

func repositoryScanPatchTaskWorkspace(scan *corev1alpha1.RepositoryScan, branch string) *corev1alpha1.WorkspaceConfig {
	workspace := repositoryScanTaskWorkspace(scan, corev1alpha1.WorkspaceIntentWrite)
	if workspace != nil {
		workspace.PushBranch = strings.TrimSpace(branch)
		// The supervisor's delta check inspects the changed files' new
		// content, so a remediation that removes a hardcoded credential
		// still publishes (the secret is gone from the new content) while a
		// delta that introduces one fails closed — same policy the monitor
		// implementation tasks use.
		workspace.RejectSecretLikeContent = true
	}
	return workspace
}
