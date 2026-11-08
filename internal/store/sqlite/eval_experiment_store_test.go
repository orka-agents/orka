package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func evalExperimentTestRecord(candidate, baselineUID string, caseKeys ...string) *store.EvalExperimentRecord {
	if len(caseKeys) == 0 {
		caseKeys = []string{"sha256:case-a", "sha256:case-b"}
	}
	return &store.EvalExperimentRecord{
		SchemaVersion:   store.EvalExperimentSchemaVersion,
		Namespace:       "team-a",
		CandidateRun:    candidate,
		CandidateRunUID: candidate + "-uid",
		BaselineRun:     "baseline",
		BaselineRunUID:  baselineUID,
		CaseSetDigest:   "sha256:cases",
		CaseKeys:        caseKeys,
		GateDigest:      "sha256:gate",
		Hypothesis:      "stricter prompt",
		ReservedAt:      time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
	}
}

// Reservations count per baseline and per held-out case key, across
// namespaces, and a repeat for the same candidate keeps its grant without
// counting twice.
func TestReserveEvalExperimentLimits(t *testing.T) {
	s := newEvalTrialTestStore(t)
	ctx := t.Context()
	limits := store.EvalExperimentLimits{MaxPerBaseline: 2, MaxPerCase: 3}

	first := evalExperimentTestRecord("candidate-1", "baseline-a")
	got, err := s.ReserveEvalExperiment(ctx, first, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseAttempts: 1}, got)

	got, err = s.ReserveEvalExperiment(ctx, first, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseAttempts: 1}, got,
		"a repeat keeps the grant and does not count again")

	stored, err := s.GetEvalExperiment(ctx, first.Namespace, first.CandidateRunUID)
	require.NoError(t, err)
	require.Equal(t, first, stored)
	_, err = s.GetEvalExperiment(ctx, "team-b", first.CandidateRunUID)
	require.ErrorIs(t, err, store.ErrNotFound)

	changed := *first
	changed.Hypothesis = "something else"
	_, err = s.ReserveEvalExperiment(ctx, &changed, limits)
	require.ErrorIs(t, err, store.ErrDuplicateMismatch)

	other := evalExperimentTestRecord("candidate-2", "baseline-a")
	other.Namespace = "team-b"
	got, err = s.ReserveEvalExperiment(ctx, other, limits)
	require.NoError(t, err)
	require.True(t, got.Granted)

	spent := evalExperimentTestRecord("candidate-3", "baseline-a", "sha256:case-z")
	got, err = s.ReserveEvalExperiment(ctx, spent, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{BaselineAttempts: 2, CaseAttempts: 0}, got,
		"the baseline budget is spent across namespaces")

	// A subset of the same cases under a fresh baseline is not fresh
	// evidence: case-b was already in two experiments.
	subset := evalExperimentTestRecord("candidate-4", "baseline-b", "sha256:case-b", "sha256:case-c")
	got, err = s.ReserveEvalExperiment(ctx, subset, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseAttempts: 3}, got)

	heldOut := evalExperimentTestRecord("candidate-5", "baseline-c", "sha256:case-b")
	got, err = s.ReserveEvalExperiment(ctx, heldOut, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{BaselineAttempts: 0, CaseAttempts: 3}, got,
		"case-b's held-out budget is spent")

	fresh := evalExperimentTestRecord("candidate-6", "baseline-c", "sha256:case-a", "sha256:case-c")
	got, err = s.ReserveEvalExperiment(ctx, fresh, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseAttempts: 3}, got)
}

func TestReserveEvalExperimentValidation(t *testing.T) {
	s := newEvalTrialTestStore(t)
	limits := store.EvalExperimentLimits{MaxPerBaseline: 1, MaxPerCase: 1}
	for name, mutate := range map[string]func(*store.EvalExperimentRecord, *store.EvalExperimentLimits){
		"schema": func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) {
			r.SchemaVersion = "orka.eval.experiment.v0"
		},
		"no baseline":   func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.BaselineRunUID = "" },
		"no case set":   func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CaseSetDigest = " sha256:x" },
		"no case keys":  func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CaseKeys = nil },
		"unsorted keys": func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CaseKeys = []string{"b", "a"} },
		"repeated keys": func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CaseKeys = []string{"a", "a"} },
		"no time":       func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.ReservedAt = time.Time{} },
		"zero limit":    func(_ *store.EvalExperimentRecord, l *store.EvalExperimentLimits) { l.MaxPerCase = 0 },
		"no candidate":  func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CandidateRunUID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			record, l := evalExperimentTestRecord("candidate", "baseline"), limits
			mutate(record, &l)
			_, err := s.ReserveEvalExperiment(t.Context(), record, l)
			require.ErrorIs(t, err, store.ErrValidation)
		})
	}
	_, err := s.ReserveEvalExperiment(t.Context(), nil, limits)
	require.ErrorIs(t, err, store.ErrValidation)
}
