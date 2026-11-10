package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func evalDecisionSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS eval_experiment_outcomes (
			namespace         TEXT NOT NULL,
			candidate_run_uid TEXT NOT NULL,
			record_json       TEXT NOT NULL,
			decided_at        TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, candidate_run_uid)
		)`,
		`CREATE TABLE IF NOT EXISTS eval_decisions (
			seq               INTEGER PRIMARY KEY,
			namespace         TEXT NOT NULL,
			candidate_run_uid TEXT NOT NULL,
			id                TEXT NOT NULL,
			record_json       TEXT NOT NULL,
			decided_at        TIMESTAMP NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_decisions_id ON eval_decisions(namespace, candidate_run_uid, id)`,
		`CREATE INDEX IF NOT EXISTS idx_eval_experiments_listing ON eval_experiments(namespace, reserved_at, candidate_run_uid)`,
	}
}

func requireEvalField(kind, field, value string) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return store.ValidationErrorf("%s %s is required and must not have surrounding whitespace", kind, field)
	}
	return nil
}

// requireEvalCandidate checks the fields that key every experiment record.
func requireEvalCandidate(kind, namespace, candidateRun, candidateRunUID string) error {
	for _, field := range []struct{ name, value string }{
		{"namespace", namespace}, {"candidateRun", candidateRun}, {"candidateRunUID", candidateRunUID},
	} {
		if err := requireEvalField(kind, field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

func validateEvalExperimentOutcome(outcome *store.EvalExperimentOutcome) error {
	if outcome == nil {
		return store.ValidationErrorf("eval experiment outcome is required")
	}
	if outcome.SchemaVersion != store.EvalExperimentOutcomeSchemaVersion {
		return store.ValidationErrorf("eval experiment outcome schemaVersion must be %q", store.EvalExperimentOutcomeSchemaVersion)
	}
	if err := requireEvalCandidate("eval experiment outcome", outcome.Namespace, outcome.CandidateRun, outcome.CandidateRunUID); err != nil {
		return err
	}
	switch outcome.Result {
	case store.EvalExperimentResultPassed, store.EvalExperimentResultFailed:
		if outcome.Binding == nil {
			return store.ValidationErrorf("eval experiment outcome binding is required for a %s result", outcome.Result)
		}
	case store.EvalExperimentResultIncomparable:
	default:
		return store.ValidationErrorf("eval experiment outcome result must be Passed, Failed or Incomparable")
	}
	if outcome.DecidedAt.IsZero() {
		return store.ValidationErrorf("eval experiment outcome decidedAt is required")
	}
	return nil
}

func validateEvalDecision(decision *store.EvalDecisionRecord) error {
	if decision == nil {
		return store.ValidationErrorf("eval decision is required")
	}
	if decision.SchemaVersion != store.EvalDecisionSchemaVersion {
		return store.ValidationErrorf("eval decision schemaVersion must be %q", store.EvalDecisionSchemaVersion)
	}
	if err := requireEvalCandidate("eval decision", decision.Namespace, decision.CandidateRun, decision.CandidateRunUID); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"id": decision.ID, "actor.username": decision.Actor.Username, "reason": decision.Reason,
	} {
		if err := requireEvalField("eval decision", field, value); err != nil {
			return err
		}
	}
	switch decision.Decision {
	case store.EvalDecisionApproved:
		if decision.Binding == nil {
			return store.ValidationErrorf("eval decision binding is required for an approval")
		}
	case store.EvalDecisionRejected:
	default:
		return store.ValidationErrorf("eval decision must be approved or rejected")
	}
	if len(decision.Reason) > store.MaxEvalDecisionReasonBytes {
		return store.ValidationErrorf("eval decision reason must be at most %d bytes", store.MaxEvalDecisionReasonBytes)
	}
	if decision.DecidedAt.IsZero() {
		return store.ValidationErrorf("eval decision decidedAt is required")
	}
	return nil
}

// RecordEvalExperimentOutcome stores an experiment's outcome once.
func (s *Store) RecordEvalExperimentOutcome(ctx context.Context, outcome *store.EvalExperimentOutcome) (*store.EvalExperimentOutcome, error) {
	if err := validateEvalExperimentOutcome(outcome); err != nil {
		return nil, err
	}
	normalized := *outcome
	normalized.DecidedAt = outcome.DecidedAt.UTC()
	encoded, err := json.Marshal(&normalized)
	if err != nil {
		return nil, fmt.Errorf("encode eval experiment outcome: %w", err)
	}
	if len(encoded) > store.MaxEvalExperimentOutcomeBytes {
		return nil, store.ValidationErrorf("eval experiment outcome must encode to at most %d bytes", store.MaxEvalExperimentOutcomeBytes)
	}
	return writeOperation(ctx, s, "RecordEvalExperimentOutcome", nil, func(ctx context.Context) (*store.EvalExperimentOutcome, error) {
		var stored *store.EvalExperimentOutcome
		err := s.inWriteTransaction(ctx, func(ctx context.Context, _ *sql.Tx) error {
			executor := s.taskDataExecutor(ctx)
			if err := requireEvalExperimentReservation(ctx, executor, normalized.Namespace, normalized.CandidateRunUID); err != nil {
				return err
			}
			existing, err := getEvalExperimentOutcome(ctx, executor, normalized.Namespace, normalized.CandidateRunUID)
			if err == nil {
				// The first outcome wins: a repeated decision never changes it.
				stored = existing
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if _, err := executor.ExecContext(ctx,
				`INSERT INTO eval_experiment_outcomes (namespace, candidate_run_uid, record_json, decided_at) VALUES (?, ?, ?, ?)`,
				normalized.Namespace, normalized.CandidateRunUID, string(encoded), normalized.DecidedAt); err != nil {
				return err
			}
			stored = &normalized
			return nil
		})
		return stored, err
	})
}

// AppendEvalDecision appends a decision on a decided experiment.
func (s *Store) AppendEvalDecision(ctx context.Context, decision *store.EvalDecisionRecord) error {
	if err := validateEvalDecision(decision); err != nil {
		return err
	}
	normalized := *decision
	normalized.DecidedAt = decision.DecidedAt.UTC()
	encoded, err := json.Marshal(&normalized)
	if err != nil {
		return fmt.Errorf("encode eval decision: %w", err)
	}
	return writeOperationErr(ctx, s, "AppendEvalDecision", func(ctx context.Context) error {
		return s.inWriteTransaction(ctx, func(ctx context.Context, _ *sql.Tx) error {
			return s.appendEvalDecisionWrite(ctx, &normalized, encoded)
		})
	})
}

func (s *Store) appendEvalDecisionWrite(ctx context.Context, decision *store.EvalDecisionRecord, encoded []byte) error {
	executor := s.taskDataExecutor(ctx)
	var existing string
	err := executor.QueryRowContext(ctx,
		`SELECT record_json FROM eval_decisions WHERE namespace = ? AND candidate_run_uid = ? AND id = ?`,
		decision.Namespace, decision.CandidateRunUID, decision.ID).Scan(&existing)
	switch {
	case err == nil:
		if !bytes.Equal([]byte(existing), encoded) {
			return fmt.Errorf("%w: eval decision %s already holds a different record", store.ErrDuplicateMismatch, decision.ID)
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	outcome, err := getEvalExperimentOutcome(ctx, executor, decision.Namespace, decision.CandidateRunUID)
	if err != nil {
		return err
	}
	if decision.Decision == store.EvalDecisionApproved {
		if outcome.Result != store.EvalExperimentResultPassed {
			return fmt.Errorf("%w: experiment %s/%s did not pass", store.ErrConflict, decision.Namespace, decision.CandidateRun)
		}
		if outcome.Binding == nil || *outcome.Binding != *decision.Binding {
			return fmt.Errorf("%w: the approval binds to a different experiment", store.ErrConflict)
		}
	}
	_, err = executor.ExecContext(ctx,
		`INSERT INTO eval_decisions (namespace, candidate_run_uid, id, record_json, decided_at) VALUES (?, ?, ?, ?, ?)`,
		decision.Namespace, decision.CandidateRunUID, decision.ID, string(encoded), decision.DecidedAt)
	return err
}

// evalExperimentCursor is the position after the last experiment of a page.
type evalExperimentCursor struct {
	ReservedAt      time.Time `json:"reservedAt"`
	CandidateRunUID string    `json:"candidateRunUID"`
}

func encodeEvalExperimentCursor(record store.EvalExperimentRecord) string {
	encoded, _ := json.Marshal(evalExperimentCursor{ReservedAt: record.ReservedAt.UTC(), CandidateRunUID: record.CandidateRunUID})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeEvalExperimentCursor(after string) (*evalExperimentCursor, error) {
	if after == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(after)
	var cursor evalExperimentCursor
	if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.ReservedAt.IsZero() || cursor.CandidateRunUID == "" {
		return nil, store.ValidationErrorf("eval experiment listing position is invalid")
	}
	cursor.ReservedAt = cursor.ReservedAt.UTC()
	return &cursor, nil
}

// ListEvalExperiments returns one page of experiments with their outcomes
// and decisions, read from one snapshot.
func (s *Store) ListEvalExperiments(ctx context.Context, namespace string, options store.EvalExperimentListOptions) (*store.EvalExperimentPage, error) {
	limit := options.Limit
	if limit == 0 {
		limit = store.DefaultEvalExperimentListLimit
	}
	if limit < 0 || limit > store.MaxEvalExperimentListLimit {
		return nil, store.ValidationErrorf("eval experiment listing limit must be 1 to %d", store.MaxEvalExperimentListLimit)
	}
	cursor, err := decodeEvalExperimentCursor(options.After)
	if err != nil {
		return nil, err
	}
	page := &store.EvalExperimentPage{Items: []store.EvalExperimentEntry{}}
	err = s.readSnapshot(ctx, func(executor taskDataExecutor) error {
		page.Items, page.Next = page.Items[:0], ""
		return listEvalExperimentsRead(ctx, executor, namespace, options.CandidateRunUID, cursor, limit, page)
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func listEvalExperimentsRead(ctx context.Context, executor taskDataExecutor, namespace, candidateRunUID string,
	cursor *evalExperimentCursor, limit int, page *store.EvalExperimentPage) error {
	query := `SELECT e.record_json, o.record_json FROM eval_experiments e
		LEFT JOIN eval_experiment_outcomes o ON o.namespace = e.namespace AND o.candidate_run_uid = e.candidate_run_uid
		WHERE e.namespace = ?`
	args := []any{namespace}
	if candidateRunUID != "" {
		query += ` AND e.candidate_run_uid = ?`
		args = append(args, candidateRunUID)
	}
	if cursor != nil {
		query += ` AND (e.reserved_at > ? OR (e.reserved_at = ? AND e.candidate_run_uid > ?))`
		args = append(args, cursor.ReservedAt, cursor.ReservedAt, cursor.CandidateRunUID)
	}
	rows, err := executor.QueryContext(ctx, query+` ORDER BY e.reserved_at, e.candidate_run_uid LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	index := map[string]int{}
	for rows.Next() {
		var experimentJSON string
		var outcomeJSON sql.NullString
		if err := rows.Scan(&experimentJSON, &outcomeJSON); err != nil {
			return err
		}
		var entry store.EvalExperimentEntry
		if err := json.Unmarshal([]byte(experimentJSON), &entry.Experiment); err != nil {
			return fmt.Errorf("decode eval experiment record: %w", err)
		}
		if outcomeJSON.Valid {
			entry.Outcome = &store.EvalExperimentOutcome{}
			if err := json.Unmarshal([]byte(outcomeJSON.String), entry.Outcome); err != nil {
				return fmt.Errorf("decode eval experiment outcome: %w", err)
			}
		}
		index[entry.Experiment.CandidateRunUID] = len(page.Items)
		page.Items = append(page.Items, entry)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = encodeEvalExperimentCursor(page.Items[limit-1].Experiment)
	}
	if len(page.Items) == 0 {
		return nil
	}
	uids := make([]any, 0, len(page.Items)+1)
	uids = append(uids, namespace)
	for _, entry := range page.Items {
		uids = append(uids, entry.Experiment.CandidateRunUID)
	}
	decisionRows, err := executor.QueryContext(ctx,
		`SELECT candidate_run_uid, record_json FROM eval_decisions WHERE namespace = ? AND candidate_run_uid IN (?`+
			strings.Repeat(", ?", len(page.Items)-1)+`) ORDER BY seq`, uids...)
	if err != nil {
		return err
	}
	defer func() { _ = decisionRows.Close() }()
	for decisionRows.Next() {
		var uid, encoded string
		if err := decisionRows.Scan(&uid, &encoded); err != nil {
			return err
		}
		var decision store.EvalDecisionRecord
		if err := json.Unmarshal([]byte(encoded), &decision); err != nil {
			return fmt.Errorf("decode eval decision: %w", err)
		}
		if i, ok := index[uid]; ok && i < len(page.Items) {
			page.Items[i].Decisions = append(page.Items[i].Decisions, decision)
		}
	}
	return decisionRows.Err()
}

func requireEvalExperimentReservation(ctx context.Context, executor taskDataExecutor, namespace, candidateRunUID string) error {
	var one int
	err := executor.QueryRowContext(ctx,
		`SELECT 1 FROM eval_experiments WHERE namespace = ? AND candidate_run_uid = ?`, namespace, candidateRunUID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no experiment reservation for candidate %s/%s", store.ErrNotFound, namespace, candidateRunUID)
	}
	return err
}

func getEvalExperimentOutcome(ctx context.Context, executor taskDataExecutor, namespace, candidateRunUID string) (*store.EvalExperimentOutcome, error) {
	var encoded string
	err := executor.QueryRowContext(ctx,
		`SELECT record_json FROM eval_experiment_outcomes WHERE namespace = ? AND candidate_run_uid = ?`,
		namespace, candidateRunUID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: experiment %s/%s has no outcome", store.ErrNotFound, namespace, candidateRunUID)
	}
	if err != nil {
		return nil, err
	}
	var outcome store.EvalExperimentOutcome
	if err := json.Unmarshal([]byte(encoded), &outcome); err != nil {
		return nil, fmt.Errorf("decode eval experiment outcome: %w", err)
	}
	return &outcome, nil
}
