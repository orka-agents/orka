/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sandboxextv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	sandboxextv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestPlanAgentExecutionMatrix(t *testing.T) {
	scheme := newTestScheme()
	baseAgent := validPlannerAgent()

	tests := []struct {
		name                   string
		mutateTask             func(*corev1alpha1.Task)
		mutateAgent            func(*corev1alpha1.Agent)
		objects                []client.Object
		agentSandboxEnabled    bool
		acpRuntimeEnabled      bool
		wantPath               agentExecutionPath
		wantReason             string
		wantWorkspaceStatusErr string
	}{
		{
			name:              "built-in agent task uses ACP RuntimePool",
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathACP,
		},
		{
			name: "built-in Copilot task uses ACP RuntimePool",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeCopilot
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathACP,
		},
		{
			name:       "disabled ACP runtime fails closed without legacy fallback",
			wantPath:   agentExecutionPathRejected,
			wantReason: "no fallback execution path",
		},
		{
			name: "conformant external runtimeRef remains fail-closed",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
					RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external-v2"},
				}
			},
			objects:           []client.Object{plannerExternalRuntime()},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "Task dispatch is not supported until the v2 dispatcher is wired",
		},
		{
			name: "OpenCode uses ACP RuntimePool",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeOpencode
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathACP,
		},
		{
			name: "priorTaskRef continuation is rejected",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: "parent"}
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "use sessionRef",
		},
		{
			name: "transaction token delegation is rejected before ACP admission",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Transaction = &corev1alpha1.TaskTransaction{ID: "txn-1"}
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "transaction token delegation",
		},
		{
			name: "task resources are rejected until a RuntimePool class is selected",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "custom Kubernetes resources",
		},
		{
			name: "agent resources are rejected until a RuntimePool class is selected",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "custom Kubernetes resources",
		},
		{
			name: "task execution placement is rejected before ACP admission",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{RuntimeClassName: "kata"}
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "execution placement",
		},
		{
			name: "agent execution placement is rejected before ACP admission",
			mutateAgent: func(agent *corev1alpha1.Agent) {
				agent.Spec.Execution = &corev1alpha1.ExecutionSpec{NodeSelector: map[string]string{"disk": "ssd"}}
			},
			acpRuntimeEnabled: true,
			wantPath:          agentExecutionPathRejected,
			wantReason:        "execution placement",
		},
		{
			name: "legacy execution workspace is rejected in favor of Task.spec.workspace",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					Enabled:     true,
					Provider:    corev1alpha1.WorkspaceProviderAgentSandbox,
					TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "sandbox-template"},
				}}
			},
			objects: []client.Object{
				&sandboxextv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-template", Namespace: defaultNS}},
				&sandboxextv1beta1.SandboxWarmPool{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-template", Namespace: defaultNS}},
			},
			agentSandboxEnabled:    true,
			acpRuntimeEnabled:      true,
			wantPath:               agentExecutionPathRejected,
			wantReason:             "use Task.spec.workspace",
			wantWorkspaceStatusErr: "use Task.spec.workspace",
		},
		{
			name: "execution workspace resolution failure is surfaced for status update",
			mutateTask: func(task *corev1alpha1.Task) {
				task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
					Enabled:     true,
					Provider:    corev1alpha1.WorkspaceProviderAgentSandbox,
					TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "missing-template"},
				}}
			},
			agentSandboxEnabled:    true,
			acpRuntimeEnabled:      true,
			wantPath:               agentExecutionPathRejected,
			wantReason:             "failed to resolve execution workspace",
			wantWorkspaceStatusErr: "missing-template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validPlannerTask()
			agent := baseAgent.DeepCopy()
			if tt.mutateTask != nil {
				tt.mutateTask(task)
			}
			if tt.mutateAgent != nil {
				tt.mutateAgent(agent)
			}

			r := newUnitReconciler(scheme, tt.objects...)
			r.AgentSandboxEnabled = tt.agentSandboxEnabled
			r.ACPRuntimeEnabled = tt.acpRuntimeEnabled

			plan := r.planAgentExecution(context.Background(), task, agent)
			if plan.path != tt.wantPath {
				t.Fatalf("plan path = %q, want %q (plan=%#v)", plan.path, tt.wantPath, plan)
			}
			if tt.wantReason != "" && !strings.Contains(plan.rejectionReason, tt.wantReason) {
				t.Fatalf("rejection reason = %q, want substring %q", plan.rejectionReason, tt.wantReason)
			}
			if tt.wantWorkspaceStatusErr == "" {
				if plan.workspaceStatusError != nil {
					t.Fatalf("workspaceStatusError = %v, want nil", plan.workspaceStatusError)
				}
				return
			}
			if plan.workspaceStatusError == nil || !strings.Contains(plan.workspaceStatusError.Error(), tt.wantWorkspaceStatusErr) {
				t.Fatalf("workspaceStatusError = %v, want substring %q", plan.workspaceStatusError, tt.wantWorkspaceStatusErr)
			}
		})
	}
}

func plannerExternalRuntime() *corev1alpha1.AgentRuntime {
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	governance := corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities{
		Mode:                     corev1alpha1.AgentRuntimeWorkspaceGovernanceStrict,
		OrkaOwnedWorkspaceDeltas: true, PromptScopedBrokerAuthorization: true,
		NoDirectSCMPublication: true, OrkaOwnedCleanRoomPublication: true,
		ExactInstanceFencing: true, DuplicateSafeMutations: true, CancellationSettlement: true,
	}
	profile := corev1alpha1.AgentRuntimeProfileSpec{
		Digest: digest("a"), DigestSchemaVersion: int32(harnessv2.ProfileDigestSchemaVersion), ACPProfile: "acp.v1", AdapterName: "external",
		AdapterDigest: digest("b"), ProviderKind: "external", Model: "model",
		AgentConfigurationDigest: digest("c"), ToolPolicyDigest: digest("d"), ApprovalPolicyDigest: digest("e"),
		MCPConfigurationDigest: digest("f"), WorkspaceIntent: corev1alpha1.WorkspaceIntentRead,
		ProxyCredentialRole: "provider", ProxyCredentialScope: "model:model", ResourceClass: "standard",
	}
	return &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "external-v2", Namespace: defaultNS, UID: "external-runtime-uid", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
			Capabilities:    &corev1alpha1.AgentRuntimeCapabilitiesSpec{RuntimeInstanceID: "external-instance", Profile: profile, WorkspaceGovernance: governance},
		},
		Status: corev1alpha1.AgentRuntimeStatus{Ready: true, ObservedGeneration: 1, ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
			RuntimeInstanceID: "external-instance", RuntimeProfileDigest: profile.Digest, WorkspaceGovernance: governance,
		}},
	}
}

func validPlannerTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: defaultNS},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: "do work",
		},
	}
}

func validPlannerAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
		},
	}
}
