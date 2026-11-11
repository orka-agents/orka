/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/executionmode"
)

// guideAgentConfig builds the guide configuration from its flags. The guide
// Agent lives in the watch namespace, and an owner reference can only name a
// Deployment in that same namespace.
func guideAgentConfig(
	enabled bool,
	runtime, model string,
	maxTurns int,
	docsImage, ownerDeployment, watchNamespace, podNamespace string,
	mode executionmode.Mode,
) (controller.GuideAgentConfig, error) {
	config := controller.GuideAgentConfig{
		Enabled:         enabled,
		Namespace:       strings.TrimSpace(watchNamespace),
		Runtime:         corev1alpha1.AgentRuntimeType(strings.ToLower(strings.TrimSpace(runtime))),
		Model:           strings.TrimSpace(model),
		DocsImage:       strings.TrimSpace(docsImage),
		ContractVersion: mode.ContractVersion(),
		OwnerDeployment: strings.TrimSpace(ownerDeployment),
	}
	if !enabled {
		return config, nil
	}
	if maxTurns < 1 || maxTurns > 1000 {
		return controller.GuideAgentConfig{}, fmt.Errorf("--guide-max-turns must be between 1 and 1000, got %d", maxTurns)
	}
	config.MaxTurns = int32(maxTurns)
	if config.OwnerDeployment != "" && config.Namespace != strings.TrimSpace(podNamespace) {
		return controller.GuideAgentConfig{}, fmt.Errorf(
			"--guide-owner-deployment needs the controller to run in the watch namespace %q, but it runs in %q; omit it there",
			config.Namespace, podNamespace)
	}
	return config, nil
}
