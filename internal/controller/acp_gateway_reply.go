package controller

import (
	"context"
	"fmt"
	"slices"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/aitools"
)

// projectACPReplyPolicy runs only for new bindings, before profile/descriptor
// sealing. The copied Task is a snapshot input, never a persisted spec change.
func (r *TaskReconciler) projectACPReplyPolicy(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, external bool) (*corev1alpha1.Task, error) {
	allowed := effectiveACPAllowedTools(task, agent)
	selected := slices.Contains(allowed, aitools.GatewayReplyToolName)
	if external && !selected {
		return task, nil
	}
	// Pending durable linkage must defer freezing.
	eligible, err := r.GatewayService.ResolveReplyEligibility(ctx, task)
	if err != nil {
		return nil, err
	}
	permitted := eligible && aitools.ACPGatewayReplyScopeAllows(task)
	if external {
		if !permitted {
			return nil, permanentACPAgentConfiguration(fmt.Errorf("reply_in_conversation requires an eligible gateway Task and permitted scope"))
		}
		return task, nil // Registered external policy must remain byte-for-byte exact.
	}
	explicit := agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowedTools != nil
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowedTools != nil {
		explicit = true
	}
	grant := permitted && (!explicit || selected)
	if grant == selected {
		return task, nil
	}
	if grant {
		if allowed == nil && agent != nil && agent.Spec.Runtime != nil {
			allowed = acp.BuiltInRuntimeNativeToolNames(string(agent.Spec.Runtime.Type))
		}
		allowed = append(allowed, aitools.GatewayReplyToolName)
	} else {
		allowed = slices.DeleteFunc(allowed, func(name string) bool { return name == aitools.GatewayReplyToolName })
	}
	projected := task.DeepCopy()
	if projected.Spec.AgentRuntime == nil {
		projected.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	projected.Spec.AgentRuntime.AllowedTools = sortedUnique(allowed)
	return projected, nil
}
