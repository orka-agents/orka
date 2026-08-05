package controller

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	defaultACPWorkspaceCredentialKey = "token"
	maxACPWorkspaceCredentialBytes   = 32 << 10
)

// freezeWorkspaceCredentialVersions records only non-secret Secret
// resourceVersions at reservation. The credential broker later requires the
// exact version to remain current, so rotation after reservation fails closed
// instead of silently changing the operation's authority.
func (d *ACPDispatcher) freezeWorkspaceCredentialVersions(ctx context.Context, task *corev1alpha1.Task) error {
	if task == nil || task.Status.Execution == nil || task.Spec.Workspace == nil {
		return nil
	}
	workspace := task.Spec.Workspace
	execution := task.Status.Execution
	readVersion, err := d.workspaceCredentialResourceVersion(ctx, task, workspace.ReadCredentialRef)
	if err != nil {
		err = classifyACPWorkspaceCredentialFreezeError(
			err, store.PromptCredentialSourceRead, "read workspace credential", execution.ReadCredentialResourceVersion,
		)
		return fmt.Errorf("freeze read workspace credential: %w", err)
	}
	publicationReadVersion, err := d.workspaceCredentialResourceVersion(ctx, task, workspace.PublicationReadCredentialRef)
	if err != nil {
		err = classifyACPWorkspaceCredentialFreezeError(
			err, store.PromptCredentialTargetRead, "publication read credential", execution.PublicationReadCredentialResourceVersion,
		)
		return fmt.Errorf("freeze publication read credential: %w", err)
	}
	publicationVersion, err := d.workspaceCredentialResourceVersion(ctx, task, workspace.PublicationCredentialRef)
	if err != nil {
		err = classifyACPWorkspaceCredentialFreezeError(
			err, store.PromptCredentialTargetWrite, "publication write credential", execution.PublicationCredentialResourceVersion,
		)
		return fmt.Errorf("freeze publication write credential: %w", err)
	}
	forgeVersion, err := d.workspaceCredentialResourceVersion(ctx, task, workspace.ForgeCredentialRef)
	if err != nil {
		err = classifyACPWorkspaceCredentialFreezeError(
			err, store.PromptCredentialForge, "forge credential", execution.ForgeCredentialResourceVersion,
		)
		return fmt.Errorf("freeze forge credential: %w", err)
	}
	type frozenCredentialVersion struct {
		role     store.PromptCredentialRole
		expected string
		current  string
	}
	for label, version := range map[string]frozenCredentialVersion{
		"read credential":              {store.PromptCredentialSourceRead, execution.ReadCredentialResourceVersion, readVersion},
		"publication read credential":  {store.PromptCredentialTargetRead, execution.PublicationReadCredentialResourceVersion, publicationReadVersion},
		"publication write credential": {store.PromptCredentialTargetWrite, execution.PublicationCredentialResourceVersion, publicationVersion},
		"forge credential":             {store.PromptCredentialForge, execution.ForgeCredentialResourceVersion, forgeVersion},
	} {
		if version.expected != version.current {
			return &acpWorkspaceCredentialBlockedError{
				role: version.role, label: label, expectedResourceVersion: version.expected,
			}
		}
	}
	if execution.ReadCredentialResourceVersion == readVersion &&
		execution.PublicationReadCredentialResourceVersion == publicationReadVersion &&
		execution.PublicationCredentialResourceVersion == publicationVersion &&
		execution.ForgeCredentialResourceVersion == forgeVersion {
		return nil
	}
	return d.patchExecution(ctx, task, func(status *corev1alpha1.TaskExecutionStatus) {
		if status.ReadCredentialResourceVersion == "" {
			status.ReadCredentialResourceVersion = readVersion
		}
		if status.PublicationReadCredentialResourceVersion == "" {
			status.PublicationReadCredentialResourceVersion = publicationReadVersion
		}
		if status.PublicationCredentialResourceVersion == "" {
			status.PublicationCredentialResourceVersion = publicationVersion
		}
		if status.ForgeCredentialResourceVersion == "" {
			status.ForgeCredentialResourceVersion = forgeVersion
		}
	})
}

func (d *ACPDispatcher) workspaceCredentialResourceVersion(
	ctx context.Context,
	task *corev1alpha1.Task,
	reference *corev1alpha1.WorkspaceCredentialReference,
) (string, error) {
	if reference == nil || strings.TrimSpace(reference.Name) == "" {
		return "", nil
	}
	reader := client.Reader(d.Client)
	if d.APIReader != nil {
		reader = d.APIReader
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: strings.TrimSpace(reference.Name)}, secret); err != nil {
		return "", err
	}
	key := strings.TrimSpace(reference.Key)
	if key == "" {
		key = defaultACPWorkspaceCredentialKey
	}
	value, ok := secret.Data[key]
	if !ok || len(bytes.TrimSpace(value)) == 0 || len(value) > maxACPWorkspaceCredentialBytes || bytes.ContainsAny(value, "\x00") {
		return "", fmt.Errorf("secret key %q is missing or invalid", key)
	}
	if strings.TrimSpace(secret.ResourceVersion) == "" {
		return "", fmt.Errorf("secret resourceVersion is unavailable")
	}
	return secret.ResourceVersion, nil
}

type acpCredentialVersions struct {
	SourceRead  string
	TargetRead  string
	TargetWrite string
	Forge       string
}

func resolvePromptCredentialBindings(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
) ([]store.PromptCredentialBinding, acpCredentialVersions, error) {
	if reader == nil || task == nil || task.Spec.Workspace == nil {
		return nil, acpCredentialVersions{}, nil
	}
	type requestedBinding struct {
		role      store.PromptCredentialRole
		reference *corev1alpha1.WorkspaceCredentialReference
		set       func(*acpCredentialVersions, string)
	}
	workspace := task.Spec.Workspace
	requests := []requestedBinding{
		{store.PromptCredentialSourceRead, workspace.ReadCredentialRef, func(v *acpCredentialVersions, value string) { v.SourceRead = value }},
		{store.PromptCredentialTargetRead, workspace.PublicationReadCredentialRef, func(v *acpCredentialVersions, value string) { v.TargetRead = value }},
		{store.PromptCredentialTargetWrite, workspace.PublicationCredentialRef, func(v *acpCredentialVersions, value string) { v.TargetWrite = value }},
		{store.PromptCredentialForge, workspace.ForgeCredentialRef, func(v *acpCredentialVersions, value string) { v.Forge = value }},
	}
	bindings := make([]store.PromptCredentialBinding, 0, len(requests))
	versions := acpCredentialVersions{}
	for _, requested := range requests {
		if requested.reference == nil || strings.TrimSpace(requested.reference.Name) == "" {
			continue
		}
		secret := &corev1.Secret{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: strings.TrimSpace(requested.reference.Name)}, secret); err != nil {
			return nil, acpCredentialVersions{}, err
		}
		key := strings.TrimSpace(requested.reference.Key)
		if key == "" {
			key = defaultACPWorkspaceCredentialKey
		}
		value, ok := secret.Data[key]
		if !ok || len(bytes.TrimSpace(value)) == 0 || len(value) > maxACPWorkspaceCredentialBytes || bytes.ContainsAny(value, "\x00") {
			return nil, acpCredentialVersions{}, fmt.Errorf("secret key %q is missing or invalid", key)
		}
		binding := store.PromptCredentialBinding{
			Role: requested.role, Namespace: task.Namespace, SecretName: secret.Name, SecretKey: key,
			SecretUID: string(secret.UID), ResourceVersion: secret.ResourceVersion,
		}
		if err := binding.Validate(); err != nil {
			return nil, acpCredentialVersions{}, err
		}
		bindings = append(bindings, binding)
		requested.set(&versions, secret.ResourceVersion)
	}
	return bindings, versions, nil
}
