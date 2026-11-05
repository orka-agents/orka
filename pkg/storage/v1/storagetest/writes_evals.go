package storagetest

import (
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The eval trial write cases.

const (
	evalRunName = "pr-review"
	evalRunUID  = "eval-run-uid"
)

func catalogEvalTrial(caseID, outcome string) *storagev1.EvalTrialRecord {
	record := &storagev1.EvalTrialRecord{
		SchemaVersion: storagev1.EvalTrialSchemaVersion,
		Namespace:     workflowNamespace,
		EvalRun:       evalRunName,
		EvalRunUID:    evalRunUID,
		CaseID:        caseID,
		Trial:         1,
		TaskName:      "eval-" + caseID,
		Outcome:       outcome,
		Verdict:       "needs_changes",
		Grades:        []storagev1.EvalTrialGrade{{Grader: "verdict", Status: "pass", Score: 1, Reason: "held back"}},
		Snapshot:      storagev1.EvalTrialSnapshot{CaseSetDigest: "sha256:cases"},
		CompletedAt:   workflowAt(1),
	}
	if outcome == storagev1.EvalTrialOutcomeInfraError {
		record.InfraClass, record.Grades = "RuntimeLost", nil
	}
	return record
}

func observeEvalTrials(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	records, err := p.ListEvalTrials(t.Context(), workflowNamespace, evalRunName, evalRunUID)
	require.NoError(t, err)
	return records
}

func init() {
	type evalCase = writeCase[storagev1.StateProvider]
	stateWriteCases = append(stateWriteCases,
		evalCase{
			method: "CreateEvalTrial",
			args: rebuiltArgs(func() []any {
				return []any{catalogEvalTrial("case-a", storagev1.EvalTrialOutcomePass)}
			}),
			observe: observeEvalTrials,
		},
		evalCase{
			method: "DeleteEvalTrials",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateEvalTrial(t.Context(), catalogEvalTrial("case-a", storagev1.EvalTrialOutcomePass)))
				require.NoError(t, p.CreateEvalTrial(t.Context(),
					catalogEvalTrial("case-b", storagev1.EvalTrialOutcomeInfraError)))
			},
			args:    fixed(workflowNamespace, evalRunName),
			observe: observeEvalTrials,
			// Recording a trial again after the delete commits shows that a
			// replay returns the delete's outcome without deleting again.
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateEvalTrial(t.Context(), catalogEvalTrial("case-a", storagev1.EvalTrialOutcomePass)))
			},
		},
	)
}
