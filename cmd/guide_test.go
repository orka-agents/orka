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
	const ns, owner = "orka-system", "orka-controller"
	v2 := executionmode.HarnessV2
	config, err := guideAgentConfig(true, " Claude ", " model ", 30, " image ", owner, ns, ns, v2)
	if err != nil {
		t.Fatal(err)
	}
	if config.Runtime != corev1alpha1.AgentRuntimeClaude || config.Model != "model" || config.DocsImage != "image" ||
		config.MaxTurns != 30 || config.Namespace != "orka-system" || config.OwnerDeployment != "orka-controller" ||
		config.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		t.Fatalf("config = %#v", config)
	}

	if _, err := guideAgentConfig(false, "", "", 0, "", owner, "tenant", ns, v2); err != nil {
		t.Fatalf("a disabled guide ignores its other flags: %v", err)
	}
	// Disabled, the owner still identifies a guide Agent created before, but
	// only a Deployment in the watch namespace can own it.
	if config, _ := guideAgentConfig(false, "", "", 0, "", owner, ns, ns, v2); config.OwnerDeployment != owner {
		t.Fatalf("a disabled guide must keep its owner Deployment: %#v", config)
	}
	if config, _ := guideAgentConfig(false, "", "", 0, "", owner, "tenant", ns, v2); config.OwnerDeployment != "" {
		t.Fatalf("a disabled guide must drop an owner outside the watch namespace: %#v", config)
	}

	for _, turns := range []int{0, 1001, 1<<32 + 30} {
		if _, err := guideAgentConfig(true, "claude", "m", turns, "i", "", ns, ns, v2); err == nil ||
			!strings.Contains(err.Error(), "--guide-max-turns") {
			t.Fatalf("maxTurns %d: err = %v", turns, err)
		}
	}

	_, err = guideAgentConfig(true, "claude", "m", 30, "i", owner, "tenant", ns, v2)
	if err == nil || !strings.Contains(err.Error(), "--guide-owner-deployment") {
		t.Fatalf("an owner Deployment outside the watch namespace must be rejected: %v", err)
	}
	if _, err := guideAgentConfig(true, "claude", "m", 30, "i", "", "tenant", ns, v2); err != nil {
		t.Fatalf("without an owner Deployment the namespaces may differ: %v", err)
	}
}
