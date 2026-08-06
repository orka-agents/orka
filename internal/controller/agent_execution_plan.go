/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type agentExecutionPath string

const (
	agentExecutionPathACP      agentExecutionPath = "acp-runtime-pool"
	agentExecutionPathExternal agentExecutionPath = "acp-external-runtime"
	agentExecutionPathRejected agentExecutionPath = "rejected"
)

type agentExecutionPlan struct {
	path                 agentExecutionPath
	externalRuntimeName  string
	rejectionReason      string
	workspaceStatusError error
}

func agentACPPlan() agentExecutionPlan {
	return agentExecutionPlan{path: agentExecutionPathACP}
}

func rejectAgentExecutionPlan(reason string) agentExecutionPlan {
	return agentExecutionPlan{path: agentExecutionPathRejected, rejectionReason: reason}
}

func rejectAgentExecutionPlanWithWorkspaceStatus(reason string, err error) agentExecutionPlan {
	plan := rejectAgentExecutionPlan(reason)
	plan.workspaceStatusError = err
	return plan
}

// planAgentExecution owns the controller routing decision for type: agent
// Tasks. Built-in Codex, Claude, Copilot, and OpenCode runtimes use only the ACP v2
// RuntimePool path. External runtimeRef registrations and conformance remain
// available, but Task dispatch fails closed until the v2 dispatcher support
// boundary is enabled. There is no legacy turn or Job fallback.
func (r *TaskReconciler) planAgentExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) agentExecutionPlan {
	workspaceRequest, err := r.resolveExecutionWorkspaceRequest(ctx, task)
	if err != nil {
		return rejectAgentExecutionPlanWithWorkspaceStatus(
			fmt.Sprintf("failed to resolve execution workspace: %v", err),
			err,
		)
	}
	if workspaceRequest != nil {
		err := fmt.Errorf("Task.spec.execution.workspace is not supported by the ACP core runtime; use Task.spec.workspace") //nolint:staticcheck // Field path begins the user-facing validation message.
		return rejectAgentExecutionPlanWithWorkspaceStatus(err.Error(), err)
	}

	if agent == nil || agent.Spec.Runtime == nil {
		return rejectAgentExecutionPlan("agent runtime configuration is required")
	}
	if agent.Spec.Runtime.RuntimeRef != nil && strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) != "" {
		name := strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
		return rejectAgentExecutionPlan(externalAgentRuntimeDispatchUnsupportedReason(name))
	}
	if !r.ACPRuntimeEnabled {
		return rejectAgentExecutionPlan("ACP core runtime is disabled; built-in agent runtimes have no fallback execution path")
	}

	if reason := agentACPRuntimeUnsupportedReason(task, agent); reason != "" {
		return rejectAgentExecutionPlan(reason)
	}
	if task.Spec.PriorTaskRef != nil {
		return rejectAgentExecutionPlan("priorTaskRef continuation is not supported by the ACP core runtime; use sessionRef")
	}

	switch agent.Spec.Runtime.Type {
	case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode:
	default:
		return rejectAgentExecutionPlan(fmt.Sprintf("agent runtime %q is not supported by the ACP core runtime", agent.Spec.Runtime.Type))
	}

	switch agent.BuiltInContractVersion() {
	case corev1alpha1.AgentRuntimeContractHarnessV2:
		return agentACPPlan()
	case corev1alpha1.AgentRuntimeContractHarnessV1:
		return rejectAgentExecutionPlan("agent is classified orka.harness.v1; the harness v1 execution plane is not enabled on this release and v2 execution never substitutes for it")
	default:
		return rejectAgentExecutionPlan("agent runtime.contractVersion is unclassified; a missing selector is never interpreted as either protocol and execution admission fails closed")
	}
}

func externalAgentRuntimeDispatchUnsupportedReason(name string) string {
	return fmt.Sprintf("external AgentRuntime %q Task dispatch is not supported until the v2 dispatcher is wired", strings.TrimSpace(name))
}

func externalAgentRuntimeReadinessReason(task *corev1alpha1.Task, runtime *corev1alpha1.AgentRuntime) string {
	if runtime == nil || runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return "external AgentRuntime must use orka.harness.v2"
	}
	if !runtime.Status.Ready || runtime.Status.ObservedGeneration != runtime.Generation || runtime.Status.ObservedCapabilities == nil {
		return fmt.Sprintf("external AgentRuntime %q has not passed current-generation v2 conformance", runtime.Name)
	}
	if runtime.Spec.Capabilities == nil || runtime.Spec.Capabilities.Profile == nil ||
		runtime.Status.ObservedCapabilities.RuntimeInstanceID == "" ||
		runtime.Status.ObservedCapabilities.RuntimeProfileDigest != runtime.Spec.Capabilities.Profile.Digest {
		return fmt.Sprintf("external AgentRuntime %q does not have an exact observed runtime identity/profile", runtime.Name)
	}
	if task != nil {
		intent := effectiveACPWorkspaceIntent(task)
		if runtime.Spec.Capabilities.Profile.WorkspaceIntent != intent {
			return fmt.Sprintf("external AgentRuntime %q profile workspace intent %q does not match Task intent %q", runtime.Name, runtime.Spec.Capabilities.Profile.WorkspaceIntent, intent)
		}
	}
	if !runtime.Spec.Capabilities.WorkspaceGovernance.Strict() || !runtime.Status.ObservedCapabilities.WorkspaceGovernance.Strict() {
		return fmt.Sprintf("external AgentRuntime %q does not provide strict workspace governance", runtime.Name)
	}
	return ""
}

func (r *TaskReconciler) rejectPlannedAgentExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	plan agentExecutionPlan,
) (ctrl.Result, error) {
	if plan.workspaceStatusError != nil {
		if statusErr := r.markExecutionWorkspaceValidationFailed(ctx, task, plan.workspaceStatusError); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}
	return r.failTask(ctx, task, plan.rejectionReason)
}

func agentACPRuntimeUnsupportedReason(task *corev1alpha1.Task, agent *corev1alpha1.Agent) string {
	if task == nil {
		return ""
	}
	switch {
	case task.Spec.Transaction != nil:
		return "ACP core runtime tasks do not support transaction token delegation"
	case agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Autonomous:
		return "ACP core runtime tasks do not support Agent.spec.coordination.autonomous; disable autonomous coordination"
	case task.Spec.RetryPolicy != nil && task.Spec.RetryPolicy.MaxRetries > 0:
		return "ACP core runtime tasks do not support effective Task.spec.retryPolicy retries; maxRetries must be 0"
	case effectiveAgentResources(task, agent):
		return "ACP core runtime tasks do not support custom Kubernetes resources until a reviewed RuntimePool resource class is selected"
	case resolveExecution(task, agent) != nil:
		return "ACP core runtime tasks do not support per-Task execution placement"
	default:
		return ""
	}
}

func effectiveAgentResources(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	if task != nil && (len(task.Spec.Resources.Requests) > 0 || len(task.Spec.Resources.Limits) > 0) {
		return true
	}
	return agent != nil && (len(agent.Spec.Resources.Requests) > 0 || len(agent.Spec.Resources.Limits) > 0)
}
