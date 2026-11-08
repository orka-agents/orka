package storagetest

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The eval trial and experiment write cases.

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

func catalogEvalExperiment(candidate string) *storagev1.EvalExperimentRecord {
	return &storagev1.EvalExperimentRecord{
		SchemaVersion:   storagev1.EvalExperimentSchemaVersion,
		Namespace:       workflowNamespace,
		CandidateRun:    candidate,
		CandidateRunUID: candidate + "-uid",
		BaselineRun:     evalRunName,
		BaselineRunUID:  evalRunUID,
		CaseSetDigest:   "sha256:cases",
		CaseKeys:        []string{"sha256:case-a", "sha256:case-b"},
		GateDigest:      "sha256:gate",
		Hypothesis:      "stricter review prompt",
		ReservedAt:      workflowAt(1),
	}
}

func observeEvalExperiments(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	var records []*storagev1.EvalExperimentRecord
	for _, candidate := range []string{"candidate-a", "candidate-b"} {
		record, err := p.GetEvalExperiment(t.Context(), workflowNamespace, candidate+"-uid")
		if errors.Is(err, storagev1.ErrNotFound) {
			continue
		}
		require.NoError(t, err)
		records = append(records, record)
	}
	return records
}

func catalogEvalBinding() *storagev1.EvalDecisionBinding {
	return &storagev1.EvalDecisionBinding{
		CandidateAgent:  storagev1.EvalAgentIdentity{Name: "candidate-agent", UID: "candidate-agent-uid", Generation: 1},
		CandidateDigest: "sha256:candidate",
		BaselineAgent:   storagev1.EvalAgentIdentity{Name: "reviewer", UID: "reviewer-uid", Generation: 3},
		BaselineDigest:  "sha256:stable",
		CaseSetDigest:   "sha256:cases", GraderSetDigest: "sha256:graders", GateDigest: "sha256:gate",
	}
}

func catalogEvalOutcome(candidate, result string) *storagev1.EvalExperimentOutcome {
	return &storagev1.EvalExperimentOutcome{
		SchemaVersion:   storagev1.EvalExperimentOutcomeSchemaVersion,
		Namespace:       workflowNamespace,
		CandidateRun:    candidate,
		CandidateRunUID: candidate + "-uid",
		Result:          result,
		Reasons:         []string{"the target improved"},
		Binding:         catalogEvalBinding(),
		Baseline:        storagev1.EvalExperimentArmOutcome{Run: evalRunName, RunUID: evalRunUID, Planned: 2, Completed: 2, Failed: 2},
		Candidate: storagev1.EvalExperimentArmOutcome{Run: candidate, RunUID: candidate + "-uid", Planned: 2, Completed: 2, Passed: 2,
			Trials: []storagev1.EvalExperimentTrialOutcome{{CaseID: "case-a", Trial: 1, Outcome: storagev1.EvalTrialOutcomePass,
				Grades: []storagev1.EvalExperimentGrade{{Grader: "verdict", Status: "pass"}}}}},
		DecidedAt: workflowAt(2),
	}
}

func catalogEvalDecision(id string) *storagev1.EvalDecisionRecord {
	return &storagev1.EvalDecisionRecord{
		SchemaVersion:   storagev1.EvalDecisionSchemaVersion,
		ID:              id,
		Namespace:       workflowNamespace,
		CandidateRun:    "candidate-a",
		CandidateRunUID: "candidate-a-uid",
		Decision:        storagev1.EvalDecisionApproved,
		Reason:          "the evidence holds",
		Actor:           storagev1.EvalDecisionActor{Username: "alice", Groups: []string{"promoters"}},
		Binding:         catalogEvalBinding(),
		DecidedAt:       workflowAt(3),
	}
}

func observeEvalExperimentEntries(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	entries, err := p.ListEvalExperiments(t.Context(), workflowNamespace, "")
	require.NoError(t, err)
	return entries
}

func reserveCatalogEvalExperiment(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	granted, err := p.ReserveEvalExperiment(t.Context(), catalogEvalExperiment("candidate-a"),
		storagev1.EvalExperimentLimits{MaxPerBaseline: 2, MaxPerCase: 2})
	require.NoError(t, err)
	require.True(t, granted.Granted)
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
		evalCase{
			method: "ReserveEvalExperiment",
			// One attempt is already held on the baseline, so the
			// reservation takes the last one the limits allow.
			setup: func(t *testing.T, p storagev1.StateProvider) {
				granted, err := p.ReserveEvalExperiment(t.Context(), catalogEvalExperiment("candidate-a"),
					storagev1.EvalExperimentLimits{MaxPerBaseline: 2, MaxPerCase: 2})
				require.NoError(t, err)
				require.True(t, granted.Granted)
			},
			args: rebuiltArgs(func() []any {
				return []any{catalogEvalExperiment("candidate-b"), storagev1.EvalExperimentLimits{MaxPerBaseline: 2, MaxPerCase: 2}}
			}),
			observe: observeEvalExperiments,
		},
		evalCase{
			method: "RecordEvalExperimentOutcome",
			setup:  reserveCatalogEvalExperiment,
			args: rebuiltArgs(func() []any {
				return []any{catalogEvalOutcome("candidate-a", storagev1.EvalExperimentResultPassed)}
			}),
			observe: observeEvalExperimentEntries,
		},
		evalCase{
			method: "AppendEvalDecision",
			// One decision is already recorded, so the append is the second.
			setup: func(t *testing.T, p storagev1.StateProvider) {
				reserveCatalogEvalExperiment(t, p)
				_, err := p.RecordEvalExperimentOutcome(t.Context(), catalogEvalOutcome("candidate-a", storagev1.EvalExperimentResultPassed))
				require.NoError(t, err)
				require.NoError(t, p.AppendEvalDecision(t.Context(), catalogEvalDecision("decision-1")))
			},
			args: rebuiltArgs(func() []any {
				return []any{catalogEvalDecision("decision-2")}
			}),
			observe: observeEvalExperimentEntries,
		},
	)
}
