/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package controller

import (
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func agentOmitsBuiltInContract(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Type != "" &&
		agent.Spec.Runtime.RuntimeRef == nil && agent.Spec.Runtime.ContractVersion == nil
}

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
