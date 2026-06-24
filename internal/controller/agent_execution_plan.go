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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

type agentExecutionPath string

const (
	agentExecutionPathHarnessWrapper agentExecutionPath = "harness-wrapper"
	agentExecutionPathWorkerJob      agentExecutionPath = "worker-job"
	agentExecutionPathRejected       agentExecutionPath = "rejected"
)

type agentExecutionPlan struct {
	path                 agentExecutionPath
	rejectionReason      string
	workspaceStatusError error
}

func agentHarnessWrapperPlan() agentExecutionPlan {
	return agentExecutionPlan{path: agentExecutionPathHarnessWrapper}
}

func rejectAgentExecutionPlan(reason string) agentExecutionPlan {
	return agentExecutionPlan{path: agentExecutionPathRejected, rejectionReason: reason}
}

func rejectAgentExecutionPlanWithWorkspaceStatus(reason string, err error) agentExecutionPlan {
	plan := rejectAgentExecutionPlan(reason)
	plan.workspaceStatusError = err
	return plan
}

// planAgentExecution owns the controller's current routing decision for type:
// agent Tasks. It assumes earlier Pending-phase validation has already checked
// task/agent compatibility, basic execution-workspace policy, coordination, and
// provider references. The remaining decision is whether the Task can advance as
// a harness-wrapper turn, should use a future worker Job path, or must be
// rejected with a normalized reason.
func (r *TaskReconciler) planAgentExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) agentExecutionPlan {
	if reason := agentHarnessWrapperUnsupportedReason(task, agent); reason != "" {
		return rejectAgentExecutionPlan(reason)
	}

	workspaceRequest, err := r.resolveExecutionWorkspaceRequest(ctx, task)
	if err != nil {
		return rejectAgentExecutionPlanWithWorkspaceStatus(
			fmt.Sprintf("failed to resolve execution workspace: %v", err),
			err,
		)
	}
	if workspaceRequest != nil {
		err := fmt.Errorf("execution workspace is not supported by harness runtime yet")
		return rejectAgentExecutionPlanWithWorkspaceStatus(err.Error(), err)
	}

	if task.Spec.PriorTaskRef != nil && r.EnforceNamespaceIsolation {
		priorNS := strings.TrimSpace(task.Spec.PriorTaskRef.Namespace)
		if priorNS != "" && priorNS != task.Namespace {
			return rejectAgentExecutionPlan("cross-namespace priorTaskRef is not supported by harness runtime when namespace isolation is enforced")
		}
	}

	return agentHarnessWrapperPlan()
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

func agentHarnessWrapperUnsupportedReason(task *corev1alpha1.Task, agent *corev1alpha1.Agent) string {
	if task == nil {
		return ""
	}
	switch {
	case task.Spec.Transaction != nil:
		return "agent CLI runtime tasks do not support transaction token delegation with the harness wrapper yet"
	case effectiveAgentResources(task, agent):
		return "agent CLI runtime tasks do not support custom Kubernetes resources with the harness wrapper yet"
	case resolveExecution(task, agent) != nil:
		return "agent CLI runtime tasks do not support execution placement with the harness wrapper yet"
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
