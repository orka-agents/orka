package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func evalOutcomeTestRecord(record *store.EvalExperimentRecord, result string) *store.EvalExperimentOutcome {
	outcome := &store.EvalExperimentOutcome{
		SchemaVersion:   store.EvalExperimentOutcomeSchemaVersion,
		Namespace:       record.Namespace,
		CandidateRun:    record.CandidateRun,
		CandidateRunUID: record.CandidateRunUID,
		Result:          result,
		Baseline:        store.EvalExperimentArmOutcome{Run: record.BaselineRun, RunUID: record.BaselineRunUID, Planned: 3, Completed: 3, Passed: 1, Failed: 2},
		Candidate: store.EvalExperimentArmOutcome{Run: record.CandidateRun, RunUID: record.CandidateRunUID, Planned: 3, Completed: 3, Passed: 3,
			Trials: []store.EvalExperimentTrialOutcome{{CaseID: "c1", Trial: 1, Outcome: store.EvalTrialOutcomePass,
				Grades: []store.EvalExperimentGrade{{Grader: "verdict", Status: "pass"}}}}},
		DecidedAt: time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC),
	}
	if result != store.EvalExperimentResultIncomparable {
		outcome.Binding = &store.EvalDecisionBinding{
			CandidateAgent:  store.EvalAgentIdentity{Name: "candidate-agent", UID: "agent-uid", Generation: 2},
			CandidateDigest: "sha256:candidate",
			BaselineAgent:   store.EvalAgentIdentity{Name: "reviewer", UID: "reviewer-uid", Generation: 1},
			BaselineDigest:  "sha256:baseline",
			CaseSetDigest:   "sha256:cases", GraderSetDigest: "sha256:graders", GateDigest: "sha256:gate",
		}
	}
	return outcome
}

func evalDecisionTestRecord(outcome *store.EvalExperimentOutcome, id, decision string) *store.EvalDecisionRecord {
	return &store.EvalDecisionRecord{
		SchemaVersion:   store.EvalDecisionSchemaVersion,
		ID:              id,
		Namespace:       outcome.Namespace,
		CandidateRun:    outcome.CandidateRun,
		CandidateRunUID: outcome.CandidateRunUID,
		Decision:        decision,
		Reason:          "reviewed the evidence",
		Actor:           store.EvalDecisionActor{Username: "alice", Groups: []string{"promoters"}},
		Binding:         outcome.Binding,
		DecidedAt:       time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	}
}

func reserveEvalTestExperiment(t *testing.T, s *Store, candidate string) *store.EvalExperimentRecord {
	t.Helper()
	record := evalExperimentTestRecord(candidate, "baseline-uid", "sha256:"+candidate)
	reservation, err := s.ReserveEvalExperiment(t.Context(), record, store.EvalExperimentLimits{MaxPerBaseline: 10, MaxPerCase: 10})
	require.NoError(t, err)
	require.True(t, reservation.Granted)
	return record
}

// An outcome needs a reservation and is written once: a later, different
// outcome for the same candidate returns the first.
func TestRecordEvalExperimentOutcomeIsWrittenOnce(t *testing.T) {
	s := newEvalTrialTestStore(t)
	unreserved := evalOutcomeTestRecord(evalExperimentTestRecord("ghost", "baseline-uid"), store.EvalExperimentResultPassed)
	_, err := s.RecordEvalExperimentOutcome(t.Context(), unreserved)
	require.ErrorIs(t, err, store.ErrNotFound)

	record := reserveEvalTestExperiment(t, s, "candidate")
	first := evalOutcomeTestRecord(record, store.EvalExperimentResultPassed)
	stored, err := s.RecordEvalExperimentOutcome(t.Context(), first)
	require.NoError(t, err)
	require.Equal(t, first, stored)

	later := evalOutcomeTestRecord(record, store.EvalExperimentResultIncomparable)
	later.Reasons = []string{"baseline EvalRun \"baseline\" is gone or was recreated"}
	stored, err = s.RecordEvalExperimentOutcome(t.Context(), later)
	require.NoError(t, err)
	require.Equal(t, first, stored)

	entries, err := s.ListEvalExperiments(t.Context(), "team-a", "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, *record, entries[0].Experiment)
	require.Equal(t, first, entries[0].Outcome)
}

func TestRecordEvalExperimentOutcomeValidates(t *testing.T) {
	s := newEvalTrialTestStore(t)
	record := reserveEvalTestExperiment(t, s, "candidate")
	for name, mutate := range map[string]func(*store.EvalExperimentOutcome){
		"schema":            func(o *store.EvalExperimentOutcome) { o.SchemaVersion = "v0" },
		"result":            func(o *store.EvalExperimentOutcome) { o.Result = "Pending" },
		"binding on a pass": func(o *store.EvalExperimentOutcome) { o.Binding = nil },
		"decidedAt":         func(o *store.EvalExperimentOutcome) { o.DecidedAt = time.Time{} },
		"candidate":         func(o *store.EvalExperimentOutcome) { o.CandidateRunUID = " " },
	} {
		t.Run(name, func(t *testing.T) {
			outcome := evalOutcomeTestRecord(record, store.EvalExperimentResultPassed)
			mutate(outcome)
			_, err := s.RecordEvalExperimentOutcome(t.Context(), outcome)
			require.ErrorIs(t, err, store.ErrValidation)
		})
	}
}

// Decisions are appended in order. An approval needs a passed outcome with
// the same binding; a rejection needs only an outcome. A repeated ID is a
// no-op, and a different record under it is refused.
func TestAppendEvalDecision(t *testing.T) {
	s := newEvalTrialTestStore(t)
	passed := reserveEvalTestExperiment(t, s, "passed")
	failed := reserveEvalTestExperiment(t, s, "failed")
	undecided := reserveEvalTestExperiment(t, s, "undecided")
	passedOutcome, err := s.RecordEvalExperimentOutcome(t.Context(), evalOutcomeTestRecord(passed, store.EvalExperimentResultPassed))
	require.NoError(t, err)
	failedOutcome, err := s.RecordEvalExperimentOutcome(t.Context(), evalOutcomeTestRecord(failed, store.EvalExperimentResultFailed))
	require.NoError(t, err)

	err = s.AppendEvalDecision(t.Context(), evalDecisionTestRecord(evalOutcomeTestRecord(undecided, store.EvalExperimentResultPassed), "d0", store.EvalDecisionRejected))
	require.ErrorIs(t, err, store.ErrNotFound)
	err = s.AppendEvalDecision(t.Context(), evalDecisionTestRecord(failedOutcome, "d1", store.EvalDecisionApproved))
	require.ErrorIs(t, err, store.ErrConflict)
	stale := evalDecisionTestRecord(passedOutcome, "d2", store.EvalDecisionApproved)
	changed := *passedOutcome.Binding
	changed.CandidateAgent.Generation++
	stale.Binding = &changed
	require.ErrorIs(t, s.AppendEvalDecision(t.Context(), stale), store.ErrConflict)

	approve := evalDecisionTestRecord(passedOutcome, "d3", store.EvalDecisionApproved)
	require.NoError(t, s.AppendEvalDecision(t.Context(), approve))
	require.NoError(t, s.AppendEvalDecision(t.Context(), approve))
	different := *approve
	different.Reason = "changed my mind"
	require.ErrorIs(t, s.AppendEvalDecision(t.Context(), &different), store.ErrDuplicateMismatch)
	reject := evalDecisionTestRecord(passedOutcome, "d4", store.EvalDecisionRejected)
	reject.Binding = nil
	require.NoError(t, s.AppendEvalDecision(t.Context(), reject))
	require.NoError(t, s.AppendEvalDecision(t.Context(), evalDecisionTestRecord(failedOutcome, "d5", store.EvalDecisionRejected)))

	entries, err := s.ListEvalExperiments(t.Context(), "team-a", passed.CandidateRunUID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, []store.EvalDecisionRecord{*approve, *reject}, entries[0].Decisions)

	entries, err = s.ListEvalExperiments(t.Context(), "team-a", "")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	byRun := map[string]store.EvalExperimentEntry{}
	for _, entry := range entries {
		byRun[entry.Experiment.CandidateRun] = entry
	}
	require.Len(t, byRun["passed"].Decisions, 2)
	require.Len(t, byRun["failed"].Decisions, 1)
	require.Nil(t, byRun["undecided"].Outcome)
	require.Empty(t, byRun["undecided"].Decisions)

	other, err := s.ListEvalExperiments(t.Context(), "team-b", "")
	require.NoError(t, err)
	require.Empty(t, other)
}

func TestAppendEvalDecisionValidates(t *testing.T) {
	s := newEvalTrialTestStore(t)
	record := reserveEvalTestExperiment(t, s, "candidate")
	outcome, err := s.RecordEvalExperimentOutcome(t.Context(), evalOutcomeTestRecord(record, store.EvalExperimentResultPassed))
	require.NoError(t, err)
	for name, mutate := range map[string]func(*store.EvalDecisionRecord){
		"schema":           func(d *store.EvalDecisionRecord) { d.SchemaVersion = "v0" },
		"decision":         func(d *store.EvalDecisionRecord) { d.Decision = "maybe" },
		"approval binding": func(d *store.EvalDecisionRecord) { d.Binding = nil },
		"actor":            func(d *store.EvalDecisionRecord) { d.Actor.Username = "" },
		"reason":           func(d *store.EvalDecisionRecord) { d.Reason = "" },
		"reason too long":  func(d *store.EvalDecisionRecord) { d.Reason = string(make([]byte, store.MaxEvalDecisionReasonBytes+1)) },
		"id":               func(d *store.EvalDecisionRecord) { d.ID = "" },
		"decidedAt":        func(d *store.EvalDecisionRecord) { d.DecidedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			decision := evalDecisionTestRecord(outcome, "d1", store.EvalDecisionApproved)
			mutate(decision)
			require.ErrorIs(t, s.AppendEvalDecision(t.Context(), decision), store.ErrValidation)
		})
	}
}
