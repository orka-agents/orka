package sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func newEvalTrialTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db, path)
}

func evalTrialTestRecord(run, uid, caseID string, trial int32, outcome string) *store.EvalTrialRecord {
	record := &store.EvalTrialRecord{
		SchemaVersion: store.EvalTrialSchemaVersion,
		Namespace:     "team-a",
		EvalRun:       run,
		EvalRunUID:    uid,
		CaseID:        caseID,
		Trial:         trial,
		TaskName:      "eval-" + caseID,
		Outcome:       outcome,
		Verdict:       "needs_changes",
		Grades:        []store.EvalTrialGrade{{Grader: "verdict", Status: "pass", Score: 1, Reason: "held back"}},
		Snapshot:      store.EvalTrialSnapshot{CaseSetDigest: "sha256:abc"},
		CompletedAt:   time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
	}
	if outcome == store.EvalTrialOutcomeInfraError {
		record.InfraClass = "RuntimeLost"
		record.Grades = nil
	}
	return record
}

func TestEvalTrialStoreRoundTripAndScope(t *testing.T) {
	ctx := t.Context()
	s := newEvalTrialTestStore(t)

	require.NoError(t, s.CreateEvalTrial(ctx, evalTrialTestRecord("run", "uid-1", "b-case", 2, store.EvalTrialOutcomeFail)))
	require.NoError(t, s.CreateEvalTrial(ctx, evalTrialTestRecord("run", "uid-1", "a-case", 1, store.EvalTrialOutcomePass)))
	require.NoError(t, s.CreateEvalTrial(ctx, evalTrialTestRecord("run", "uid-1", "b-case", 1, store.EvalTrialOutcomeInfraError)))
	// Another incarnation and another run never leak into the listing.
	require.NoError(t, s.CreateEvalTrial(ctx, evalTrialTestRecord("run", "uid-0", "a-case", 1, store.EvalTrialOutcomePass)))
	require.NoError(t, s.CreateEvalTrial(ctx, evalTrialTestRecord("other", "uid-2", "a-case", 1, store.EvalTrialOutcomePass)))

	records, err := s.ListEvalTrials(ctx, "team-a", "run", "uid-1")
	require.NoError(t, err)
	require.Len(t, records, 3)
	got := make([]string, 0, len(records))
	for _, record := range records {
		got = append(got, fmt.Sprintf("%s/%d", record.CaseID, record.Trial))
	}
	require.Equal(t, []string{"a-case/1", "b-case/1", "b-case/2"}, got)
	require.Equal(t, "RuntimeLost", records[1].InfraClass)
	require.Equal(t, evalTrialTestRecord("run", "uid-1", "a-case", 1, store.EvalTrialOutcomePass), &records[0])

	require.NoError(t, s.DeleteEvalTrials(ctx, "team-a", "run"))
	for _, uid := range []string{"uid-0", "uid-1"} {
		records, err = s.ListEvalTrials(ctx, "team-a", "run", uid)
		require.NoError(t, err)
		require.Empty(t, records)
	}
	records, err = s.ListEvalTrials(ctx, "team-a", "other", "uid-2")
	require.NoError(t, err)
	require.Len(t, records, 1)
}

func TestEvalTrialStoreRecordsAreImmutable(t *testing.T) {
	ctx := t.Context()
	s := newEvalTrialTestStore(t)
	record := evalTrialTestRecord("run", "uid-1", "a-case", 1, store.EvalTrialOutcomePass)
	require.NoError(t, s.CreateEvalTrial(ctx, record))
	require.NoError(t, s.CreateEvalTrial(ctx, record), "an identical repeat is idempotent")

	changed := evalTrialTestRecord("run", "uid-1", "a-case", 1, store.EvalTrialOutcomeFail)
	require.ErrorIs(t, s.CreateEvalTrial(ctx, changed), store.ErrDuplicateMismatch)
	records, err := s.ListEvalTrials(ctx, "team-a", "run", "uid-1")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, store.EvalTrialOutcomePass, records[0].Outcome)
}

func TestEvalTrialStoreValidation(t *testing.T) {
	ctx := t.Context()
	s := newEvalTrialTestStore(t)
	for name, mutate := range map[string]func(*store.EvalTrialRecord){
		"schema":        func(r *store.EvalTrialRecord) { r.SchemaVersion = "orka.eval.trial.v0" },
		"namespace":     func(r *store.EvalTrialRecord) { r.Namespace = "" },
		"padded run":    func(r *store.EvalTrialRecord) { r.EvalRun = " run" },
		"uid":           func(r *store.EvalTrialRecord) { r.EvalRunUID = "" },
		"case":          func(r *store.EvalTrialRecord) { r.CaseID = "" },
		"task":          func(r *store.EvalTrialRecord) { r.TaskName = "" },
		"trial":         func(r *store.EvalTrialRecord) { r.Trial = 0 },
		"outcome":       func(r *store.EvalTrialRecord) { r.Outcome = "error" },
		"infra on pass": func(r *store.EvalTrialRecord) { r.InfraClass = "RuntimeLost" },
		"infra class":   func(r *store.EvalTrialRecord) { r.Outcome = store.EvalTrialOutcomeInfraError },
		"completed":     func(r *store.EvalTrialRecord) { r.CompletedAt = time.Time{} },
		"oversized reasons": func(r *store.EvalTrialRecord) {
			r.Grades[0].Reason = strings.Repeat("x", store.MaxEvalTrialRecordBytes)
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := evalTrialTestRecord("run", "uid-1", "a-case", 1, store.EvalTrialOutcomePass)
			mutate(record)
			require.ErrorIs(t, s.CreateEvalTrial(ctx, record), store.ErrValidation)
		})
	}
	require.ErrorIs(t, s.CreateEvalTrial(ctx, nil), store.ErrValidation)
	records, err := s.ListEvalTrials(ctx, "team-a", "run", "uid-1")
	require.NoError(t, err)
	require.Empty(t, records)
}
