/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"errors"
	"fmt"
	"path"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
)

// resolveHarnessV1PublicReadOnlyWorkspace admits the one workspace lane whose
// safety properties the compatibility wrapper can enforce: a credential-free
// public source cloned for a built-in Codex turn under its read-only sandbox.
func resolveHarnessV1PublicReadOnlyWorkspace(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	target resolvedHarnessV1Target,
) (*corev1alpha1.WorkspaceConfig, error) {
	if task == nil || task.Spec.Workspace == nil {
		return nil, nil
	}
	if agent == nil || agent.Spec.Runtime == nil {
		return nil, errors.New("harness v1 public read-only workspace requires an Agent")
	}
	if target.backend != corev1alpha1.AgentExecutionBackendHarnessWrapper || target.runtimeRef != nil {
		return nil, errors.New("harness v1 public read-only workspace requires the built-in wrapper")
	}
	if agent.Spec.Runtime.Type != corev1alpha1.AgentRuntimeCodex {
		return nil, fmt.Errorf(
			"harness v1 public read-only workspace does not support runtime %q",
			agent.Spec.Runtime.Type,
		)
	}
	if err := validateHarnessV1PublicReadOnlyWorkspace(task.Spec.Workspace); err != nil {
		return nil, err
	}
	return task.Spec.Workspace.DeepCopy(), nil
}

func validateHarnessV1PublicReadOnlyWorkspace(workspace *corev1alpha1.WorkspaceConfig) error {
	if workspace == nil {
		return nil
	}
	if workspace.Intent != "" && workspace.Intent != corev1alpha1.WorkspaceIntentRead {
		return errors.New("harness v1 public workspace requires read intent")
	}
	if workspace.GitRepo == "" || workspace.GitRepo != strings.TrimSpace(workspace.GitRepo) {
		return errors.New("harness v1 public workspace requires a canonical credential-free gitRepo")
	}
	if _, err := workspaceRepository(workspace); err != nil {
		return fmt.Errorf("validate harness v1 public workspace repository: %w", err)
	}
	if err := validateHarnessV1PublicWorkspaceAuthority(workspace); err != nil {
		return err
	}
	return validateHarnessV1PublicWorkspaceLocation(workspace)
}

func validateHarnessV1PublicWorkspaceAuthority(workspace *corev1alpha1.WorkspaceConfig) error {
	if workspace.ReadCredentialRef != nil || workspace.PublicationReadCredentialRef != nil ||
		workspace.PublicationCredentialRef != nil || workspace.ForgeCredentialRef != nil {
		return errors.New("harness v1 public workspace rejects every read, publication, and forge credential reference")
	}
	if strings.TrimSpace(workspace.PublicationGitRepo) != "" || workspace.PublicationRepository != nil ||
		strings.TrimSpace(workspace.PRBaseBranch) != "" || strings.TrimSpace(workspace.PushBranch) != "" ||
		strings.TrimSpace(workspace.ExpectedRemoteSHA) != "" || workspace.CreatePR {
		return errors.New("harness v1 public workspace rejects publication and pull-request authority")
	}
	if workspace.MaxChangedFiles != nil || len(workspace.AllowedPaths) != 0 ||
		workspace.DenyRepositoryControlPaths || workspace.RejectBinaryFiles || workspace.RejectSecretLikeContent {
		return errors.New("harness v1 public workspace rejects v2 publication policy fields")
	}
	return nil
}

func validateHarnessV1PublicWorkspaceLocation(workspace *corev1alpha1.WorkspaceConfig) error {
	for label, value := range map[string]string{
		"branch": workspace.Branch,
		"ref":    workspace.Ref,
	} {
		if value != strings.TrimSpace(value) || strings.HasPrefix(value, "-") ||
			strings.Contains(value, "..") || strings.Contains(value, "@{") ||
			strings.ContainsAny(value, "\x00\r\n ~^:?*[\\") {
			return fmt.Errorf("harness v1 public workspace %s %q is invalid", label, value)
		}
	}
	if subPath := workspace.SubPath; subPath != "" {
		cleaned := path.Clean(subPath)
		if subPath != strings.TrimSpace(subPath) || path.IsAbs(subPath) || cleaned != subPath ||
			cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return fmt.Errorf("harness v1 public workspace subPath %q is invalid", subPath)
		}
	}
	return nil
}

func validateFrozenHarnessV1PublicReadOnlyWorkspace(body agentExecutionSnapshotBody) error {
	if body.Workspace == nil {
		return nil
	}
	if body.HarnessV1 == nil ||
		body.HarnessV1.Backend != string(corev1alpha1.AgentExecutionBackendHarnessWrapper) ||
		corev1alpha1.AgentRuntimeType(body.RuntimeType) != corev1alpha1.AgentRuntimeCodex {
		return errors.New("frozen harness v1 public workspace is not bound to the supported read-only wrapper runtime")
	}
	allowedTools, allowedToolsSet, _, allowBash := frozenHarnessV1ToolPolicy(body)
	if !allowedToolsSet || len(allowedTools) != 0 || allowBash == nil || *allowBash {
		return errors.New("frozen harness v1 public workspace requires write-capable native tools and Bash to remain disabled")
	}
	return validateHarnessV1PublicReadOnlyWorkspace(body.Workspace)
}

// applyHarnessV1WorkspaceMetadata projects only the frozen, credential-free
// workspace coordinates into the digest-bound turn request. Callers must run
// this before request validation and CanonicalStartTurnRequestDigest.
func applyHarnessV1WorkspaceMetadata(
	request *harness.StartTurnRequest,
	body agentExecutionSnapshotBody,
) error {
	if body.Workspace == nil {
		return nil
	}
	if request == nil {
		return errors.New("harness v1 turn request is required for frozen workspace metadata")
	}
	if err := validateFrozenHarnessV1PublicReadOnlyWorkspace(body); err != nil {
		return err
	}
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	request.Metadata["gitRepo"] = body.Workspace.GitRepo
	request.Metadata["gitBranch"] = body.Workspace.Branch
	request.Metadata["gitRef"] = body.Workspace.Ref
	request.Metadata["workspaceSubPath"] = body.Workspace.SubPath
	request.Metadata["readOnly"] = booleanTrueValue
	return nil
}
