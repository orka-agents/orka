package sqlite

import (
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestHarnessV1AttemptHasNoSessionSideEffect(t *testing.T) {
	tests := []struct {
		name    string
		attempt store.HarnessV1Attempt
		want    bool
	}{
		{
			name: "pre-submit Session conflict",
			attempt: store.HarnessV1Attempt{
				State: store.HarnessV1AttemptRejected, TerminalReason: "SessionConflict",
			},
			want: true,
		},
		{
			name: "receipt proves possible Session side effect",
			attempt: store.HarnessV1Attempt{
				State: store.HarnessV1AttemptRejected, TerminalReason: "SessionConflict",
				TerminalReceiptDigest: "sha256:receipt",
			},
			want: false,
		},
		{
			name: "wrapper rejection may have a Session turn",
			attempt: store.HarnessV1Attempt{
				State: store.HarnessV1AttemptRejected, TerminalReason: "PromptNotAccepted",
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := harnessV1AttemptHasNoSessionSideEffect(test.attempt); got != test.want {
				t.Fatalf("harnessV1AttemptHasNoSessionSideEffect() = %t, want %t", got, test.want)
			}
		})
	}
}
