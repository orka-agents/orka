package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func evalExperimentTestRecord(candidate, baselineUID string) *store.EvalExperimentRecord {
	return &store.EvalExperimentRecord{
		SchemaVersion:   store.EvalExperimentSchemaVersion,
		Namespace:       "team-a",
		CandidateRun:    candidate,
		CandidateRunUID: candidate + "-uid",
		BaselineRun:     "baseline",
		BaselineRunUID:  baselineUID,
		CaseSetDigest:   "sha256:cases",
		GateDigest:      "sha256:gate",
		Hypothesis:      "stricter prompt",
		ReservedAt:      time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
	}
}

// Reservations count per baseline and per case set, across namespaces, and a
// repeat for the same candidate keeps its grant without counting twice.
func TestReserveEvalExperimentLimits(t *testing.T) {
	s := newEvalTrialTestStore(t)
	ctx := t.Context()
	limits := store.EvalExperimentLimits{MaxPerBaseline: 2, MaxPerCaseSet: 3}

	first := evalExperimentTestRecord("candidate-1", "baseline-a")
	got, err := s.ReserveEvalExperiment(ctx, first, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseSetAttempts: 1}, got)

	got, err = s.ReserveEvalExperiment(ctx, first, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseSetAttempts: 1}, got,
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

	spent := evalExperimentTestRecord("candidate-3", "baseline-a")
	got, err = s.ReserveEvalExperiment(ctx, spent, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{BaselineAttempts: 2, CaseSetAttempts: 2}, got,
		"the baseline budget is spent across namespaces")

	fresh := evalExperimentTestRecord("candidate-4", "baseline-b")
	got, err = s.ReserveEvalExperiment(ctx, fresh, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{Granted: true, BaselineAttempts: 1, CaseSetAttempts: 3}, got)

	heldOut := evalExperimentTestRecord("candidate-5", "baseline-c")
	got, err = s.ReserveEvalExperiment(ctx, heldOut, limits)
	require.NoError(t, err)
	require.Equal(t, store.EvalExperimentReservation{BaselineAttempts: 0, CaseSetAttempts: 3}, got,
		"the held-out case set budget is spent")
}

func TestReserveEvalExperimentValidation(t *testing.T) {
	s := newEvalTrialTestStore(t)
	limits := store.EvalExperimentLimits{MaxPerBaseline: 1, MaxPerCaseSet: 1}
	for name, mutate := range map[string]func(*store.EvalExperimentRecord, *store.EvalExperimentLimits){
		"schema": func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) {
			r.SchemaVersion = "orka.eval.experiment.v0"
		},
		"no baseline":  func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.BaselineRunUID = "" },
		"no case set":  func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CaseSetDigest = " sha256:x" },
		"no time":      func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.ReservedAt = time.Time{} },
		"zero limit":   func(_ *store.EvalExperimentRecord, l *store.EvalExperimentLimits) { l.MaxPerCaseSet = 0 },
		"no candidate": func(r *store.EvalExperimentRecord, _ *store.EvalExperimentLimits) { r.CandidateRunUID = "" },
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
