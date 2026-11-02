/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/capabilitygap"
	"github.com/orka-agents/orka/internal/tools"
)

// enableCapabilityGapSignals installs the capability-gap recorder when the
// flag is set. The tool catalog is frozen here, after every built-in and
// brokered tool is registered, so tool labels stay bounded.
func enableCapabilityGapSignals(enabled bool, registries ...*tools.Registry) {
	if !enabled {
		capabilitygap.SetDefault(nil)
		return
	}
	capabilitygap.SetDefault(capabilitygap.NewRecorder(capabilityGapToolCatalog(registries...)))
}

func capabilityGapToolCatalog(registries ...*tools.Registry) []string {
	names := tools.CoordinationToolNames()
	for _, registry := range registries {
		if registry != nil {
			names = append(names, registry.Names()...)
		}
	}
	for _, runtime := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
	} {
		names = append(names, acp.BuiltInRuntimeNativeToolNames(string(runtime))...)
	}
	return names
}
