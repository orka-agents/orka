/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"slices"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRuntimeSecretCandidatesOpencode(t *testing.T) {
	candidates := RuntimeSecretCandidates(corev1alpha1.AgentRuntimeOpencode)
	for _, want := range []string{"opencode-credentials", "opencode-api-key"} {
		if !slices.Contains(candidates, want) {
			t.Fatalf("RuntimeSecretCandidates(opencode) = %#v, want %q", candidates, want)
		}
	}
	if slices.Contains(candidates, "openai-api-key") {
		t.Fatalf("RuntimeSecretCandidates(opencode) = %#v, want no generic OpenAI fallback", candidates)
	}

	candidates[0] = "changed-candidate"
	if got := RuntimeSecretCandidates(corev1alpha1.AgentRuntimeOpencode)[0]; got == "changed-candidate" {
		t.Fatal("RuntimeSecretCandidates returned shared backing storage")
	}
}
