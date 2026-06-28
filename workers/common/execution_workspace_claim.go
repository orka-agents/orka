/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"fmt"
	"os"
	"strings"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

func executionWorkspaceCompletionMessage(
	taskNamespace string,
	taskName string,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	ref workspace.WorkspaceRef,
) string {
	provider := strings.TrimSpace(workspaceEnv.Provider)
	if provider == "" {
		provider = string(corev1alpha1.WorkspaceProviderAgentSandbox)
	}
	if provider == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return fmt.Sprintf("Task %s/%s completed in %s workspace", taskNamespace, taskName, provider)
	}
	if claimName := strings.TrimSpace(ref.ClaimName); claimName != "" {
		return fmt.Sprintf("Task %s/%s completed in %s workspace %s", taskNamespace, taskName, provider, claimName)
	}
	return fmt.Sprintf("Task %s/%s completed in %s workspace", taskNamespace, taskName, provider)
}
func executionWorkspaceExecutor(workspaceEnv workerenv.ExecutionWorkspaceEnv) (workspace.WorkspaceExecutor, error) {
	switch strings.TrimSpace(workspaceEnv.Provider) {
	case "", string(corev1alpha1.WorkspaceProviderAgentSandbox):
		return getAgentSandboxWorkspaceExecutor(), nil
	case string(corev1alpha1.WorkspaceProviderSubstrate):
		return getSubstrateWorkspaceExecutor()
	default:
		return nil, fmt.Errorf("unsupported execution workspace provider %q", workspaceEnv.Provider)
	}
}

func workspaceTemplateNamespace(workspaceEnv workerenv.ExecutionWorkspaceEnv, taskNamespace string) string {
	if ns := strings.TrimSpace(workspaceEnv.TemplateNamespace); ns != "" {
		return ns
	}
	return taskNamespace
}

func workspaceClaimNamespace(
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	taskNamespace string,
	templateNamespace string,
) string {
	if ns := strings.TrimSpace(workspaceEnv.ClaimNamespace); ns != "" {
		return ns
	}
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return templateNamespace
	}
	legacy := workerenv.ParseAgentSandboxEnv(os.Getenv)
	return agentSandboxClaimNamespace(legacy, taskNamespace, templateNamespace)
}

func workspaceClaimName(
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
	claimNamespace string,
	taskNamespace string,
	templateNamespace string,
) string {
	if claimName := strings.TrimSpace(workspaceEnv.ClaimName); claimName != "" {
		return claimName
	}
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return ""
	}
	legacy := workerenv.ParseAgentSandboxEnv(os.Getenv)
	legacy.ReusePolicy = workspaceEnv.ReusePolicy
	legacy.ReuseKey = workspaceEnv.ReuseKey
	legacy.TemplateName = workspaceEnv.TemplateName
	return agentSandboxSessionClaimName(legacy, claimNamespace, taskNamespace, templateNamespace)
}

func workspaceWarmPoolPolicy(workspaceEnv workerenv.ExecutionWorkspaceEnv) string {
	if strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) {
		return ""
	}
	return agentSandboxClaimWarmPoolPolicy(workerenv.ParseAgentSandboxEnv(os.Getenv).WarmPoolPolicy)
}

func executionWorkspaceResidentProcess(workspaceEnv workerenv.ExecutionWorkspaceEnv) bool {
	return strings.TrimSpace(workspaceEnv.Provider) == string(corev1alpha1.WorkspaceProviderSubstrate) &&
		strings.TrimSpace(workspaceEnv.ProcessMode) == string(corev1alpha1.ExecutionWorkspaceProcessModeResident)
}

func executionWorkspaceResidentKey(workspaceEnv workerenv.ExecutionWorkspaceEnv, ref workspace.WorkspaceRef) string {
	if key := strings.TrimSpace(workspaceEnv.ResidentKey); key != "" {
		return key
	}
	if key := strings.TrimSpace(workspaceEnv.ReuseKey); key != "" {
		return key
	}
	if key := strings.TrimSpace(ref.ID); key != "" {
		return key
	}
	return strings.TrimSpace(ref.ClaimName)
}
