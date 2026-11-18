package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

func TestPromptsKeepRequiredRuntimeEvidenceInScope(t *testing.T) {
	report := intake.Report{Title: "Runtime authorization boundary"}
	packet := source.Packet{}
	checks := checksPrompt(report, packet)
	require.Contains(t, checks, "declare the actual missing requirements")
	require.Contains(t, checks, "do not substitute a source-only harness for that behavior")

	for _, tc := range []struct {
		name, prompt, decision string
	}{
		{"reproduce", reproductionPrompt(report, packet, nil), "reproduced"},
		{"verify", verificationPrompt(report, packet, "", nil, false), "approved"},
		{"verify-revised", verificationPrompt(report, packet, "", nil, true), "approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, tc.prompt, "Source-level observations do not establish unexecuted cluster, authorization, or network behavior.")
			require.Contains(t, tc.prompt, "Return "+tc.decision+"=false when required runtime behavior or prerequisites remain unverified")
			require.NotContains(t, tc.prompt, "MUST return")
			require.Contains(t, tc.prompt, observationInstructions)
		})
	}
}
