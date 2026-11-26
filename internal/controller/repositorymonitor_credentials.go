package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
	"github.com/orka-agents/orka/internal/workerenv"
)

type repositoryMonitorCredentialRefs struct {
	read            *corev1.LocalObjectReference
	publicationRead *corev1.LocalObjectReference
	publication     *corev1.LocalObjectReference
	forge           *corev1.LocalObjectReference
	commitSigning   *corev1.LocalObjectReference
}

func repositoryMonitorReadCredentialRef(monitor *corev1alpha1.RepositoryMonitor) *corev1.LocalObjectReference {
	if monitor == nil {
		return nil
	}
	if localObjectReferenceName(monitor.Spec.ReadCredentialRef) != "" {
		return monitor.Spec.ReadCredentialRef
	}
	return monitor.Spec.GitSecretRef
}

func repositoryMonitorCredentialRefsForWrite(monitor *corev1alpha1.RepositoryMonitor) (repositoryMonitorCredentialRefs, error) {
	if monitor == nil {
		return repositoryMonitorCredentialRefs{}, fmt.Errorf("repository monitor is required")
	}
	refs := repositoryMonitorCredentialRefs{
		read:            monitor.Spec.ReadCredentialRef,
		publicationRead: monitor.Spec.PublicationReadCredentialRef,
		publication:     monitor.Spec.PublicationCredentialRef,
		forge:           monitor.Spec.ForgeCredentialRef,
		commitSigning:   monitor.Spec.Commit.SigningCredentialRef,
	}
	fields := []struct {
		name string
		ref  *corev1.LocalObjectReference
	}{
		{name: "spec.readCredentialRef", ref: refs.read},
		{name: "spec.publicationReadCredentialRef", ref: refs.publicationRead},
		{name: "spec.publicationCredentialRef", ref: refs.publication},
		{name: "spec.forgeCredentialRef", ref: refs.forge},
	}
	seen := make(map[string]string, len(fields)+1)
	for _, field := range fields {
		name := localObjectReferenceName(field.ref)
		if name == "" {
			return repositoryMonitorCredentialRefs{}, fmt.Errorf("%s is required for repository monitor write workflows", field.name)
		}
		if previous := seen[name]; previous != "" {
			return repositoryMonitorCredentialRefs{}, fmt.Errorf("%s and %s must reference distinct Secrets", previous, field.name)
		}
		seen[name] = field.name
	}
	if signingName := localObjectReferenceName(refs.commitSigning); signingName != "" {
		if previous := seen[signingName]; previous != "" {
			return repositoryMonitorCredentialRefs{}, fmt.Errorf("%s and spec.commit.signingCredentialRef must reference distinct Secrets", previous)
		}
	}
	return refs, nil
}

func localObjectReferenceName(ref *corev1.LocalObjectReference) string {
	if ref == nil {
		return ""
	}
	return strings.TrimSpace(ref.Name)
}

func repositoryMonitorWriteCredentialsRequired(monitor *corev1alpha1.RepositoryMonitor) bool {
	if monitor == nil {
		return false
	}
	implementationEnabled := monitor.Spec.Targets.Issues.Enabled &&
		(monitor.Spec.IssueWorkflow.Implementation.Enabled == nil || *monitor.Spec.IssueWorkflow.Implementation.Enabled) &&
		monitor.Spec.Agents.Implementer != nil && strings.TrimSpace(monitor.Spec.Agents.Implementer.Name) != ""
	repairEnabled := monitor.Spec.Repair.Enabled && monitor.Spec.Agents.Repairer != nil && strings.TrimSpace(monitor.Spec.Agents.Repairer.Name) != ""
	return implementationEnabled || repairEnabled
}

func repositoryMonitorForgeCredentialsRequired(monitor *corev1alpha1.RepositoryMonitor) bool {
	return monitor != nil && (repositoryMonitorWriteCredentialsRequired(monitor) || monitor.Spec.Triggers.GitHub.Labels.Enabled || monitor.Spec.Review.Publish.Enabled || monitor.Spec.Repair.Enabled)
}

func (r *RepositoryMonitorReconciler) validateRepositoryMonitorCredentialRefs(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (string, string, error) {
	if monitor == nil {
		return "", "", nil
	}
	if repositoryMonitorWriteCredentialsRequired(monitor) {
		if _, err := repositoryMonitorCredentialRefsForWrite(monitor); err != nil {
			return repositoryMonitorReasonGitSecretInvalid, err.Error(), nil
		}
	} else if repositoryMonitorForgeCredentialsRequired(monitor) && localObjectReferenceName(monitor.Spec.ForgeCredentialRef) == "" {
		return repositoryMonitorReasonGitSecretInvalid, "spec.forgeCredentialRef is required for controller-owned GitHub mutations", nil
	}

	refs := []struct {
		field string
		ref   *corev1.LocalObjectReference
	}{
		{field: "spec.readCredentialRef", ref: monitor.Spec.ReadCredentialRef},
		{field: "spec.gitSecretRef", ref: monitor.Spec.GitSecretRef},
		{field: "spec.publicationReadCredentialRef", ref: monitor.Spec.PublicationReadCredentialRef},
		{field: "spec.publicationCredentialRef", ref: monitor.Spec.PublicationCredentialRef},
		{field: "spec.forgeCredentialRef", ref: monitor.Spec.ForgeCredentialRef},
	}
	validated := map[string]struct{}{}
	for _, credential := range refs {
		name := localObjectReferenceName(credential.ref)
		if name == "" {
			continue
		}
		if _, ok := validated[name]; ok {
			continue
		}
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: monitor.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return repositoryMonitorReasonGitSecretInvalid, fmt.Sprintf("%s %q not found in namespace %q", credential.field, name, monitor.Namespace), nil
			}
			return "", "", err
		}
		if !repositoryMonitorGitSecretHasToken(&secret) {
			return repositoryMonitorReasonGitSecretInvalid, fmt.Sprintf("%s %q must contain a non-empty token, password, or %s key", credential.field, name, workerenv.GitHubToken), nil
		}
		validated[name] = struct{}{}
	}
	if _, _, err := publisher.ValidateCommitIdentity(monitor.Spec.Commit.AuthorName, monitor.Spec.Commit.AuthorEmail); err != nil {
		return repositoryMonitorReasonGitSecretInvalid, fmt.Sprintf("spec.commit identity is invalid: %v", err), nil
	}
	if signingName := localObjectReferenceName(monitor.Spec.Commit.SigningCredentialRef); signingName != "" {
		if _, reused := validated[signingName]; reused {
			return repositoryMonitorReasonGitSecretInvalid, "spec.commit.signingCredentialRef must reference a Secret distinct from SCM and forge credentials", nil
		}
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: signingName, Namespace: monitor.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return repositoryMonitorReasonGitSecretInvalid, fmt.Sprintf("spec.commit.signingCredentialRef %q not found in namespace %q", signingName, monitor.Namespace), nil
			}
			return "", "", err
		}
		if _, err := repositoryMonitorSigningKeySecretKey(&secret); err != nil {
			return repositoryMonitorReasonGitSecretInvalid, fmt.Sprintf("spec.commit.signingCredentialRef %q: %v", signingName, err), nil
		}
	}
	return "", "", nil
}

var repositoryMonitorSigningKeyCandidateKeys = []string{"signingKey", "key", "id_ed25519"}

func repositoryMonitorSigningKeySecretKey(secret *corev1.Secret) (string, error) {
	if secret == nil {
		return "", fmt.Errorf("secret is required")
	}
	for _, key := range repositoryMonitorSigningKeyCandidateKeys {
		raw := secret.Data[key]
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		if _, _, err := publisher.ParseSSHSigningKey(raw); err != nil {
			return "", fmt.Errorf("key %q is not a valid unencrypted OpenSSH private key: %w", key, err)
		}
		return key, nil
	}
	return "", fmt.Errorf("must contain an unencrypted OpenSSH private key under signingKey, key, or id_ed25519")
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCommitSigningCredentialRef(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (*corev1alpha1.WorkspaceCredentialReference, error) {
	if monitor == nil {
		return nil, nil
	}
	name := localObjectReferenceName(monitor.Spec.Commit.SigningCredentialRef)
	if name == "" {
		return nil, nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: monitor.Namespace}, &secret); err != nil {
		return nil, fmt.Errorf("get commit signing Secret %q: %w", name, err)
	}
	key, err := repositoryMonitorSigningKeySecretKey(&secret)
	if err != nil {
		return nil, fmt.Errorf("commit signing Secret %q: %w", name, err)
	}
	return &corev1alpha1.WorkspaceCredentialReference{Name: name, Key: key}, nil
}

func repositoryMonitorCommitAuthor(monitor *corev1alpha1.RepositoryMonitor) (string, string) {
	if monitor == nil {
		return publisher.CommitAuthorName, publisher.CommitAuthorEmail
	}
	name, email, err := publisher.ValidateCommitIdentity(monitor.Spec.Commit.AuthorName, monitor.Spec.Commit.AuthorEmail)
	if err != nil {
		return publisher.CommitAuthorName, publisher.CommitAuthorEmail
	}
	return name, email
}

func repositoryMonitorCommitSignOffTrailer(monitor *corev1alpha1.RepositoryMonitor) string {
	if monitor == nil || !monitor.Spec.Commit.SignOff {
		return ""
	}
	name, email := repositoryMonitorCommitAuthor(monitor)
	return "Signed-off-by: " + name + " <" + email + ">\n"
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCredentialToken(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, field string, ref *corev1.LocalObjectReference) (string, error) {
	name := localObjectReferenceName(ref)
	if name == "" {
		return "", nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: monitor.Namespace}, &secret); err != nil {
		return "", fmt.Errorf("failed to get repository monitor %s %q: %w", field, name, err)
	}
	for _, key := range []string{repositoryMonitorTokenKey, repositoryMonitorPasswordKey, workerenv.GitHubToken} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("repository monitor %s %q must contain a token, password, or %s key", field, name, workerenv.GitHubToken)
}

func (r *RepositoryMonitorReconciler) repositoryMonitorForgeToken(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (string, error) {
	if monitor == nil || localObjectReferenceName(monitor.Spec.ForgeCredentialRef) == "" {
		return "", fmt.Errorf("spec.forgeCredentialRef is required for controller-owned GitHub mutations")
	}
	return r.repositoryMonitorCredentialToken(ctx, monitor, "forge credential Secret", monitor.Spec.ForgeCredentialRef)
}
