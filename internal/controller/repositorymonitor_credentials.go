package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

type repositoryMonitorCredentialRefs struct {
	read            *corev1.LocalObjectReference
	publicationRead *corev1.LocalObjectReference
	publication     *corev1.LocalObjectReference
	forge           *corev1.LocalObjectReference
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
	seen := make(map[string]string, len(fields))
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
	return monitor != nil && (repositoryMonitorWriteCredentialsRequired(monitor) || monitor.Spec.Triggers.GitHub.Labels.Enabled)
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
	return "", "", nil
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
