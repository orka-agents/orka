/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestGuideAgentConfigFromFlags(t *testing.T) {
	config, err := guideAgentConfig(true, " Claude ", " model ", 30, " image ", "orka-controller", "orka-system", "orka-system", executionmode.HarnessV2)
	if err != nil {
		t.Fatal(err)
	}
	if config.Runtime != corev1alpha1.AgentRuntimeClaude || config.Model != "model" || config.DocsImage != "image" ||
		config.MaxTurns != 30 || config.Namespace != "orka-system" || config.OwnerDeployment != "orka-controller" ||
		config.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("config = %#v", config)
	}

	if _, err := guideAgentConfig(false, "", "", 0, "", "orka-controller", "tenant", "orka-system", executionmode.HarnessV2); err != nil {
		t.Fatalf("a disabled guide ignores its other flags: %v", err)
	}

	for _, turns := range []int{0, 1001, 1<<32 + 30} {
		if _, err := guideAgentConfig(true, "claude", "m", turns, "i", "", "orka-system", "orka-system", executionmode.HarnessV2); err == nil ||
			!strings.Contains(err.Error(), "--guide-max-turns") {
			t.Fatalf("maxTurns %d: err = %v", turns, err)
		}
	}

	_, err = guideAgentConfig(true, "claude", "m", 30, "i", "orka-controller", "tenant", "orka-system", executionmode.HarnessV2)
	if err == nil || !strings.Contains(err.Error(), "--guide-owner-deployment") {
		t.Fatalf("an owner Deployment outside the watch namespace must be rejected: %v", err)
	}
	if _, err := guideAgentConfig(true, "claude", "m", 30, "i", "", "tenant", "orka-system", executionmode.HarnessV2); err != nil {
		t.Fatalf("without an owner Deployment the namespaces may differ: %v", err)
	}
}
