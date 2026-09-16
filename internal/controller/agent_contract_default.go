/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package controller

import (
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

// agentOmitsBuiltInContract reports whether agent selects a built-in runtime
// without an explicit contractVersion. Admission allows that because the
// namespace is bound to exactly one execution mode, so the omission can only
// mean that mode.
func agentOmitsBuiltInContract(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Type != "" &&
		agent.Spec.Runtime.RuntimeRef == nil && agent.Spec.Runtime.ContractVersion == nil
}

// withEffectiveBuiltInContract returns agent, or a copy with the controller's
// own contract filled in when a built-in Agent omitted it. Every planning and
// binding path reads the contract through this so an Agent created in the
// same kubectl apply as its Task is classified before the Agent reconciler
// has persisted the value. The stored object is never modified here.
func withEffectiveBuiltInContract(agent *corev1alpha1.Agent, mode executionmode.Mode) *corev1alpha1.Agent {
	if !agentOmitsBuiltInContract(agent) || mode == "" {
		return agent
	}
	resolved := agent.DeepCopy()
	if err := executionmode.DefaultBuiltInAgentContract(resolved, mode); err != nil {
		return agent
	}
	return resolved
}
