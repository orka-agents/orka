package storagetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The oversight decision write cases.

func observeOversight(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	decision, err := p.GetOversightDecision(t.Context(), "team", "a@0")
	decision.RecordedAt = time.Time{}
	feedback, feedbackErr := p.ListLatestOversightFeedback(t.Context(), "team", []string{"a@0"})
	return []any{observed(t, decision, err), observed(t, feedback, feedbackErr)}
}

func init() {
	type oversightCase = writeCase[storagev1.StateProvider]
	appendOne := func(t *testing.T, p storagev1.StateProvider) {
		_, err := p.AppendOversightDecisions(t.Context(), "team", []storagev1.OversightDecisionRecord{
			testOversightDecision("a@0", "a", "d1", 0),
		})
		require.NoError(t, err)
	}
	stateWriteCases = append(stateWriteCases,
		oversightCase{
			method:  "AppendOversightDecisions",
			args:    fixed("team", []storagev1.OversightDecisionRecord{testOversightDecision("a@0", "a", "d1", 0)}),
			observe: observeOversight,
		},
		oversightCase{
			method: "RecordOversightFeedback",
			setup:  appendOne,
			args: fixed("team", []storagev1.OversightFeedbackRecord{{DecisionID: "a@0", Label: "false_alarm"}},
				oversightObservedAt.Add(time.Hour)),
			observe: observeOversight,
		},
	)
}
