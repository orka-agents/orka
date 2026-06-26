/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"testing"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

func TestExecutionWorkspaceClaimNamespace(t *testing.T) {
	if got := workspaceTemplateNamespace(
		workerenv.ExecutionWorkspaceEnv{TemplateNamespace: " template-ns "},
		testTaskNamespace,
	); got != "template-ns" {
		t.Fatalf("workspaceTemplateNamespace explicit = %q, want template-ns", got)
	}
	if got := workspaceTemplateNamespace(workerenv.ExecutionWorkspaceEnv{}, testTaskNamespace); got != testTaskNamespace {
		t.Fatalf("workspaceTemplateNamespace default = %q, want task-ns", got)
	}
	if got := workspaceClaimNamespace(
		workerenv.ExecutionWorkspaceEnv{ClaimNamespace: " claim-ns "},
		testTaskNamespace,
		"template-ns",
	); got != "claim-ns" {
		t.Fatalf("workspaceClaimNamespace explicit = %q, want claim-ns", got)
	}
	if got := workspaceClaimNamespace(
		workerenv.ExecutionWorkspaceEnv{},
		testTaskNamespace,
		"template-ns",
	); got != testTaskNamespace {
		t.Fatalf("workspaceClaimNamespace default = %q, want task-ns", got)
	}
}

func TestExecutionWorkspaceClaimName(t *testing.T) {
	if got := workspaceClaimName(
		workerenv.ExecutionWorkspaceEnv{ClaimName: " claim-a "},
		"claim-ns",
		testTaskNamespace,
		"template-ns",
	); got != "claim-a" {
		t.Fatalf("workspaceClaimName explicit = %q, want claim-a", got)
	}
	if got := workspaceClaimName(
		workerenv.ExecutionWorkspaceEnv{Provider: string(corev1alpha1.WorkspaceProviderSubstrate)},
		"claim-ns",
		testTaskNamespace,
		"template-ns",
	); got != "" {
		t.Fatalf("workspaceClaimName substrate = %q, want empty generated actor claim", got)
	}
	t.Setenv(workerenv.AgentSandboxReusePolicy, "session")
	got := workspaceClaimName(
		workerenv.ExecutionWorkspaceEnv{
			Provider:     string(corev1alpha1.WorkspaceProviderAgentSandbox),
			ReusePolicy:  string(corev1alpha1.WorkspaceReusePolicySession),
			ReuseKey:     "session-a",
			TemplateName: "template-a",
		},
		"claim-ns",
		testTaskNamespace,
		"template-ns",
	)
	if got == "" {
		t.Fatalf("workspaceClaimName session reuse = empty, want deterministic claim name")
	}
}

func TestExecutionWorkspaceWarmPoolAndResidentHelpers(t *testing.T) {
	t.Setenv(workerenv.AgentSandboxWarmPoolPolicy, "template")
	if got := workspaceWarmPoolPolicy(workerenv.ExecutionWorkspaceEnv{
		Provider: string(corev1alpha1.WorkspaceProviderAgentSandbox),
	}); got != "default" {
		t.Fatalf("workspaceWarmPoolPolicy agent-sandbox = %q, want default", got)
	}
	if got := workspaceWarmPoolPolicy(workerenv.ExecutionWorkspaceEnv{
		Provider: string(corev1alpha1.WorkspaceProviderSubstrate),
	}); got != "" {
		t.Fatalf("workspaceWarmPoolPolicy substrate = %q, want empty", got)
	}
	if !executionWorkspaceResidentProcess(workerenv.ExecutionWorkspaceEnv{
		Provider:    string(corev1alpha1.WorkspaceProviderSubstrate),
		ProcessMode: string(corev1alpha1.ExecutionWorkspaceProcessModeResident),
	}) {
		t.Fatalf("executionWorkspaceResidentProcess substrate resident = false")
	}
	if executionWorkspaceResidentProcess(workerenv.ExecutionWorkspaceEnv{
		Provider:    string(corev1alpha1.WorkspaceProviderAgentSandbox),
		ProcessMode: string(corev1alpha1.ExecutionWorkspaceProcessModeResident),
	}) {
		t.Fatalf("executionWorkspaceResidentProcess agent-sandbox resident = true, want false")
	}
	ref := workspace.WorkspaceRef{ID: "id-a", ClaimName: "claim-a"}
	if got := executionWorkspaceResidentKey(
		workerenv.ExecutionWorkspaceEnv{ResidentKey: " explicit "},
		ref,
	); got != "explicit" {
		t.Fatalf("executionWorkspaceResidentKey explicit = %q, want explicit", got)
	}
	if got := executionWorkspaceResidentKey(
		workerenv.ExecutionWorkspaceEnv{ReuseKey: " reuse "},
		ref,
	); got != "reuse" {
		t.Fatalf("executionWorkspaceResidentKey reuse = %q, want reuse", got)
	}
	if got := executionWorkspaceResidentKey(workerenv.ExecutionWorkspaceEnv{}, ref); got != "id-a" {
		t.Fatalf("executionWorkspaceResidentKey ref = %q, want id-a", got)
	}
}
